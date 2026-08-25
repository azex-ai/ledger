# 审计报告 — consumer-surface (territory C)

## 摘要

我按契约要求先只读 `README.md` / `docs/frontend.md` / `ledger.go` 的导出签名写下「我以为它是这么用的」，再读实现对差。**差得很多，而且差的方向一致：文档和 example 承诺的语义比实现更"自动"。** 最严重的一条已实跑证实——`examples/billing`（唯一的计量计费参考实现）跑完之后用户余额纹丝未动，example 自己打印 `final balance: 100 USDT (expected 84.25)` 然后 **exit 0**：Reserve/Settle 完全不动账，而 example 的注释断言它动了。照抄它做 SaaS 计费 = 永远不收费。

第二类是 `Worker.Subscribe` 的兄弟，而且不止一个：`svc.Worker(cfg)` 交回来的 Worker **静默关闭了三个 job**（webhook 投递、full reconcile、batch attestation），而 README:152 明写 `svc.Worker` 给的就是「rollup, expiry, reconcile, snapshots, **event delivery**」。其中 `SetAttestor` 全仓**没有任何非测试代码调用过**——P6 批量存证链在所有已发布入口里都是死的，而 `DefaultWorkerConfig()` 却给它配了 60s 间隔，让人以为它在跑。

第三类是「推荐 API 会永久破坏另一个安全机制」：任何经 `RunInTx` 落的 journal 一律 `unsigned_tx_mode`，于是该维度的 `VerifiedBalance` **永久 UNDEFINED**，`Reserve{RequireVerifiedBalance:true}` 对该账户永远拒付；而 `RunInTx` 的 godoc caveat 列表没提，`examples/tx-compose`（RunInTx 的规范示例）没提，`docs/COOKBOOK.md` Recipe 4 反而主动推荐这个姿势。

整体判断：**引擎实现的谨慎程度远高于它的消费方表面。** 大量 fail-closed / TOCTOU / 精度处理做得很硬，但一个只读 README 和 examples 的人会被系统性地引向错误用法——README 里所有 Go 代码块都还停在 uid 化之前的 `*ID` int 字段（一行都编译不过），44 个导出 facade 方法有 11 个（含整个完整性子系统）根本没进 README 的 "API Surface"。

---

## 发现

### [Critical] `examples/billing` 演示的计量计费流程一分钱都不收，并打印一个自相矛盾的"预期值"后 exit 0

- **位置**：`examples/billing/main.go:115-141`（注释 116-118、断言 140-141）；根因 `postgres/reserver_store.go:410-420`、`postgres/reserver_store.go:277-296`
- **判定**：CONFIRMED（实跑）
- **失效场景**：
  `Reserve` 只插一行 `reservations`（`reserver_store.go:281-293`），**不落 journal**；`Settle` 只做 `UpdateReservationSettle`，并显式写 `JournalID: pgtype.Int8{}` 且注释 "no journal linked by the one-shot settle"（`reserver_store.go:411-415`）。所以「预留 20 / 实扣 15.75」这一整套在账上等于 **0**。
  而 example 的注释写的是：

  ```
  // Step 3: compute run finishes — actual cost was $15.75.
  // Settle debits the actual amount and automatically releases the $4.25 remainder.
  ...
  // Expected: 100.00 - 15.75 = 84.25 USDT
  ```

  实跑输出（scratch DB，`postgres://postgres:pw@localhost:54999/audit_c_billing`）：

  ```
  topped up: 100.00 USDT
  reserved: uid=01a03840-... amount=20 status=active
  settled: actual_cost=15.75 (remainder released automatically)
  final balance: 100 USDT (expected 84.25)      <-- 100 ≠ 84.25
  ```
  `echo $?` → **0**。DB 侧核对：`select count(*) from journals` = 1（只有那笔 top-up），`reservations.journal_id` 为 NULL。

  一个消费方照抄 `examples/billing` 做 AI 算力/API 计量计费：用户每次跑任务都会被正确地"占额度"，但**账面永远不扣钱**，余额只涨不跌。发现时间点取决于对账（`main_wallet` 债务 vs 实际服务成本），可能是几个月后。
- **最小复现**：
  1. `createdb scratch`
  2. `DATABASE_URL=postgres://.../scratch go run ./examples/billing`
  3. 读最后一行：打印 100，注释说 84.25，退出码 0。
- **为什么现有机制没拦住**：example 只 `fmt.Printf` 不 `assert`。对照 `examples/event-subscribe/main.go:154-161`——那个 example 明确写了「exit 0 with no delivery would report success for a run in which nothing happened」并在超时时 return error。billing 没做这一步，所以"跑得通"被当成"演示成立"。
  更糟的是仓库内两份文档互相矛盾：`docs/COOKBOOK.md:245-246` 正确地写了 "Reserve does **not** move the balance … Post the actual debit journal … as part of your settle flow"，`examples/credits-topup/main.go:155-156, 173-181` 也正确地补了 `credits_spend` journal。**只有挂着 "SaaS-style metered billing" 招牌的那个 example 是错的**——也就是最可能被照抄的那个。同时 COOKBOOK 自己在 `:227` 又写 "`Settle` captures the actual amount and **auto-releases the remainder** atomically"，与它下面三行的 bullet 打架，说明契约本身没定死。

---

### [Major] `Worker.Subscribe` 文档承诺 at-most-once（出错即丢），实现是 at-least-once（出错重投）

