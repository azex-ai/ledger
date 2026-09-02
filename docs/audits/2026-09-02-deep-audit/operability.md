# 审计报告 — operability（可运维性）

> Territory I，第二轮深度审计。基线 commit `fd87cae`。
> 契约：`docs/plans/2026-09-02-deep-audit-contract.md` + `docs/plans/2026-08-25-financial-audit-contract.md` §1/§3。
> 镜头：`working-agreements.md` §3「未运行≠通过 / 降级必须落痕 / fail-closed」。
> 本territory 审的不是「这个库有没有仪表」，而是「**消费方的 on-call 凌晨三点能不能用它**」。

---

## 摘要

**这个库的仪表盘接了线，但一半的表针没接到发动机上，而说明书上的读数名字有一多半是错的。**

三条互相咬合的结论：

1. **`core.Metrics` 的 32 个方法里有 12 个在全仓没有任何生产调用点**（codegraph + grep 双向确认）。
   缺口不是随机的——它整块落在 `postgres/` 层，因为**`postgres/` 包根本没有 `core.Metrics` 依赖**。
   于是「账本写入」这件事本身（`JournalPosted` / `JournalFailed` / `JournalLatency` /
   `IdempotencyCollision` / `TemplateFailed` / `BookingTransitioned` / 三个 Reserve 计数器）
   **在结构上不可能被观测**。一个健康账本和一个完全停摆的账本，在这 12 个 series 上读数完全相同：恒 0。

2. **文档在为这些不存在的读数写处置流程。** README 广告的 6 个指标名里 4 个不存在、其余 2 个恒 0；
   RUNBOOK §6/§7 是两节完整的 on-call 处置流程，其告警源永远不会触发；CAPACITY 的头号 SLO
   （journal post latency）挂在一个永远为空的 histogram 上。而 P0 那一节（§2 偿付能力）的三条命令
   **全部执行不了**：`--currency 1` 传的是 id 而 CLI 要 uid、curl 的路径与方法都不对、jq 表达式
   针对的字段名和数组名都不存在。§3 的确认 SQL 查的是一张**不存在的表**（`checkpoint_rollup_queue`，
   实际叫 `rollup_queue`）——这正是上轮 `ledger_app` 那条的兄弟。

3. **默认全黑，且没有任何东西会告诉你。** `ledger.New(pool)` 的默认是 `NopLogger` + `NopMetrics`，
   `Worker` 不自动接 `SetFullReconciler`。三者叠加：整套检测（15 项对账 check）不跑，
   而唯一会说「它没跑」的那行启动日志，被第一项默认（NopLogger）吃掉了。
   **观测配置的零值，关掉的正是那个本该告诉你其余配置也关着的机制。**
   同一个仓库里的 `server/` 用包级 `slog` 打四条不可静音的危险配置警告——作者显然知道这个顾虑，
   只是没把它应用到库本体。

上轮修复复核发现两条「修了但没接上」：`EventStore.SetLogger`（为修上轮 Minor 而加，**零生产调用点**）
与 `full_coverage`（库路径已可为真，但**唯一的运维入口 `ledger-cli reconcile --full` 永远为假**，
且 DR 演练记录把这个症状误归因成「seed 脚本的问题」）。

**0 Critical / 11 Major / 12 Minor。** 没有 Critical 是诚实的结论：本 territory 找到的都不是
「算错钱」，而是「算错了没人知道 / 知道了查不出来」。但第 1 条与第 2 条叠加后，
**P0 事故响应会在第一条命令上失败**，这是本报告最要紧的实际后果。

---

## 发现

### [Major] I-1 · `core.Metrics` 32 个方法里 12 个零生产调用点，账本写入路径整体无仪表

- **位置**：`core/metrics.go:24-128`（接口，32 方法）；`observability/prometheus.go:106-291`（32 个 collector 全部 `MustRegister`）；缺口根因 `postgres/`（全包无 `core.Metrics` 引用）
- **判定**：CONFIRMED（grep 与 `codegraph callers` 两种独立方法结论一致）
- **零调用点的 12 个**：

  | 接口方法 | 注册的指标名 | 生产调用点 |
  |---|---|---|
  | `JournalPosted` | `ledger_journals_posted_total` | **0** |
  | `JournalFailed` | `ledger_journals_failed_total` | **0** |
  | `JournalLatency` | `ledger_journal_post_seconds` | **0** |
  | `JournalEntryCount` | `ledger_journal_entry_count` | **0** |
  | `IdempotencyCollision` | `ledger_idempotency_collisions_total` | **0** |
  | `TemplateFailed` | `ledger_template_failed_total` | **0** |
  | `BookingTransitioned` | `ledger_bookings_transitioned_total` | **0** |
  | `ReserveCreated` | `ledger_reservations_created_total` | **0** |
  | `ReserveSettled` | `ledger_reservations_settled_total` | **0** |
  | `ReserveReleased` | `ledger_reservations_released_total` | **0** |
  | `ActiveReservations` | `ledger_reservations_active` | **0** |
  | `ReservedAmount` | `ledger_reserved_amount_units` | **0** |

  其余 20 个有调用点，全部在 `service/`（rollup 4、reconcile 4、onchain 8、delivery 4）。

- **失效场景**：
  `core.Metrics` 是**面向消费方的 port**，其 doc comment（`core/metrics.go:14-15`）明写
  「one method per emitted signal」。一个消费方按 README 的指引嵌入 `NoopMetrics` 并实现
  `JournalPosted` 接进自己的 dashboard（README:511 与 `core/metrics.go:22-23` 都拿它当范例），
  得到的是一条**恒 0 的曲线**。
  - 最自然的「账本死了」告警 `rate(ledger_journals_posted_total[5m]) == 0` → **永久触发**，
    于是被关掉；
  - 「journal 吞吐」dashboard → 恒 0，和真停摆无法区分；
  - `ledger_reservations_active` 恒 0 → 「预留泄漏」这类问题在 Prometheus 侧完全不可见
    （`GET /system/health` 的 `active_reservations` 是另一条独立的 DB 查询路径，唯一还能看的地方）。

  这是 `working-agreements.md` §3 的教科书形态：**「如果这一步其实根本没执行，我现在看到的输出会有任何不同吗？」→ 不会。**

- **根因（结构性，不是漏写几行）**：`core.Engine`（`core/engine.go`）是唯一携带 `Metrics` 的载体，
  而它只被注入到 `service/*`（`service/rollup.go:70`、`reconcile.go:435`、`worker.go:116`、
  `expiration.go:54`、`snapshot.go:47`、`attestation.go:100`、`partition.go:34`…）。
  `postgres/` 里 `PostJournal` / `ExecuteTemplate` / `Reserve` / `Settle` / `Transition`
  这些真正发生业务动作的地方，**没有任何途径拿到 `Metrics`**（`grep 'core.Metrics' postgres/` = 0 命中）。
  所以这 12 个方法不是「忘了调」，是**没有地方能调**。
- **为什么现有机制没拦住**：`core/observability_test.go` 与 `observability/prometheus_test.go`
  逐个调用全部 32 个方法验证适配器不 panic——它们证明的是「适配器能被调用」，
  不是「生产代码会调用」。`service/rollup_test.go:627-643` 的 `recordingMetrics` 同样为
  全部 32 个方法写了空实现。**没有任何门禁检查「接口里的方法是否有生产发射点」。**
  上轮 D-ops「make the operational signals mean something」（`90035d6`）修的是
  已发射指标的语义，没有问「哪些根本不发射」。

---

### [Major] I-2 · README 广告的 6 个指标名，4 个不存在、2 个恒 0

