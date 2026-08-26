# 审计整改契约 — 2026-08-26

> **本文件是本波的唯一契约源（SoT）。** 每个 worker 开工前必须读完 §1–§4 与自己那条任务。
> 发现问题清单：`docs/audits/2026-08-25-financial-engineering/TODO.md`（74 条）。
> 完整论证与最小复现在同目录各 territory 报告原文里 —— **不要只看 TODO 的一行标题就动手**。
>
> Aaron 已拍板两件事（2026-08-26）：**本波做全量 74 条**；**允许破坏性变更**（沿用无外部
> 消费方期的 no-compat 约定，`TODO.md` §10 已在维护破坏性变更清单，本波的追加进去）。

## 1. 为什么不按 TODO 的章节分工

TODO 按主题分组便于阅读，但**主题与文件不是一一对应**：`ledger.go` 同时出现在 §3/§4/§5，
`service/reconcile.go` 同时出现在 §4/§6/§8，`core/interfaces.go` 同时出现在 §2/§7。
按主题 spawn = 多个 worktree 改同一个非契约文件 = `abstractions.md` 明列的反向信号。

**本波按「文件独占域」切分。** 每条任务独占一组文件，独占面之外只读。
一条 TODO 条目落在哪个任务，由它**要改的文件**决定，不由它在 TODO 里的章节决定。

## 2. 号段分配（开工前已定死，不得自选）

| 资源 | 现状 | 本波分配 |
|---|---|---|
| migration | 最大 `004_balance_role_upgrade_guard` | **005 起**，见 §4 逐任务独占列 |
| invariant | 最大 `I-34` | **I-35 起**，见 §4 逐任务独占列 |
| bizcode（账本段 14xxx） | 用到 `14009` | **14010 起**，W1-C 独占分配权 |
| reconcile check 名 | 见 `service/reconcile.go` | 新增须在 bus 里报备，D-ops 独占命名权 |

**任何任务不得自行占用未分配的号段。** 需要额外号段 → `bus send team-lead`。

## 3. 全局红线（违反即回退，不进 review）

1. **`financial.md`** —— 金额只用 `decimal.Decimal`；journal append-only，纠错只走 reversal；
   **DB 事务内禁止外部调用**（§2 的 Reserve 那条 Major 正是踩了这条）。
2. **`deployment.md`** —— 已合入 main 的 migration **永不修改**，错了写新的；每个新 migration
   必须带 down 脚本。
   ⚠️ **勘误（2026-08-26，Team Lead）**：本条初版还要求「必须可重入（`IF NOT EXISTS`）」，
   那是全局规则的字面搬用，**对本仓不成立**——`001_baseline` 的 31 个 `CREATE TABLE` 与
   002/003/004 全部是裸写法，golang-migrate 的版本表已保证单次执行。**跟随本仓既有写法**，
   不要为了满足这一条去加 `IF NOT EXISTS`（那会与全仓风格不一致，且掩盖真正的重复执行问题）。
   新建表仍需显式 `GRANT` 给 `ledger_app` / `ledger_ro`（042 的 GRANT 循环只覆盖它执行时已存在的表）。
3. **`working-agreements.md` §3 —— 静默失败是最高级别的 bug**：未运行 ≠ 通过；降级必须落痕；
   fail-closed 而非 fail-open。本波有 6 条发现就是这个形态（`full_coverage`、扫了 0 个报
   complete、scanner fail-open、存证 job 静默跳过、Warn 打进 NopLogger、守卫拒绝不留痕）。
   **修完自查一句：如果这一步根本没跑，我看到的输出会有任何不同吗？**
4. **`working-agreements.md` §4 —— 断言前回查 SoT**：TODO 的一行是线索不是事实。
   动手前读那条对应报告的原文（失效场景 + 为什么现有机制没拦住）。
5. **标 `PLAUSIBLE` 的先验证再修**（本波 8 条）。验证不成立 → `bus checkpoint` 记录理由后
   标为不修，**不要为了有产出而改一个不存在的问题**。
6. **每条修复必须有一个「修复前是红的」的测试**。断言 `require.NoError` 不算验证效果 ——
   §8 有一条 Major 就是这么来的（8 次 `Settle` 调用全靠 NoError 站台）。