- **位置**：doc `service/worker.go:163-166`；实现 `service/delivery/local.go:67-81`；example 复述同一条错误 `examples/event-subscribe/main.go:96-97`；pin 测试断言相反语义 `service/worker_subscribe_test.go:87-124`
- **判定**：CONFIRMED
- **失效场景**：godoc 原文 "If a handler returns an error the event is logged and **still marked delivered** — blocking the queue on a buggy handler is worse than a missed notification."。实现走 `d.poller.MarkRetry(...)`（`local.go:74`），SQL 侧再按 `attempts+1 >= max_attempts` 转 `dead`（`postgres/sql/queries/events.sql:65-69`）。
  一个只读 godoc（和 example 注释）的消费方会得出「handler 至多被调一次」，于是写一个**非幂等** handler——典型形态是"收到 deposit confirmed 事件 → 给用户加积分 / 发通知 / 触发下游转账"。第一次 handler 因为下游 500 返回 error，事件被重投，加积分执行第二次。金额级别就是重复入账。
- **最小复现**：注册一个 handler，第一次返回 error 第二次返回 nil，观察 handler 被调用两次且第二次 `MarkDelivered`。这正是 `TestWorker_Subscribe_HandlerErrorDoesNotBlockQueue` 已经断言的行为（`worker_subscribe_test.go:118-123`：`assert.NotContains(t, poller.delivered, int64(1))` + `assert.Contains(t, poller.retried, int64(1))`）。
- **为什么现有机制没拦住**：测试断言的是**实现**，doc 说的是**另一回事**，没有任何门禁比对两者。测试名与注释（"at-least-once, mirroring webhook delivery"）都正确，只有消费方唯一会读的那一面（godoc + example 注释）是错的。

---

### [Major] `svc.Worker(cfg)` 静默返回一个关掉了三个 job 的 Worker，而 README 明写它包含 event delivery

- **位置**：`ledger.go:712-739`（只 wire 了 `SetPartitionService` / `SetPool` / `SetLocalPoller`）；README:152；`cmd/ledgerd/main.go:156-171`
- **判定**：CONFIRMED
- **失效场景**：README:152 原文 —— "Background worker (rollup, expiry, reconcile, snapshots, **event delivery**): `worker := svc.Worker(service.DefaultWorkerConfig()); go worker.Run(ctx)`"。
  但 `Worker()` 从不调 `SetEventDeliverer`，于是 `Run` 里 `if w.eventDeliverer != nil`（`service/worker.go:294`）为假，**webhook 投递 goroutine 根本不启动**。库模式消费方按 README 接完之后，`events` 表里所有事件永远停在 pending：不投递、不重试、不 dead-letter、不报错、不打日志。同理 `SetFullReconciler` 不 wire → README:33 宣传的 full reconciliation suite 不跑；`SetAttestor` 不 wire → 见下一条。
  对比 `cmd/ledgerd/main.go:163,171` 服务模式**确实**手工补了 `SetFullReconciler` + `SetEventDeliverer`。也就是说「完整 worker」的知识只存在于 service-mode 的 main.go 里，没有任何 example 或文档告诉库模式消费方要补这两步。
- **最小复现**：`svc := ledger.New(pool)` → `w := svc.Worker(service.DefaultWorkerConfig())` → `go w.Run(ctx)` → 制造几次 booking transition → `select status, count(*) from events group by 1`：全是 pending，日志里一个字都没有。
- **为什么现有机制没拦住**：每个 `Set*` 方法自己的 godoc 都诚实写了 "If not set, the job is skipped"（`worker.go:122-123,127-132,147-151`），但**调用方视角**（`svc.Worker` 的 godoc + README）只字未提。这正是 Subscribe 那一类：单看被调用方文档没错，单看调用方文档也没错，接缝处漏了。

---

### [Major] `Worker.SetAttestor` 全仓无任何生产调用点——P6 批量存证链在所有已发布入口都不运行，而 `DefaultWorkerConfig` 给它配了间隔

- **位置**：`service/worker.go:147-153`（唯一定义）、`service/worker.go:66-68`（`AttestInterval: 60s` / `AttestBatchSize: 1000`）；`cmd/ledgerd/main.go`（零处提及 Attestor / Anchor）
- **判定**：CONFIRMED
- **失效场景**：`grep -rn "SetAttestor" --include="*.go" .` 去掉 `_test.go` 与 `service/worker.go` 自身后 = **0 命中**。`examples/tamper-evident/main.go:149-153` 手工 `attestSvc.RunAttestBatch(ctx, 500)` 跑一次，不经 worker。`cmd/ledgerd` 连 `WithAttestor` 都没有。
  结果：一个消费方 `ledger.New(pool, ledger.WithAttestor(a, v))` + `svc.Worker(DefaultWorkerConfig())` + `Run` —— 每笔 journal 确实签名了，但**批量存证链一格都不往前走**，外部 anchor 上的 head 永远是初始值，`svc.VerifyLedger` 因此拿不到可比对的链。防篡改体系只装了一半，且这一半的失效是完全无声的（`DefaultWorkerConfig` 里那两个字段让人确信它在跑）。
- **最小复现**：按 README 的 observability + WithAttestor 接好，跑一天，`select max(seq) from attestations`（或等价表）不动。
- **为什么现有机制没拦住**：`WithAttestor` 的 godoc（`ledger.go:117-134`）详细解释了签名语义，但没说"你还必须自己 `svc.AttestationService(anchor)` + `worker.SetAttestor(...)`"。`AttestationService` 的 godoc（`ledger.go:314-332`）也只说它返回什么，不说谁该驱动它。

---

### [Major] `svc.Worker` 声称"零值字段一律填默认"，但漏了 `AttestInterval` / `AttestBatchSize`，导致存证 job 静默跳过（Warn 打进 NopLogger）

