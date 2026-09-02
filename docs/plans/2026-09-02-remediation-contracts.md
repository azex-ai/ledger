# 第二轮审计整改契约 — 2026-09-02

> **本文件是本波的唯一契约源（SoT）。** 每个 worker 开工前必须读完 §1–§5 与自己那条任务。
> 发现清单：`docs/audits/2026-09-02-deep-audit/TODO.md`（去重后的全量条目，按任务归属排列，
> 每条带 id / `file:line` / 修法 / pin 要求）。完整论证与最小复现在同目录各 territory 报告原文与
> `lead-verification.md` —— **不要只看 TODO 的一行标题就动手**。
>
> **Status（2026-09-03）：Wave 1 / Wave 2 全部合入 main；Wave 3 收口中（对抗式复审 ×2、INVARIANTS 引用收紧、归档）。** 复审结论出来后本文件转 CLOSED。
>
> Aaron 2026-09-02 拍板：**全量修复**（6 Critical + 全部 Major + 全部 Minor）。
> 沿用无外部消费方期的 no-compat 约定（lead 已核：两条产品线 go.mod 零引用），允许破坏性变更，
> 但每一条破坏性变更都要进 `TODO.md` §破坏性变更清单，写清消费方需要做什么。

## 0. 这一波与上一波的区别：先扫兄弟，再修

上一轮（`2026-08-26-audit-remediation-contracts.md`）修了 8 Critical + 74 Major/Minor，本轮审计的结论是
**每一条都真的落地了、每一条都停在了自己那一层**——六条新 Critical 里五条是上轮某条修复的同形兄弟。
所以本波给每条任务加一道硬要求：

> **修任何一条之前，先 grep 出这个形态在全仓的所有实例，把清单写进 bus checkpoint，然后一起修。**
> 只修 TODO 点名的那一处 = 不合格。

审计 README §3 主题一的「兄弟表」是起点，不是终点。

## 1. 按「文件独占域」切分（同上一波）

一条 TODO 条目落在哪个任务，由它**要改的文件**决定，不由它来自哪份报告决定。每条任务独占一组文件，
独占面之外只读。跨域接缝在 §4 逐条写明，接缝两侧各自 checkpoint 自己那一半，**任何一半单独上线都不构成修复**。

## 2. 号段分配（开工前定死，不得自选）

| 资源 | 现状 | 本波分配 |
|---|---|---|
| migration | 最大 `015_checkpoint_updated_at_index` | **016 起**，见 §4 逐任务独占列 |
| invariant | 最大 `I-48` | **I-49 起**，见 §4 逐任务独占列 |
| bizcode（账本段 14xxx） | 用到 `14010` | **14011 起**，D-contract 独占分配权 |
| reconcile check 名 | `checkpoint_balance` / `journal_dr_cr` / `non_negative_balances` / `pending_journal_timeout` / `unauthorized_journals` … | 新增须 `bus send team-lead` 报备，D-tamper 独占命名权 |
| `core.Metrics` 新方法 | 32 个 | D-ops 独占追加权；**追加 = 对实现方的破坏性变更**，必须同时提供可嵌入的 `core.NopMetrics`-style 基类并记进破坏性清单 |

需要额外号段 → `bus send team-lead`。

## 3. 全局红线（违反即回退，不进 review）