- **位置**：`README.md:552-555`
- **判定**：CONFIRMED（逐个对照 `observability/prometheus.go` 的 32 个 `Name:` 字面量）
- **失效场景**：README「Observability」一节是消费方接指标时唯一会读的东西：

  | README 写的 | 实际注册名 | 状态 |
  |---|---|---|
  | `ledger_journals_posted_total` | 同名 | 名字对，**恒 0**（I-1） |
  | `ledger_journal_latency_seconds` | `ledger_journal_post_seconds` | **不存在**，且恒 0 |
  | `ledger_reservations_active` | 同名 | 名字对，**恒 0**（I-1） |
  | `ledger_pending_rollups` | `ledger_rollups_pending` | **不存在**（词序反了） |
  | `ledger_balance_drift` | `ledger_balance_drift_units` | **不存在**（缺后缀） |
  | `ledger_reconcile_gap` | `ledger_reconcile_gap_units` | **不存在**（缺后缀） |

  **六个里没有一个是「照抄就能用」的**。消费方按 README 写 PromQL → 四条查空、两条恒 0，
  且 Prometheus 对不存在的 metric name 不报错，只返回空结果集 —— 一个查空的告警规则
  与一个从不违规的健康系统，在告警平台上完全同形（`working-agreements.md` §3）。

- **顺带**：`README.md:559` 说接口「intentionally narrow (~20 methods)」——实际 **32**
  （`sed -n '/^type Metrics interface/,/^}/p' core/metrics.go | grep -cE '^\t[A-Z]'` = 32）。
  上轮 TODO §5 已把「README 说 ~20，实际 30」记为 Minor，未修，且数字又涨了 2。
- **为什么现有机制没拦住**：仓库为 README 的**方法表**建了机器门禁
  （`readme_api_surface_test.go`：任何导出的 `*Service` 方法不在 README 表里就红），
  但**指标名字符串不在任何门禁覆盖内**。同一份 README 里，被机器盯着的那部分是对的，
  没被盯着的那部分 6 个错 4 个。

---

### [Major] I-3 · RUNBOOK §6 / §7 是两节为「永远不会触发的告警」写的 on-call 处置流程

- **位置**：`docs/RUNBOOK.md:398`（§6 alert source `IdempotencyCollision{journal_type_code="..."}`）、
  `docs/RUNBOOK.md:428`（§7 alert source `JournalFailed{journal_type_code, reason}`）、
  `docs/RUNBOOK.md:1239`（§14 表格里的 `ledger_template_failed_total{template, reason}`）
- **判定**：CONFIRMED
- **失效场景**：三个告警源全部来自 I-1 的零调用点集合。具体后果：
  - **§7 的整张 reason 表**（`unbalanced` / `unauthorised_classification` /
    `insufficient_balance` / `currency_mismatch` / `db_error`）是一套**从未被任何代码产生过的词表**
    ——`JournalFailed` 没有调用点，所以没有任何地方决定过 reason 取值。
    RUNBOOK §14（`:1239`）还让 on-call「Cross-reference `reason` against §7's table
    (same reason vocabulary)」——把一份虚构词表当成两节文档之间的共享契约。
  - **§6** 教 on-call 区分「客户端重试」「坏的 key 派生」「重放攻击」三种原因——
    这个判断永远不会被触发，而**幂等冲突本身是真实存在的**（`postgres/idempotency_match.go`
    会返回 `ErrConflict`），只是没有任何计数器记录它。真发生了重放攻击，Prometheus 一片安静。
  - RUNBOOK §14 是上轮 D-ops 修复新写的一节，用的是**真实的导出指标名**（`ledger_*_total`），
    §1–§7 是旧的，用的是 **Go 方法名冒充指标名**（`ReconcileCompleted` / `PendingRollups` /
    `CheckpointAge` / `IdempotencyCollision` / `JournalFailed`），且标签名也是编的
    （`journal_type_code` 实际是 `journal_type`；`class_code` 实际是 `class`）。
    §4（`:294`）还把 `CheckpointAge` 叫成 histogram——它是 `GaugeVec`。
- **最小复现**：`grep -c "ledger_journals_failed_total\|ledger_idempotency_collisions_total" docs/RUNBOOK.md` = 0；
  `codegraph callers JournalFailed` → No callers found。
- **为什么现有机制没拦住**：文档里的指标名与代码里的 `Name:` 字面量之间没有任何联结。
  上轮为「RUNBOOK 没有链上指标条目」加了 §14（正确），但没有回头校验 §1–§7 引用的指标是否存在、
  是否会发射。

---

### [Major] I-4 · P0 偿付能力那一节的三条命令全部执行不了

- **位置**：`docs/RUNBOOK.md:211`、`:218`、`:220`（§2 Solvency check failed，标注 **Severity: P0**）
- **判定**：CONFIRMED
- **失效场景**：三条命令逐条核：

  1. `docs/RUNBOOK.md:211` —「Alert source: `SolvencyCheck` returning `solvent: false` …
     via `POST /api/v1/system/solvency` (when wired)」
     → 真实路由是 **`GET /platform/solvency`**（`server/routes.go:76`，
     `docs/openapi.yaml:1390`）。**方法和路径都不对**，且 `/system/solvency` 这个路径不存在。
     另外这条 P0 **没有任何指标源**——`core.Metrics` 里没有 solvency 相关方法（全 32 个已核）。
  2. `docs/RUNBOOK.md:218` — `ledger-cli solvency --currency 1`
     → `cmd/ledger-cli/main.go:259` 的 flag 是 `fs.String("currency", "", "currency uid")`，
     经 `svc.SolvencyChecker().SolvencyCheck(ctx, *currency)` 传的是 **uid**。
     传 `1` 会走到 not-found。这是 I-18（uid-only）迁移之后没同步的残留。
  3. `docs/RUNBOOK.md:220` —
     `curl http://ledger/api/v1/system/balances | jq '.rollups[] | select(.classification_code=="custodial")'`
     → 三处不匹配：响应是 `{code,message,data}` 信封，列表在 **`data.list`** 而不是 `.rollups`
     （`server/response.go:18-21` `PagedResponse.List` json tag `"list"`；
     `server/handler_system.go:82` `httpx.OK(w, PagedResponse[systemRollupResponse]{...})`）；
     字段是 **`classification_uid`** 不是 `classification_code`
     （`server/handler_system.go:60-65`）。`jq '.rollups[]'` 对缺失键会直接报
     `Cannot iterate over null`。

  合起来：**on-call 在 P0 页上,三条确认命令一条都跑不通**。这一节的其余内容（三种成因分析、
  `capital_loss` 补记）是对的且有价值——问题正是它看起来完全可信，直到第一条命令报错。
- **为什么现有机制没拦住**：`server/openapi_contract_test.go` 双向比对 Go wire struct 与
  `docs/openapi.yaml`（上轮加的，管用），但**RUNBOOK / DR / CAPACITY 不在任何契约门禁的覆盖面内**。

---

### [Major] I-5 · RUNBOOK §3 的确认 SQL 查一张不存在的表

- **位置**：`docs/RUNBOOK.md:262-263`
- **判定**：CONFIRMED
- **失效场景**：
  ```sql
  SELECT COUNT(*) FROM checkpoint_rollup_queue;
  SELECT MAX(now() - created_at) FROM checkpoint_rollup_queue;
  ```
  表名是 **`rollup_queue`**（`postgres/sql/migrations/001_baseline.up.sql`，
  `grep -ohE "CREATE TABLE [a-z_]+" postgres/sql/migrations/*.up.sql` 里没有
  `checkpoint_rollup_queue`）。两条都以
  `ERROR: relation "checkpoint_rollup_queue" does not exist` 收场。
  §3 是 rollup 积压——最常被打开的一节。列名 `created_at` 本身是对的
  （`rollup_queue.created_at TIMESTAMPTZ NOT NULL DEFAULT now()`），只有表名错。
  这是 lead 要找的「`ledger_app` 从未被 migration 创建」那条的兄弟：**RUNBOOK 的 SQL 从未被执行过。**
