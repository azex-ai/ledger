# 全代码库深度审计（第二轮）— 2026-09-02

> 十个 territory 的只读审计 + lead 交叉分析。契约见 `docs/plans/2026-09-02-deep-audit-contract.md`。
> 基线 commit `fd87cae`（0.6.0 之后，含 R2 anchor、I-42 符号权威收敛、三轮整改）。
>
> **本目录是审计当时的快照。** 各 territory 报告保持交付时原文；本 README 与 `lead-verification.md`
> 是唯一随处置状态维护的两份。上一轮（2026-08-25）的方法与结论见
> `docs/audits/2026-08-25-financial-engineering/README.md`，本轮**不重报**它已关闭的条目。

## 处置状态（2026-09-02 更新）

Aaron 拍板 **全量修复**。契约 `docs/plans/2026-09-02-remediation-contracts.md`，任务清单 `TODO.md`，lead 逐条证伪记录在 `lead-verification.md`。

**Wave 1（六条 Critical + 契约层）已合入五条，第六条待 ff：**

| # | 处置 | 合入 | lead 证伪 |
|---|---|---|---|
| C-1 提现闸金额读 checkpoint | 闸开启时 availableBase = min(锁外验签重算 V, 锁内 entries-only 重算 E)，不读 checkpoint；I-32 措辞改写，新 I-49 | `a9993fe` | 拆修复两 pin 红；min 换 V 红 |
| C-2 capital 方向反 | 五个模板方向修正（capital ×2、settlement ×2、fee_charge）+ equity 改 debit-normal + migration 016 修存量行 + 每模板偿付能力 pin + SQL 符号机械 gate（I-50） | ff `edbd994` | 拆回 presets 5 条 + solvency + gate 红 |
| C-3 ReversalOfUID 零校验 | 冲销链完整性提升为 I-51（含兄弟 EventUID rule 4）；pending / batch 锁序统一为一个 canonical order | `dfa2017` | 拆回三文件 7 pin 红（两个真 40P01）；EventUID 检查置 false 红 |
| C-4 扫链失败推进游标 | 任何 ingest 失败不推进 + 死信 + wedged 告警（I-52）；扫描停在 reorg 可变链尖之后（I-53） | ff `0774ef3` | hold-cursor 守卫置 false 红 |
| C-5 TxLogSeq 依赖地址集 | 改为 receipt 内零基位置，pin 经过 `FetchDeposits`；I-20 重写 | ff `0774ef3` | 随上 |
| C-6 dev_credit 不在名单 | 模板保护改结构派生（任一腿落 is_system 分类即拒），`deposit-tolerance` 同闸 + admin scope（§7.11） | `0c86040` | 拆回 handler/routes 四 pin 红 |
| W1-facade（默认与 clone 边界） | `Worker.Run` 在 NopLogger 下拒绝启动除非 opt-in；clone 逃逸四入口全堵；I-40 换 go/ast 门禁；I-54 | 待 ff | 守卫置 false 两 pin 红 |

Wave 1 的兄弟扫描额外挖出并修掉：`deposit-tolerance` 铸币路径、`EventUID` 输入门、`EnableOnchain` 从不传 pool 使单飞锁全死、`advanceSweep` dispatch 分支绕过 GasCeiling。

**Wave 2（Major / Minor 主体）八条全部合入：**