- **位置**：声明 `ledger.go:708-711`；实现 `ledger.go:745-790`（`mergeWorkerConfig` 逐字段列表里**没有** Attest 两项）；后果 `service/worker.go:288` → `service/worker.go:324-329`
- **判定**：CONFIRMED
- **失效场景**：`Worker` 的 godoc 原文 "Any zero-valued field on cfg is filled in from service.DefaultWorkerConfig so callers get a safe-by-default Worker even when they pass a partially populated config or `service.WorkerConfig{}`." —— `mergeWorkerConfig` 覆盖了 14 个字段（Rollup* / Expiration* / Reconcile / Snapshot / SystemRollup / EventDelivery* / EventClaimLease / FullReconcile / Partition*），**`AttestInterval` 和 `AttestBatchSize` 不在其中**。
  于是 `svc.Worker(service.WorkerConfig{})`（`examples/fullstack/backend/main.go:107` 正是这个写法）+ `worker.SetAttestor(...)` → `runLoop(ctx, "attestation", 0, ...)` → `interval <= 0` 分支：`w.logger.Warn(...)` 后 `<-ctx.Done()` 直接挂住。默认 logger 是 `core.NopLogger()`（`ledger.go:148`），**那条 Warn 被丢弃**。消费方得到的观测信号 = 零。
  `AttestBatchSize = 0` 同理会让 `RunAttestBatch(ctx, 0)` 一条都不存。
- **最小复现**：`w := svc.Worker(service.WorkerConfig{}); w.SetAttestor(as); w.Run(ctx)` —— 无 panic、无日志、无存证。
- **为什么现有机制没拦住**：`mergeWorkerConfig` 是手写的逐字段 if 列表，加新 config 字段时没有任何机制强制同步；`runLoop` 的 fail-soft（`worker.go:320-322` 注释说"只丢这个 loop，不丢整个 worker"）把一个本应是启动期硬错误的配置缺失降级成了运行期静默。契约 §1.4 的第 1 问（"这段代码整个不执行会有测试红吗"）在这里答案是"不会"。

---

### [Major] 经 `RunInTx` 落的 journal 永久 `unsigned_tx_mode`，会把该维度的 `VerifiedBalance` 永久打成 UNDEFINED

- **位置**：`postgres/ledger_store.go:418-431`（tx 模式无条件 `AuthStatusUnsignedTxMode`）、`postgres/ledger_store.go:578-596`（`ExecuteTemplateBatch` tx 路径同样）→ `postgres/verified_balance_store.go:129-158` → `core/auth.go:401-414`
- **判定**：CONFIRMED（读完整调用链）
- **失效场景**：`VerifiedBalance` 要求该 (holder, currency, classification) 维度上**每一笔**贡献 journal 都通过授权校验；`unsigned_tx_mode` 的 journal digest/signature 都是空 → `core.VerifyJournalAuth` 在 `core/auth.go:401` 返回 `ErrUnauthorizedJournal` → 整个维度余额 UNDEFINED。
  `ListContributingEntryVerdicts` 扫的是该维度**全部历史 entry**，没有时间窗；append-only 又保证那笔 journal 永远删不掉，reversal 也只是再加一笔而不抵消它的存在。**所以只要用过一次 `RunInTx` 落账，那个账户维度的 `Reserve{RequireVerifiedBalance:true}` 就永久失败**——对消费方而言就是"这个用户永远提不了款"，且没有任何补救 API。
  这不是边角路径：`RunInTx` 是 README:448-464 专门推荐的组合写入方式，`examples/tx-compose` 与 `examples/crypto-deposit:216-247` 都走它，`onchainTxComposer`（`ledger.go:683-687`）也走它。
- **最小复现**：`ledger.New(pool, WithAttestor(a,v))` → `svc.RunInTx(func(tx){ tx.JournalWriter().ExecuteTemplate(ctx,"deposit_confirm",p) })` → `svc.VerifiedBalanceReader().VerifiedBalance(...)` → `ErrUnauthorizedJournal`（永久）。
- **为什么现有机制没拦住**：README:466-477 **是**写了这件事（要用 `svc.Authorize` + `PostAuthorized`）。但：
  1. `RunInTx` 自己的 godoc caveat 列表（`ledger.go:402-406`）只列了 GetBalance 隔离级别和 advisory lock，**没列签名缺口**——只读 godoc 的人看不到；
  2. `examples/tx-compose`（RunInTx 的规范示例）从头到尾没提；
  3. `docs/COOKBOOK.md:244-246` 反向推荐 —— "Post the actual debit journal … **composed in the same `RunInTx` as the `Settle`**"，即 COOKBOOK 主动教了会中毒的写法。
  三份文档，一份对两份错，且错的两份是更常被照抄的那两份。

---

### [Major] 在 `RunInTx` 回调里再调 `tx.RunInTx` 会从连接池另开一个**独立**事务，原子性静默失效

- **位置**：`ledger.go:412-416`（`RunInTxWithOptions` 无条件 `s.pool.BeginTx`，从不检查 `s.tx`）；clone 保留 pool：`ledger.go:493`
- **判定**：CONFIRMED（读代码）；死锁部分 PLAUSIBLE
- **失效场景**：回调拿到的是同一个 `*ledger.Service` 类型，`RunInTx` 是它的公开方法，编译器不拦。消费方把一段"自己也用了 RunInTx 的业务函数"塞进外层 RunInTx（跨模块调用时极容易发生），内层事务从池里另取连接独立提交：
  - 外层回滚时内层**已经提交**了 → 部分落账，正是 RunInTx 存在的目的的反面；
  - 内层若要写外层已 `SELECT FOR UPDATE` / `pg_advisory_xact_lock` 锁住的同一 (holder, currency)，会等一个只有外层提交才会释放的锁，而外层在等内层返回 → **自死锁**，靠连接超时才解（PLAUSIBLE，未实跑）。