- **附**：机械核过 RUNBOOK 里出现的全部 SQL 对象名，只有这一个不存在
  （`balance_checkpoints` / `bookings` / `checkpoint_rebuilds` / `classifications` /
  `events` / `journal_entries` / `journal_entries_default` / `journals` /
  `webhook_subscribers` 全部存在；`pg_class` / `pg_partition_tree` 是系统目录）。
  RUNBOOK 引用的 14 个 Go 符号（`Onchain.scanChainOnce`、`PartitionService.EnsureUpcoming`、
  `SweepPolicy.GasCeiling` …）**全部存在**，日志字符串
  `"service: rollup: process item failed"`（`:1238` 引用）也**确实存在**于
  `service/rollup.go:136`。**错的只有指标名和这一个表名——恰好是最难在 code review 里看出来的两类。**

---

### [Major] I-6 · `ledger-cli` 自称 read-only，`reconcile --full` 会写生产库；DR 拿这个「只读」当依据

- **位置**：`cmd/ledger-cli/main.go:1`（"read-only investigation tool"）、`:12`（"Read-only by design:
  the CLI never posts journals or mutates state"）、`:249-250`（`svc.FullReconciler(cfg)` → `RunFullReconciliation`）
  → 写入点 `service/reconcile.go:808`/`:846`/`:878`/`:895`（`s.querier.SetScanCursor`）
  → `postgres/reconcile_queries.go:323` → `postgres/sql/queries/reconcile.sql:210`
  （`INSERT INTO reconcile_scan_cursors ...`）
- **判定**：CONFIRMED
- **失效场景**：三个具体后果：
  1. **写到证据上**。`docs/DR.md:114-115` 原话：「A restored ledger must pass the same invariant
     checks the live system runs (**this is the reason `cmd/ledger-cli` is read-only — point it
     anywhere**)」。而 `docs/DR.md:91-92` 又说事故库「is evidence, and you may need a second
     attempt」。按这两句的字面指引，取证人员会把 CLI 指向事故库跑 `reconcile --full`，
     从而 **UPSERT `reconcile_scan_cursors`**——而这张表正是上轮 threat-model §4-3
     认定的、`ledger_app` 可写且**攻击者会篡改**的检测层游标。取证动作污染取证对象。
  2. **和调度中的 worker 抢 lap 记账**。`full_reconcile` job（`service/worker.go:345`）
     与 CLI 共用同一行游标；一次人工 `reconcile --full` 会把 lap 重置/推进，
     使 worker 那一 lap 的 `lap_scanned` 累计失真（`service/reconcile.go:870-900` 的
     M-1 覆盖判定就建立在这个累计值上）。
  3. **凭证要求与文档相反**。`RUNBOOK.md:634` 说 `ledger_ro` 是「Metabase / BI / reporting」用的
     只读角色。按 CLI 的「read-only」自述，运维会用 `ledger_ro` 的 `DATABASE_URL`——
     然后在 `SetScanCursor` 上撞 42501，报告里出现
     `"checkpoint scan cursor persist failed"` 且 `Passed=false`，**看起来像账本出了问题**。
- **最小复现**：`DATABASE_URL=<ledger_ro 凭证> ledger-cli reconcile --full`
  → 报告含 `checkpoint scan cursor persist failed` 且 `overall_passed:false`。
- **为什么现有机制没拦住**：`cmd/ledger-cli/main_test.go` 只有 24 行，唯一的断言是
  flag usage 文案不含硬编码 check 数。**CLI 没有任何行为测试**，「read-only」这个断言
  从未被验证过。

---

### [Major] I-7 · `ledger-cli` 的 30s 全局超时低于 reconcile 单个 check 的 2 分钟预算 → 报告一片红

- **位置**：`cmd/ledger-cli/main.go:100`（`context.WithTimeout(context.Background(), 30*time.Second)`，
  覆盖**所有**子命令）vs `service/reconcile.go:338-340`（`Check2Timeout` 默认 **2 分钟**）
- **判定**：CONFIRMED（数值对比与代码路径都直读；未在真实大库上实跑，见「我没能验证的」）
- **失效场景**：
  `cmdReconcile`（`:248`）传的是 `service.FullReconciliationConfig{}`，`withDefaults()`
  把 `Check2ScanLimit=5000` / `Check2Timeout=2m` 填上。check #2 内部
  `scanCtx, _ := context.WithTimeout(ctx, s.cfg.Check2Timeout)`（`reconcile.go:658`）——
  `ctx` 是 CLI 那个 30s 的，所以有效上限 = **30 秒**，永远碰不到自己的 2 分钟预算。
  超过 30s 之后的连锁：
  1. `pageLoop` 优雅退出，标 `partialReason = "scan timed out after 2m0s"`
     ——**文案本身就在撒谎**，它报告了一个从未生效的 2 分钟预算；
  2. 紧接着的 `s.querier.SetScanCursor(ctx, ...)`（`:808`）**故意用 `ctx` 而非 `scanCtx`**
     （`reconcile.go:668-671` 有注释解释：「must still succeed after scanCtx's own deadline」
     ——这个推理对 worker 成立，对 CLI 恰好反了），此时 `ctx` 已 Done →
     `result.Passed = false` + `"checkpoint scan cursor persist failed"`；
  3. 之后的 check #3…#15 全部拿到已取消的 ctx，逐个 `Passed=false`。
  **净效果：on-call 在一个健康的大账本上跑 `ledger-cli reconcile --full`，
  拿回一份「十几项检查失败」的报告。** 这是 P1 误报生成器，且 CLI 没有任何 flag 可以调高超时。
  CLI 自己的 doc comment（`main.go:6`）把这条命令列为头号用途：「Reconciliation triage」。
- **为什么现有机制没拦住**：`reconcile --full` 的所有测试都在 `service/` 包里直接调
  `RunFullReconciliation(context.Background())`（无超时）。**CLI 这一层的 ctx 从未进入任何测试。**

---

### [Major] I-8 · anchor 发布失败被无限静默吞掉，且 `core.Metrics` 对整个防篡改链零覆盖

- **位置**：`service/attestation.go:227-235`（`Publish` 失败 → `logger.Error` → **返回 nil**）、
  `:319`（`catchUpAnchor` 的 anchor head 不可用同样只 log）、
  `service/worker.go:377`（`_, _, err := w.attestation.RunAttestBatch(...)` — **丢弃 attested 计数与 seq**）
- **判定**：CONFIRMED
- **失效场景**：
  `core.Metrics` 的 32 个方法里，**关于 P5/P6 防篡改链的有 0 个**（逐个核过）。具体是：

  | 事件 | 有指标吗 |
  |---|---|
  | attest 批次落后 / 长时间 0 条 | **无**（`attested` 被 `_` 丢弃） |
  | anchor 写失败 | **无**（只有 `logger.Error`） |
  | anchor head 不可用 | **无** |
  | `VerifiedBalance` 返回 UNDEFINED | **无** |
  | 未发布的 attestation 积压深度 | **无**（也没有查询能读出这个数） |

  于是：**anchor 连续三个月发布失败，唯一的痕迹是日志行**。而
  `service/worker.go:262-266` 自己的注释说得很清楚——anchor 是「the only thing that lets
  VerifyLedger detect a wholesale DB-level history rewrite」。
  这个唯一的外部见证死掉之后，`ledger_reconcile_check_results_total`、
  `ledger_reconciliations_completed_total` 全部照常绿灯（`unauthorized_journals` 只验 P5 签名，
  不看 anchor）。**「有 anchor 且在工作」与「anchor 死了三个月」在指标面上完全同形。**
  再叠加 I-11（默认 NopLogger），连日志行也可以不存在。
- **为什么现有机制没拦住**：worker 启动时的 `attestation_anchor` bool + Warn
  （`service/worker.go:259-266`，m-7 修复）覆盖的是**「启动时有没有配 anchor」**，
  不覆盖**「配了的 anchor 后来还工作不工作」**。这正是 §3 的第二条：
  降级（anchorless 运行）在启动那一刻落了痕，运行期降级（anchor 配了但一直失败）没有。

---

### [Major] I-9 · worker 全部后台 goroutine 无 panic 兜底，消费方自己的 `Subscribe` handler panic 会打死整个进程