1. **`financial.md`**：金额只用 `decimal.Decimal`；journal append-only，纠错只走 reversal；DB 事务内禁止外部调用。
2. **`deployment.md`**：已合入 main 的 migration 永不修改；新 migration 带 down 脚本；新建表 / 函数 / 序列显式 `GRANT` + `ALTER ... OWNER TO ledger_owner`（本轮 D 报告：001 的 ownership 清扫是一次性循环，002–015 建的对象全没扫到）。**跟随本仓既有写法**，不加 `IF NOT EXISTS`。
3. **`working-agreements.md` §3 静默失败**：未运行 ≠ 通过；降级必须落痕；fail-closed。本轮四个主题里有两个（默认静默通道、检测结论未被使用）就是这个形态。修完自查：**如果这一步根本没跑，我看到的输出会有任何不同吗？**
4. **`working-agreements.md` §4 回查 SoT**：TODO 的一行是线索。动手前读对应报告原文的「失效场景」与「为什么现有机制没拦住」。
5. **标 PLAUSIBLE 的先验证再修**。验证不成立 → `bus checkpoint` 记理由后标不修。
6. **每条修复必须有一个「修复前是红的」测试**。且这个测试必须**从消费方会走的真实入口出发**（`ledger.New(pool)` / facade / HTTP handler），不许自己拼 store 绕过接线——本轮 F 报告：I-32/I-33 六条 pin 全是手拼 store，把 facade 的 `AuthVerifier` 换成 nil 全绿。pin 的源码必须引用被测导出符号（B 报告：一条锁序 pin 从不调用被测函数）。
7. **反转实验只在自己的 worktree 里做**（本轮审计期间共享工作树被投毒 19 次）。
8. **surgical**：只改追溯得到 TODO 条目（及其兄弟清单）的行。

## 4. 波次、任务与文件独占

### Wave 1 — Critical 与契约层（6 条并行，必须先合入 main）

| 任务 | 覆盖 | 独占文件 | 号段 |
|---|---|---|---|
| **W1-gate** 提现闸用可信金额（C-1）+ 闸的 HTTP/RunInTx 边界 | C-C1；B「`VerifiedBalanceReader()` 在 clone 上无守卫」的 **store 侧**；C-m7 pin 注释 | `postgres/reserver_store.go`、`postgres/verified_balance_store.go`、`postgres/verified_balance_pin_test.go`、`postgres/attested_auth_pin_test.go`、`docs/INVARIANTS.md` **仅 I-32 / I-33 段** | **I-49** |
| **W1-sign** 符号与偿付能力（C-2 + A 的全部 Major） | A-C1、A 的 `checkout_settlement` / `fees` / `balance_trends` / holder fee 消失 / FX solvency / 快照自愈；新增 SQL 符号 gate；每个已发货 preset 一条偿付能力 pin | `presets/`（**除** `protected_templates.go`）、`postgres/sql/queries/platform_balances.sql`、`balance_trends.sql`、`holder.sql`、`checkpoints.sql`（仅快照部分）、`postgres/solvency_test.go`、`postgres/balance_trends_store_test.go`、`postgres/holder_store_test.go`、`postgres/snapshot*.go`、新 gate 测试文件 | **I-50**（SQL 符号 gate） |
| **W1-ledgerstore** 冲销输入门（C-3）+ `ledger_store.go` 域内的并发条目 | A-C2；B「`ExecuteTemplateBatch` tx 模式锁序」「`ConfirmPending`/`CancelPending` 锁序」；B「bookings 行锁与 balance lock 取序相反」；C-m6 `attestJournal` 重放标签 | `postgres/ledger_store.go`、`postgres/reversal_fraction_store.go`、`postgres/pending_store.go`、`postgres/lock_order_test.go`、`postgres/reversal_fraction_test.go`、`postgres/sql/queries/journals.sql` | **I-51**（冲销链完整性） |
| **W1-onchain** 扫链资金路径（C-4、C-5 + G 全部 Major/Minor） | G 全部；B「watcher 无 leader」「`Sweeper.lastFee` 无界」；I 移交的三个 onchain 指标**只定义调用点、方法由 D-ops 追加**（接缝见下） | `service/onchain*.go`、`chains/evm/**`、`postgres/registration_rescan_store.go`、`postgres/deposit_address*.go`、`postgres/sql/queries/deposit*.sql` `registration*.sql`、`core/onchain.go`、`docs/INVARIANTS.md` **仅 I-20 段** | **I-52 I-53**、migration **016**（若 rescan 改 sqlc） |
| **W1-templates** 受保护模板结构化（C-6） | D-C1；D「一个 write key 能铸币」的其余模板；`server/handler_journals.go` 域内 H 的 inline schema 条目 | `presets/protected_templates.go`、`server/handler_journals.go`、`server/handler_journals_template_test.go`、`server/server.go` **仅 Config 的 ProtectedTemplateCodes / AllowGenericTemplatePost 段** | — |
| **W1-facade** facade 默认与 clone 边界 | E「NopLogger 吞掉一切」「`RunInTx` clone 三入口逃逸」「Worker 静默关 job」；B「`Service.Worker()` 不守 `s.tx`」「`VerifiedBalanceReader()` 无守卫」的 **facade 侧**；I「默认全黑」「`SetFullReconciler` 不自动接线」；F「`SetPool`/`SetPartitionService`/`AuthVerifier` 无真实路径 pin」 | `ledger.go`、`ledger_*_test.go`（根包）、`service/worker.go` | **I-54**（默认可观测性契约） |