- **最小复现**：`svc.RunInTx(ctx, func(tx *ledger.Service) error { return tx.RunInTx(ctx, func(inner *ledger.Service) error { ... }) })`。
- **为什么现有机制没拦住**：没有嵌套检测（`s.tx != nil` 时应直接报错或降级为 savepoint），godoc 未提 "nested"，也没有测试覆盖嵌套调用。

---

### [Major] README 里所有 Go 代码块都停在 uid 化之前，一行都编译不过

- **位置**：README:104-114、127-134、137-149、307-315、324-330、366-373、388-396、443
- **判定**：CONFIRMED
- **失效场景**：README 用的字段在当前 API 里**不存在**：
  | README 写的 | 实际（`core/journal.go:65-91`、`core/booking.go:12-81`、`core/template.go:39-51`） |
  |---|---|
  | `core.JournalInput{JournalTypeID: jtID}` | `JournalTypeUID string` |
  | `core.EntryInput{CurrencyID: 1, ClassificationID: clsID}` | `CurrencyUID string` / `ClassificationUID string` |
  | `GetBalance(ctx, 42, 1, clsID)` | `GetBalance(ctx, holder int64, currencyUID, classificationUID string)`（`core/interfaces.go:97`） |
  | `core.TemplateParams{CurrencyID: 1}` | `CurrencyUID string` |
  | `core.TransitionInput{BookingID: booking.ID}` | `BookingUID string`；`core.Booking` 也没有 `ID` 字段，只有 `UID` |
  | `svc.RegisterChannel("stripe", adapter)` | `RegisterChannel(adapter channel.Adapter) error`（**单参**，`ledger.go:586`） |
  `docs/COOKBOOK.md:231-237` 同病（`CurrencyID: creditsID`）。
  「第一次用这个库的人」从 README Quick Start 复制第一段代码 → 编译失败 → 只能改去读 `examples/embed`。这不致命，但它意味着 **README 的代码从未被任何 CI 编译过**，所以 README 里其他代码级断言（比如上面那些 Worker / 幂等承诺）同样没有任何保护。
- **为什么现有机制没拦住**：README 代码块不参与构建。`examples/` 参与构建，所以是对的——两者已经漂移了一整代 API。

---

### [Major] README:38 宣称"每一次写操作都要幂等键"，但 `Settle` / `Release` / `FinalizeSettlement` / `Transition` 都没有；重试 `Settle` 拿到的错误无法与真冲突区分

- **位置**：README:38；`core/reserve.go:96-99`（`SettleInput` 无 key）、`core/interfaces.go:154-176`（`Release(ctx, uid)` / `FinalizeSettlement(ctx, uid)` 无 key）、`core/booking.go:66-81`（`TransitionInput` 无 key）；重试语义 `postgres/reserver_store.go:378-389`
- **判定**：CONFIRMED
- **失效场景**：`Settle` 的重试保护只靠预留状态机：第二次调用命中 `!status.CanTransitionTo(settled)` → `ErrInvalidTransition`（`reserver_store.go:387-389`），映射到 bizcode **14004**（`pkg/httpx/response.go:138-139`），`bizcode.Retryable(14004) == false`，用户文案 "Invalid state transition"。
  消费方的实际处境：第一次 `Settle(rsv, 15.75)` 网络超时，不知道有没有落。重试 → 拿到 `ErrInvalidTransition`。这个错误**同时**表示：
  - (a) 我上次其实成功了（应视作成功，继续）；
  - (b) 别人用**不同金额**settle 了同一个预留（必须告警）；
  - (c) 预留过期被 sweeper released（必须退款/重开）。
  `Settle` 返回签名是 `error`（无 `*Reservation`），所以消费方连"最终 settled 多少"都拿不到，必须另发一次读请求，而那次读又没有与 settle 的原子关系。对照 `SettlePartialInput`（`core/reserve.go:111-122`）——同一个文件里，累加语义的那个**要求**幂等键并有完整三态说明，一次性那个没有。这个不一致本身说明"要不要 key"没有统一裁决。
- **为什么现有机制没拦住**：README:38 是一句 blanket 承诺，没有任何按入口逐一核对的门禁；`docs/INVARIANTS.md` I-3 讲的是 journal/booking 的幂等，没覆盖 reservation 生命周期的后半段。
- **移交**：「这几条路径不带 key 到底安不安全」属 territory B，我这里只报"消费方按 README 承诺去写重试逻辑会踩空"。

---

### [Major] `core.ErrUnauthorizedJournal` 没有 bizcode 映射，篡改检测在错误面上表现为"可重试的内部错误"

- **位置**：`pkg/httpx/response.go:121-156`（`resolveError` 的 switch 里没有这一支）→ default `bizcode.Wrap(19999, "internal error", err)`；`pkg/bizcode/errors.go:155-166`（`Retryable(19999) == true`）、`errors.go:120`（文案 "An unexpected error occurred"）
- **判定**：CONFIRMED
- **失效场景**：`ErrUnauthorizedJournal` 是整个防篡改体系唯一的对外信号（`core/errors.go:23-32`：无签名 / digest 对不上 / 验签失败 / `VerifiedBalance` 判定 UNDEFINED 都走它）。经 `pkg/httpx` 映射后：HTTP **500**、code **19999**、文案 **"An unexpected error occurred"**、`Retryable == true`。
  后果两层：
  1. 客户端的通用退避重试循环会**反复冲击**一个永远不会变的拒绝（因为 19999 被归类为"未分类失败，假定为瞬时依赖抖动"，`bizcode/errors.go:151-153`）；
  2. 运维在告警面板上看到的是一个普通 500，而不是"有 journal 的授权对不上"——最需要人立刻看的那个信号被伪装成了噪声。
  `ErrRollupPending`（`core/errors.go:33-39`）同样落 default，语义上碰巧对（可重试），但文案一样误导。