- **位置**：`service/worker.go:271-407`（最多 11 个 `g.Go(...)`，无一 `recover`）；
  全仓 `grep -rn "recover()" --include='*.go'` 生产代码 **0 命中**
  （唯一一处是 `server/server.go:439` 的注释）
- **判定**：CONFIRMED
- **失效场景**：两条具体路径：
  1. **消费方代码在库的 goroutine 里跑**。`Worker.Subscribe(handler)`（`service/worker.go:198`）
     注册的是消费方闭包，由 `delivery.LocalDispatcher.ProcessBatch`
     （`service/delivery/local.go:67` `d.callback.Deliver(ctx, evt.Event)`）在
     `runLoop` 的 goroutine 里直接调用。handler 里一个 nil map 写入 / 越界 →
     goroutine panic 未捕获 → **Go 运行时终止整个进程**。
     消费方自己 HTTP 层的 recover 中间件够不到这里。而 `Subscribe` 的 doc comment
     （`:176-197`，21 行，讲了 at-least-once、幂等、Run 之前订阅）**只字未提这一点**。
  2. **库自己会 panic**。`postgres/convert.go:51-57` 的 `mustNumericToDecimal` 是**故意 panic** 的
     （fail-closed，方向正确）。它被 `postgres/attestation_store.go:64,87`（attestation job）、
     `postgres/ledger_store.go:1212`（checkpoint 读，rollup 路径）等调用。
     **同一个 panic 在 HTTP 路径上是一个 500**（`server/server.go:519` 装了
     `middleware.Recoverer`），**在 worker 路径上是进程死亡**。
     这个不对称既没文档也没测试。
- **为什么现有机制没拦住**：`Run` 的 doc comment（`service/worker.go:221-250`，30 行）
  详尽讨论了「哪些 job 是可选的、怎么让跳过可见」，完全没有讨论 job 崩了会怎样。
  `Run` 的返回值也帮不上忙——所有 `runLoop` 都恒返回 nil，所以 `g.Wait()` 只可能返回 nil
  （ctx 取消时），**「worker 正常退出」与「worker 从未失败」不可区分，因为它没有失败通道**。

---

### [Major] I-10 · 三个后台 job 家族无任何指标；「job 已连续 6 小时失败」在 Prometheus 上不可见

- **位置**：`service/worker.go`（全文件无 `metrics` 字段——`NewWorker` 只取 `engine.Logger()`，
  见 `:118-126`）；`service/locked_job.go`（同，只有 logger）；
  `service/expiration.go:44,63`（**有 `metrics` 字段，从头到尾没用过**：
  `grep -n "s.metrics\." service/expiration.go` = 0 命中）
- **判定**：CONFIRMED
- **失效场景**：逐 job 核「tick 成功 / 失败 / 跳过」的可观测面：

  | job | 成功 | 失败 | 因锁跳过 |
  |---|---|---|---|
  | rollup | `RollupProcessed` + `RollupLatency` ✅ | `RollupItemFailed`（**仅当释放 claim 成功**，`service/rollup.go:110,133` 在 `else` 分支里）| n/a |
  | expiration | 无 | 仅 `logger.Error`（`worker.go:293,296,303`） | 仅 `logger.Info`（`locked_job.go:108`） |
  | reconcile | `ReconcileCompleted` ✅ | 仅 log | 仅 log |
  | snapshot | `SnapshotLatency` ✅ | 仅 log | n/a |
  | system_rollup | 无 | 仅 log | 仅 log |
  | full_reconcile | `ReconcileCheckResult` ✅ | 仅 log | 仅 log |
  | partition | 无 | 仅 log | 仅 log |
  | attestation | 无（见 I-8） | 仅 log | 仅 log |
  | event_delivery | `EventDelivered/Failed/Dead` ✅ | 仅 log | n/a |
  | event_callback | **无**（`LocalDispatcher` 零指标，见 I-12） | 仅 log | n/a |

  三个结构性缺口：
  1. **没有 `job_last_success_timestamp_seconds` 这类通用信号**。一个 replica 卡在
     `reconcile` 里 7 小时，其余 replica 每 tick 打一行
     `"advisory lock held by another replica, skipping"`（**Info 级**，`locked_job.go:108`），
     整个 fleet 的 reconcile 实际停摆——而 `ledger_reconciliations_completed_total`
     只是**不再增长**，没有任何 gauge 说「上次成功是几点」。
  2. **`RollupItemFailed` 只在 `ReleaseRollupClaim` 成功时才发**
     （`service/rollup.go:104-112` / `:125-135` 的 `if/else`）。释放 claim 也失败时
     （DB 抖动，恰恰是最该告警的时刻）**计数器不增**，只剩两行 Error 日志。
     这是「失败的失败」被静默——`working-agreements.md` §3 的直接违反。
  3. `ExpirationService` 拿了 `metrics` 却一次没用：`ReserveReleased`
     （I-1 里的零调用点之一）**本来就该在这里发**。「过期抽走了 N 笔冻结」当前无任何指标。
- **为什么现有机制没拦住**：没有任何门禁问「新加的 job 有没有指标」。上轮
  `expiration` 被包进 `NewLockedJob`（concurrency 修复，`worker.go:291`）时，
  加的是 leader 选举，没有顺带加可观测。

---

### [Major] I-11 · 库的默认配置把所有诊断关掉，而唯一会说「其余保护也关着」的通道就是被关掉的那个

- **位置**：`ledger.go:148-149`（`logger: core.NopLogger()`、`metrics: core.NopMetrics()`）；
  `ledger.go:817-860`（`Service.Worker` 接了 partition / localPoller / attestor / pool，
  **唯独没接 `SetFullReconciler`**）；对照 `server/server.go:492-503`（四条包级 `slog.Warn`）
- **判定**：CONFIRMED
- **失效场景**：`ledger.New(pool)`（README Quick Start 的形态）之后：
  - 全部 job 失败 Error → `/dev/null`
  - `"worker: starting" full_reconcile=false / attestation=false`（`worker.go:254-261`）→ `/dev/null`
  - `"batch attestation is running with no anchor configured"` Warn（`:263`）→ `/dev/null`
  - `"negative balance on debit-normal account"` Warn（`rollup.go:244`）→ `/dev/null`
  - `"unauthorized journal"` Warn（`reconcile.go:1491`）→ `/dev/null`
  - 32 个指标全部恒 0

  并且 **`SetFullReconciler` 不自动接**（全仓生产调用点只有 `examples/fullstack/backend/main.go:123`）
  ——15 项对账 check（含 `unauthorized_journals`、`checkpoint_balance`、`journal_dr_cr`
  这些防篡改的主力）**默认一次都不跑**。而 `DefaultWorkerConfig` 照样给它配了
  `FullReconcileInterval: time.Hour`（`worker.go:65`），**看起来是开着的**——
  这与上轮已修的 `AttestInterval` 缺口（TODO §3「`svc.Worker` 声称零值填默认，却漏了
  `AttestInterval` → 存证 job 静默跳过」）是**同一个形态**，只是换了一个字段，没被一起修。
  `SetAttestor` 被自动接了，`SetFullReconciler` 没有——同一个函数体里的两种处理，无解释。

  最要紧的是这个组合的**自指性**：
  > 观测配置的零值（NopLogger），关掉的正是那个本该告诉你「检测配置的零值也关着」的机制。

  同一个仓库里 `server/` 做对了：`newServer` 用**包级 `slog`**（消费方无法静音）打四条
  危险配置警告。作者知道这个顾虑，只是没把它应用到库本体。
- **为什么现有机制没拦住**：`ledger_test.go` / `ledger_subscribe_wiring_test.go`
  是为「facade 有没有接线」建的门禁（上轮 `SetLocalPoller` 事故之后建的，管用），
  但它们**逐个 pin 已知的接线点**，没有一条断言「所有 `Worker.Set*` 都被 facade 覆盖」。
  `SetFullReconciler` 从来不在名单上，所以增补名单时也不会漏——它压根没进过名单。

---