**Wave 1 内部接缝**

- `docs/INVARIANTS.md` 被 W1-gate（I-32/I-33）与 W1-onchain（I-20）同时命中：**只改自己那段，新 invariant 追加在文末自己的号段**，不重排。
- W1-templates 依赖 `presets/` 的分类 `is_system` 数据，但**不改** `presets/` 的模板定义——结构性派生的实现放在 `server/` 侧（或 `presets/protected_templates.go` 内基于模板腿计算），W1-sign 改方向不影响它。
- W1-facade 的 `service/worker.go` 与 W1-onchain 的 `service/onchain.go` 无重叠；`Worker.SetOnchain` 之类若需新接线，由 W1-facade 加，W1-onchain 只提供方法。

### Wave 2 — Major 主体（blocked on Wave 1 全部合入）

| 任务 | 覆盖 | 独占文件 | 号段 |
|---|---|---|---|
| **D-tamper** 验证结论怎么被使用（C 的 M-1…M-9、m-1…m-5） | `VerifyLedger` 采样方向 + 未覆盖 entry 检查 + 锚回退语义 + R2 head 不可变性 + 多 key verifier + NaN 拒收 + `unauthorized_journals` 零扫描不报绿 + anchor 失败可见 | `service/attest_verify*.go`、`service/attestation*.go`、`service/reconcile.go` **仅 unauthorized_journals 段**、`anchors/r2/**`、`anchordev/`、`anchortest/`、`authdev/`、`postgres/convert.go`（NaN）、`docs/INVARIANTS.md` I-26 I-27 I-28 I-45 段 | **I-55 I-56**、migration **017**（NaN CHECK） |
| **D-threat** DB 角色 / ownership / 审计 / 入站 webhook（D 的全部 Major/Minor） | 007 函数 ownership + `NOSUPERUSER` bootstrap 路径 + `account_policies` 守卫与审计 + 审计表 INSERT 权限 + `webhook_subscribers.secret` + nonce 重放缓存接线 + 死表 + 路由鉴权门禁 | 新 migration、`postgres/roles_test.go`、`postgres/grant_coverage_test.go`、`postgres/*guard*_test.go`、`server/middleware*.go`、`server/routes.go`、`server/handler_webhooks.go`、`postgres/webhook_subscriber_store.go`、`channel/`、`docs/INVARIANTS.md` I-22 I-25 I-35 段 | migration **018 019 020**、**I-57 I-58** |
| **D-contract** 契约面（H 的全部 Major/Minor + J-6 后端侧） | openapi `parameters` / 类型 format / inline schema / `required` 补全 / 出站 webhook 序列化 / `expires_at` 空串 / 排序方向 / `info.version` 门禁 / Go 导出面破坏性检测 / `core.Metrics` 内部 id 泄露 | `docs/openapi.yaml`、`docs/api.md`、`server/openapi_contract_test.go`、`server/handler_*.go`（**除** `handler_journals.go`，W1 已改）、`server/server.go`（**除** W1-templates 段）、`service/delivery/webhook.go`、`pkg/httpx/`、`pkg/bizcode/`、`internal/idschema/`、新 API 破坏性检测 gate | bizcode **14011 起** |
| **D-ops** 可观测与取证（I 的全部 Major/Minor） | `postgres/` 层接 `core.Metrics`（结构性：postgres 包新增 Metrics 依赖）；attest / anchor / worker job / onchain 指标；panic 兜底；RUNBOOK / DR / CAPACITY 全部命令与表名核对；`ledger-cli` 只读性、超时、verify 接 R2、审计表读者 | `core/metrics.go`、`observability/`、`pkg/slogadapter/`、`pkg/otel/`、`service/locked_job.go`、`service/cleanup_context.go`、`service/partition.go`、`service/rollup.go`、`cmd/ledger-cli/`、`docs/RUNBOOK.md`、`docs/DR.md`、`docs/CAPACITY.md`；`postgres/*.go` **仅限新增 metrics 调用行**（Wave 1 合入后） | **I-60 I-61**（原 I-59 I-60，2026-09-03 与 D-lock 对调） |
| **D-lock** 剩余并发条目（B 未被 W1 吸收的） | `ClosePeriod` 锁与屏障 + 对账检查；`acquireClusterLock` 超时；`dimCache` 回滚污染；delivery `MarkDelivered` ctx；`EventStore.SetLogger` 接线；`journals.sql` 过时注释 | `postgres/period_close_store.go`、`postgres/migrate*.go`、`postgres/dim_cache*.go`、`postgres/event_store.go`、`service/delivery/local.go`、`service/reconcile.go` **仅新增 period_close 检查** | **I-59**（关账屏障并发语义） |
| **D-surface** 文档与 examples（E 的文档类 + 数字一致性） | README `balance_role` 缺失 / CHANGELOG 0.6.0 Go API 破坏清单 / `[Unreleased]` r2 / COOKBOOK / `docs/api.md` 散文错误 / examples 注入 logger / preset 数量 / invariants 条数 | `README.md`、`CHANGELOG.md`、`docs/COOKBOOK.md`、`docs/frontend.md`、`examples/**`、`readme_api_surface_test.go` | — |
| **D-tests** 测试可信度（F 的全部 + 各报告移交给 F 的） | INVARIANTS「每条至少一条 pin 且 pin 引用被测符号」gate；`information_schema` 精度 pin；`-tags e2e` 进 CI；`go-release.yml` 与 PR gate 对齐；`roles_test` 集群级 SUPERUSER；`assertBalanced` 恒真；handler mock 一致性 | `.github/workflows/**`、`Makefile`、`.golangci.yml`、`docs/INVARIANTS.md` **仅 Pinned by 行与 I-7 / I-34 补 pin**、`internal/postgrestest/`、新 gate 测试文件、`presets/*_test.go` 的 `assertBalanced` helper | — |
| **D-web** `@azex/ledger-react`（J 的全部） | 死图表 / 假空态 / holds 空态 / styles 全局泄漏 / recharts 浮点 / types-conform 覆盖 / `errorText` 与 `fields` 前端侧 / headless 格式化器 / 幂等 key 作用域 / a11y ratchet | `web/**` | — |