7. **surgical**：只改追溯得到 TODO 条目的行。顺手改的相邻代码会让 review 无法判断因果。

## 4. 波次、任务与文件独占

### Wave 1 — 契约层（3 条并行，必须先合入 main）

改的是**别人要依赖的形状**。Wave 2 全部 blocked on 它们。

| 任务 | TODO 条目 | 独占文件 | 号段 |
|---|---|---|---|
| **W1-A** 写操作幂等键补齐 | §2 首条 Major、§5「校验错误信息引用不存在的字段名」 | `core/reserve.go`、`core/booking.go`、`postgres/reserver_store.go`、`postgres/sql/queries/reservations.sql` | migration **005**、**I-35** |
| **W1-B** `BalanceCheckpoint` uid 化 + I-18 门禁 | §7 第 3、4 条 Major | `core/checkpoint.go`、`postgres/checkpoint_integrity_store.go`、`server/contract_pin_test.go` | — |
| **W1-C** 错误分类：bizcode + 瞬时可重试 | §3「`ErrUnauthorizedJournal` 无 bizcode」、§5「无瞬时可重试分类」 | `pkg/bizcode/`、`pkg/httpx/`、`core/errors.go` | bizcode **14010 起** |

⚠️ W1-A 与 W1-B 都要动 `core/interfaces.go`：**只追加、不重排**，各自只碰自己那个 interface
（W1-A = `Reserver`/`Booker`；W1-B = `CheckpointIntegrityStore`）。这是本仓上一波验证过的
减冲突办法。

### Wave 2 — 主体（8 条并行，blocked on Wave 1）

| 任务 | 覆盖 | 独占文件 | 号段 |
|---|---|---|---|
| **D-money** 记账正确性 | §1 全部**除**「17 处符号」 | `postgres/sql/queries/platform_balances.sql`、`checkpoints.sql`、`presets/fx.go`、`postgres/precision.go`、`postgres/convert.go` | **I-36** |
| **D-lock** 锁与键空间 | §2 剩余 7 条 | `postgres/sql/queries/journals.sql`、`postgres/ledger_store.go`、`postgres/registration_rescan_store.go`、`service/snapshot.go`、`service/locked_job.go` | **I-37** |
| **D-attest** facade 与接线 | §3 的 1-5、8；§5 的 Subscribe / 嵌套 RunInTx / facade 逃逸 / 双 Worker / PendingStore 注释 | `ledger.go`、`service/worker.go`、`service/delivery/`、`postgres/pending_store.go`、`core/auth.go` | **I-38** |
| **D-threat** DB 角色与守卫 | §4 的 1、2、4、6、7；§9 审计列 | 新 migration、`postgres/partition_store.go`、`postgres/roles_test.go` | migration **006 007**、**I-39 I-40** |
| **D-contract** HTTP 与契约面 | §3-6 提现闸；§4-5 铸币、§4-8 holder 文案；§5-13 `NewWithConfig`；§7 剩余 5 条 | `server/`、`docs/openapi.yaml`、`.github/workflows/`、`postgres/sql/queries/holder.sql` | **I-41** |
| **D-ops** 可观测与链上 | §6 全部；§4-3 的 Go 侧「扫 0 个不许报 complete」；§5-14 `core.Metrics`；§8-9 check1 测试 | `service/reconcile.go`、`service/rollup.go`、`service/onchain.go`、`chains/evm/`、`cmd/ledger-cli/`、`core/metrics.go`、`postgres/event_store.go`、`docs/RUNBOOK.md` | **I-42** |
| **D-surface** 文档与 examples | §5 的文档/example 类 8 条；§8-10 | `README.md`、`examples/`、`docs/frontend.md` | — |
| **D-tests** 测试可信度 | §8 的 1-8 | `*_test.go`（`server/contract_pin_test.go` 除外，W1-B 独占）、`postgres/idempotency_match.go` | — |

⚠️ **D-threat 与 D-ops 在 §4-3 上有一条接缝**：游标表的 guard 归 D-threat（migration），
「扫了 0 个不得报 `Complete=true`」归 D-ops（`service/reconcile.go`）。两边都要在 bus 上
checkpoint 自己那半做完了，**任何一半单独上线都不构成修复**。