## 上轮修复复核

### R-1 · `EventStore.SetLogger` —— 为修上轮 Minor 而生，**零生产调用点**（回归）

- **位置**：`postgres/event_store.go:64-82`（定义 + 22 行 doc comment）；
  `grep -rn "SetLogger" --include='*.go' .` 生产调用点 **0**
- **判定**：CONFIRMED
- 上轮 TODO §6 Minor：「`event_store` 的『claim lost』用包级 `slog`，消费方注入的 logger 看不到」。
  修复方式是**加一个 setter**。而：
  - `ledger.New` 里 `s.eventStore = postgres.NewEventStore(pool)`（`ledger.go:169`）—— 不调；
  - `ledger.Service.Worker` 里 `eventPoller := postgres.NewEventStore(s.pool);
    eventPoller.SetClaimLease(cfg.EventClaimLease)`（`ledger.go:835-836`）—— 只调了
    `SetClaimLease`，**紧挨着的 `SetLogger` 没调**，尽管 `s.logger` 就在手边。
  - setter 自己的 doc 写着「Until this is called, warn falls back to slog.Default() ——
    the historical behavior, **unchanged**, so not calling this is not a regression」。
    确实没人调，所以**行为一分未变**：那三行 claim-lost 警告对每一个 facade 消费方
    仍然绕过注入的 logger。
- **这正是 C1 的形态在修复内部的复现**：机制存在、示例存在、doc 存在，唯独没接到真实路径上。
  契约 §1.4 的三问里第 2 问（「这个测试做的准备工作，真实调用方也会做吗」）在这里的答案是「不会」——
  连测试都没有，只有一个没人叫的 setter。

### R-2 · `full_coverage` —— 库路径已修好，**运维入口仍恒为假**，且 DR 演练误归因

- **位置**：`service/reconcile.go:483-497`（check #8 已删，注释详述理由）✅；
  `service/reconcile_full_test.go:256-271` `TestFullReconciliation_FullCoverageCanBeTrue` ✅；
  **但** `cmd/ledger-cli/main.go:249` + `ledger.go:434`（`full.SetAuthCheck(s.queryStore, s.authVerifier)`，
  CLI 走 `ledger.New(pool)` 无 `WithAttestor` → `s.authVerifier == nil`）
  → `service/reconcile.go:1437-1445`（verifier nil → `Complete=false`）
- **判定**：CONFIRMED
- 修复本身是对的：check #8 删除后 `FullCoverage` **结构上可以为真**，且有两条 pin
  （单测 `reconcile_full_test.go:266` + DB 集成 `reconcile_unauthorized_journals_test.go:77`）。
- **但两条 pin 都做了消费方不会做的准备**：`svc.SetAuthCheck(&fakeJournalQueryProvider{},
  alwaysValidVerifier{})` / 真 verifier —— 都传了**非 nil 的 verifier**。
  而 `ledger-cli`（**唯一的运维入口**，也是 RUNBOOK §1 和 DR §5 都指定要跑的那条命令）
  **没有任何 flag 能提供 verifier**（对比：`ledger-cli verify` 有 `--pubkey-hex` / `--key-id`）。
  所以 **`ledger-cli reconcile --full` 在任何部署上、任何数据下，`full_coverage` 恒为 `false`**
  ——正是上轮那条 Major 的原始症状，在原始的复现命令上原样保留。
- **DR 演练自己撞上了这个，并且归因错了**。`docs/DR.md:192-194`：
  > (`full_coverage: false` only because `unauthorized_journals` needs an `AuthVerifier`
  > this throwaway seed script never wired via `ledger.WithAttestor` —— **a seed-script gap,
  > not a restore defect**.)

  不是 seed 脚本的缺口：**通过 `ledger-cli` 无论如何都提供不了 verifier**。
  这份演练记录把一个产品缺陷记成了一次性的环境瑕疵，下一个读它的人会跳过。
  （§1.4 的第 3 问：「它断言的命题，和它名字声称的命题，是同一个吗」——
  `FullCoverageCanBeTrue` 证明的是「库路径能为真」，读起来像「这个信号可用了」。）

### R-3 · `balance_drift_units` —— 已真修 ✅

- `service/rollup.go:238-261`：现在计算的是 `drift`（健康时 `decimal.Zero`，违规时
  `newBalance.Neg()` 的正幅度），**无条件 `Set`**，所以能复位。
- M-3 追加的 `NegativeBalanceDetected`（单调 Counter，`:259`）解决了「同 label 不同 holder
  互相覆盖」的二阶问题，`core/metrics.go:76-89` 的 20 行注释与
  `docs/RUNBOOK.md:1277-1286` 的告警指引一致且正确。
- 这一条是本轮复核里质量最高的修复：**修了指标语义、加了不可被覆盖的伴生信号、写了告警指引，三件都做了。**

### R-4 · 链上六指标「零 RUNBOOK 条目」—— 已修，但表里混进一个永不发射的 ✅/⚠️

- `docs/RUNBOOK.md:1213-1286` 新增 §14，用**真实导出名**逐个给出含义与处置 ✅。
- ⚠️ 表里的 `ledger_template_failed_total`（`:1239`）永远不会触发（见 I-3）。
- ⚠️ 两个**确实会发射**的链上指标反而没进表：`ledger_sweep_address_unreadable_total`
  与 `ledger_deposit_review_required_total`（`grep -c` RUNBOOK = 0）。
  后者是 M3 补偿控制的核心信号（大额充值转人工复核），§13 有整节处置流程却没把它列为告警源。

### R-5 · DR 演练已实跑 ✅，但留下两处需要收口

- `docs/DR.md:148-215` 是一次真实的 PITR 演练记录（容器日志时间戳、`last_value` 106 vs
  `max(id)` 80、`pg_dump` 路径 120==120），并**证伪**了 migration 008 注释里的
  「sequence regresses after PITR」说法。这是本仓最扎实的一份运维证据。
- **I-42 撤回声明的残留：已正确处理，无残留**（lead 问的那条）。核过三处：
  `docs/INVARIANTS.md:3464-3479` 有明确的「Correction (2026-08-26)」段落，写明该 claim
  为假、给出实测数字、并说明 migration 008 的注释**故意不改**（`deployment.md`：已落地的
  migration 永不编辑）。`postgres/sql/migrations/008_...up.sql:27-28` 的原文保留，符合该决定。
  CHANGELOG 无残留。
- ⚠️ 但 **`docs/DR.md:280-286` 仍挂着一条已被否决的建议**：
  「**Recommendation**: migration 008's comment should be corrected … Left to Team Lead
  per this task's instructions to report before touching code.」
  ——决定已经做了（INVARIANTS：不改），DR.md 没同步。只读 DR.md 的人会去改 migration 008。

---

## [Minor] 其余发现

### I-12 · 库模式的事件投递零指标，而 CAPACITY 拿这些指标定 SLO

- `service/delivery/local.go` 全文件无 `core.Metrics`（对比 `webhook.go:78` 有）。
  `EventDelivered` / `EventDeliveryFailed` / `EventDead` 只有 `WebhookDeliverer` 会发。
- `docs/CAPACITY.md:68` 的 SLO「Event delivery: 99% delivered < 5 min，dead-letters page
  within 30 min，Measured by `ledger_events_delivered_total` / `ledger_events_dead_total`」
  ——**对走 `Worker.Subscribe` 的库模式消费方（即主要消费形态）完全不可测**。
- 另：全 32 个指标里**没有事件队列深度 gauge**（`PendingRollups` 只管 rollup），
  RUNBOOK §5 只能靠手写 SQL 确认积压。

### I-13 · 死信表与两张取证表写得进、读不出、文档不提

- `ingest_dead_letters`：`ListDeadLetters`（`postgres/ingest_dead_letter_store.go:66`）
  **只被测试引用**——无 HTTP 端点、无 CLI 子命令、无 facade 访问器。
  而 `core/onchain.go:149` 的注释称它是「model for **on-call triage**」。
  RUNBOOK / DR / INVARIANTS / README 里 `ingest_dead_letters` 出现 **0** 次。