**Wave 2 接缝**

1. **`service/reconcile.go`** 被 D-tamper（unauthorized_journals 段）与 D-lock（新增 period_close 检查）同时命中：D-lock 只**追加**一个 check 函数与注册行，不碰既有 check；合并顺序 D-tamper 先。
2. **`postgres/*.go` 的 metrics 接线**（D-ops）跨所有 store 文件，只允许**新增调用行**，不改逻辑；Wave 1 全部合入后才开工，合并顺序放 Wave 2 最后。
3. **`docs/INVARIANTS.md`** 五个任务都碰：各自只改独占段 + 文末自己的号段；D-tests 只改 `Pinned by` 行。Team Lead 合并时解冲突。
4. **`message.fields`**（J-8 后半）：服务端写入点归 D-contract（`pkg/httpx` + handler 校验错误），前端映射归 D-web；两半都要 checkpoint。
5. **RUNBOOK 的角色指引**（D 报告）：文字归 D-ops 的 RUNBOOK 独占，D-threat 把要改的内容写进 `bus send d-ops`。
6. **`ledger-cli verify` 接 R2**（C-M6）：`anchors/r2` 是独立 module，root 不能 import 它。D-ops 负责决定形态（建议：CLI 保留 `--anchor-file`，新增 `--anchor-cmd`/stdin JSON 或把 verify 暴露为消费方 composition root 里的一个函数并改 RUNBOOK 口径），D-tamper 提供 `VerifyLedger` 对任意 `core.Anchor` 的可用性。

