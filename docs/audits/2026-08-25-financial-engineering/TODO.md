# 审计遗留工作清单

> 源：`docs/audits/2026-08-25-financial-engineering/`（8 份 territory 报告，90 条发现）。
> **8 条 Critical 已修**，处置表见本目录 `README.md`。本文件列**剩下的 44 Major + 35 Minor**。
>
> **给接手者**：每条都给了 `file:line` 和它来自哪份报告 —— 完整论证、失效场景、最小复现
> 都在那份报告里，别只看这里的一行标题就动手。标 `PLAUSIBLE` 的**先验证再修**，
> 审计员自己没走完。
>
> 按**主题**分组而非按报告分组：同一处代码常被多个 territory 从不同角度命中，
> 按主题干可以一次改完。

## 0. 先读这个：C1 删掉服务二进制后哪些条目已失效

`30bd872` 删掉了 `cmd/ledgerd` / `deploy/` / `Dockerfile`。以下条目**整条或部分作废**，
不要照着修：

| 条目 | 状态 |
|---|---|
| `ledgerd` 从未接线 P5/P6 防篡改（structure） | **✅ 已失效** —— 那个二进制不存在了 |
| `cmd/ledgerd` / compose / Helm 默认把整波关着（threat-model） | **⚠️ 部分失效** —— compose/Helm 已删；「库模式默认不接线」仍成立，见 §3 |
| Helm 无法开启已发布特性（我的 operability 报告） | **✅ 已失效** |
| `/metrics` 在业务端口无鉴权（threat-model Minor） | **⚠️ 取决于消费方怎么挂 `server/`**，不再是本库的部署决定 |

`server/` **保留**（它不是 composition root，且 `ledger-react` 依赖 `openapi.yaml` 契约），
所以涉及 `server/` 与 `openapi.yaml` 的条目**仍然有效**。

---

## 1. 记账正确性（会算错钱，优先级最高）

| 级别 | 条目 | 位置 | 报告 |
|---|---|---|---|
| Major | **偿付能力把 user-side debit-normal 费用账当成负债** —— 每笔提现手续费让平台看起来少偿付一笔手续费 | 见报告 | `financial-correctness.md` |
| Major | **`effective_at` 回溯记账不会让已写入的历史快照失效** —— 不是实时余额错，是历史快照永不重算 | 见报告 | `financial-correctness.md` |
| Minor | **同一个符号语义有 17 处独立实现**，未知 `normal_side` 时有三种互不相同的处置 | 见报告 | `financial-correctness.md` |
| Minor | `presets/fx.go` 的文档与自己的代码符号相反 | `presets/fx.go` | `financial-correctness.md` |
| Minor | `journal_entries.id` 单独不唯一，I-5 的单调性完全依赖序列 | — | `financial-correctness.md` |
| Minor | I-5 的载荷前提只有散文守卫，无机器门禁 | — | `financial-correctness.md` |
| Minor | 精度报错信息把 currency uid 冒充 code | — | `financial-correctness.md` |
| Minor | 金额路径上的死防御分支选择「警告并继续」而非「拒绝」（当前不可达，已实测） | `postgres/convert.go:122` | `lead-financial-spotchecks.md` |

⚠️ **前两条与已修的 C7（转账/收费方向）同源** —— 都是符号约定问题。建议一起做，
并把「17 处独立实现」收敛成单一判断点，否则下一个符号 bug 只是时间问题。

## 2. 幂等与并发

| 级别 | 条目 | 位置 | 报告 |
|---|---|---|---|
| Major | **`Settle`/`Release`/`FinalizeSettlement`/`Transition` 没有幂等 key**，重放返回 `ErrInvalidTransition` —— 与 I-3 声称的「每个状态变更都要幂等键」直接矛盾，README:38 也这么写 | — | `concurrency.md` + `consumer-surface.md`（两处独立命中） |
| Major | **`registration_rescans` 是三处 claim 机制里唯一没有 claim-token guard 的** —— 过期 worker 能改回游标、把已完成 job 改回 pending、抹掉活 worker 的 claim | — | `concurrency.md` |
| Major | **调用方可控的 `idempotency_key` 与 balance 锁共用同一把 advisory lock 键空间** → 传 `idempotency_key="balance:1:1"` 可构造 ABBA 死锁 | — | `concurrency.md` |
| Major | **`ExecuteTemplateBatch` 的 balance lock 只在单个 journal 内排序**，跨 journal 无全局序 → 调用方可控的 ABBA 死锁 | — | `concurrency.md` |
| Major | **`Reserve(RequireVerifiedBalance:true)` 在 tx 模式下会在调用方已开的事务里发起外部调用** —— 违反 `financial.md` 红线 | `postgres/reserver_store.go:107-113` | `concurrency.md` |
| Major | **`expiration` job 无 leader 选举、`GetExpiredReservations` 无 claim**；且过期能在 in-flight `Settle` 下抽走冻结 | — | `concurrency.md` |
| Minor | 三套互不相干的哈希共用一个 advisory lock 键空间，且注释自相矛盾 | — | `concurrency.md` |
| Minor | `RebuildCheckpoint` 会被它要修的东西永久挡住 | — | `concurrency.md` |