- `config_table_changes` 与 `reconcile_scan_cursor_changes`
  （`006_threat_model_guard_coverage.up.sql:184,254`）——为闭合上轮 §9 共同盲区
  「篡改之后能不能复盘」而建的两张取证表，同样**零读取路径**
  （无 sqlc query、无 Go store、无端点、无 CLI），RUNBOOK/DR 里 **0** 次。
  Trigger 在写，没人能读。lead 问的「事后取证『谁在何时改了哪张配置表』能用 CLI 查到吗」
  → **不能**，只能手写 SQL，且没有任何文档告诉运维这两张表存在。
  （对照：`checkpoint_rebuilds` 做对了，RUNBOOK §1 给了完整查询。）

### I-14 · `scanChainOnce` 在 ingest 失败后照样推进游标，而 `ChainCursorLag` 回落到「健康」

- `service/onchain.go:1246-1250`（`IngestDeposit` 失败 → `log().Error` → `continue`）
  → `:1252` 无条件 `SetCursor(ctx, chainID, to)` → `:1256` `ChainCursorLag(chainID, latest-to)`。
- 死信只在 `errors.Is(err, core.ErrConflict)` 时记（`:686-689`），其余失败（DB 抖动、
  分类缺失、精度拒绝）**不留任何持久痕迹**，且没有 `DepositIngestFailed` 指标。
- 可观测面的净结果：「本轮干净扫完」与「本轮扫到 5 笔充值全部落地失败、且永久跳过」
  在指标上**完全同形**（`chain_cursor_lag_blocks` 都回到接近 0）。
  **资金损失判定移交 territory G**（见「移交」）；这里只报可观测缺口。

### I-15 · HTTP 访问日志与 API 错误日志绕开注入的 logger，且每个 4xx 都打 Error

- `server/middleware_logger.go:56`（`slog.Info("http request", ...)`）、
  `server/handler_holder.go:198`（`slog.Info("holder token minted", "key", …, "holder", req.Holder)`）、
  `pkg/httpx/response.go:171`（`slog.Error("api error", …, "err", err)`）——
  三处都用包级 `slog`，消费方 `WithLogger` 注入的 logger 收不到。
- `httpx.Error` 对**每一个** 4xx（404 / 参数校验失败 / 幂等冲突）都打 **Error 级**。
  一个扫描器打一批坏 uid 就能把 Error 级日志刷满，真正的 5xx 被埋掉；
  按日志级别做告警的消费方拿不到可用信号。
- 顺带：`handler_holder.go:198` 把 `holder`（用户标识）写进日志——与
  `middleware_logger.go:32-33` 自己「Query strings are intentionally dropped —— they may
  contain holder IDs」的策略直接矛盾。

### I-16 · OTEL span 携带金额与 holder，发往 APM 供应商，无脱敏层、无文档

- `postgres/` 的 16 个 span 的属性统计：`account_holder` ×7、`idempotency_key` ×6、
  `amount`/`actual_amount` ×4、`currency_uid` ×4、`actor_id` ×2、`reservation_uid` ×4
  （如 `postgres/ledger_store.go:430-435`、`postgres/reserver_store.go:83-88`）。
- `pkg/otel/tracing.go` 无任何脱敏；`pkg/slogadapter` 同样是纯透传（`slogadapter.go:24-26`）。
- `README.md:562-566`:「OTEL trace propagation is **automatic** … **No injection needed**;
  just configure the global tracer provider」——即**消费方为任何目的配了全局 tracer 的那一刻，
  逐笔金额与 holder ID 就开始流向他们的 APM 供应商**，无开关、无文档提示。
  APM 供应商通常是比账本 DB 更低的信任层级。

### I-17 · CAPACITY.md 的头号 SLO 与头号扩容信号都挂在永不写入的 histogram 上

- `docs/CAPACITY.md:64`（SLO「Journal post latency p99 < 50ms，Measured by
  `ledger_journal_post_seconds`」）与 `:73`（扩容信号 #1，同一指标）——
  `JournalLatency` 零调用点（I-1），该 histogram 永远为空，p99 无值。
- `:5` 引用「the Helm alert thresholds (`metrics.prometheusRules.thresholds`)」，
  `:29` 引用告警规则 `LedgerRollupBacklog` / `LedgerCheckpointAgeHigh`；
  `docs/RUNBOOK.md:370` 引用 `LedgerEventDeliveryDead`。
  **这三个规则名与那份 Helm chart 全仓不存在**（随 C1 `30bd872` 删除，`ls deploy` → No such file）。
  on-call 搜不到，无法判断是自己部署漏配还是文档过期。

### I-18 · DR §5 的验收指令与 CLI 实际输出不符，且 check 数已再次漂移

- `docs/DR.md:121`:「`ledger-cli reconcile --full` **# must print PASS on every check**」
  ——CLI 输出的是 `jsonOut(report)`（`main.go:254`），JSON 里是 `"passed": true/false`，
  **从不出现 "PASS"**。演练记录自己（`:190-191`）写的是 `passed: true` / `overall_passed: true`。
- `docs/DR.md:190`:「all **13** runnable checks」——`RunFullReconciliation` 现在
  append **15** 个 check（`grep -c "checks = append" service/reconcile.go` = 15），
  其中 1 个跳过 → 14 runnable。演练在 `bb5a583`（08-26），
  `untagged_holder_kind` 在 `2ec4f16`（08-27）加入——**次日就漂了**。
  仓库为这个漂移建过机器门禁
  （`cmd/ledger-cli/main_test.go:20` `TestReconcileFullFlagUsage_DoesNotHardcodeACheckCount`），
  但它**只盯 `reconcileFullFlagUsage` 这一个字符串常量**，同样的硬编码数字在 DR.md 里立刻重现。

### I-19 · `server.SetReady` 无人自动调用 → 消费方的 readiness 探针可能永远 503

- `server/server.go:543` `SetReady` 的唯一生产调用点是 `examples/fullstack/backend/main.go:243`。
  自己接 `server.New` 的消费方若不调，`/system/ready` 永久 503（`handler_system.go:47`），
  pod 永不进 LB 轮转，**且没有任何日志提示**。
- `README.md:885` 说 `/system/ready`「returns 503 until migrations + worker have booted」——
  暗示是自动的，实际是纯手动 flag。方向是 fail-closed（安全），但沉默。

### I-20 · CLI 的 `verify` 只能验 dev 版 anchor / verifier，生产 R2 部署无取证路径

- `cmd/ledger-cli/main.go:337-338` 硬编码 `anchordev.NewLocalFileAnchor(*anchorFile)` +
  `authdev.NewLocalVerifier(...)`。
- `anchordev` 的定位（`CLAUDE.md` File Layout）是「**Dev/test only** —— same machine as the
  database it must be independent of」；生产 anchor 是 `anchors/r2`（Cloudflare R2 + Object Lock）。
- 于是 **on-call 在一个用 R2 anchor 的生产部署上，没有任何 CLI 路径能跑 `verify`**——
  P6 五步验证（含篡改定位 `--reference-dir`）只对 dev anchor 可用。
  这是 `anchors/r2` 作为独立 module 之后没有回头补的口子。

### I-21 · `cmd/ledger-cli` 的假 drift 守卫

- `cmd/ledger-cli/main.go:425-427`:
  ```go
  // keep imported types referenced so the file fails fast if any drift.
  var _ = strings.TrimSpace
  var _ = core.NormalSideDebit
  ```
  这两行**不会检测任何 drift**——它们只是让两个未使用的 import 能编译过，
  注释里的理由是虚构的。同时违反 `golang.md`「不允许 `_ = someVar // retained for later use`」。
  同形的还有 `service/onchain.go:1255` `_ = cfg // cfg reserved for future per-chain scan tuning`。

### I-22 · CLAUDE.md / Makefile 的运维入口描述过期