### Wave 3 — 收尾（串行）

| 任务 | 覆盖 | 说明 |
|---|---|---|
| **W3-sign** 符号语义收敛 | §1「同一符号语义 17 处独立实现」 | **必须最后做**：它跨 `core/` `service/` `postgres/` `presets/` 与 10 处 SQL，与上面每一个域都重叠。Wave 2 全部合入后再开工，否则会与所有人冲突 |
| **W3-lead** 集成收口 | — | Team Lead：INVARIANTS 编号连续性、TODO.md 归档为已处置、CHANGELOG、发版口径 |

## 5. 每条任务的 Done 标准（统一）

一条任务算完成，必须四项齐备，缺一不算：

1. `make vet` + `make lint` + `make test` 全绿（`make test` 需要 Docker，走 testcontainers）
2. 每条修复有对应的 pin/回归测试，且**在修复前是红的**（证据写进 bus checkpoint）
3. 动到 invariant 的，`docs/INVARIANTS.md` 同步更新；动到 HTTP 面的，`docs/openapi.yaml` 同步
4. `git status` 只有本任务独占面内的文件

**破坏性变更**：本波允许，但每一条都要追加进 `TODO.md` §10 的破坏性变更清单，
写清「消费方需要做什么」——不是只写「改了签名」。

## 6. 协作协议

- 写盘一律走 worktree：`wt create ledger <slug>`，分支 `feat/<slug>`
- 调度走 `bus`：`bus board` → `bus inbox <me>` → `bus claim <id> <me>` → 干活 →
  `bus done <id> <me>`；阶段性 `bus checkpoint <me> "<进度>"`
- 踩到独占面之外的文件 → **停下，`bus send team-lead`**，不要自行扩大范围
- 发现新的失效形态 → `bus learn <me> "<经验>"`
- 流水线而非批处理：一条完成即 review + 合并，不等同波全部完成

---

## 7. Wave 1.5 — 契约收紧（Aaron 2026-08-26 拍板，插在 Wave 2 之前）

Wave 1 的两条改动各自弱化了一条不变式。Aaron 两条都选了**严格路线**：
**不为排期方便软化账本契约。** 因此 Wave 2 推迟到本波合入之后。

两条任务的独占面互不重叠，可并行。

| 任务 | 目标 | 独占文件 |
|---|---|---|
| **W15-A** `Transition` 幂等键**必填** | 消掉 W1-A 给 I-3 加的例外，恢复「每个状态变更都要幂等键」的全称命题 | `core/booking.go`、`postgres/booking_store.go`、`service/onchain.go`、`service/expiration.go`、`server/handler_bookings.go`、`server/handler_webhooks.go` |
| **W15-B** `core.BalanceCheckpoint` 去内部 id | 恢复 I-18 的原措辞：**没有任何 `core` 类型暴露内部 BIGSERIAL id** | `core/checkpoint.go`、`postgres/rollup_adapter.go`、`postgres/checkpoint_integrity_store.go`、`service/rollup.go`、`service/reconcile.go`（**仅 checkpoint 相关部分**） |

### W15-A 的真正难点：键怎么派生

16 个调用点，**10 个在 `service/onchain.go`**。这不是机械替换：

- 按 `api-contract.md` §9，系统事件发起方必须从**源事件派生确定性 key**，禁止随机数、禁止时间戳、
  禁止在重试路径内重新生成
- ⚠️ **陷阱**：同一个 booking 可以合法地多次转移到同一状态 —— 提现 preset 的
  `failed → reserved` 就是重试路径。所以 `<booking_uid>-<to_status>` **会撞**，
  第二次合法重试会被误判成重放而静默短路。派生源必须包含区分两次合法尝试的东西
  （源事件 id / tx hash + chain id / 尝试序号），不能只有 booking 与目标状态。
- 每个调用点的语义不同（充值确认 / 审核通过 / 归集发出 / 重组检测 …），**逐个想**，
  不要套一个通用公式了事

### W15-B 的两条路线（自己选，但要给证据）

目标是「I-18 原措辞成立」，不是「必须把字段改成 string」。两条都能达成：