- **可达性说明**：目前 HTTP 侧 `handleCreateReservation`（`server/handler_reservations.go:79-86`）不设 `RequireVerifiedBalance`，所以走 HTTP 暂时够不到；**但 `pkg/httpx` 是库明确提供给消费方在自己 handler 边界做映射的**（CLAUDE.md 列它为 boundary adapter），库模式消费方一旦用它就直接命中。
- **为什么现有机制没拦住**：`resolveError` 的 switch 是手写枚举，`core/errors.go` 新增哨兵时没有任何门禁要求同步；`pkg/httpx/response_test.go` 只测已映射的那些。

---

### [Major] 防篡改提款闸（`RequireVerifiedBalance`）没有任何 HTTP 表面，服务模式部署用不上，且文档没说

- **位置**：`server/handler_reservations.go:80-86`（构造 `core.ReserveInput` 时不读该字段）；`core/reserve.go:73` 该字段**有** json tag `require_verified_balance,omitempty`
- **判定**：CONFIRMED
- **失效场景**：字段带 json tag 会让人以为它是线上契约的一部分，但 `createReservationRequest`（`handler_reservations.go:14-20`）是一个独立的 DTO，根本没有这个字段，`docs/openapi.yaml` 里 0 命中。于是任何以 standalone service 形态部署 `ledgerd` 的消费方，**即使正确配了 `WithAttestor` + anchor，也无法让任何一笔提款走这道闸**。
  同时全仓 grep：`RequireVerifiedBalance` 只出现在 `README.md` 的 examples 清单一行、`docs/RUNBOOK.md`、`docs/INVARIANTS.md`。`docs/api.md` / `docs/openapi.yaml` / `docs/COOKBOOK.md` / `docs/frontend.md` **全部 0 命中**。一个只读 README + api.md 的人根本不知道这个机制存在。
- **为什么现有机制没拦住**：INVARIANTS I-32 的 pin 测试都在 `postgres` 包层（`TestReserve_RequireVerifiedBalance_*`），证明了机制可用，没有任何测试或门禁问"这个机制的对外入口在哪"。

---

### [Major] `examples/crypto-deposit` 教的手搓 ingest 路径绕过 `AutoCreditCeiling` 闸门，且用内存地址表；它的头注释声称生产 API "还没有"——其实早就有了

- **位置**：`examples/crypto-deposit/main.go:16-23`（"not part of this branch yet" / "Once service.OnchainService lands"）、`:169-247`（手搓 `ingestDeposit`）、`:250-270`（`inMemoryAddressRegistry`）；实际已存在的 API `service/onchain.go:503`（`EnsureDepositAddress`）、`service/onchain.go:598`（`IngestDeposit`）、`ledger.go:639`（`EnableOnchain`）
- **判定**：CONFIRMED
- **失效场景**：
  1. **闸门被绕过**：`Onchain.IngestDeposit` 在 `service/onchain.go:802` 有 `tc.AutoCreditCeiling.IsPositive() && booking.Amount.GreaterThan(tc.AutoCreditCeiling)` → 转人工复核；`EnableOnchain`（`ledger.go:661-666`）还在启动期强制每个 token 必须显式配 ceiling（secure-by-default fence，做得很硬）。example 的手搓版**一条都没有**：任意金额、单一 RPC 来源、直接自动入账。
  2. **内存注册表**：`inMemoryAddressRegistry` 重启即失忆。消费方照抄之后，重启前派生给用户的充值地址在重启后 `GetByAddress` 查不到 → 真金白银进来但无法归属。
  3. **头注释是过期的**：它明确告诉读者去手搓，因为"生产实现还没落地"。两个方法都在。
- **为什么现有机制没拦住**：example 的注释不参与任何一致性检查；`EnableOnchain` 的 fence 只保护走它的人，example 教的是不走它。
- **移交**：ceiling 绕过的安全影响归 territory D。

---

### [Major] `examples/credits-topup` 把实扣 journal 放在 Settle **之后、事务之外**，崩在中间就是"用了额度没扣钱"

- **位置**：`examples/credits-topup/main.go:170`（Settle）→ `:177-182`（另起一次 `ExecuteTemplate("credits_spend")`）
- **判定**：CONFIRMED
- **失效场景**：两次独立写、无共同事务、无补偿。进程在两者之间崩溃 / DB 连接闪断 → 预留已 settled（额度按 32 消耗），但 `credits_spend` journal 没落 → 用户白嫖 32 credits。而这正是 `docs/COOKBOOK.md:244-246` 要求"composed in the same `RunInTx`"想避免的。
  这个 example 在 Reserve/Settle 不动账这件事上是**对的**（`:155-156` 明确写了 "does NOT move"），所以它是消费方最可能信任的那份参考——却在原子性上给了错误示范。
  注意它和上一条 M5 构成一个夹缝：COOKBOOK 说的"放进同一个 RunInTx"会触发 `unsigned_tx_mode` 中毒。**当前没有一种写法同时满足原子性和可验证性**（除非绕 `Authorize` + `PostAuthorized`，而没有任何 example 演示过这个组合）。
- **为什么现有机制没拦住**：example 只验证"跑得通"。

---

### [Major] README 的 "API Surface" 缺 11 个导出方法，包括整个完整性子系统；而 `CheckpointIntegrity` 的 godoc 说提款路径 **MUST** 用它