- `CLAUDE.md:149` 仍列 `| deploy/helm/ledger/ | Kubernetes Helm chart |`——该目录已随 C1 删除。
- `CLAUDE.md:54` 写 `make docker # docker compose up --build`——实际
  `Makefile:26` 是 `docker compose up -d postgres`。
- CLAUDE.md 是 agent 与新人的第一张地图，指向不存在的目录会让人以为自己 checkout 不完整。

### I-23 · `jsonToStringMetadata` 解析失败静默返回 nil

- `postgres/convert.go:465-468`：JSONB metadata 反序列化失败 → `slog.Warn` → `return nil`。
  调用方拿到「没有 metadata」而不是「metadata 读不出来」。
  该 metadata 参与幂等比对（`postgres/idempotency_match.go` 的
  `bookingMetadataMatches`），所以静默丢失会把幂等三态判到错误分支。
  日志走包级 `slog`，且只截前 200 字节。**幂等后果的严重性移交 territory B。**

---

## 被我自己推翻的假设（保留记录）

**「RUNBOOK §9 的紧急停机 SQL 关不掉写入」**——我推断 migration 008 之后 `ledger_app`
在 `journal_entries` 上只有**列级** INSERT，而 `docs/RUNBOOK.md:518` 的
`REVOKE INSERT ON journal_entries FROM ledger_app`（表级 REVOKE）按 PostgreSQL 语义
不应移除列级授权 → 紧急停机不生效。

**实测证伪。** 起了一个一次性 `postgres:17.2-alpine`（`audit-i-pgtest`，端口 15499，
与共享 dev-postgres 无关，跑完立刻 `docker rm -f`），复现 008 的授权形态
（表级 GRANT → REVOKE → 列级 `GRANT INSERT (amount, note)`）后执行表级
`REVOKE INSERT ON je FROM ledger_app`：

```
after 008-style grant:  column_privileges → amount/INSERT, note/INSERT
after table-level REVOKE: column_privileges → (0 rows)
SET ROLE ledger_app; INSERT INTO je (amount, note) ... → ERROR: permission denied for table je
```

表级 REVOKE **确实**连带清除同类型的列级授权。**§9 步骤 2 是对的。**
而且 §9 本身在这个问题上写得比我预想的细致得多——它自己就区分了父表 ACL 与
per-partition 授权（`:527-547`），并在步骤 4 用列级 GRANT 循环恢复而不是表级 GRANT
（`:577-600`），还给了「以 `ledger_app` 显式带 `id` INSERT 必须 42501」的验收动作。
**§9 是这份 RUNBOOK 里质量最高的一节，与 §2/§3 的状态反差本身值得注意**：
被上轮审计翻过的段落是准确的，没被翻过的段落错到跑不通。

记在这里是因为它标出了「看起来可疑但没问题」的位置，下一个审计者不必重走。

---

## 移交

- **→ audit-G-onchain**：`service/onchain.go:1246-1252` 游标在 ingest 失败后仍推进，
  且非 `ErrConflict` 的失败不进死信 → **是否构成永久漏账/丢钱**由你判定（我只报可观测缺口，I-14）。
- **→ audit-B-concurrency**：`postgres/convert.go:465-468` metadata 解析失败静默返回 nil，
  该值参与 `postgres/idempotency_match.go` 的幂等三态比对（I-23）。
- **→ audit-C-tamper**：`core.Metrics` 对 P5/P6 链零覆盖（I-8）——anchor 长期失效在指标面不可见；
  另 `ledger-cli verify` 只支持 `anchordev`，生产 R2 部署无验证路径（I-20）。
- **→ audit-D-threat**：`config_table_changes.changed_by` 恒为 `current_user`（= `ledger_app`），
  且 trigger 只覆盖 UPDATE，不覆盖 INSERT/DELETE（`006_...up.sql:279-300`）；
  `server/handler_holder.go:198` 把 holder ID 写进包级 slog；
  OTEL span 外泄金额/holder 到 APM 供应商（I-16）。
- **→ audit-E-consumer-surface**：`SetFullReconciler` 不被 `Service.Worker` 自动接线，
  而 `DefaultWorkerConfig` 给它配了 interval（I-11 的一半）；
  `Worker.Subscribe` 的 doc 未提 handler panic 会杀进程（I-9）。
- **→ audit-F-test-credibility**：`TestFullReconciliation_FullCoverageCanBeTrue` 与
  `reconcile_unauthorized_journals_test.go:77` 都做了 `ledger-cli` 做不到的接线（R-2）；
  `cmd/ledger-cli` 全包只有 24 行测试，唯一断言是 flag 文案。
- **→ team-lead（流程）**：审计期间 `core/onchain.go` 在工作树里被**未提交地修改**
  （`AutoCreditCeilingConfigured` 被改成 `return true`，导致 `go test ./core/` 有 2 个子测试红）。
  看形态是某个 territory 在做「拆掉再看」的证伪实验但没按契约 §2 恢复。
  我没有动它（恢复可能打断对方在跑的实验）。**基线目前是脏的**，请协调。

---

## 我没能验证的

- **`ledger-cli reconcile --full` 的 30s 超时（I-7）没有在真实规模的库上实跑。**
  「30s < Check2Timeout 2m」与「`SetScanCursor` 用父 ctx」是直读代码 CONFIRMED，
  「报告会出现十几项虚假失败」是顺着代码路径推的——需要一个 check#2 扫描超过 30s
  的库才能实测。小库上跑不出来（这本身也是它一直没被发现的原因）。
- **`ledger-cli` 需要哪个 DB 角色，没有实测。** 我只证明了 `reconcile --full` 会写
  `reconcile_scan_cursors`，没有以 `ledger_ro` 实连一次确认失败信息的具体形态。
- **panic 会打死进程（I-9）没有实跑。** 这是 Go 的语言语义（未 recover 的 goroutine panic
  终止进程），并且我确认了生产代码零 `recover()`，但没有构造一个 panic 的
  `Subscribe` handler 实测。
- **指标基数没有实测。** 我逐个核了标签集（`journal_type` / `class` / `to_status` /
  `currency_id` / `chain_id` / `check` / `reason` / `template` / `passed` / `success`），
  **没有任何 holder uid 或自由字符串进标签**，`DepositReviewRequired` 的 `reason`
  是受限二选一（`service/onchain.go:915`）——**结论是没有基数爆炸风险**，
  但这是静态判断，未在真实部署上量过 series 数。唯一的软肋是
  `journal_type` / `template` / `class` 的基数由消费方创建多少个分类/模板决定，
  库这一侧无上限保护。
- **DR 的 restore drill 我没有重跑。** 我读了 `bb5a583` 的记录并核了它引用的代码事实
  （sequence 说法的撤回、`full_coverage` 那句归因），但没有独立复现那次 PITR。
- **没有评估告警阈值是否合理**——本仓已不发货任何告警规则，无从评起。
- **`anchors/r2` 的可运维面只做了接口层判断**（I-20 只证明 CLI 不支持它），
  没有读 R2 anchor 自身的失败/重试/可观测实现——那属于 territory C 的入口。

---

## 附录 A · 事件 × 指标矩阵

契约点名的八个事件，逐个问：**有没有一个指标能区分「没发生」与「发生了但失败」？**
（`log-only` = 只有日志行，且该日志在 `NopLogger` 默认下消失；`—` = 什么都没有）