1. **uid 化该类型** —— 直接把 `CurrencyID`/`ClassificationID int64` 换成 uid string。
   代价：`service/rollup.go:235` 的 per-item upsert 与 `service/reconcile.go:1345` 的
   `map[int64]` 都要改，rollup 热路径可能新增 uid↔id 解析。
2. **把它移出 `core`** —— 它从不经任何 `Service` 访问器（已核实），本就是 rollup/reconcile
   引擎的工作表示而非领域类型。移到 `service`（或 postgres adapter）后，
   「没有 `core` 类型带内部 id」自动成立，且热路径零改动。
   顺带把 W1-B 的 `core.RebuiltCheckpoint` 收回成 `core.BalanceCheckpoint`（uid 版），
   少一个类型。

**Team Lead 倾向路线 2**（结构上更对：`core` 不该装存储引擎的工作表示）。
但**如果你选路线 1，必须实测 rollup 热路径的成本**，不许凭感觉说「差不多」。

### 两条共同的红线

- **不许回滚 `395d545` / `e86147c`** —— 往前改，不要 revert。W1-B 那套 schema 派生的 I-18
  门禁是本波最有价值的产物之一，必须原样保留并**继续通过**
- 恢复 `docs/INVARIANTS.md` 里 I-3 / I-18 被弱化的措辞，并**删掉**当时写下的例外段落
- 同步 `docs/audits/2026-08-25-financial-engineering/TODO.md` §10：Wave 1 记的破坏性变更条目
  要更新成最终形态（例如「`TransitionInput` 新增**可选**字段」现在是**必填**）
- 越界文件（`examples/` 等）**只做编译修复**，不夹带行为变更

---

## 8. Wave 2 发车说明（2026-08-26，Wave 1 + 1.5 已全部合入）

号段重新核对后的实际分配（`I-34` 仍是当前最大，`005` 是当前最大 migration）：

| 任务 | invariant | migration |
|---|---|---|
| D-money | **I-35** | — |
| D-lock | **I-36** | — |
| D-attest | **I-37** | — |
| D-threat | **I-38 I-39** | **006 007** |
| D-contract | **I-40** | — |
| D-ops | **I-41** | — |
| D-surface / D-tests | — | — |

### 已被 Wave 1 / 1.5 提前做掉的条目（不要重复做）

- §2 首条 Major（终态操作幂等键）→ W1-A + W15-A
- §3「`ErrUnauthorizedJournal` 无 bizcode」、§5「无瞬时可重试分类」→ W1-C
- §7 第 3、4 条 Major（`BalanceCheckpoint` uid / I-18 门禁）→ W1-B + W15-B
- §5「校验错误信息引用不存在的字段名」→ W1-A

### 两处必须知道的接缝

1. **`postgres/ledger_store.go` 被 D-lock 与 D-attest 同时命中，但区域不同**：
   D-lock 只碰 `balancePairsFromEntries` / `acquireBalanceLocks` / `ExecuteTemplateBatch`
   （约 68–132 与批量分支）；D-attest 只碰 tx 模式的 `AuthStatusUnsignedTxMode` 分支
   （约 427、574–590）。**双方都不得重排、不得整文件 gofmt 之外的格式变动。**
   合并顺序：**D-lock 先合，D-attest 后合**，冲突由 Team Lead 解。
2. **§4-3（检测层游标）仍是跨域接缝**：guard/migration 归 D-threat，
   「扫了 0 个不得报 `Complete=true`」的 Go 侧归 D-ops。**任何一半单独上线都不构成修复**，
   两边都要在 bus 上 checkpoint 自己那半完成。

### Wave 1 留下的三条追加任务（已在 board）

`#24` → D-lock（`ErrTransient` 接线）｜`#25` → D-threat（收据表守卫）｜`#28` → D-tests（门禁派生去重）

### 本波新增的硬要求（来自 Wave 1 的教训）

- **注释里的「以后要记得」= 缺一个门禁**。W15-A 的 deposit 键安全性挂在「生命周期无环」上，
  只写在注释里、没有任何检查 —— 合并前补了 `presets/lifecycle_acyclic_test.go`。
  你写下任何「因为 X 成立所以这样做是安全的」，就要问：**X 能不能机器检查？**能就去做。
