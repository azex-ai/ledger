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