### Wave 3 — 收口（串行，Team Lead）

| 任务 | 说明 |
|---|---|
| **W3-sweep** 全仓兄弟扫描复核 | 对 Wave 1/2 每条 checkpoint 里的「兄弟清单」抽样复核；对 README §3 主题一的 12 行逐条问「还有第 N 处吗」 |
| **W3-lead** 集成收口 | INVARIANTS 编号连续、TODO.md 归档为已处置、CHANGELOG `[Unreleased]`、审计 README 处置表更新、发版口径 |
| **W3-review** 对抗式复审 | 按 `working-agreements.md` §2：money-path 改动请第二意见（codex adversarial review 或独立 reviewer），带明确攻击面：「这个修复在什么输入下仍会算错钱」 |

## 5. Done 标准（统一，缺一不算）

1. `make vet` + `make lint` + `make test` 全绿（Docker / testcontainers）；`chains/evm` 与 `anchors/r2` 子模块各自 `go test ./...`
2. 每条修复有对应 pin，**修复前是红的**（证据 = bus checkpoint 里贴红的输出），且从真实入口出发（§3 第 6 条）
3. **兄弟清单**写进 checkpoint（§0）
4. 动到 invariant → `docs/INVARIANTS.md` 同步；动到 HTTP 面 → `docs/openapi.yaml` 同步；破坏性变更 → `TODO.md` §破坏性变更清单
5. `git status` 只有独占面内的文件

## 6. 协作协议

- 写盘一律走 worktree：`wt create ledger <slug>`，分支名 = `<slug>`（如 `w1-gate`）
- 调度走 `bus`（cwd = 主仓 `/Users/aaron/projects/ledger`，`bus -d` 指向主仓 `.team`）：`bus board` → `bus inbox <me>` → `bus claim <id> <me>` → 干活 → 阶段性 `bus checkpoint <me> "<进度 + 兄弟清单 + 红测试输出>"` → `bus done <id> <me>` → `bus send team-lead done "<slug> 分支就绪"`
- 长命令显式 timeout：`go test` 一律 600000ms
- 踩到独占面之外 → 停下 `bus send team-lead`，不自行扩大
- 新失效形态 → `bus learn <me> "<经验>"`
- 流水线：一条完成即 review + 合并，不等同波全部完成；Team Lead 合并前亲手证伪（拆掉修复看 pin 红）

## 7. Lead 拍板的语义口径（Aaron 说「全量修复」，未逐条拍语义；以下为 lead 推荐口径，先按此做，标「待 Aaron 确认，可回退」）