| # | 事件 | 指标 | 能区分「未发生 vs 失败」？ | 证据 |
|---|---|---|---|---|
| 1 | **attest 批次落后** | **无** | ❌ 完全不可见 | `service/worker.go:377` 把 `attested`/`seq` 用 `_` 丢弃；`core.Metrics` 无 attest 方法 |
| 2 | **anchor 写失败** | **无** | ❌ log-only，且失败被吞（返回 nil） | `service/attestation.go:227-235`、`:319` |
| 3 | **`VerifiedBalance` 返回 UNDEFINED** | **无** | ❌ 完全不可见 | `postgres/verified_balance_store.go`（无 metrics 依赖）；`core.Metrics` 无对应方法 |
| 4 | **reconcile 发现不平** | `ledger_reconciliations_completed_total{success}`、`ledger_reconcile_gap_units{currency_id}`、`ledger_reconcile_check_results_total{check,passed}` | ✅ **可以** —— `ReconcileCheckResult` 发的是 `Passed && Complete`，跳过/截断都算不通过（fail-closed） | `service/reconcile.go:541,1604,1607,1713,1717` |
| 5 | **worker job 锁丢失 / 未获取** | **无** | ❌ 「被别的 replica 持有，跳过」是 **Info 级日志**；无 `job_last_success` gauge | `service/locked_job.go:102-111`；`service/worker.go` 全文件无 metrics |
| 6 | **delivery 积压** | webhook 模式：`ledger_events_delivery_failed_total` / `ledger_events_dead_total` ⚠️ 部分；**库模式（Subscribe）：无** | ⚠️ 半可见 —— 有失败计数，**无队列深度 gauge**；`LocalDispatcher` 零指标 | `service/delivery/webhook.go:174,203,211,213` vs `service/delivery/local.go`（0 命中） |
| 7 | **expiration 抽走冻结** | **无** | ❌ `ReserveReleased` 零调用点；`ExpirationService` 持有 `metrics` 却一次没用 | `service/expiration.go:44,63`；`grep "s.metrics\." service/expiration.go` = 0 |
| 8 | **onchain scanner 落后区块** | `ledger_chain_cursor_lag_blocks{chain_id}` | ⚠️ 能看「落后」，**不能看「扫过了但充值落地失败」** —— ingest 失败后游标照推，lag 回落到 0 | `service/onchain.go:1218,1234,1256`；失败路径 `:1246-1250` |

**汇总：8 个事件里 4 个完全不可见、2 个半可见、1 个可见、1 个（#4）做得对。**
唯一做对的那个（reconcile）正是上轮审计整改过的。

### 附录 A2 · 32 个已注册指标的发射状态

| 状态 | 数量 | 指标 |
|---|---|---|
| ✅ 有生产调用点 | 20 | `rollups_processed_total`、`rollups_pending`、`rollup_seconds`、`rollup_items_failed_total`、`checkpoint_age_seconds`、`balance_drift_units`、`negative_balance_detected_total`、`reconciliations_completed_total`、`reconcile_gap_units`、`reconcile_check_results_total`、`snapshot_seconds`、`events_delivered_total`、`events_delivery_failed_total`、`events_dead_total`、`chain_cursor_lag_blocks`、`deposit_reorg_detected_total`、`sweep_unattributed_total`、`sweep_address_unreadable_total`、`registration_rescan_failed_total`、`deposit_review_required_total` |
| ❌ **零生产调用点** | **12** | `journals_posted_total`、`journals_failed_total`、`journal_post_seconds`、`journal_entry_count`、`idempotency_collisions_total`、`template_failed_total`、`bookings_transitioned_total`、`reservations_created_total`、`reservations_settled_total`、`reservations_released_total`、`reservations_active`、`reserved_amount_units` |

发射的 20 个全部来自 `service/`；零发射的 12 个全部对应 `postgres/` 层的动作，
而 `postgres/` 包**没有 `core.Metrics` 依赖**（`grep 'core.Metrics' postgres/` = 0）。
这不是遗漏，是缺一条依赖边。

### 附录 B · 文档引用 × 实际存在

| 文档位置 | 引用的东西 | 实际 |
|---|---|---|
| `README.md:553` | `ledger_journal_latency_seconds` | ❌ 不存在（`ledger_journal_post_seconds`），且恒空 |
| `README.md:554` | `ledger_pending_rollups` | ❌ 不存在（`ledger_rollups_pending`） |
| `README.md:554` | `ledger_balance_drift` | ❌ 不存在（`..._units`） |
| `README.md:554` | `ledger_reconcile_gap` | ❌ 不存在（`..._units`） |
| `README.md:552` | `ledger_journals_posted_total` | ⚠️ 名字对，恒 0 |
| `README.md:553` | `ledger_reservations_active` | ⚠️ 名字对，恒 0 |
| `README.md:559` | 「~20 methods」 | ❌ 32 |
| `RUNBOOK.md:37` | `ReconcileCompleted{success="false"}` | ⚠️ 是 Go 方法名；实名 `ledger_reconciliations_completed_total`（会发射） |
| `RUNBOOK.md:253` | `PendingRollups` gauge | ⚠️ 实名 `ledger_rollups_pending`（会发射） |
| `RUNBOOK.md:294` | `CheckpointAge{class_code}` histogram | ⚠️ 实名 `ledger_checkpoint_age_seconds{class}`，且是 **Gauge** 不是 histogram |
| `RUNBOOK.md:398` | `IdempotencyCollision{journal_type_code}` | ❌ 标签名错（`journal_type`），且**永不发射** |
| `RUNBOOK.md:428` | `JournalFailed{journal_type_code, reason}` + 5 行 reason 表 | ❌ 标签名错，**永不发射**，reason 词表从未被任何代码产生 |
| `RUNBOOK.md:1239` | `ledger_template_failed_total{template, reason}` | ❌ 名字对，**永不发射** |
| `RUNBOOK.md:211` | `POST /api/v1/system/solvency` | ❌ 实际 `GET /platform/solvency` |
| `RUNBOOK.md:218` | `ledger-cli solvency --currency 1` | ❌ flag 收 uid，不是 id |
| `RUNBOOK.md:220` | `.rollups[] \| select(.classification_code==...)` | ❌ 实际 `data.list[]` + `classification_uid` |
| `RUNBOOK.md:262-263` | `FROM checkpoint_rollup_queue` | ❌ 表不存在（实际 `rollup_queue`） |
| `RUNBOOK.md:370` | 告警 `LedgerEventDeliveryDead` | ❌ 全仓不存在（随 `deploy/` 删除） |
| `CAPACITY.md:5` | `metrics.prometheusRules.thresholds`（Helm） | ❌ Helm chart 已删 |
| `CAPACITY.md:29` | `LedgerRollupBacklog` / `LedgerCheckpointAgeHigh` | ❌ 全仓不存在 |
| `CAPACITY.md:64,73` | `ledger_journal_post_seconds` p99 | ❌ 恒空（`JournalLatency` 零调用点） |
| `CAPACITY.md:68` | `ledger_events_delivered_total` 作 SLO | ⚠️ 库模式（Subscribe）不发射 |
| `DR.md:115` | 「`cmd/ledger-cli` is read-only」 | ❌ `reconcile --full` 写 `reconcile_scan_cursors` |
| `DR.md:121` | 「must print PASS on every check」 | ❌ 输出是 JSON `"passed": true` |
| `DR.md:190` | 「all 13 runnable checks」 | ❌ 现在 15 个 check（14 runnable） |
| `DR.md:192-194` | `full_coverage:false` 归因为「seed-script gap」 | ❌ 是产品缺陷：CLI 无法提供 verifier |
| `DR.md:280-286` | 「migration 008's comment should be corrected」 | ❌ 已被否决（`INVARIANTS.md:3474-3479`：故意不改） |
| `CLAUDE.md:149` | `deploy/helm/ledger/` | ❌ 目录已删 |
| `CLAUDE.md:54` | `make docker # docker compose up --build` | ❌ 实际 `docker compose up -d postgres` |
| `RUNBOOK` 全部 14 个 Go 符号 | `Onchain.scanChainOnce` 等 | ✅ 全部存在 |
| `RUNBOOK` 全部 SQL 对象名（除上述一处） | `rollup_queue` 之外的 9 张表 | ✅ 全部存在 |
| `RUNBOOK:1238` 日志字符串 | `"service: rollup: process item failed"` | ✅ 存在（`service/rollup.go:136`） |
| `RUNBOOK §9` 紧急停机 SQL | 表级 REVOKE + per-partition 循环 + 列级恢复 | ✅ **实测正确**（见「被我自己推翻的假设」） |
