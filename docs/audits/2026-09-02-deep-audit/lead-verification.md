# Team Lead 复核记录 — 2026-09-02 深度审计

> 每条 Critical / Major 在进入处置表前由 lead 亲手复核。「报告说红不算数，我自己看它红过才算。」
> 基线 commit `fd87cae`。临时复现测试用后即删，工作树不留改动。

## C-Critical-1 · 提现闸只验签名，放款金额取自可篡改的 `balance_checkpoints` — **CONFIRMED（实跑）**

- 来源：`tamper-evident.md`（territory C）
- 位置：`postgres/reserver_store.go:206`（`requireVerifiedAvailableBalance` 丢弃 `VerifiedBalance` 返回的 entries-only 金额）→ `:274-295`（`reserveWithQueries` 用 `sumBalancesByRoleWithQueries` → `ListComputedBalancesForHolders`，即 `COALESCE(cp.balance,0) + SUM(entries > cp.last_entry_id)`，`checkpoints.sql:55-58`）
- 复现（临时测试 `postgres/zz_lead_repro_test.go`，已删）：
  1. 签名存入 1000（`LedgerStore.WithAuth(attestor).PostJournal`）
  2. 以 pool 凭证 `INSERT INTO balance_checkpoints (...) VALUES (holder, cur, avail_cls, 1000000, 0)`
  3. `VerifiedBalance` 返回 **1000**（正确，entries-only）
  4. `Reserve(Amount=500000, RequireVerifiedBalance=true)` → **成功**，返回 reservation `01a05ffe-…` 金额 500000
- 结论：闸门是二元的（「所有贡献 journal 都签了吗」），可预留金额仍来自 checkpoint+delta。设计 `2026-08-21-tamper-evident-ledger-design.md:26` 明写「checkpoint 是不可信缓存，提现路径全量重算，不读 checkpoint」；`ledger.go:544-548` 的 godoc 也说提现路径必须走 `RecomputeBalance`。**实现没有兑现设计。**
- 为什么现有 pin 没拦：`TestReserve_RequireVerifiedBalance_AllowsWhenEverythingSigned`（`verified_balance_pin_test.go:336`）只在 checkpoint 为空的新库上跑，checkpoint+delta 与 entries-only 恰好相等。
- 定级：**Critical**（持 `ledger_app` 的攻击者在全部签名有效前提下抽走远超真实余额的资金——正是这个库为之而建的威胁模型）。

## C 的 Major 抽核（读码，均 CONFIRMED）

| 条目 | 核到的证据 |
|---|---|
| M-1 `VerifyLedger` 第 4 步采最旧而非最新 | `service/attest_verify.go:399-400` 注释写 "most recent"，调 `ListJournals(ctx, "", N)`；`journals.sql:96-100` 是 `WHERE id > cursor ORDER BY id ASC LIMIT n`，cursor 空 = 0 → 恒为最早 N 条 |
| M-3 锚回退 / 抹掉判 DRIFT 且 CLI 退出 0 | `attest_verify.go:465-467` `anchorSeq < maxSeqSeen` → `VerifyStatusDrift`；`cmd/ledger-cli/main.go:349-351` 只对 TAMPERED / NOT_RUN `os.Exit(1)`；`anchorSeq==0` 时 `:382` 的 head 比对永不触发 |
| M-8 `unauthorized_journals` 零条已签名仍 `Passed=true, Complete=true` | `service/reconcile.go:1458-1460` 未签名 `continue` 不计 `checked`；`:1505-1508` 只要未打满页就 `Passed` 保持 true，finding 文案里 `checked` 可为 0 |
| M-7 worker 无 recover | `grep -rn 'recover()' service/` = 0 命中；NaN 通过 `NUMERIC CHECK (>0)` 由 C 在临时容器实测。Go 侧 panic 未实跑 |

## H / J / B 顶部发现抽核（读码，均 CONFIRMED）

| 来源 | 条目 | 核到的证据 |
|---|---|---|
| H | `GET /snapshots` 的 openapi query 参数与 handler 三处全错 | `docs/openapi.yaml:1256-1263` 声明 `currency` / `from` / `to`；`server/handler_system.go:190-198` 读 `currency_uid` / `start` / `end`，缺失即 400。`openapi_contract_test.go` 只反射 struct，不读 `parameters` |
| H | 出站 webhook 绕过 httpx 的 UTC 序列化 | `service/delivery/webhook.go:235` 直接 `json.Marshal(evt.Event)`；`pkg/httpx/response_test.go:195-214` 明说 pgx 会交回带本地偏移的 time，httpx 层专为此做了 UTC 归一，webhook 路径不经过它 |
| J | `BalancesPage` 的 30 天趋势图恒 400、静默消失 | `web/.../BalancesPage.tsx:38-43` 的 `snapParams` 只有 `holder/start/end`，无 `currency_uid`；`handler_system.go:191-195` 对 `currency_uid` 硬必填；调用点只解构 `data`，不读 `isError`。两个 skin 对称地错，skin-parity 门禁按构造为绿 |
| B | `ConfirmPending`/`CancelPending` 锁序与全仓相反 → 可互锁 | `postgres/pending_store.go:248-251` 只预锁 `(holder, cur)`，随后 `PostJournal` 按全局升序先要 `(-holder, cur)`；并发 `AddPending`（先拿 `-holder` 再要 `holder`）即 ABBA。B 已实跑复现 40P01（探针已删） |
| B | `ClosePeriod` 无锁无屏障，I-15 在并发下不成立 | `postgres/period_close_store.go:50` 单条 INSERT；`postgres/ledger_store.go:951-960` 的关账闸是 READ COMMITTED 下的普通读；越线 journal 无对账检查兜底 |

## 流程发现（lead 自记）

多个 agent 在**同一工作树**上做 mutation test（`MUT:` 反转 → 跑 → 还原），窗口期内其他 agent 的 `go test` 会拿到污染结果。B 报告点名了两次投毒窗口。契约 §2 允许「可逆反转」但没要求隔离到 worktree —— 这是契约缺陷，下轮改为「反转实验一律在独立 worktree」。已即时通知 F。

## A-Critical-1 · `capital_injection` / `capital_withdraw` 的 custodial 腿方向反 — **CONFIRMED（读码 + A 实跑）**