| 项 | 问题 | 推荐口径 | 归属 |
|---|---|---|---|
| 7.1 模板方向修复的交付路径 | `entry_template_lines` 有 `BEFORE UPDATE` 守卫且 `InstallTemplatePresets` 对已存在模板只校验不更新，源码改对后存量库装不上 | **源码修正 + 一条 migration 以 owner 身份在事务内 `ALTER TABLE ... DISABLE TRIGGER` 修正行、写 `config_table_changes` 审计行、再 `ENABLE`**。不走 `*_v2` 模板码（零消费方的库不该留永久命名疤痕）。worker 若有证据认为不可行，`bus send team-lead` 说明后可改走 v2 | W1-sign |
| 7.2 `checkout_settlement` 语义 | 仓内三种互斥读法，代码与三者都不自洽（A-M2） | 以 **journal type 的 `HolderKind=deposit` / `DisplayLabel` 为准**：商户 holder 的 `main_wallet` 增加 net（DR，debit-normal），`fees` 增加 fee（CR，credit-normal），`custodial` 增加 gross（CR）。在 `presets/settlement.go` 头注释写下唯一语义并删掉另两种；`transfer.go:43` 的相反注释同步删 | W1-sign |
| 7.3 FX 下的 `SolvencyCheck` | 买入币种永久 `solvent=false`（A-M6） | **扩口径**：资产侧 = `custodial` + 所有 `is_system` 且 `balance_role=''`（非负债、非备忘）的 system 账（含 `settlement` / `spread`），并把「什么算托管资产」从 `code='custodial'` 硬编码改为按分类属性判定（同时关 A-N3）。不做「宣告不适用」 | W1-sign |
| 7.4 默认 `NopLogger` / `NopMetrics` | 修复靠一条默认静默的通道交付（E-M1 / I-M11） | `ledger.New` 默认不变（库不该替消费方选 logger），但 **`Worker.Run` 在 logger 为 Nop 时返回 error**，除非消费方显式 `ledger.WithSilentWorker()`（或等价 opt-in）。`Subscribe`-after-`Run` 从 Error 日志改为**返回 error**。`SetFullReconciler` 由 `svc.Worker` 自动接线 | W1-facade |
| 7.5 受保护模板的派生规则 | 手工四码名单（D-C1） | **结构派生**：任何模板只要有一条腿落在 `is_system` 分类上，就默认受保护（`POST /journals/template` 拒绝），`Config.AllowGenericTemplatePost` 是唯一放行口；`presets.ProtectedTemplateCodes()` 保留为「额外硬编码名单」。pin 改为从模板表反向枚举，不再遍历实现返回值 | W1-templates |
| 7.6 提现闸的金额语义 | I-32 明写「不是更严的金额检查」（C-C1） | **改规则**：`RequireVerifiedBalance=true` 时，可预留额 = `VerifiedBalance` 的 entries-only 重算值之和 − 活跃预留，**不读 checkpoint**。I-32 措辞相应改写；这是「规则本身错了」那一类 | W1-gate |
| 7.7 锚回退 / 为空的判定 | DRIFT 视为良性（C-M3） | `anchorSeq==0 && maxSeqSeen>0` → **NOT_RUN**（fail-closed）；相对上次观测值回退 → **TAMPERED**（上次观测值落库，新增表归 D-tamper 的 migration 号段）；`anchortest` 加 head 非回退 phase | D-tamper |
| 7.8 `ReversalOfUID` 输入门 | 零校验（A-C2） | `PostJournal` 带 `ReversalOfUID` 时：被引用 journal 不得本身是冲销；本 journal 的 entries 必须是被引用 journal entries 的**同维度反向子集**（逐维度 ≤ 剩余可冲销额）；非反向子集 → `ErrInvalidInput`，累计超额 → `ErrConflict`（与 `reversalEntriesFor` 既有 overshoot 检查对称）。等价于把冲销链完整性提升为不变式（I-51） | W1-ledgerstore |
| 7.9 `TxLogSeq` 的定义 | 依赖查询地址集（G-C2） | 改为 **receipt 内的零基位置**（同一 tx 的 Transfer 日志按 `lg.Index` 排序后的序号，reorg 重挖后稳定；不是块级 `lg.Index` 本身——lead 原措辞有误，w1-onchain 按括号意图落地）而非结果集内序号；I-20 措辞同步 | W1-onchain |
| 7.10 扫链失败的游标语义 | ingest 失败仍推进（G-C1） | **任何 ingest 失败都不推进游标**（与 rescan 对齐）；非 `ErrConflict` 失败进死信并计指标；连续失败 N 次后停止推进并告警，不做「跳过这一笔」 | W1-onchain |

以上每条在对应任务 `bus done` 时由 lead 在审计 README「处置状态」里登记为「已按 7.x 口径修复，待 Aaron 确认」。
| 7.11 `POST /journals/deposit-tolerance`（w1-templates 复核时发现的同形铸币路径） | 调用方任选 expected/actual/tolerance，直接执行 `deposit_confirm_pending` 等受保护模板，绕开模板闸 | 对 plan 内每个模板走同一道结构派生闸（∪ 名单），仅 `AllowGenericTemplatePost` 显式放行；路由改 admin scope；默认 403 | W1-templates（允许改 `routes.go` 该行；D-threat 接缝已登记） |