- **位置**：README:558-620（自称完整的 API Surface 表）；缺失项对照 `ledger.go` 的 44 个 `func (s *Service) X`
- **判定**：CONFIRMED（机械 diff）
- **失效场景**：README 中零命中的导出方法：
  `AccountPolicies` / `AttestationService` / `AuthVerifier` / **`CheckpointIntegrity`** / `EnableOnchain` / `HolderReader` / `InstallDevCreditPreset` / `Onchain` / `PeriodCloser` / `RunInTxWithOptions` / `TrialBalanceReader` / **`VerifiedBalanceReader`** / `VerifyLedger`（13 项 0 命中，其中 2 项被 examples 清单顺带提到）。
  最要命的一条：`ledger.go:465-470` 的 godoc 写着 "See core.CheckpointIntegrityStore: **withdrawal / large-amount paths must call RecomputeBalance instead of BalanceReader.GetBalance**"。而 README 从头到尾只教 `svc.BalanceReader().GetBalance`，`grep -l RecomputeBalance examples/*/main.go` = **0 个 example 用过它**。
  也就是说：库里有一条"提款必须走这个 API"的硬规则，它只写在一个 README 没收录的方法的 godoc 里，没有任何 example 演示，没有任何门禁强制。一个照 README 做提款的消费方 100% 会用 `GetBalance`——而 `GetBalance` 读的是 checkpoint+delta，正是防篡改设计里被认为不可信的那条路径。
- **为什么现有机制没拦住**：README 的 API 表是手工维护的，没有"facade 导出方法 ⊆ README"的机械断言。

---

### [Major] 库消费方没有任何「瞬时可重试」错误分类，`bizcode.Retryable` 只服务 HTTP

- **位置**：`core/errors.go:6-40`（14 个哨兵，全部是"用错了"或"业务拒绝"，无一表示瞬时）；`pkg/bizcode/errors.go:155`（`Retryable(code int)`，输入是**业务码**不是 error）
- **判定**：CONFIRMED
- **失效场景**：契约要求消费方能区分三类。实测：
  - 「我调用错了」→ `ErrInvalidInput` / `ErrInvalidTransition` / `ErrConflict` ✅
  - 「账本拒绝了这笔业务」→ `ErrInsufficientBalance` / `ErrAccountFrozen` / `ErrPeriodClosed` / `ErrPrecisionExceeded` ✅
  - 「暂时失败，可重试」→ **没有**。序列化冲突（`GetBalance` 跑 REPEATABLE READ，`RunInTxWithOptions` 允许 Serializable）、死锁（advisory lock 争用）、连接中断，全部作为裸 pgx error 被 `fmt.Errorf("...: %w", err)` 包出来。全仓 grep `40001` / `40P01` / `SerializationFailure` = **0 命中**（`pkg/bizcode` 那几处是注释）。
  消费方要写重试就必须 `errors.As(err, &pgconn.PgError{})` 自己判 SQLSTATE —— 而这要求他 import `pgx`，直接打破 README:560 "your application code depends only on the domain layer" 的承诺。
- **为什么现有机制没拦住**：错误分类的完整性只在 HTTP 边界被想过（`docs/api.md:121-127` 写得很完整），库边界没有对应物。

---

### [Minor] 四个 example 共用的 `ensureCurrency` 把任意错误当成 "not found"，且从不校验找到的币种精度

- **位置**：`examples/embed/main.go:150-165`、`examples/billing/main.go:145-160`、`examples/event-subscribe/main.go:173-188`、`examples/tx-compose/main.go:169-184`、`examples/fullstack/backend/main.go:274-289`；同形态的 `ensureJournalType` / `ensureClassification`：`examples/embed/main.go:142-148,167-178`
- **判定**：CONFIRMED
- **失效场景**：两个问题叠在一起。
  1. `ensureJournalType` / `ensureClassification` 写的是 `if err == nil { return ... }`，**任何**错误（连接中断、超时、权限）都掉进 create 分支，把一个瞬时故障变成一次莫名其妙的 create 冲突。这正是 `discipline.md` §6 禁的形态，出现在库的"hello world"里。
  2. `ensureCurrency` 找到同 code 的币种就直接返回，**从不比对 `Exponent`**。而各 example 对同一个 `"USDT"` 声明的精度不一致：`embed` / `billing` / `event-subscribe` / `tx-compose` 用 `Exponent: 18`，`examples/fullstack/backend/main.go:284` 用 `Exponent: 6`。同一个 DB 上先跑谁谁说了算，后跑的静默接受一个和自己声明不同的精度。精度是钱的属性（`checkAmountPrecision` 会据此拒收/判定），一个消费方照抄这个 helper 就等于把"币种精度"变成了先到先得。
- **为什么现有机制没拦住**：examples 只被编译和运行，没有对产出状态的断言。

### [Minor] `PendingStore` 的类型注释说构造函数会在缺 bundle 时报错——它不会；且 `InstallPendingBundle` 在 README 里 0 命中

- **位置**：`postgres/pending_store.go:23-31`（"resolved once at construction time and cached; … the constructor will return an error"）vs `postgres/pending_store.go:43-51`（`NewPendingStore` 不返回 error、不解析任何东西，其自身注释 `:44-48` 还说"resolved per call, never cached"——同一个文件里两段注释互相矛盾）；`ledger.go:295-301` 转述了错误的那一版
- **判定**：CONFIRMED
- **失效场景**：README:27 把 "Pending two-phase deposits" 列为头牌能力，README:578-579 把 `svc.PendingBalanceWriter()` / `svc.PendingTimeoutSweeper()` 放进 API 表，**都没提**必须先 `presets.InstallPendingBundle`（README 全文 0 命中，`InstallDefaultPresets` / `InstallExtendedPresets` 也都不含它，见 `presets/templates.go:330-339`）。消费方以为构造期会告诉他缺东西（godoc 这么写的），实际要到第一笔 `AddPending` 才炸。属于 M2/M4 同一族的接线陷阱，只是失败是响的而不是静默的，故 Minor。