- `presets/templates.go:71` 声明 `custodial` 为 `NormalSideCredit`；`deposit_confirm` 等全部入金模板用 **CR** custodial 表示托管资产增加（`templates.go:134,145,163`）。`presets/capital.go:35` 的 `capital_injection` 却 **DR** custodial → 托管余额减 1000；`:45` 的 `capital_withdraw` 对称反。`GetSystemSideCustodialBalance`（`platform_balances.sql:99-127`）直接消费这个数，`SolvencyCheck` 因此被注资钉成 `solvent=false`。
- A 实跑数字：存 500 + 注资 1000 → `custodial=-500 margin=-1000 solvent=false`（应为 1500 / +1000 / true）。
- 与上轮 C7 同源（按标准会计「DR 资产=增加」写的，而本库把 custodial 定为 credit-normal），C7 修了三个 preset 没横扫其余。`presets/capital_test.go:73` 的 pin 断言的正是错误方向——**修对会让测试变红**。

## A-Critical-2 · 手写 `ReversalOfUID` 让 C8「冲销剩余全部静默少退」复活 — **CONFIRMED（实跑）**

- 临时测试 `postgres/zz_lead_repro2_test.go`（已删）：存 100 → `PostJournal` 一张四腿净零、标 `ReversalOfUID=J` 的合法 journal（余额仍 100）→ `ReverseJournalFraction(J,1,1)` 返回 nil，只冲了 50 → **余额剩 50**。
- 根因：`postgres/ledger_store.go:931-945` 对 `ReversalOfUID` 只查「uid 存在」；`reversal_fraction_store.go:518-536` 把所有 `reversal_of=J` 的 entries 当既有冲销。`ReverseJournal*` 有「原 journal 本身是冲销 → ErrConflict」的护栏，`PostJournal` 没有对称护栏。
- 触发面：库模式专有（HTTP `postJournalRequest` 不接受该字段）。不需要攻击者，一个为可审计性给自家更正 journal 打 `reversal_of` 标签的消费方就会撞上。I-2 只承诺上界，本例累计恰等于原额，pin 必绿。

## G-Critical-1 · 前向扫链在 `IngestDeposit` 失败后照样推进游标 — **CONFIRMED（读码）**

- `service/onchain.go:1246-1250`：`for _, s := range sightings { if _, err := o.IngestDeposit(ctx, s); err != nil { o.log().Error(...) } }`，随后 `:1252` 无条件 `SetCursor(ctx, chainID, to)`。任何非 `ErrConflict` 的失败（DB 抖动 / 币种解析失败 / 精度拒绝）只留一行日志（默认 NopLogger → 丢弃），无死信、无指标，游标不回头 → 真实充值永久丢失。
- 同文件 registration rescan 路径失败即不推进，两条路径失败语义相反——上轮 scanner 层那条 Major 的 service 层同形。

## G-Critical-2 · `TxLogSeq` 是「本次查询结果集内」的序号，同一笔 tx 在 watcher 与 rescan 下派生出不同幂等键 — **CONFIRMED（读码）**

- `chains/evm/reader.go:143-154`：`seqByTx` 在**过滤后**（跳过未注册 token `:145`、跳过畸形日志 `:150`）按本次 `allLogs` 递增；`FetchDeposits` 的地址过滤集在 watcher（全量地址）与 rescan（单地址）下不同 → 同一 tx 内有多笔转入不同注册地址时序号漂移。`service/onchain.go:651` 直接用它拼 `depositIdempotencyKey` → 双记账或进死信。I-20 的三条 pin 没有一条经过 `FetchDeposits`。

## D-Critical-1 · `dev_credit` 不在 `ProtectedTemplateCodes()`，write-scope key 经 `/journals/template` 可铸币 — **CONFIRMED（实跑 httptest）**

- 临时测试 `server/zz_lead_repro3_test.go`（已删，mock 层，默认保护配置）：
  ```
  POST /journals/template dev_credit         -> HTTP 201, ExecuteTemplate called with "dev_credit"
  POST /journals/template capital_injection  -> HTTP 201, ExecuteTemplate called with "capital_injection"
  POST /journals/template fee_charge         -> HTTP 201, ExecuteTemplate called with "fee_charge"
  ```
- `presets/protected_templates.go:37-44` 只列四个 deposit 码；`server/handler_journals.go:237-247` 只查这张名单。`/dev/credits` 的 admin + `ENV=dev` + `DevCreditEnabled` 三重门禁在生成同一张 journal 的另一个端点上不存在。同一 handler 里 `handlePostJournal` 用的是结构性 `is_system` 判据（`:166-176`），`handlePostTemplate` 用手工名单——两条防线形状不同，是上轮 `grant_coverage_test`「同源派生」形态在 HTTP 层重开。
- 附带：`capital_injection` / `fee_charge` 等所有触碰 system 分类的模板同样放行——上轮「write-scope key 能铸造不可区分入账」那条 Major 只关了四个 deposit 码。

## 第二批 Major 抽核（读码）

| 来源 | 条目 | 判定 | 证据 |
|---|---|---|---|
| A | `balance_trends.sql` 是第 18 处符号实现且方向错 | CONFIRMED | `balance_trends.sql:42-43` 按 `entry_type` 直接判 inflow=credit / outflow=debit，不看 `normal_side`；用户 `main_wallet` 是 debit-normal，存款（DR）被记成 outflow。I-42 的 Go 侧收敛没扫 SQL |
| A | `fees` 收入账被两个模板写相反符号 | CONFIRMED | `presets/fee.go:26` 声明 `fees` credit-normal；`fee.go:54` `fee_charge` **DR** fees（减少），`settlement.go:53` `checkout_settlement_*` **CR** fees（增加）。两笔手续费相消 |
| I | `core.Metrics` 账本写入路径零仪表 | CONFIRMED（抽 5） | `JournalPosted` / `JournalFailed` / `IdempotencyCollision` / `ReserveCreated` / `BookingTransitioned` 在 core/observability/测试之外零调用点；对照 `ChainCursorLag` 有 3 处 |
| D | 006 审计表对 `ledger_app` 开 INSERT，`changed_by/at` 可伪造 | CONFIRMED | `006_threat_model_guard_coverage.up.sql:229,273` `GRANT SELECT, INSERT ... TO ledger_app` |
| G | `GasCeiling` 文档单位 wei、实现按 gwei 比 | CONFIRMED | `core/onchain.go:334` 字段文档写 wei；`chains/evm/sweeper.go:29-31` 明写 GasPrice 归一成 gwei「即 GasCeiling 配置的单位」，`:110` 按 9 位小数归一。两处文档互相矛盾，按 core 文档配 wei 的消费方闸门抬高 10^9 倍 |

## E 顶部发现抽核（读码）