| 任务 | 合入 | lead 证伪 | 要点 |
|---|---|---|---|
| D-surface | ✅ | README 代码块改错方法名 → 可运行 gate 红 | README 代码块对真库实跑；CHANGELOG 补回 Wave 1 全部破坏性变更；examples 全部 exit 0 |
| D-tests | ✅（首版退回一次） | 删光 I-6 pin bullet 只留标题：首版绿（只查标题）→ 终版红 | 每条 invariant 必有可解析 pin；schema 精度 / NOT NULL 门禁；e2e 进 CI；发版流水线复用同一 verify |
| D-tamper | ✅ | 空锚守卫置 false → NOT_RUN pin 红 | `VerifyLedger` 采最新 + 未覆盖 entry 永不 VERIFIED；锚回退 TAMPERED（I-55）；r2 one-object-per-seq（I-56）；多 key verifier；NaN 拒收；零签名不报绿 |
| D-web | ✅ | 忽略 `isError` → ErrorState pin 红 | 三处假空态修正；幂等 key 按 payload；recharts 走 formatAmount；query-consumption 门禁 |
| D-threat | ✅ | `revokeLedgerOwner` 吞错 → 两条失败路径 pin 红 | ownership 机械 gate（I-57）；审计 trigger 从目录派生（I-58）；审计表只经 trigger 写；非 superuser bootstrap 可装到底（007 有意修改，契约 §8）；`Migrate` 逐 migration 提权窗口（**W3 对抗式复审**） |
| D-lock | ✅ | 关账共享锁换 `true` → 屏障 pin 红 | 关账屏障 + `period_close_violations` 对账（I-59）；迁移锁有界；tx 内 dimCache 视图；`Deactivate*` 真生效 |
| D-contract | ✅ | openapi 参数改名 → `ParamsMatchGoHandlers` 红 | openapi 参数 / 类型 / inline / required / 出站 Event 全部进门禁；Go 导出面破坏性快照门禁；余额读路径 loose index scan（9,600→102 行）；`message.fields` 真写入；reverse 键服务端派生 |
| D-ops | ✅ | worker `recover()` 分支置 false → panic pin 红 | postgres 层接 `core.Metrics`（32→41 方法，4 个签名改 uid）；job 家族指标；panic 兜底（I-60）；metrics 调用点完整性门禁（I-61）；RUNBOOK/DR/CAPACITY 逐条核到代码；`ledger-cli` 不再自称只读、加 `config-history` |

**Wave 3（收口 + 对抗式复审）**

两名只读复审员（`w3-review/money-path.md`、`w3-review/gates.md`）对整改后的 main 做了证伪：

| 复审 | 结果 | 处置 |
|---|---|---|
| money-path（8 个攻击面） | 3 攻破 1 部分。**Critical：I-49 修了 `min(V,E)`，但被减掉的 hold 项读 `reservations` 可写列，一条 UPDATE 让闸对 1000 授权 2000**；另 `SolvencyCheck` / `enforce_min_balance` / `ConfirmPending` 仍读 checkpoint、未覆盖伪造 journal 自称 tx_mode 只 DRIFT、默认全关无 Warning、`unauthorized_journals` 一页一条签名即跳过其余、`Migrate` 窗口角色级、`event_uid` 冒领锁死无解除 | W3-holds ✅（闸内 hold = Σ 未过期 `reserved_amount`，不读任何结算声明；`Settle` 拒过期）；W3-fixes ✅（SolvencyCheck entries-only、未覆盖 entry 不看自称、StartupReport 五条 Warning、Complete = 全核、custodial scope 逐 code、`anchor_observations` owner 写入且拒超链高）；W3-fixes-2 进行中（M-3 owner-only 解除、M-5 文档层）；M-1 / ConfirmPending / M-5 机制层 / 签名 receipt → Aaron |
| gates（34 次 mutation） | 23 处盲区，3 Critical：路由 scope 降级全绿；pin 引用门禁 63 条只 10 条真红；破坏性变更门禁在 CI 恒绿 / 恒 skip | W3-citations ✅（白名单清空）；W3-gates-fixes 进行中 |

方法教训（进本 README 的正文）：契约 §0 的兄弟扫描仍被**形态描述**框住——C-1 的兄弟被写成「读了 checkpoint」，真正的形态是「决定放多少钱的算式里有项来自攻击者可写的表」，`reservations` 是第二项、receipt 表是第三项。下一轮的扫描要按「谁能写这张表」列算式的每一项，而不是按上一处 bug 的表名。

**待 Aaron 确认的语义口径**（契约 §7，已按推荐落地，可回退）：equity 改 debit-normal；`checkout_settlement` 语义（custodial 装欠用户净额、平台自赚进 fees）；托管资产口径改注入式 `CustodialClassCodes`；`Worker.Run` 对 NopLogger fail-closed。

## 0. 一句话结论

> **上一轮的修复全部真的落地了——每一条都停在了它自己那一层。**

十份报告的「上轮修复复核」段结论一致：8 条 Critical 与三波整改的每一项都接到了真实路径，
新加的门禁种回漂移都会红（F 实测六个、C 实测五个）。这一轮没有一条发现是「修复没落地」。

这一轮的六条 Critical 里有五条是**上一轮某条修复的同形兄弟**：修了三个 preset 的借贷方向，
另外两个 preset 同错；修了冲销推导，冲销的输入门没关；修了「谁签的」，「多少钱」仍读不可信缓存；
修了 scanner 层的 fail-open，service 层同一形态照旧；给四个 deposit 模板加了保护名单，
凭空造钱的 `dev_credit` 不在名单上。**每次修复都没有做「这个形态在别处还有吗」的横向扫描。**