## 3. 防篡改链的接线与覆盖

⚠️ C1 删掉了服务二进制，但**库模式的接线问题仍在**：消费方要自己串起
`WithAttestor` → `svc.AttestationService(anchor)` → `worker.SetAttestor(...)` →
逐调用点传 `RequireVerifiedBalance`，四步没有一步在 facade 层被提示。

| 级别 | 条目 | 位置 | 报告 |
|---|---|---|---|
| Major | **`Worker.SetAttestor` 全仓无任何生产调用点** —— P6 批量存证链在库模式默认不运行，而 `DefaultWorkerConfig` 给它配了间隔（看起来像开着的） | `service/worker.go:147` | `consumer-surface.md` + `concurrency.md` + `test-credibility.md`（三处独立命中） |
| Major | **`svc.Worker` 声称「零值字段一律填默认」，却漏了 `AttestInterval`/`AttestBatchSize`** → 存证 job 静默跳过，Warn 打进 `NopLogger` | `ledger.go` | `consumer-surface.md` |
| Major | **`svc.Worker(cfg)` 静默返回一个关掉了三个 job 的 Worker**，而 README 明写它包含 event delivery | `ledger.go` | `consumer-surface.md` |
| Major | **经 `RunInTx` 落的 journal 永久 `unsigned_tx_mode`**，会把该维度的 `VerifiedBalance` 永久打成 UNDEFINED | — | `consumer-surface.md` |
| Major | **`ConfirmPending`/`CancelPending` 的 journal 在配置了 Attestor 的 pool 模式下也永远不签名** | — | `concurrency.md` |
| Major | **提现闸没有任何 HTTP 表面** —— 挂了 `server/` 的消费方用不上，且文档没说 | `server/handler_reservations.go` | `consumer-surface.md` + `structure.md` |
| Major | **`core.ErrUnauthorizedJournal` 没有 bizcode 映射** —— 篡改检测在错误面上表现为「可重试的内部错误」 | `pkg/bizcode/` | `consumer-surface.md` |
| Minor | `CanonicalJournalDigest` 不覆盖 `Metadata`，且这个排除未被文档声明 | `core/auth.go` | `test-credibility.md` |

## 4. 威胁模型剩余缺口

| 级别 | 条目 | 位置 | 报告 |
|---|---|---|---|
| Major | **§14 的「权限从 trigger 派生」对意图是 fail-open，I-25 的全称命题为假且无门禁** | `001_baseline.up.sql` §14 | `threat-model.md` |
| Major | **分区维护路径要求应用持有 owner 权限，而 owner 的 `TRUNCATE` 绕开 append-only trigger** | `service/partition.go` | `threat-model.md` |
| Major | **检测层自己的游标可被 `ledger_app` 改写**，且「扫了 0 个」上报为 `Complete=true, Passed=true` | `reconcile_scan_cursors` | `threat-model.md` |
| Major | **`ledger_ro`（BI 角色）能读出站 webhook HMAC 密钥** —— 只读凭证升级成对下游的完整性能力 | `webhook_subscribers.secret` | `threat-model.md` |
| Major | **一个 write-scope API key 就能铸造与真实充值不可区分的入账**，签名机制不覆盖这一层 | — | `threat-model.md` |
| Minor | **`001_baseline` 复用同名既有 role 而不校正其属性** → I-22 可在共享集群上静默为假 | `001_baseline.up.sql` §1 | `threat-model.md` |
| Minor | `ledger_owner`/`ledger_ro` 以 `LOGIN` 无密码创建 | `001_baseline.up.sql` §1 | `threat-model.md` |
| Minor | holder 面把内部 journal type code 直接发给终端用户（`user-facing-surfaces.md`） | `server/handler_holder.go` | `threat-model.md` |

## 5. 消费方表面与文档