- **交付三样齐备**：`bus done` + `bus send team-lead result` + 最终回复。
  上一波有 agent 报 idle 但正文零送达，Team Lead 只能自己重跑一遍它的活。

### ⚠️ 波内已知红：`TestInvariantsDocIsOrderedAndGapless`（Team Lead 的调度缺陷，非分支缺陷）

我把 I-35..I-41 一次性预分配给八个并行分支，于是**任何单个分支在隔离状态下写入自己的号，
都会在 `docs/INVARIANTS.md` 里留下空洞**，这条门禁必然红（首次由 D-threat 于 `I-38 follows I-34` 撞上）。

- **门禁是对的，报的是真实空洞。** 不许弱化它去容忍空洞，不许任何分支改自己的号去凑连续
  （改号 = 与兄弟分支撞车，代价远大于一条红测试）
- **归 Team Lead 收口**：每合并一个分支立即复跑；全部合入后**必须为绿**；
  若某个号段最终无人使用，由 W3-lead 统一重编号
- **各分支的 Done 判据据此调整**：允许这一条红，但最终回复必须**逐字写明**
  「除 `TestInvariantsDocIsOrderedAndGapless` 外全绿」——
  **「全绿」与「有一条红」不能同时成立**，含糊表述会让真实的失败藏在里面

**顺带暴露的另一处未声明争用**：`docs/INVARIANTS.md` 被八个分支共同写入。
各分支**只追加自己那一节，不重排、不动别人段落**；合并冲突由 Team Lead 解。
下次分波时，共享文档与共享号段应当与代码文件一样进独占表。

---

## 9. Wave 3（收尾波，2026-08-26）

Wave 2 八域全部合入，main 绿，`I-34..I-41` 连续，migration 至 `007`。本波两条并行 + Team Lead 收口。

| 任务 | 覆盖 | 独占 | 号段 |
|---|---|---|---|
| **W3-sign** | §1「同一符号语义 17 处独立实现」 | `core/account_policy.go`、`service/rollup.go`、`service/reconcile.go` 的符号判断部分、`postgres/ledger_store.go` 的符号判断部分、`postgres/trial_balance_store.go`、`postgres/reconcile_queries.go`、以及 5 个 `.sql` 里的符号表达式 | migration **009**、**I-42** |
| **W3-id** | board `#37` `journal_entries.id` 跨分区不唯一 | 新 migration、`postgres/roles_test.go`、`postgres/grant_coverage_test.go` | migration **008**、**I-43** |
| **W3-lead** | CHANGELOG、TODO 处置表、发版口径 | Team Lead | — |

### 号段分配的教训（已在 §8 记过，本波按新办法做）

预分配号段导致八个分支各自留洞、`TestInvariantsDocIsOrderedAndGapless` 反复红，
且八个分支全撞 `docs/INVARIANTS.md`。**本波只有两条任务，仍按分配走，但 Team Lead 在
合并时重编号，各分支不得自行改号**——办法同 Wave 2，只是规模小到不至于反复冲突。

### W3-sign 的设计要点（不是机械替换）

- Go 侧收敛成**单一判断点**（一个函数），其余调用它，不各自实现
- SQL 侧 10 处表达式、3 种写法（4-way CASE + `ELSE 0`、3-way + 兜底当 debit、`OR` 合并式、
  字符串比较式、`MIN(normal_side)`）—— 建议收敛成一个 `IMMUTABLE` 的 SQL 函数，
  由 migration 009 提供，各查询调用它
- ⚠️ **性能**：符号判断在 rollup / reconcile / balance 的热路径上逐行执行。若走 SQL 函数，
  必须确认 Postgres 能内联（简单 SQL 函数 + `IMMUTABLE`），并**实测**对照
  `postgres/benchmarks_test.go` 的既有基线。**不许凭感觉说「差不多」**——本仓有先例要求实测数字
- ⚠️ **`ELSE 0` 是唯一会静默少算钱的那一份**（`checkpoints.sql:62`）：该 entry 不计入余额。
  收敛时这种「未知符号 → 静默吞掉」必须变成**拒绝**，不是保留