## 1. 数量

| 严重度 | 原始条数（十份报告机械计数） | 说明 |
|---|---|---|
| Critical | **6** | 全部由 lead 复核 CONFIRMED |
| Major | 86 | 跨 territory 重复命中约 8 组（见 §3），去重后约 80 |
| Minor | 92 | 去重后约 88 |

原始条数 = 各报告 `### [级别]` 标题计数。去重清单在处置阶段建 `TODO.md` 时精确化。

## 2. 六条 Critical

| # | territory | 发现 | 位置 | lead 复核 |
|---|---|---|---|---|
| **C-1** | C | **验签提现闸只验签名，可预留金额仍取自攻击者可写的 `balance_checkpoints`**。设计明写「提现路径全量重算、不读 checkpoint」，`requireVerifiedAvailableBalance` 把 `VerifiedBalance` 算出的可信数丢弃，随后按 checkpoint+delta 判额 | `postgres/reserver_store.go:206,274-295`；`checkpoints.sql:55-58` | **实跑**：签名余额 1000，伪造 checkpoint 后 `Reserve(500000, RequireVerifiedBalance:true)` 成功 |
| **C-2** | A | **`capital_injection` / `capital_withdraw` 的 `custodial` 腿方向反**。custodial 是 credit-normal，入金模板全用 CR 增加它，注资模板却 DR。注资一次把 `SolvencyCheck`（唯一的无背书发行告警）永久钉成 false。**pin 断言的正是错误方向，修对会红** | `presets/capital.go:35,45`；`presets/capital_test.go:73` | 读码 + A 实跑（注资 1000 → `custodial=-500 margin=-1000`） |
| **C-3** | A | **手写 `ReversalOfUID` 让上轮 C8 复活**。`PostJournal` 对该字段只查「uid 存在」；一张四腿净零、标 `reversal_of=J` 的合法 journal 会让 `ReverseJournalFraction(J,1,1)` 只冲一半、返回 nil，所有对账检查全绿。库模式专有 | `postgres/ledger_store.go:931-945`；`reversal_fraction_store.go:518-536` | **实跑**：存 100 → 净零 journal → 冲销「剩余全部」→ 余额剩 50 |
| **C-4** | G | **前向扫链在 `IngestDeposit` 失败后照样推进游标**。非 `ErrConflict` 失败只留一行日志（默认 NopLogger 丢弃），无死信、无指标，游标不回头 → 真实充值永久丢失。B 与 I 在移交段独立命中同一行号（**本轮三方命中，信号最强**） | `service/onchain.go:1246-1252` | 读码（无分支） |
| **C-5** | G | **`TxLogSeq` 是「本次查询结果集内」的序号**，watcher（全量地址）与 rescan（单地址）对同一笔 tx 派生不同幂等键 → 双记账或进死信。I-20 的 pin 没有一条经过 `FetchDeposits` | `chains/evm/reader.go:143-154`；`service/onchain.go:651` | 读码 |
| **C-6** | D | **`dev_credit` 不在 `ProtectedTemplateCodes()`**，write-scope key 经 `POST /journals/template` 在任何 ENV 下铸币，绕开 `/dev/credits` 的三重门禁。`capital_injection` / `fee_charge` 等触碰 system 分类的模板同样放行 | `presets/protected_templates.go:37-44`；`server/handler_journals.go:237-247` | **实跑 httptest**：三个模板默认配置下均 201 |

复现细节、临时测试代码与逐条证据见 `lead-verification.md`。

## 3. 贯穿性主题（按失效机理重排）

### 主题一：修复停在自己那一层——同形兄弟没被扫（Critical 里占 5 条）