### [Minor] `server.NewWithConfig` 23 个位置参数，且配置非法时 `panic`

- **位置**：`server/server.go:257-285`
- **判定**：CONFIRMED
- **失效场景**：库的公开构造函数在参数非法时 `panic(fmt.Sprintf("server: invalid config: %v", err))`，消费方无法优雅处理（`examples/fullstack/backend/main.go:232-234` 因此也只能跟着 `panic`）。23 个位置参数意味着任何一次接口顺序调整都可能编译通过却接错 store，而且没有办法省略可选依赖。

### [Minor] `core.Metrics` 有 30 个方法（README 说 ~20），且 `nopMetrics` 未导出，没有可嵌入的基类

- **位置**：`core/metrics.go:12`（接口，30 个方法）、`core/metrics.go:91`（`type nopMetrics struct{}`，小写）；README:536-537
- **判定**：CONFIRMED
- **失效场景**：README 说 "write a thin adapter against `core.Metrics`. The interface is intentionally narrow (~20 methods)"。实际 30 个。想只上报一个指标的消费方必须手写 30 个方法——`core.NopMetrics()` 返回的是接口不是可嵌入的结构体，所以连"嵌入 nop 再覆盖一个"这条捷径都没有。`core.Logger`（3 方法）的对比说明这不是刻意的设计取向。

### [Minor] preset bundle 数量在四个地方说了三个不同的数

- **位置**：README:125 "9 bundles"、README:253 "nine preset bundles"、README:272 "All 9 bundles"、README:257-266 表格列了 **8** 行、README:617 "Install all **8** preset bundles"、`ledger.go:547-549` godoc 列了 **7** 个名字；实际 `presets.InstallExtendedPresets` 装 **8** 个（`presets/templates.go:330-339`），仓库里定义了 **10** 个 `*Bundle()`（多出 `PendingBundle` / `DevCreditBundle`，两者都不在 Extended 里）。
- **判定**：CONFIRMED

### [Minor] 校验错误信息引用了已经不存在的字段名

- **位置**：`core/booking.go:85`（`booking_id must be positive`，实为 `BookingUID == ""`）、`core/reserve.go:81`（`currency_id must be positive`，实为 `CurrencyUID == ""`）、`core/reserve.go:103`（`reservation_id must be positive`，实为 `ReservationUID == ""`）
- **判定**：CONFIRMED
- **失效场景**：消费方拿到 "booking_id must be positive" 会去找一个数字字段并检查它是否 > 0，而真实原因是一个字符串 uid 为空。`api-contract.md §1` 的 `message.fields` key 要求与请求体字段名对齐——这些字符串一旦被当作字段级错误透出，key 就是错的。

### [Minor] `examples/tx-compose` 声称演示回滚，但从不验证 journal 真的不在

- **位置**：`examples/tx-compose/main.go:139-166`
- **判定**：CONFIRMED
- **失效场景**：注释写 "The journal must NOT appear in the database after this call returns"，代码只是 `if rollbackErr != nil { fmt.Printf(...) }`。`rollbackErr` 必然非 nil（回调直接 return error），所以那条打印**恒真**——它证明的是"我返回了错误"，不是"事务回滚了"。若回滚机制本身坏掉，这个 example 照样打印 "journal was rolled back" 并 exit 0。契约 §1.4 第 1 问："这段代码整个不执行会失败吗" → 不会。

### [Minor] facade 的若干方法在 `RunInTx` clone 上会逃出事务

- **位置**：`ledger.go:333-340`（`AttestationService` 用 `s.pool`）、`ledger.go:363-369`（`VerifyLedger`：`NewAttestationStore(s.pool)` 但 `s.queryStore` 是 tx-bound——**同一次校验里混用两个数据视图**）、`ledger.go:639-668`（`EnableOnchain` 用 `s.pool` 且把 `s.onchain` 写在**会被丢弃的 clone** 上，外层 `svc.Onchain()` 仍是 nil，即"调了等于没调"）、`ledger.go:191`（`Pool()` 在 clone 上返回真池子）
- **判定**：PLAUSIBLE（未构造实际调用验证，仅读 `withTx`（`ledger.go:486-518`）与各方法的 `s.tx` 处理差异；`SnapshotBackfiller` / `FullReconciler` 都正确处理了 `s.tx`，这三个没有，不一致本身就是信号）
- **失效场景**：`RunInTx` 的 godoc 只说"不要在回调返回后留着这个 clone"，没说"clone 上有一部分方法本来就不在事务里"。

### [Minor] `svc.Worker()` 改写共享 `eventStore` 状态；一个 Service 建两个 Worker 会互抢事件队列

- **位置**：`ledger.go:719`（`s.eventStore.SetClaimLease(cfg.EventClaimLease)`）、`ledger.go:737`（`w.SetLocalPoller(s.eventStore)`）
- **判定**：PLAUSIBLE（未验证并发写是否被 race detector 捕获）
- **失效场景**：`Worker()` 看起来是纯构造器，实际会改 Service 共享的 `eventStore`。两次调用（不同 cfg）后者覆盖前者的 claim lease；两个 Worker 同时 `Run` 会用同一个 poller 争抢同一批事件。若第二次 `Worker()` 发生在第一个 Worker 已经在跑之后，`SetClaimLease` 是与 `Run` 并发的无同步字段写。

### [Minor] `examples/fullstack/backend` 重复实现了 `ledger.Migrate` 已内建的 URL scheme 转换

- **位置**：`examples/fullstack/backend/main.go:68-78` vs `postgres/migrate.go:32-38,64-76`（`toMigrateURL` 已接受三种 scheme）
- **判定**：CONFIRMED — 无功能危害，但它教消费方复制一段库已经负责的逻辑，且暗示 `ledger.Migrate` 不接受 `postgres://`（README:97 又是直接传 `dbURL`，两处示范不一致）。