| 级别 | 条目 | 位置 | 报告 |
|---|---|---|---|
| Major | **README 里所有 Go 代码块都停在 uid 化之前，一行都编译不过** | `README.md` | `consumer-surface.md` |
| Major | **README 的 "API Surface" 缺 11 个导出方法**（含整个完整性子系统）；而 `CheckpointIntegrity` 的 godoc 说提现路径 **MUST** 用它 | `README.md` | `consumer-surface.md` |
| Major | **`Worker.Subscribe` 文档承诺 at-most-once，实现是 at-least-once** | `service/worker.go:157` | `consumer-surface.md` |
| Major | **在 `RunInTx` 回调里再调 `tx.RunInTx` 会另开独立事务**，原子性静默失效 | `ledger.go` | `consumer-surface.md` |
| Major | **`examples/crypto-deposit` 教的手搓 ingest 绕过 `AutoCreditCeiling` 闸门**，且用内存地址表 | `examples/crypto-deposit/` | `consumer-surface.md` |
| Major | **`examples/credits-topup` 把实扣 journal 放在 `Settle` 之后、事务之外** —— 崩在中间就是「用了额度没扣钱」（**与已修的 C5 同形**） | `examples/credits-topup/` | `consumer-surface.md` |
| Major | 库消费方没有任何「瞬时可重试」错误分类，`bizcode.Retryable` 只服务 HTTP | `pkg/bizcode/` | `consumer-surface.md` |
| Minor | facade 的若干方法在 `RunInTx` clone 上会逃出事务 | `ledger.go` | `consumer-surface.md` |
| Minor | `svc.Worker()` 改写共享 `eventStore` 状态；一个 Service 建两个 Worker 会互抢事件队列 | `ledger.go` | `consumer-surface.md` |
| Minor | 四个 example 共用的 `ensureCurrency` 把任意错误当 "not found"，且从不校验精度 | `examples/*/main.go` | `consumer-surface.md` |
| Minor | `examples/tx-compose` 声称演示回滚，但从不验证 journal 真的不在 | `examples/tx-compose/` | `consumer-surface.md` |
| Minor | `PendingStore` 的类型注释说构造函数会报错 —— 它不会；`InstallPendingBundle` 在 README 里 0 命中 | — | `consumer-surface.md` |
| Minor | `server.NewWithConfig` 23 个位置参数，配置非法时 `panic` | `server/server.go` | `consumer-surface.md` |
| Minor | `core.Metrics` 有 30 个方法（README 说 ~20），`nopMetrics` 未导出，无可嵌入基类 | `core/metrics.go` | `consumer-surface.md` |
| Minor | preset bundle 数量在四个地方说了三个不同的数 | — | `consumer-surface.md` |
| Minor | 校验错误信息引用了已经不存在的字段名 | — | `consumer-surface.md` |
| Minor | `examples/fullstack/backend` 重复实现了 `ledger.Migrate` 已内建的 URL scheme 转换 | `examples/fullstack/` | `consumer-surface.md` |

## 6. 可观测与运维

| 级别 | 条目 | 位置 | 报告 |
|---|---|---|---|
| Major | **`full_coverage` 永远为假** —— check #8 永久 `Complete=false` 且一票否决，`"full suite passed"` 是死代码 | `service/reconcile.go:922,434` | `operability.md` |
| Major | **`balance_drift_units` 被喂了余额而不是漂移**，永不复位；与真正的 checkpoint 篡改探测器共用一条告警规则，**消音会把后者一起消掉** | `service/rollup.go:230` | `operability.md` |
| Major | **链上资金路径六个指标零告警、零 RUNBOOK**（含「归集到无法归属的代币」「已入账充值在链上消失」两个偿付能力事件） | `observability/prometheus.go` | `operability.md` |
| Major | **归集的重试状态只在内存** → 卡单 + 重启 = 该链归集永久堵死，且从不去链上读那笔的 fee | `service/onchain.go:329` / `chains/evm/sweeper.go:45` | `onchain-money-path.md` |
| Major | **余额扫描两条路径失败语义相反**（fail-open vs fail-closed），走哪条取决于该链碰巧有没有 Multicall3 | `chains/evm/scanner.go:145` vs `:180` | `onchain-money-path.md` |
| Minor | gas ceiling 校验的量与实际支付的量不是同一个 | `service/onchain.go:1422` vs `chains/evm/sweeper.go:203` | `onchain-money-path.md` |
| Minor | `ledger-cli` help 说 10 个 check，实际 14 | `cmd/ledger-cli/main.go:234` | `operability.md` |
| Minor | `event_store` 的「claim lost」用包级 `slog`，消费方注入的 logger 看不到 | `postgres/event_store.go` | `concurrency.md` |

## 7. 契约与结构