## 8. 勘误与追加拍板（整改期间）

- **§3 第 2 条勘误（2026-09-02，D-threat D-M2）**：允许**有意修改**已合入的 migration 007，且仅限把 superuser-only 语句改成逐属性条件执行。理由：golang-migrate 不校验文件哈希，已装过 007 的库不重跑、不受影响；失败点在 007 内部、后置 migration 无法回溯；无外部消费方。文件头必须写明修改日期与原因。这是本仓第二次对「migration 不可变」做显式例外（上一次是 2026-08-26 的「不加 IF NOT EXISTS」），两次都是全局规则字面搬用与本仓事实不符。
- **migrate.go 安装机制变更**（先跑 001 → `GRANT ledger_owner TO runner` → `Up()` → defer `REVOKE`）：接受落地，标为 §2 结构决策，**W3 对抗式复审**必审项。
- **`POST /journals/{uid}/reverse` 幂等键**：服务端从 journal uid 派生，客户端不得提供（w1-templates 落地、d-web 修 client、d-contract 收 openapi）。H-M3b 取消。
- **F-m2 pin 门禁的 advisory 名单**：允许「先立机制、逐步收紧」，但 advisory 集合必须是显式、只缩不扩的白名单常量；32 条 Enforced-by 引用风格重写记为 W3 条目。
| 7.12 闸内 hold 项的可信来源（W3 对抗式复审 C-1） | `SumActiveReservations` 读 `reservations.status/settled_amount`，守卫放行 `active→settling/settled/released` 与 `settled_amount` 增大，`ledger_app` 一条 UPDATE 让 hold 归零，闸对同一笔余额授权两次 | 闸开启时 hold = Σ 非过期预留的 `reserved_amount`（不可改列）− Σ append-only receipt/leg 表里的已结算/已释放额；不读可变列。伪造 INSERT 只会让 hold 变大（安全方向） | W3（w3-holds）。**2026-09-03 修订**：worker 实跑发现 receipt/leg 表对 `ledger_app` 仍开 INSERT（伪造 release receipt 同样归零 hold），且过期预留仍可结算。改为 **(B) 完全保守**：`Settle*`/`Finalize` 拒绝已过期预留，闸内 hold = Σ 未过期预留的 `reserved_amount`，不读任何结算/释放声明（合法结算/释放后到过期前闸内可用额偏小，有意为之）。**(A) 给 receipt 加签**记为 §7.14 待 Aaron 决定 |
| 7.13 复审 M-2 / M-4 / M-6 / M-7 / m-1…m-6 | 见 `w3-review/money-path.md` | SolvencyCheck 改 entries-only；未覆盖 entry 不看 `auth_status` 自称；`StartupReport` 对每项关着的保护出 Warning；`unauthorized_journals` 的 Complete = 全部已核；custodial scope 逐 code 校验；`''→available` 升级禁止；`anchor_observations` 写入收口 | W3（w3-fixes） |
| 7.14 复审 M-1 / M-3 / M-5 + hold 项方案 (A)（待 Aaron） | `enforce_min_balance` 读 checkpoint（透支闸可被 INSERT 破）；`event_uid` 冒领后 booking 永久锁死无解除路径；`Migrate` 提权窗口是角色级，迁移期间应用连接池 owner-equivalent | 本波不动代码。推荐：M-1 明文声明信任边界 + 依赖 reconcile check #2 检测（热路径 entries-only 重算代价高）或引入签名 checkpoint；M-3 owner-only 解除路径；M-5 文档要求迁移凭证 ≠ 应用凭证，且迁移窗口内应用不得在线；hold 项若保守口径的产品代价不可接受，则做 (A)：ReserverStore 持 Attestor 给 receipt/leg 签名，闸在事务外验签 | Aaron |