### [Minor] examples 里的两处代码风格问题会被照抄

- `examples/crypto-deposit/main.go:245`：`_ = confirmedEvent` —— `golang.md` 明令禁止的"先造再丢"，出现在参考实现里。
- `examples/tx-compose/main.go:76-84`：通过 `svc.DBTX()` 跑 `CREATE TABLE` DDL，示范了用账本 facade 执行任意 DDL。

---

## 移交

- **territory A（金融正确性）**：`presets/fx.go` 与 `docs/COOKBOOK.md:255-262` 的 cash-out 两腿（credits 100 ↔ USDT 1）里，汇率是写死在两个 `Amounts` 里的，没有任何地方校验两腿的比价一致——消费方传错一个数就是凭空造钱。我没往下追。
- **territory B（并发/幂等）**：`Settle` / `Release` / `FinalizeSettlement` / `Transition` 四个写入口不带幂等键（详见上面 [Major]），状态机去重在并发重试下是否等价于键去重，需要 B 判。另 `Booker.Transition` 的去重键是 `(status, channel_ref, amount)`（`postgres/booking_store.go:336-350`），而 `channel/adapter.go:26-30` 把 webhook 的重放保护整个押在它身上，值得 B 核。
- **territory D（威胁模型）**：`examples/crypto-deposit` 手搓 ingest 绕过 `AutoCreditCeiling`（M11 第 1 点）；`ErrUnauthorizedJournal` 映射成 HTTP 500 "An unexpected error occurred"（M9）在 `user-facing-surfaces.md` 意义上是"脱敏过头"——把安全告警脱成了噪声。
- **territory E（测试可信度）**：① `service/delivery/local.go` 的 `ProcessBatch` 除 `worker_subscribe_test.go` 外无独立测试，dead-letter 分支（走 SQL）无 Go 侧覆盖；② 没有任何测试覆盖"`Subscribe` 在 `Run` 之后调用"（此时 `Run` 已读过 `w.localDeliverer == nil`，订阅永不生效，且是无同步的并发字段写）；③ 见下方「⚠️ 工作区异常」——`5c0047c` 的 facade 路径 pin 现在不在工作区里。
- **territory F（结构与演化）**：`core/interfaces.go` 里 7 个接口超过 3 个方法（`ClassificationStore` 7、`JournalWriter` 6、`Reserver` 6…），`core.Metrics` 30 个方法——与 `golang.md`「port 1-3 方法」的偏离程度值得 F 定性。另 README 的 API 表与 facade 导出集无机械同步（M13），属于契约演化没有门禁。

---

## ⚠️ 工作区异常（非审计发现，需 team lead 立刻处置）

审计开始时 `git status` 是 clean（会话开头快照如此）。**我写完报告后再查，工作区多了一条删除**：

```
$ git status --porcelain
 D ledger_subscribe_wiring_test.go
```

`git show HEAD:ledger_subscribe_wiring_test.go` 仍在，内容是
`TestServiceWorker_SubscribeWorksWithoutManualWiring` —— **正是 `5c0047c` 那个 Subscribe 修复的回归 pin**，
它的注释原文就写着「The four tests in service/worker_subscribe_test.go do not protect it …
Delete the wiring from ledger.go and all four still pass. This test goes through the facade a
consumer calls, so it fails when the wiring is gone.」

我全程未创建 / 修改 / 删除任何仓库文件（本次唯一写盘动作是 `go run ./examples/billing`
指向 `ledger-audit-pg` 上的一次性 scratch 库 `audit_c_billing`，跑完已 DROP）。
所以这条删除来自本 squad 的其他并发 agent 或 team lead，**违反契约 §1.1 只读纪律**，
且删掉的恰好是本 territory 存在理由那个 bug 的唯一护栏。

处置建议：`git checkout -- ledger_subscribe_wiring_test.go` 并查清是谁动的。

---

## 我没能验证的

- **`unsigned_tx_mode` 中毒（M5）我没实跑。** 调用链读完整了（`ledger_store.go:427` → `verified_balance_store.go:140-158` → `core/auth.go:401`），但没有构造 `WithAttestor` + `RunInTx` + `VerifiedBalance` 的端到端复现。特别是：批量存证跑过之后，`unsigned_tx_mode` 的 journal 在 `entry_attestations` 里会被记成 `Unauthorized` 还是留 `Unknown`，我只读到两条路都终结于 `ErrUnauthorizedJournal`，没验证过缓存判定那条分支的实际写入。**这条是我最想让 team lead 复核的。**
- **嵌套 `RunInTx` 的死锁部分（M6）是推测。** "内层从池里另开事务"是确定的（`ledger.go:413` 无条件 `s.pool.BeginTx`），"会自死锁"没跑。
- **`docs/frontend.md`（766 行）我只做了 grep 级扫读**，没有逐 hook 核对它描述的行为与 `web/packages/ledger-react` 实现是否一致——契约 §5 说 `web/` 出范围，但 frontend.md 在我的入口清单里，这块算漏。
- **`examples/tamper-evident` 与 `examples/crypto-deposit` 我只读没跑**（前者要求空库、每次生成新密钥，后者依赖内存注册表）。tamper-evident 的头注释质量明显高于其他 example，我没有找到它的问题，但也不能说它没有。
- **`svc.Worker()` 的并发写（Minor）没上 race detector 验证。**
- **`server/` 的 56 个端点我只逐个看了 reservations 一个**（因为它是 `RequireVerifiedBalance` 的落点）。其余端点的 DTO 是否遗漏了 core input 上的字段（和 `RequireVerifiedBalance` 同一形态的问题），我没有系统核过——这可能还藏着同类发现。