| 级别 | 条目 | 位置 | 报告 |
|---|---|---|---|
| Major | **openapi 的唯一 CI 门禁只验「TS = spec」，从不验「spec = Go 行为」**；且 path-filtered（纯 Go 改动不触发）、发布流水线里还缺席 | `.github/workflows/ledger-react.yml:6-16` | `structure.md` |
| Major | **`next_cursor` 从不序列化为 JSON `null`**，两个 handler 行为还不一致（一个 `omitempty` 消失、一个空字符串）—— 系统性偏离 `api-contract.md` §6 | `server/response.go:12` / `server/handler_holder.go:146` | `structure.md` |
| Major | **`core.BalanceCheckpoint` 在 Go 库 API 面暴露内部 BIGSERIAL id**，而同族 `core.Balance` 用 uid，且 `RebuildCheckpoint` 入参收 uid、返回给 id | `core/checkpoint.go` | `structure.md` + `test-credibility.md` |
| Major | **I-18（uid-only）的机械扫描门禁用硬编码词表**，漏了 `entry_id`/`policy_id` 等实际列名 | `core/` 门禁测试 | `test-credibility.md` |
| Minor | `docs/openapi.yaml` 与 handler 有具体可复现的漂移（`expires_in_sec` int64 秒 vs 文档 `expires_in` duration 串） | `docs/openapi.yaml` | `structure.md` |
| Minor | Health/Ready 探针失败路径绕开统一响应信封 | `server/` | `structure.md` |
| Minor | 部分 port 超出 `golang.md` 的 1-3 方法建议（判断题，非必然是 bug） | `core/interfaces.go` | `structure.md` |

## 8. 测试可信度

| 级别 | 条目 | 位置 | 报告 |
|---|---|---|---|
| Major | **I-4/I-11 的 TOCTOU pin 无法证伪它自己** —— 两笔并发 Reserve 之和（50+30）从不逼近余额（100），删掉 advisory lock 也会全绿。**全仓没有一个并发超额预留测试** | `postgres/reserver_store_test.go:127` | `test-credibility.md` + `concurrency.md`（两条独立路径命中同一结论） |
| Major | **Extended preset bundle（含 FX）从未在真实 Postgres 上过账测试** —— 只在语义比真实实现宽松的 fake store 上跑 | `presets/*_test.go` | `test-credibility.md` |
| Major | **`ReserverStore.Settle` 的实际效果在单测层完全没被验证** —— 8 次调用全靠 `require.NoError` 站台 | `postgres/reserver_store_test.go` | `test-credibility.md` |
| Major | snapshot backfill 只验证行数/天数，从未断言具体余额数值 | — | `test-credibility.md` |
| Minor | `AggregateCheckpointsByClassification`（`system_rollups` 的聚合来源）跨 holder 聚合从未被真实测试验证 | — | `test-credibility.md` |
| Minor | I-21 的「事件-journal 交叉链接」断言其实没测 —— `core.Booking` 结构上拿不到 `EventUID` | `service/onchain_integration_test.go:1136` | `test-credibility.md` |
| Minor | I-19（sweep 从不入账）的 postgres 层 pin 单独看只覆盖 `sent → confirmed` | — | `test-credibility.md` |
| Minor | booking metadata 幂等排除列表从 1 个键涨到 4 个，只有第一个有专属测试 | — | `test-credibility.md` |
| Minor | check1（`global_dr_cr_equality`）缺少「检测到真实全局失衡」的集成测试 | `service/reconcile.go:478` | `test-credibility.md` |
| Minor | examples 里两处代码风格问题会被照抄 | `examples/` | `consumer-surface.md` |

## 9. 共同盲区（八份报告都没提，Team Lead 补的）

**事发之后，能不能重建出攻击者做了什么？**

账本对「谁动了钱」记录极细（`journals`/`events`/`account_policy_changes`/`period_closes`/
`checkpoint_rebuilds` 全带 `actor_id`），对「**谁改了决定钱去哪的规则**」几乎不记录：

| 表 | 审计列 | UPDATE 后可查到 |
|---|---|---|
| `entry_template_lines` | 无 | 什么都没有 |
| `currencies` | 无 | 什么都没有 |
| `deposit_addresses` | 仅 `created_at` | **连「改过」这件事都看不出来** |
| `entry_templates` | 仅 `created_at` | 同上 |

C2（已修）**阻止**了这类攻击，但**看不见**它被尝试过。migration 003 的守卫会拒绝写入，
而拒绝本身不留痕迹 —— 一次失败的攻击尝试与从未发生没有区别。

**建议**：给这些规则表加 `updated_at` + `actor_id` + 一张 append-only 变更表
（`account_policy_changes` 已经是这个模式的现成范例，只是没推广）。

Team Lead 倾向**先做这条**：它是 C2 的另一半，而且便宜。

## 10. 发版

- 距上个 tag `ledger-react-v0.5.1` 已 **175+ 提交**，**14 个未推送**
- CHANGELOG 的 Unreleased 段已备齐（含本次的破坏性变更与两条会算错钱的修复）
- **版本号是 Aaron 的决定**。本次破坏性变更包括：
  - 删掉服务二进制与部署面
  - `transfer_out`/`transfer_in`/`fee_charge` 的借贷方向修正 —— **消费方需要冲销既有 journal**
  - 分数冲销的聚合口径修正 —— **需要检查含重复维度的既有冲销**
  - `NewReserverStore` 签名、`journals.event_id` 的 Go 类型（更早的变更）
- Go module 与 npm 包从 0.5.0 起版本对齐，**须同发**