| 条目 | 判定 | 证据 |
|---|---|---|
| 按 README 接线的消费方 worker 零输出，启动报告 / anchor Warn / Subscribe Error 全部落进 NopLogger | CONFIRMED | `ledger.New` 默认 logger 为 `core.NopLogger()`；`examples/fullstack/backend/main.go:121` 仓库自己的示范也传 `core.NopLogger()`。上轮「`svc.Worker` 静默关 job」的修复是打一行启动报告——报告打进了被丢弃的通道 |
| README:419-431「Add a custom lifecycle」编译过、运行必败：非系统分类必须声明 `balance_role`，而该词在 README + COOKBOOK 命中 0 次 | CONFIRMED | `grep -c 'balance_role\|BalanceRole' README.md docs/COOKBOOK.md` → 0 / 0；`postgres/classification_store.go:190` 注释明说 `ClassificationInput.BalanceRole` 是必填。README 代码块编译门禁看不见运行时 |

## 第三批抽核（读码，均 CONFIRMED）

| 来源 | 条目 | 证据 |
|---|---|---|
| B | `TestExecuteTemplateBatch_GlobalLockOrder_PreventsCrossJournalDeadlock` 从不调用被测函数 | `postgres/lock_order_test.go:172-222` 内 `ExecuteTemplateBatch(` 出现 0 次；删掉锁序修复该 pin 照样绿 |
| I | `ledger-cli` 自称 read-only，`reconcile --full` 会写生产库 | `cmd/ledger-cli/main.go:1,65` 声明 read-only；`:250` 调 `RunFullReconciliation`；`service/reconcile.go:808,847,893,904,919` 五处 `SetScanCursor` 写 `reconcile_scan_cursor`。DR 演练把这个「只读」当依据 |
| 三方独立命中 | `service/onchain.go:1246-1252` ingest 失败仍推进游标 | G 报 Critical，B 与 I 各在移交段独立点名同一行号——本轮信号最强的一条，与上轮 C1 被六个 territory 命中同形 |

## 清树重跑（F 交付后）

F 报告其反转实验共 19 个污染窗口，窗口内他人的测试结论应视为不可信。在 `git diff fd87cae --stat` 为空、`git status` 只剩 docs/ 新文件的状态下，把 lead 的三条实跑复现重跑一遍（临时测试已删）：

| 条目 | 清树结果 |
|---|---|
| C-1 伪造 checkpoint vs 验签闸 | **复现**：reserved 500000 |
| C-3 手写 `ReversalOfUID` | **复现**：全额冲销后 balance=50 |
| C-6 `dev_credit` 经 `/journals/template` | **复现**：HTTP 201，`ExecuteTemplate` 被调 |

三条 Critical 的实跑结论不受污染窗口影响。B 的 40P01 死锁复现与 A 的仓外容器实跑未由 lead 重跑，但两者的代码依据（`pending_store.go:248` 锁序、`capital.go:35` 方向）均已独立读码核实。

# 整改合并记录（Wave 1 起）

