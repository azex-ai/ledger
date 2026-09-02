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