| 上轮修了 | 本轮发现的兄弟 | 报告 |
|---|---|---|
| C7 三个 preset 借贷方向 | `capital_*` / `checkout_settlement_*` / `fee_charge` vs `settlement` 对 `fees` 符号相反 | A |
| C8 冲销推导按维度聚合 | `PostJournal.ReversalOfUID` 零校验 | A |
| C3 缓存判定跳过 live check（「谁签的」） | 闸放行金额仍读 checkpoint（「多少钱」） | C |
| I-42 Go 侧 17 处符号收敛 | `balance_trends.sql` 第 18 处，方向错 | A |
| `ExecuteTemplateBatch` pool 模式全局锁序 | tx 模式未修；`ConfirmPending`/`CancelPending` 锁序相反（B 实跑出 40P01） | B |
| `Reserve` tx 模式 VB 闸 fail-closed | `VerifiedBalanceReader()` accessor 无守卫 | B |
| `SetLocalPoller` / `SetAttestor` 加真实路径 pin | 同函数 `SetPartitionService` / `SetPool` 删掉全绿；`AuthVerifier` 换 nil 全绿 | F |
| `RunInTx` clone 三道逃逸闸 | `RegisterChannel` / `Onchain()` / `Worker()` 三个入口没装 | E |
| scanner 层 per-address fail-closed | service 层 ingest 失败推进游标 | G |
| 四个 deposit 模板进保护名单 | `dev_credit` 等不在名单 | D |
| 四张配置表加审计 trigger | `account_policies` 白名单放行三个风控开关本身，且无审计 | D |
| 001 末尾 ownership 一次性清扫 | 002–015 新建的 4 表 / 4 序列 / 9 函数全没扫到，I-35 为假 | D |

D 把它概括为「从目录派生的纪律写进了注释，实现仍是手工名单」——三处（审计 trigger、受保护模板、ownership 清扫）同形。

### 主题二：门禁守形状，不守语义

上一轮把「靠人记得」换成了「机器会拦」，本轮发现这些机器拦的是**形状**：

- `openapi_contract_test` 只反射 `$ref` 的顶层字段名——query 参数（`/snapshots` 三个全错，照 spec 调必 400）、类型 / format、inline schema、出站 webhook payload 全在门禁外（H，J 独立命中）
- skin-parity 门禁：两个 skin **对称地错**就必绿，本轮 J 的三条 Major 全是这一形态（J）
- `readme_api_surface_test` 只管方法名在表里，README 的「Add a custom lifecycle」编译过、运行必败：`balance_role` 在 README + COOKBOOK 命中 0 次（E）
- `TestInvariantsDocPinsAllExist` 只校「引用的名字存在」，I-7 / I-34 根本没有 pin 也绿（F）
- `assertBalanced` 在两腿共用 AmountKey 时恒真，是五个 preset 的唯一保护；两处 pin **主动认证缺陷**（A）
- `TestExecuteTemplateBatch_GlobalLockOrder_*` 从不调用被测函数（B）
- I-32 的 `Pinned by` 措辞把「不是更严的金额检查」写成有意语义，于是 C-1 这个缺口被规则本身合法化（C）

### 主题三：默认静默通道

`ledger.New(pool)` 的默认是 `NopLogger` + `NopMetrics`。落进这条通道的有：Worker 启动报告（上轮「静默关 job」的修复本身）、anchor 缺失 Warn、`Subscribe`-after-`Run` Error、ingest 失败 Error、anchor publish 失败 Error。仓库自己的 `examples/fullstack` 也传 NopLogger。
`postgres/` 包没有 `core.Metrics` 依赖，账本写入路径在结构上不可观测：`core.Metrics` 32 个方法 12 个零调用点，README 广告的 6 个指标 4 个不存在 2 个恒 0，RUNBOOK §6/§7 为永不触发的告警写了 on-call 流程（E / I / C / G / B 五份报告从不同角度撞上）。

### 主题四：检测算出了结论，结论没被用在决定钱去哪的地方

C-1 是最重的实例。同形还有：`VerifyLedger` 采样最旧 20 条而非最新（新伪造的永远进不了样本），且没实现设计 §8.4「找未覆盖 entry」；锚被删或回退判 `DRIFT`（良性）且 CLI 退出 0；`unauthorized_journals` 零条已签名仍 `Passed=true`（C）。事故现场侧：RUNBOOK P0 那节三条命令全错、§3 查一张不存在的表；`ledger-cli verify` 只接本地文件锚而 RUNBOOK 指定的生产载体是 R2；`ledger-cli` 自称只读但 `reconcile --full` 写生产库（I，C）。

### 跨报告重复命中（去重时合并）

NopLogger 通道（E / I / C）· `/snapshots` 参数（H / J）· attestation 零指标（C / I）· worker 无 recover（C / I）· `ledger-cli verify` 接不了 R2（C / I）· `AllowSystemClassificationPost` 文档的假环境变量（D / H）· `webhook_subscribers.secret` 暴露（D / H）· `go-release.yml` 不跑子模块（H / F）· watcher 无 leader（B / G）· ingest 失败推进游标（G / B / I）。