| 任务 | 分支 commit | lead 证伪 | 合入 | 备注 |
|---|---|---|---|---|
| W1-gate | `f48ae31` + `c7a8a7d` | 拆回基线两条 pin 红；复核发现闸开启路径把 availableBase 挪到锁外（stale-high 超卖），退回补 `min(V, E)`，mutation 实跑红 | `a9993fe` | 关 C-C1 / B-M2(store) / C-m7 / A-N6；新 I-49 |
| W1-templates | `a4f1364` + `71d2d55` | 拆回基线：RefusesTheAuditedMintingCodes / UnknownTemplateCode / DepositTolerance 两条 pin 全红；server 包 -race 绿 | 见 merge commit | 关 D-C1 / D-m9 / H-M3(Go 侧) / §7.11 deposit-tolerance；`server_test.go` 既有形状测试显式 opt-in（登记） |
| W1-ledgerstore | `63fb592`（+ EventUID 补修进行中） | 拆回基线三文件：7 条 pin 全红（含两个真 40P01）；postgres -race 绿 | 待合（等 I-50） | 兄弟扫描挖出 `EventUID` 同形，并入 I-51 |
| W1-facade | `4eaa335` | 整文件回退无法编译（新 API 被测试引用），改单点 mutation：`Worker()` 的 `s.tx` 守卫置 false → `RefusedOnTxBoundClone` + `SubscribeAfterRunIsAnError` 红；恢复后根包 + service -race 绿 | 待合（排 I-54，最后） | 一次 `go test . -run Worker` 出现过一次未定位的 FAIL，重跑全 PASS，合并时再跑全量核 flake |
| W1-ledgerstore（补） | `46386e7` | EventUID 维度检查单点置 false → `TestPostJournal_EventUID_RejectsUnrelatedJournal` 红；恢复后绿 | 待合（等 I-50） | I-51 rule 4。lead 失误：这行三次被误提交到 worker 分支（shell cwd 停在 worktree），已全部 reset；教训：跨 worktree 操作每段命令显式 `cd` |
| W1-sign | `2f7bd1e` `c705e12` `ea54bd7`（rebase 后）+ `edbd994`（lead 改号 I-49→I-50） | 拆回基线 presets/*.go + balance_trends.sql + holder.sql：presets 5 条 + solvency 全模板 pin + 符号 gate 红；rebase 后 make vet/lint/test 17 包全绿 | ff 至 `edbd994` | 关 A-C1 及 A 全部 Major/Minor；migration 016；I-50。§7.2 §7.3 有偏离（已接受，见 worker 回执）；equity 改 debit-normal 待 Aaron 确认 |
| W1-ledgerstore | `63fb592` + `46386e7` | 见上两行 | merge commit（无冲突） | I-51（含 EventUID rule 4） |
| W1-onchain | `0d31368` `0b13dc1` `0774ef3`（rebase 后；lead 调整了 I-52/I-53 在 INVARIANTS 中的位置到 I-51 之后） | hold-cursor 守卫单点置 false → `TestOnchain_Watch_HoldsCursorWhenIngestFails` 红；service -race 绿；make test 17 包绿；chains/evm 含 `-tags e2e` 绿 | ff 至 `0774ef3` | 关 G-C1 G-C2 及 G 全部；I-20 重写；I-52 I-53。G-C2 按 receipt 内零基位置落地（契约 §7.9 措辞已订正）。跨面接触：`grant_coverage_test.go` 新表分类、I-41 Pinned by 三行、`ledger.go` 两行接线（接受） |
| W1-facade | `d2aaf3a` `34d6b57`（两次 rebase 后） | 单点 mutation 两 pin 红（lead）；worker 合并后重跑 4 条关键反转全红；-race 17 包绿含 gapless | ff 至 `34d6b57` | I-54；`WithCustodialClassCodes`；Wave 1 收口 |

# Wave 2 复核

| 任务 | 分支 commit | lead 证伪 | 合入 | 备注 |
|---|---|---|---|---|
| D-tests（第一版） | `d2940b7` | 删掉 I-6 的**一条** pin bullet → 绿（预期，仍有其余 pin）；删光 I-6 **全部** bullet 只留 `**Pinned by**` 标题 → **仍绿**：新 gate 只查标题存在（`invariants_pins_test.go:290`），是「守形状不守语义」形态在 gate 自身上重现。已退回加强 | 待 | F-m2 advisory 32 条要求改成显式只缩不扩白名单；I-7 Exceptions 扩写 → W3-lead |
| D-surface | `798e2e3` | README 代码块改错方法名 → `TestREADMECustomLifecycleRuns` 红；根包 gate 全跑 | merge commit + lead 在 main 删 CLAUDE.md 的 helm 行（I-N22 半条，已知会 d-ops） | 5 个新 gate；examples 全部 exit 0（worker 记录） |
| D-tests（终版） | `9c7d249` | 同一 mutation（删光 I-6 pin bullet）→ `TestInvariantsDocEveryInvariantHasPinnedBy` 红；main 合并后 make test 全绿 | merge commit | 首版被退回一次：gate 只查标题 |
| D-tamper | `86a3988` 系列（rebase 后 8 commit）+ lead `d612221`（I-56 Enforced-by 引用补全，过 d-tests 新门禁） | 空锚守卫置 false → 两条 NOT_RUN pin 红；service/postgres -race、anchors/r2 绿；make test 除 pin 引用门禁外绿 → 引用补全后全绿 | ff | 关 C 全部 Major/Minor；I-55 I-56；migration 018；C-R1 记为披露残余限制；worker 因额度中断，收尾由 lead 完成 |
| D-lock（验证完，待合） | `b67aff9` 系列（rebase 后 11 commit）+ lead `a2e9f76`（I-61→I-59 改号、契约 §4 D-ops 号段对调为 I-60/61） | 生成 SQL 里把 `pg_advisory_xact_lock_shared('period:close')` 换成 `true` → 根包 `TestClosePeriod_WaitsForInFlightBackdatedJournal` 红；恢复后绿 | 等 d-threat（57/58）合入后合 | 一处测试冲突（`reconcile_full_test.go` 的 check 计数断言）取 main 侧；合并时需 `git apply .team/dlock-changelog-unreleased.patch` |
| D-web | `c469ae8` 系列（4 commit，rebase 后） | BalancesPage 忽略 `isError` 的 mutation → `surfaces ErrorState when the trend request fails` 红；恢复后 ledger-react 11 文件全绿。main 上首跑 7 条红全是读 `dist/` 的门禁，`npm run build` 后 41 文件 225 用例全绿（dist 门禁依赖先 build，W3 记进 web/CLAUDE.md） | ff | 关 J 全部（J-12 部分闭合：a11y census 21→55，gate 改跨 skin 相对断言；J-23 CI 步骤移交）。openapi 变更后的 `schema.ts` 再生成由 lead 在 d-contract 合入后做 |
| D-threat | `affe13e` 系列 + 接手 `6d2210c`（6 commit，rebase 后） | `revokeLedgerOwner` 改为吞错 → 两条失败路径 pin 红；core / postgres（ownership、bootstrap、audit、guard、grant、nonce）-race 绿；server -race 绿 | ff | 关 D 全部 + F-M7 + A-N2 + G-m5；I-57 I-58；migration 019 020 021；007 有意修改（契约 §8）；migrate.go 逐 migration 提权窗口——**W3 对抗式复审必审**；顺带发现 018 尾部 REVOKE 会撤掉提权窗口（W3 清理 + 静态门禁） |
| D-lock | `b67aff9` 系列（rebase 后 11 commit）+ lead `a2e9f76`（改号 I-59）+ lead `1d4cb45`（`AcquireClusterLockForTest` 保留单参形状供 d-threat 两个测试；CHANGELOG 补丁） | 见上一行（关账共享锁换 `true` → 屏障 pin 红） | ff | 关 B-M5 B-m2 B-m3 B-m4 B-m5 B-m9 B-X1；I-59；新对账检查 `period_close_violations`；`migrate.go` 与 d-threat 的改动手工合并（HEAD 文档 + ctx-aware 签名），vet/test 绿 |

**lead 事故（2026-09-03）**：合 d-lock 时 `reconcile_full_test.go` 的冲突我取了 main 侧整个 hunk，丢掉 d-lock 对 check 计数（14→15 / 15→16）的 bump；合并后只跑了 `-run 'PeriodClose|Reconcile'` 过滤集，漏掉 `TestFullReconciliation_AllPass` / `FullCoverageCanBeTrue`，main 红了约一小时，由 d-ops 发现。已修（`4c569ea`）。规矩：**手工解过冲突的包，合并后跑整包不跑过滤集**。
| D-ops | 前任 WIP + 接手 `0cadc16`（6 commit，rebase 后） | worker 的 `recover()` 分支置 false → `TestWorker_JobPanic_DoesNotCrashProcess` 红；postgres 非测试文件的删行核为 struct 字面量重排与 metrics 包装（无语义删除）；main 合并后 make vet/lint/test + 两子模块全绿 | ff | 关 I 全部（I-M7 ②③④、B-m12 转 W3；E-m16 不修）；I-60 I-61；`core.Metrics` 32→41 且改 4 个签名（破坏性，已进 CHANGELOG）；`ledger-cli config-history` |

# Wave 3（lead 直接在 main 上做的收口，每条自带 mutation 证据）

| 项 | commit | 证据 |
|---|---|---|
| 018 自提权 GRANT/REVOKE 移除 + 静态门禁 `TestMigrationsDoNotManageLedgerOwnerMembership` | `c8a2802` | 往 018 追加一条 REVOKE 语句 → 门禁红；非 superuser bootstrap 安装 pin 与 ownership pin 绿 |
| ledger-react CI `npm audit --omit=dev --audit-level=high`；CLAUDE.md 不再硬编码 invariant 数、a11y 写法例外；web/CLAUDE.md dist 门禁先 build | `1ba825e` | 根包 doc gate 绿 |
| I-M7 ②③（scan 停止原因归因、游标持久化用 detached ctx）；INVARIANTS 重复「How to add」标题 | `a88fc29` | — |
| I-M7 pin `TestCheck2GlobalBalance_CallerCancellationIsIncompleteNotFailed`；I-7 Exceptions 补全；I-62 I-63 | `88dd59b` | `scanStopReason` 去掉 caller 分支 → pin 红；INVARIANTS 四条门禁绿（480 处 pin 引用） |
| Makefile / go-verify 超时 5m→15m | `deafc4c` | postgres 包 -race 211s；并行下 302s 曾撞超时假红 |
| D-contract | 前任 10 + 接手 `c8550f5`（14 commit，rebase 后无冲突） | openapi 里把 `/snapshots` 的 `currency_uid` 改名 → `TestOpenAPIContract_ParamsMatchGoHandlers` 红；server -race 绿；make test 18 包绿。合入后 lead 再生成 `schema.ts`（+230/−107），`Booking.expires_at` 手写类型改为 `string \| null`，codegen:check 与 web 225 用例绿 | ff + lead `schema.ts` commit | 关 H 全部 + E-M9 E-M11 + J-6/J-8/J-14 后端侧；H-M6 用 loose index scan + LATERAL（余额读 9,600→102 行）；migration 022 FK；bizcode 14011 起；Go 导出面快照门禁 |

# W3 对抗式复审结论（两名复审员，只读）

| 报告 | 结论 | 处置 |
|---|---|---|
| `w3-review/money-path.md` | 8 个攻击面 3 攻破 1 部分：**I-49 的 hold 项读可写表**（Critical，实跑：闸对 1000 授权 2000）；`SolvencyCheck` / `enforce_min_balance` 仍读 checkpoint；`event_uid` 冒领锁死 booking 无解除；未覆盖伪造 journal 自称 tx_mode 只 DRIFT；`Migrate` 窗口角色级；默认全关无 Warning；`unauthorized_journals` 一页一条签名即跳过其余 | w3-holds（C-1）、w3-fixes（M-2/4/6/7 + m-1..6）、Aaron（M-1/3/5，契约 §7.14） |
| `w3-review/gates.md` | 34 次 mutation，23 处盲区，3 Critical：路由 scope 降级全绿；pin 引用门禁 63 条只 10 条真红；破坏性变更门禁在 CI 恒绿/恒 skip | w3-gates-fixes |

复审证明了契约 §0「兄弟扫描」在执行上仍被形态描述框住：C-1 的兄弟被描述成「读 checkpoint」，真正的形态是「决定放多少钱的算式里有项来自攻击者可写的表」。这条写进最终 README 的方法教训。
| W3-citations | `8a50bf5` 系列（7 commit） | core 全绿；白名单常量已为空（代码事实）。lead 两次 mutation（I-2 去掉全部反引号；I-2 前三个引用改成不存在的符号）**均绿** —— 证实 gates 复审的 C-2：leaves=0 静默跳过、且只要任一引用匹配即通过。已交 w3-gates-fixes 修门禁本身 | merge commit | 28 条 pin 因门禁看不穿本地 helper 被降为 Related tests：后续让门禁追一层本地 helper 后再升回（W3 后续） |

# W3 修复合并

| 任务 | commit | lead 证伪 | 备注 |
|---|---|---|---|
| W3-fixes（复审 M-2/M-4/M-6/M-7 + m-1/2/4/5/6） | `86d6888`（rebase 后 ff） | `unauthorized_journals` 的 `Complete = false` 改 true → 三条 pin 红（前两次 mutation 分别打在注释行与文案分支，不算数，已作废）；service/postgres -race、make test 18 包绿 | m-3 判已由 004 修（新增 ledger_app 视角 pin）；默认 custodial scope 保持「全不命中才报错」、显式 scope 逐 code 严格（接受）；migration 024 = `anchor_observations` SECURITY DEFINER 写入且拒 seq > 链高；M-4 新增 `UncoveredGracePeriod`（默认 5m）与 `UncoveredUnverifiedJournals` |
| W3-holds（复审 C-1） | `55b3731`（v2，(B)+完全保守；v1 的 migration 024 撤掉，改 025） | 生成 SQL 给闸内 hold 加回 `status <> 'released'` 依赖 → `HoldSurvivesStatusTamper` 红；恢复后 7 条 pin -race 绿；make test 18 包绿（worker） | ff | 闸内 hold = Σ 未过期预留 `reserved_amount`，不读 status/settled/receipt/leg；`Settle`/`SettlePartial` 拒绝过期，`FinalizeSettlement`/`Release` 不拒（过期回收依赖它）；(A) 签名 receipt 待 Aaron（§7.14）；worker 自查出一版假绿 pin（用真 Settle 探测过期）并修正 |
| W3-fixes-2（复审 M-3 + M-5 文档层） | `1d86c26`（rebase 后 ff） | migration 027 追加 `GRANT EXECUTE ... TO ledger_app` → `UnlinkEventJournalIsRefusedForLedgerApp` 红；恢复后 postgres 相关 -race 绿、examples 静态门禁绿 | ff | M-3：owner-only `ledger_unlink_event_journal` + 守卫「只允许非 NULL→NULL 且 owner 角色 + 事务内 flag」双条件；M-5 文档层：8 个 example 拆 `MIGRATE_DATABASE_URL`，静态门禁；**M-5 实跑定级 Critical**（应用连接迁移中途 `DROP TRIGGER journal_entries_no_update` 成功、迁移返回 nil、守卫永久消失） |
| W3-gates-fixes（复审 gates 23 处盲区） | `afce47a` 系列（11 commit，rebase 后；I-49 冲突取 main 侧——w3-holds 版已含 `postgres.ReserverStore.Reserve` 引用） | 把 `PUT /accounts/{holder}/policy` 从 admin 组挪进 write 组 → `TestRouteAuthorization_RequiredScopeMatchesOpenAPISpec` 红；恢复后 server -race 绿，make test 18 包绿 | ff | 路由→scope 门禁运行中间件链对照 openapi `x-required-scope`；pin 引用门禁白名单恒空 + 跳过登记制 + I-32/49/52/53/54 全部 blocking；破坏性变更门禁对照上一 release tag + `fetch-depth: 0`；openapi 参数 required/类型从 handler AST 派生；schema 金额列按列名派生；审计覆盖从权限派生；ownership 扫全 schema；metrics 发射用 go/ast 计数（又抓出第 5 条零测试指标） |

**W3-gates-fixes 的 7 条默认拍板（lead 全部接受，2026-09-03）**：openapi 加 `x-required-scope`/`x-required-capability`（期望表进契约而非测试）；`docs/BREAKING.md` + CHANGELOG 补 21 条自 v0.6.0 未记录的破坏性变更（lead 抽核两条：`channel/onchain.New` 现返 `(*EVMAdapter, error)`、`server.New` 已无 `snapshotter`/`systemRollup` 参数——属实）；web hardening ratio 0.8→0.74（旧基线被注释灌水）；ReconciliationPage onError 跨 skin 真分叉记豁免；`SweepUnattributed`/`PendingEvents` 登记 untested 带理由；19 条无 Go 面的 invariant 登记（第二轮改为必须指明 migration 机制）；web `npm test` 要求先 build（dist 过期即红）。
| W3-gates-fixes-2（pin 引用门禁残留） | `816e274` → `08ba366` → `cf7d13d`（3 commit，ff） | lead 三轮 mutation：I-2 去掉全部反引号（第一版红）；I-2 引用改不存在符号（第一版绿——只查带包名引用；第三版红）；I-1 裸 `VerifyJournalBalanced`→`Zz…`（第二版绿；第三版红）。core 与根包 doc gate 绿 | ff | 引用解析：pkg.Sym 对声明；裸 CamelCase 对声明/使用/字符串常量；≥6 字符含下划线的 snake_case 对全部 .sql/.go 文本；db-only 登记必须指明 migration 与对象名并被校验；顺带修 I-16/I-24/I-33 等真实断链 |
| W3-gates-fixes-3（裸引用「必须是本仓声明」） | `8e0d63e`（ff） | I-1 `VerifyJournalBalanced`→`Zz…` 红；核心 + 根包 doc gate 绿 | ff | 解析标准从「本仓 mention 过」收紧到「本仓声明」；anchortest 阶段名与 `s3.ListObjectsV2` 改为非符号写法 |
| W3-migrate（复审 M-5 机制层，Critical） | `152fc53` + `8b7443f`（rebase 后 ff；gate 测试冲突取 main 的语句级实现） | `assertSoleSessionOnCredential` 置为恒 nil → `RefusesWhileAnotherSessionHoldsTheMigrationCredential` 红；worker 变异 INHERIT TRUE → 窗口探针双红；migration 测试族 -race 绿；make lint 0、make test 18 包绿 | ff | 三层：SET-only/INHERIT FALSE 成员 + 专用连接 `SET ROLE` + `pg_stat_activity` 同角色会话守卫（GRANT 前后各一次，fail-closed）；BREAKING：单凭证在线 pod 内迁移被拒绝；`RESET ROLE` 以关闭连接代替（接受）；019 注释过期由 lead 在 main 上改（文本，§8） |
| W3-migrate-residual | `da936d2` | 测试与文档：把迁移中途新开连接可显式 `SET ROLE` 的残留钉成实测（谁关掉它 pin 就红、要求同步改文档）+ 边界断言（Migrate 返回后同一连接 `SET ROLE` → 42501）；migration 测试族 -race 绿 | merge commit | lead 事故：在 worker 尚在跑 `make test` 时 force 删了它的 worktree，导致 4 条读磁盘的静态门禁假红并丢了它未提交的残留 pin（worker 已在新 worktree 重做）。规矩：删 worktree 前确认 worker 已 done 且无未提交改动 |

# Wave 4 复核

| 任务 | commit | lead 证伪 | 备注 |
|---|---|---|---|
| W4-pending-gate（§7.18 E 项 + §7.20 V 项） | `47d4d23` + `d89d8fc` | `decimal.Min(V, E)` 换成 E → `VerifiedBaseCapsEntriesForgedInTheWindow` 红；worker 的 `if false` 注入 → 三条 V pin 红；postgres pending 族 -race、core、根包绿 | ff | `ConfirmPending` = min(V,E)（偏离「锁内仍用 E」，接受：与 I-49 同形，窗口内伪造靛 V 封顶）；tx 模式 + Attestor fail-closed；Attestor 配了但 verifier nil 视为 V-on（接受）；`NewPendingStore` 加位置参数（破坏性）；I-17 划定 `enforce_min_balance` 信任边界；I-64 |
| W4-signed-holds（§7.18 方案 A） | `fd3ad25` `2c56d98` `c854c6e`（ff） | 两处 `VerifyReservationDischargeAuth` 检查置 false → `ForgedClaimDischargesNothing` + `RejectsTamperedAmount` 红；reserve 族 -race、core、根包绿；main 合并后 make vet/lint/test 18 包 + 两子模块 + sqlc diff 全绿 | ff | migration 028（receipt/leg 三列签名）；`ReserverStore.WithAuth`（加法）；闸内 hold = Σ max(0, reserved − 验签通过的解除额)，未签/验不过/未配 Attestor 退回保守；tx 模式 claim 不签（接受）；`verifiedDischarges` 的 nil attestor/verifier 提前返回保留为纵深防御（接受：未配置部署走 028 前代码路径比「新路径恰好同值」更强）；不加新 sentinel（接受：验签失败不跨 API 边界）；I-65；domain separator 0x11 |

# Wave 5 复核（独立 review 后的修复；全部在 lead 自己的 detached worktree `lead-verify-insert` 里做 mutation，不碰 worker 的 worktree）

| 任务 | 分支 commit | lead 证伪 | 合入 | 备注 |
|---|---|---|---|---|
| W5-searchpath（R3 install-roles C1/C2，I-68，migration 030） | `f1ea3c3` | 第一次 mutation 只把 `check_journal_currency_balance()` 的 search_path 翻成 `pg_temp, public` → **仍绿**（打错函数：含 `FROM public.journal_entries` 的是上面的 `ledger_assert_journal_balanced`）；第二次去掉 schema 限定并翻 `ledger_assert_journal_balanced` 的 search_path → `TestBalanceGuard_SurvivesPgTempRelationShadowing` 红，还原后绿 | merge `983436d`（`core/invariants_pins_test.go` 登记表两侧追加，I-66 条目的 `},` 在冲突边界丢失，amend 补回）；postgres 166s 含 030 门禁覆盖 029 的 9 个函数 | 三条裁决：I-68 保号靠合并顺序；`REVOKE TEMPORARY` fail-closed 抛错 + 补救语句（不降 warning）；`ledger_assert_journal_balanced` 对 `ledger_app` GRANT EXECUTE（worker 用 ledger_app 跑 pin 才逮到的 invoker-rights 权限洞） |
| W5-insert（R3 money-out C-1/C-2/M-1/M-4、onchain-ops C-1、install-roles M3/M4，I-66/I-67，migration 029） | `e675c23` → `05a01d1`（+api-surface） | 六组 mutation 全红：bookings 初始状态检查置 false → `BookingIsBornAtTheStartOfItsLifecycle`；attestation `seq <> head+1` 置 false → `AttestationInsertMustExtendTheChain`；cursor 上限置 false → `ChainCursorCannotJumpAndEveryMoveIsRecorded`；审计 DO 循环排除 `account_policies` → `ConfigTableInsertsLeaveAForensicRow`；模板同事务判定置 true → `TemplateLineCannotBeAppendedAfterInstall`；`if false && o.rescanLookback > 0` → `Watch_LookbackRecoversAForgedCursorAdvance` | `bfd5c32`（search_path 统一后）→ merge `6fa7039`；main 上 postgres 161s / service / 根包 / core / sqlc 全绿 | 029 头注明说 **不闭合 money-out C-2**（应用层围栏归 w5-onchain-ops）；顺带发现 027 谓词不 COALESCE 的 fail-open 形态（TODO「Lead 追加」） |
| W5-readme（R3 consumer C-1..、F-M1..M10） | `dbc6c6e` | README 里 `worker, err := svc.Worker(` 改回单返回 → `TestREADMEGoBlocksCompileUnlessMarkedSnippet` 红；readme-gate marker 去掉理由 → 同 gate 红；handler 的 lifecycle 分支置 false → `TestCreateBooking_NoLifecycle` 红；**删掉 worker.go 里把 RuntimeRoleWarning 追加进 Warnings 的那行 → `-run 'RuntimeRole\|StartupReport'` no tests to run（无 pin）** | `2641fd7`（哨兵 + 两条 RuntimeRoleWarning pin + .gitignore；lead 拆掉 Warnings 追加 → 两条 pin 红）→ merge 无冲突；**漏网**：`pkg/httpx` `TestCoreSentinels_AreAllBound` 红——新哨兵未在 resolveError 分类，lead 补 `a614f6b`（映射 10001 band） | README「FullReconciler 常开」核过 `ledger.go:1150` 属实；24 个 go block 中 2 个 marker 豁免（lifecycle 值列表、消费方自写测试模板），其余全部编译 |
| W5-money-misc（R3 money-out M-2/M-3，install-roles M1/M2/M5） | `fcfd9d5` | 五组 mutation 全红：rule 2 读侧置 false → `ReverseJournalFraction_RefusesACorruptReversalChain`；rule 3 读侧置 false → `RefusesAnOverReversedChain`；会话守卫加回 `application_name IS DISTINCT FROM 'azex-ledger-migrate'` → `RefusesASessionClaimingTheMigrationApplicationName`；`prepareBaselineOwnerMembership` 恒早退 → `SecondLedgerDatabaseOnACluster`；同时拆掉 `reserver_store.go:333`/`:377` 两个 role 过滤 → 根包两条 `GatedReserve_*` 红 | `44bd923`（fleet check：unmatched CTE 置 false / over_reversed HAVING 抬高 / check 不接线 三条 mutation 红）→ merge `6e6f93b`；RUNBOOK 两节改 §19/§20，`section 18` 代码引用改 19 并 `sqlc generate`；main 全绿 | **M-2 裁决反转**：我原措辞「不通过的不计入并写 Finding」是出钱方向（被污染 journal 的真腿分不出来），采纳 worker 的拒绝求值；契约 §4 5.7 记 errata |
| W5-onchain-ops（R3 onchain-ops C-2/M-1/M-2/M-3/M-6/M-8/M-9 + money-out C-2 应用层围栏，I-69） | `9374d26` | 八组 mutation 全红：金额比对置 false → `CorroborationRejectsATamperedAmount`；`corroborateBeforeConfirm` 直接返回 corroborated → `ForgedBookingIsNotCredited`；deep miss 阈值置 false → `AutoReverse_WaitsForConsecutiveObservations`；去掉 `DepositIngestDeadLettered` 发射 → `DeadLetter_IsCountedBackloggedAndReplayable`；去掉 `ChainCursorAdvanceAge` defer → `ChainCursorAdvanceAge_GrowsWhenTheTipIsUnreachable`；`countTicks` 恒跳过 → `Run_EveryJobReportsItsTicks`；token 精度比对置 false → `Run_RefusesTokenDecimalsAboveCurrencyExponent`；去掉 `SweepOrphanedBroadcast` 发射 → `Sweep_DoesNotRebroadcastAfterALostTxHash`（第一次 corroborate 绕过插错函数导致 build failed，作废重做） | `299761e`（fixture 改 pending，并为 pending 态加 pending→confirming 过渡后再 review）→ merge `e7ed347`（`service/onchain.go` 三处手工合并：常量块两侧保留、`observeCursor` 与 `scanned` 并存、`reportLag(max(scanned,to))` 前加 `observeCursor`）；postgres/service/server/根包全绿，codegen:check 绿 | RUNBOOK §18 与 w5-money-misc 撞号，合并时后者改 §19/§20；`service/onchain.go` 与 w5-insert 的 lookback 改动手工解 |

**lead 事故（2026-09-04）**：① 合 w5-searchpath 时先 commit 再跑 gofmt，登记表 I-66 条目在冲突边界丢了 `},`，amend 补回——规矩：合并 commit 前先 `gofmt -l` + `go build`。② 一条 `cd web && npm run codegen:check` 让 shell cwd 停在 `web/`，随后三组 `go test` 全部 `[setup failed]` 误判为回归——规矩再犯：每段命令显式 `cd /Users/aaron/projects/ledger`。③ w5-readme 的新哨兵在 main 上才被 `pkg/httpx` 的派生门禁抓到：worker 只跑了改动包，没跑 `make test`；合并前应要求 worker 跑全套或由 lead 在 detached worktree 全跑一次。
| W5-gates（R3 gates-pins F-1..F-9 / P-1..P-3 / P-6 + 24 条 mutation 普查 + I-28 空洞 + 5.14 DoS 修复，I-70） | `d49eb1e`（15 commit）+ `39e13a3`（amount 量级门） | lead 四组门禁抽查全红：001 去掉 `journals.idempotency_key` UNIQUE → `TestJournalIdempotencyKey_RejectsDirectSQLDuplicate`；恢复 rollup 空批提前 return → `TestRollup_ReportsQueueDepthOnAnEmptyTick`；恢复 postgrestest 的 Docker skip → `TestPostgresFixtureFailsRatherThanSkipsWhenDockerIsAbsent`；掏空 `ledger_block_mutation()` → `TestAppendOnlyGuards_EveryTriggerRefusesItsMutation`。DoS 修复两组：`ValidateAmountMagnitude` 恒 nil → 两条 pin 红（100ms 计时上界）；去掉 journal per-entry 调用 → 入口 pin 红；fuzz 种子普通 `go test` 回放绿 | merge `f491efd` + lead `bb54591`（gofmt）+ `0e7512a`（census floor 25→23）| 合并时：gates 的新不变式由 I-66 改号 **I-70**（其分支基线无 66–69）；`unresolvableEnforcedCitations` 被 gates 改为结构化 `dbObject`，main 上 029/030 加的 I-66/I-68 两条旧格式条目由 lead 转成 kind/name 声明并把 `dbOnlyRegisterSize` 13→15（029 的 `_audit_insert` 触发器由 DO 循环生成、无法按声明解析，登记改为字面声明的三个触发器）；`appendOnlyGuardFloor` 25→23（029 把两个 attestation 表的 DELETE 守卫换成 `ledger_attestation_chain_block_delete`，另由 `TestPoisonedAttestationTailHasAWayBack` 钉住）；`service/rollup_test.go` 未 gofmt |

**Wave 5 收口（2026-09-04 01:45 SGT）**：六支全部合入，main `0e7512a` 全量验证绿——`go vet` / `golangci-lint` 0 / `sqlc diff` / postgres 170s -race / service·server·根包·core·其余小包 -race / `chains/evm` / `anchors/r2` / ledger-react codegen:check + build + 41 文件 225 用例。下一步：R3 五名复审员各自对 main `0e7512a` 复核自己的发现（零 Critical 复发才进入发版评估）。

# R3 复核（对 main `5a11533` / 代码 `0e7512a`）

| 复审员 | 结论 | lead 处置 |
|---|---|---|
| r3-install-roles（`6d49d36`，已合入） | 原 14 条：闭合 10 / 部分 3 / 未闭合 3 Minor（m1 m4 m7）/ 复发 0；**新 Critical N1**：`SET CONSTRAINTS ALL IMMEDIATE` 绕过 030 平衡守卫（三形态实测） | 契约 5.15 → w5-searchpath 做 031；m4/m1 并入；m2/m3/m7 记 TODO |
| r3-consumer（`b785a52`，已合入） | Critical 全闭合（机制性）；Major 6 闭合 4 部分（残余全在文档/契约面）；Minor 16 条中 11 未动；新 3 条（N-1 openapi 又多 2 条 api.md 未收录、N-2 首轮存证 ERROR 噪声、N-3 README 门禁 16 个 subtest 与「24 块」表述差 6） | lead 直接修文档/契约面 `f1d3992`：go-get 口径按 GOPROXY 与两个 tidy 原因重写（复审员实测新口径在 goproxy.cn 下不成立、且 `internal/miniotest` 的 replace 确实是第二个 tidy 原因）、`.env.example` 双连接 + `name:scope:secret`、openapi `POST /bookings` 补 400、api.md §2 ingester 说明 + §17 dead-letter 两端点 + transition 示例补 `idempotency_key`、CHANGELOG/fullstack README 陈旧计数。N-3 的 6 块差额 = 6 个由真库测试执行的 block（`readmeRunnableAnchors`），按设计不重复编译；N-2 记 TODO |
| r3-money-out（`c7a23d8`，报告已合入；其 `zz_r3_*` 攻击实验文件留在分支不进 main） | 原 Critical 零复发（C-1 由 029 guard、C-2 由 I-69 corroborate 闭合，反向确认各红 3 条）；M-1 检测半闭合、M-2/M-3 闭合、M-4 部分；m-1 升 CONFIRMED（rollup_queue append 堵 RebuildCheckpoint）；**新 Critical N-1**：同一真实 log 复制成多个 booking 全部签出（含配 DepositConfirmer）；N-2 review 过渡撞 `uq_bookings_channel_ref` 卡 pending；N-3 discard 在 anchor 发布后 → 另一种永久 TAMPERED | 契约 5.16 → w5-onchain-ops 做 032 + corroborate 已入账检查 + N-2/N-3；§5.1「POST /bookings 是否同样可达」并入任务；m-1/m-2/m-3/m-4 记 TODO |
| r3-onchain-ops（`f025d03`，已合入） | 零 Critical 复发；C-1/C-2 部分闭合（残余：新链首次 INSERT 游标无上限；webhook 路径 `ErrConflict` dead-letter 不计数、Backlog 仅 Reader 部署采样）；M-1..M-9 全闭合（6 条经 mutation 验 pin）；Minor 12 条中 7 闭合、5 未处理（DR §5 命令、PITR 丢 `deposit_reorgs`、CAPACITY 零链上覆盖、bash 块塞 SQL、两个死指标）；新问题：RUNBOOK 把 decimals×exponent 闸引用成 I-69。复审员自曝实验事故：曾误连另一 agent 同名容器写入游标行（已重建，无残留） | 契约 5.17 并入 032（w5-onchain-ops）；Minor 记 TODO |
| r3-gates-pins（`9d45234`，已合入） | 零复发：F-1..F-9、P-1..P-6 原 mutation 全部红在门禁本身或新 pin；W5 处置章节 6 条抽查（I-28/I-10/I-53/I-6/P-2/I-70 入口表）全部成立；两处 lead 集成改动判合理且未放松（`appendOnlyGuardFloor` 23、`dbOnlyRegisterSize` 15）；I-70 的 14 入口里 12 个依赖该门、2 个另有独立拒绝路径。新发现 R-4（导出 money helper 同 class 未覆盖 + `FuzzAllocate` 结构性盲）、R-3（release workflow submodule job 无 govulncheck）、R-2（`entry_attestations_no_delete` 无 pin） | 契约 5.18 → w5-gates-2 |
| W5-searchpath-2（5.15 N1，migration 031，m4，m1） | `cb25074`..`3c1c8b0`（6 commit） | lead 两组 mutation：掏空 `check_journal_currency_balance()` 的聚合 → `TestBalanceGuard_RefusedUnderEveryConstraintTiming` 红（3 攻击 × 每个 `tgdeferrable` 触发器的 IMMEDIATE 模式）；m4 改回 `Passed=false` → `TestCheck2GlobalBalance_CursorWriteFailureIsIncompleteNotFailed` 红。worker 自带反向确认 `TestBalanceGuard_TheSkipIsWhatMadeTimingMatter`（装回 030 的 skip → IMMEDIATE 形态必须成功） | merge `083ecb3`；main 上 postgres 208s / service 136s / 根包 / server / cli / 其余包 -race 绿，lint 0，sqlc 干净 | 031 删 xmin 跳过与 journals 级触发器、`ledger_assert_journal_balanced` 及其 EXECUTE grant（ACL 白名单回到 5）；成本表如实进 CHANGELOG/BREAKING；007 文案第三次有意修改（§8 先例）；I-68 登记表改指 031 的 `check_journal_currency_balance` |