## 4. 共同盲区：十份报告都没问的问题

**修好之后，已经记错的账怎么办？**

C-2 的 `capital_injection` 方向错、A 的 `fees` 两模板符号相反、上轮 C7 的三个 preset——这些都是**已发货模板**。任何消费方在修复前用它们记过账，账上就有方向错的 journal。journal append-only（I-2），修正只能靠冲销再重发。十份报告全部停在「把方向改对」，没有一份问：修复 PR 要不要带一条「检测存量错向 journal 并生成更正 journal」的迁移路径？`CHANGELOG` 该不该把这类修复标成「需要数据修正」的 breaking？这与 `deployment.md` 的 expand → migrate → contract 是同一件事在账本语义上的投影，而本库此前的口径是「无外部用户期允许单步破坏性迁移」——**那个口径对 schema 成立，对已记的账不成立**。

第二个（较轻）：**没有一份报告核过「今天有没有任何生产消费方接了这条防篡改链」。** C 审的是链本身，E 审的是接线体验，但「`RequireVerifiedBalance` / `WithAttestor` 在真实产品代码里的调用点数」没人查。lead 补查见 §6。

## 5. 流程发现（本轮契约的缺陷）

- **反转实验在共享工作树互相投毒**。契约 §2 允许「可逆反转、跑完立即还原」，F 做了 17 次、A/B 各做了探针，窗口期内 B、E、I 三份报告都撞上被反转的 `core/onchain.go` / `service/rollup.go`，`go test ./core/` 一度是红的。已记 `bus learn`：**下轮反转实验一律在独立 worktree（`wt`）里做，主树只读。** F 事后补了 19 次反转的逐条日志（其报告附录二）；lead 的三条实跑复现已在与 `fd87cae` 逐字节一致的清树上重跑，结论不变（`lead-verification.md` 末节）。
- **subagent 的 SendMessage 被禁用**（D / E / F / I 四个 agent 报告），只能靠 idle notification 传结果——本轮够用，但意味着 lead 无法在中途向 agent 追问。
- **只读纪律的执行口径不一**：A 在仓外独立容器 + 独立 module 实跑；D 起一次性 postgres 容器；F 在仓内反转；C / G 纯读码。下轮契约应明确「实跑的默认形态 = 仓外 worktree + 独立容器」。
- **agent 报的 Critical 需要 lead 亲手复现**这条纪律再次被证明必要：C 自己标注「未实跑，建议 lead 亲手复现再定级」，lead 复现后成立；D 同样。本轮六条 Critical 无一被推翻，但 A 的 C-2 若不核 `presets/templates.go:71` 的 normal_side 声明，单看 capital.go 的注释会得出相反结论。

## 6. lead 补查

- **生产消费方盘点**（2026-09-02）：`grep -rl 'azex-ai/ledger' ~/projects/azex ~/projects/agentoon --include=go.mod` → **0 个 go.mod 引用**；`RequireVerifiedBalance` / `WithAttestor` / `InstallDevCreditPreset` 在两条产品线的 Go 代码里零命中。即：截至今天没有任何在役产品 import 这个库，六条 Critical 里没有一条已经在生产上暴露。这不改变定级（库的契约面就是它的产品面），但决定了修复的紧迫度排序与 §4「存量数据修正」的实际范围（当前为空）。

## 7. 下一步（待拍板）

1. **Critical 六条**：逐条给 Aaron 拍板修 / 不修 / 改设计。C-1 与 C-6 涉及规则本身要改（I-32 措辞、`ProtectedTemplateCodes` 从手工名单改为结构性派生）。
2. **Major 去重建 `TODO.md`**：按主题一的「兄弟表」组织，同形一起修，并对每条追加「横向扫描：这个形态还有第几处」。
3. **新门禁**（主题二的直接产出）：SQL 符号扫描 gate；openapi `parameters` / inline schema 门禁；「每条 invariant 至少一条 pin 且 pin 源码引用被测符号」；preset 每条一个偿付能力断言；`ledger.New` 默认 logger 非 Nop 或 `Worker.Run` 对 Nop 拒绝启动。
4. **存量数据修正路径**（§4）：先拍板口径，再决定 C-2 / A-fees 修复 PR 是否带迁移。
