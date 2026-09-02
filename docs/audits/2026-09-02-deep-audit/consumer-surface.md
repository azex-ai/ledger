# 审计报告 — consumer-surface (territory E)

> 基线 `fd87cae`。除本文件与 scratchpad 里的临时验证工程外未改动任何文件。
> 本轮所有「CONFIRMED（实跑）」条目都在 **`fd87cae` 的干净 worktree** 上复验过 ——
> 审计中途主工作区被另一个 agent 改动（见文末「⚠️ 工作区异常」），所以我把
> `fd87cae` 单独 checkout 到 scratchpad 重跑了全部关键证据。

## 摘要

上一轮 territory C 报了 1 Critical + 14 Major + 11 Minor，这 26 条我逐条复核过（下方「上轮修复复核」是一张 26 行的表）。**代码层面的修复质量很高**：`examples/billing`
真的收钱了（实跑 100 → 84.25）、`credits-topup` 把 Settle 与实扣塞进同一个 `RunInTx` 并断言了余额、
`crypto-deposit` 改用真实的 `EnableOnchain`/`IngestDeposit` 并演示了 ceiling 拦截、`tx-compose`
真的去数据库查了那行不存在、嵌套 `RunInTx` 报错了、`mergeWorkerConfig` 补齐了 Attest 两项、
`Worker` 自动接了 `SetAttestor`、README 的**全部 20 个 Go 代码块现在都编译得过**（我抠出来建了一个
临时 module 实测）。八个 example 我逐个跑了，全部 exit 0。

**但这一轮最要紧的三条，全部是「修复本身通过一条默认静默的通道交付」或「修复只落在被审计的那一处」。**

1. 上一轮「`svc.Worker` 静默关掉三个 job」的修复是「README 加一段 + `Run` 启动时打一行日志说明哪些
   job 开着」。可 `ledger.New(pool)` 的默认 logger 是 `core.NopLogger()`，**那行日志被丢弃**。我按
   README Quick Start 逐字写了一个消费方跑起来：**整个进程零输出**，事件永远停在 `pending`。
   `working-agreements.md §3` 要治的「静默失败」，治它的手段本身是静默的。
2. README 的 Go 代码块**编译得过**，但「Add a custom lifecycle」那一段**跑起来直接报错** ——
   非系统分类必须显式声明 `balance_role`，而 `balance_role` 在 README 与 COOKBOOK 里是
   **0 命中**。决定「用户这笔钱能不能被 `Reserve` 花掉」的字段，消费方文档里根本不存在。
   `readme_api_surface_test.go` 只能证明方法名在表里，证明不了示例能跑。
3. I-40 给 `RunInTx` clone 装的三道逃逸闸（`AttestationService`/`VerifyLedger`/`EnableOnchain`）
   做得很硬，但**同一族的三个入口没装**：`tx.RegisterChannel` 报成功然后被丢弃、`tx.Onchain()`
   静默返回 nil、`tx.Worker()` 顺手构造出 `tx.AttestationService()` 上一行刚拒绝掉的那个对象。
   三条都实跑验证。

另外 `CHANGELOG` 的 0.6.0 破坏性清单漏掉了**全部 Go API 层的破坏性变更**（`Release`/
`FinalizeSettlement` 签名、`Settle`/`Transition` 幂等键必填、`core.RollupQueueItem` 移出 core），
而 `61177f9` 删掉升级须知时把指向完整 23 条清单的唯一指针也删了。

整体判断：**引擎与 example 已经追上来了，文档与「修复的交付通道」没有。** 上一轮的结论是
「消费方表面比引擎松」，这一轮更精确：**松的地方从 example 转移到了「消费方第一次照着 README 做的那五分钟」
和「修好的东西如何被消费方看见」。**

---

## 发现

### [Major] 按 README Quick Start 逐字接线的消费方，看不到 worker 的任何输出 —— 上一轮那条 Major 的修复走了一条默认关掉的通道

- **位置**：`ledger.go:148`（`logger: core.NopLogger()`）、`core/logger.go:11-18`（三个方法全是空实现）、
  `service/worker.go:251-267`（`Run` 的启动报告 + anchor 缺失的 Warn）、`service/worker.go:201`
  （Subscribe-after-Run 的 Error）、`service/worker.go:419`（interval<=0 的 Warn）；
  README:161-164（Quick Start 的四行）对照 README:166-180（解释段）
- **判定**：CONFIRMED（在 `fd87cae` 干净 worktree 上实跑）
- **失效场景**：
  上一轮 `svc.Worker(cfg)` 静默关掉 webhook 投递 / full reconcile 的处置是两件事：README:171-178 加了一段
  说明，`Run` 加了一行 `w.logger.Info("worker: starting", "event_delivery_webhook", ..., "full_reconcile", ...)`
  （`worker.go:254-261`），doc comment 里明写这行的目的是让「这个 job 从没跑过」变成
  「消费方可以 grep 的一行启动日志，而不是要自己注意到的一个缺席」。

  可 `ledger.New(pool)` 的默认 logger 是 `core.NopLogger()`，三个方法全是 `{}`。README:180 只说
  「Observability（logger / metrics / tracing）是 opt-in」，没有任何地方说「不注入 logger 你就看不到
  worker 报告哪些 job 没开」。

  实跑（README Tier-2 Quick Start 逐字：`ledger.New(pool)` → `InstallDefaultPresets` →
  `svc.Worker(service.DefaultWorkerConfig())` → `go worker.Run(ctx)` → 造一次 booking transition → 等 12s）：

  ```
  ---- everything below this line is the ONLY output a README-following consumer sees ----
  ---- end of consumer-visible output ----
  events: pending=1 delivered=0 dead=0
  ```

  **零行输出。** 事件停在 pending，没有报错、没有日志、没有指标（metrics 同样默认 Nop）。

  同一条通道上还挂着另外三个安全信号，全部一起被丢弃：
  - `worker.go:262-266`：配了 Attestor 但没有 anchor 时的 Warn（「VerifyLedger 无法发现整库历史重写」）。
    而 `svc.Worker` 的自动接线**永远**传 nil anchor（`ledger.go:856`），所以这条 Warn 在库模式下**必然**触发、
    且**必然**被丢弃。
  - `worker.go:199-202`：`Subscribe` 在 `Run` 之后被调用时的 Error。doc（`worker.go:195-197`）自己写着
    「Subscribe logs an Error when it detects this, but cannot return one … treat that log line as a wiring bug」——
    默认那行日志不存在。
  - `worker.go:418-421`：interval <= 0 时「跳过这个 job」的 Warn。
- **最小复现**：见上（scratchpad 工程 `workerprobe/`，指向 `fd87cae` worktree）。
- **为什么现有机制没拦住**：`examples/fullstack/backend` 是仓库里唯一「完整装配」的 example，它
  显式补了 `SetEventDeliverer` + `SetFullReconciler`（`main.go:118-123`，注释写得很好），但它
  **也没有注入 logger**（`main.go:87` 是裸 `ledger.New(pool)`）。我实跑它，输出里只有 `server/`
  自己用包级 logger 打的两条 WARN，**worker 的启动报告一行都没有** —— 也就是说，这个 example
  一边教「要补两个 Set*」，一边亲自演示了「补没补你都看不出来」。
  `Run` 那行 Info 没有任何测试断言它在默认配置下可见。

---

### [Major] README「Add a custom lifecycle」示例编译得过、跑起来必失败；而它缺的那个字段 `balance_role` 在 README 与 COOKBOOK 里 0 命中

- **位置**：README:419-431；约束在 `core/interfaces.go:328-330`；字段定义 `core/types.go:169-205,305`；
  README:243（Core Concepts 里 Classification 的描述）、README:378-385（另一个 CreateClassification 示例）
- **判定**：CONFIRMED（在 `fd87cae` 干净 worktree 上实跑）
- **失效场景**：
  `core.ClassificationInput.Validate` 现在要求：**任何 `IsSystem == false` 的分类必须显式声明
  `BalanceRole`**（`core/interfaces.go:328-330`）。README 的「Extending the Ledger → Add a custom
  lifecycle (state machine)」示例（README:419-431）既没设 `IsSystem` 也没设 `BalanceRole`：

  ```
  README:378-385 CreateClassification -> <nil>
  README:419-431 CreateClassification -> postgres: create classification: core: classification:
      non-system classification "kyc_review" must declare an explicit balance_role
      (available/pending/locked for a real liability bucket, or memo for a deliberate
      non-liability memo/cost account): invalid input
  README:391-394 CreateJournalType    -> <nil>
  ```

  README:378-385 那个能过，纯粹是因为它碰巧写了 `IsSystem: true`。

  更要紧的是这个字段的文档状态：`grep -n 'BalanceRole\|balance_role' README.md docs/COOKBOOK.md`
  = **0 命中**。README:243 的 Core Concepts 表把 Classification 描述成「Has `NormalSide`（debit-normal
  vs credit-normal）and an optional `Lifecycle` state machine」—— **只字未提 `BalanceRole`**。

  于是消费方的实际处境是：他要建自己的科目表，被一个他从没读到过的必填字段挡住，错误信息给了
  四个选项（`available`/`pending`/`locked`/`memo`），而**这四个选项的语义只写在 `docs/INVARIANTS.md`
  I-25 / I-37 和 `core/types.go` 的 godoc 里**。选 `available` 的后果是那个科目的余额可以被 `Reserve`
  花掉（I-25 的整条不变式就是在防这个）；选 `memo` 的后果是余额永远不进 available、`Reserve` 永远失败。
  一个「让报错消失」的随手选择直接决定钱能不能动。
- **最小复现**：把 README:419-431 原样粘进 `main.go` 跑一次。
- **为什么现有机制没拦住**：`readme_api_surface_test.go` 只断言「每个导出的 `*Service` 方法在 README 里
  出现过 `` `svc.X(` ``」——它保证方法名在，不保证示例能跑。README 代码块也不参与编译（我手工抠出来
  编译是能过的，所以连加一个编译门禁都抓不到这条）。没有任何 example 建过一个非系统的自定义分类。

---

### [Major] `tx.RegisterChannel(...)` 在 `RunInTx` clone 上报告成功，然后被静默丢弃 —— 正是 `EnableOnchain` 的 clone 闸存在的理由

- **位置**：`ledger.go:677-693`（`RegisterChannel`，无 `s.tx` 判断）、`ledger.go:569,607`
  （`withTx` 用 `maps.Clone` 把 channels 复制到 clone 上）；对照 `ledger.go:730-733`（`EnableOnchain` 的闸）
- **判定**：CONFIRMED（实跑）
- **失效场景**：`EnableOnchain` 的错误信息写得非常清楚，说明这个 failure mode 是被认真想过的：

  > "s.onchain would be set on a clone that RunInTx discards when the callback returns, leaving the
  > top-level Service.Onchain() nil **despite this call reporting success**"

  `RegisterChannel` 是字面意义上同一个形状，但没有闸。实跑：

  ```
  A. top-level Channels()      = [top]
  B. tx.RegisterChannel err    = <nil>   (nil = reported success)
  C. tx.Channels() inside      = [top inside-tx]
  D. svc.Channels() after      = [top]   <-- 'inside-tx' 没了
  ```

  消费方场景：在一个 `RunInTx` 里同时做「写自己的租户表 + 给这个租户注册一个 webhook channel adapter」
  —— 事务提交成功、`RegisterChannel` 返回 nil，租户表有了，channel 没了。之后
  `POST /api/v1/webhooks/<name>` 全部 404，且没有任何一处报过错。
- **最小复现**：`svc.RunInTx(ctx, func(tx *ledger.Service) error { return tx.RegisterChannel(a) })`
  → 之后 `svc.Channels()` 里没有 `a`。
- **为什么现有机制没拦住**：I-40 的第 4 条只枚举了 `AttestationService` / `VerifyLedger` /
  `EnableOnchain` 三个方法，pin 测试也只覆盖这三个。判定标准是逐个方法列举的，不是从
  「哪些方法会写 clone 上的字段 / 走 `s.pool`」机械派生的 —— 所以第四个同形方法一开始就不在名单里。

---

### [Major] `tx.Onchain()` 在 clone 上静默返回 nil，即使顶层 Service 已经 `EnableOnchain`

- **位置**：`ledger.go:767`（`Onchain()`）；根因 `ledger.go:565-608`（`withTx` 的字段列表里**没有** `onchain`）
- **判定**：CONFIRMED（实跑）
- **失效场景**：

  ```
  E. svc.EnableOnchain err     = <nil>
  F. svc.Onchain() != nil      = true
  G. tx.Onchain() == nil       = true   <-- clone 上静默是 nil
  ```

  `withTx` 明确地把 `attestor` / `authVerifier` 带到 clone 上，并写了 12 行注释解释为什么必须带
  （`ledger.go:576-585`：「否则那些直接读它们的 accessor 会误以为 WithAttestor 从没被调用过」）。
  **同一段逻辑没有应用到 `onchain`。** 结果：一个消费方在 `RunInTx` 回调里写
  `tx.Onchain().IngestDeposit(...)` —— 这是很自然的写法，因为回调里拿到的就是一个 `*ledger.Service`，
  类型系统不区分 —— 直接 nil panic。而 `EnableOnchain` 的 clone 闸恰恰是为了防「调了等于没调」，
  它的孪生读接口却给出「配了等于没配」。
- **最小复现**：`svc.EnableOnchain(...)` → `svc.RunInTx(ctx, func(tx *ledger.Service) error { _ = tx.Onchain(); return nil })`
  → nil。
- **为什么现有机制没拦住**：`TestService_EnableOnchain_RefusedOnTxBoundClone` 测的是「在 clone 上**调**
  EnableOnchain 被拒」，没有测「在 clone 上**读** Onchain()」。

---

### [Major] `tx.Worker(cfg)` 没有 clone 闸，且会顺手构造出 `tx.AttestationService()` 上一行刚拒绝掉的那个对象

- **位置**：`ledger.go:817-859`（`Worker`，无 `s.tx` 判断，全程用 `s.pool`）、`ledger.go:855-857`
  （`if s.attestor != nil { w.SetAttestor(s.attestationServiceUnchecked(nil)) }`）、
  `ledger.go:354-358`（`attestationServiceUnchecked` 用 `postgres.NewAttestationStore(s.pool)`）；
  被绕过的闸 `ledger.go:340-342`
- **判定**：CONFIRMED（实跑）
- **失效场景**：`AttestationService` 的闸给出的理由是「它直接经 `s.pool` 读写（跨很多行的 postJournal
  批处理，不属于调用方的事务），从 clone 构造出来会静默地在调用方以为自己在组合的那个事务之外操作」。
  在同一个 clone 上：

  ```
  N. tx.AttestationService()   -> svc=false err=ledger: attestation service: called on a
       transaction-bound store; ... must not be built from inside RunInTx: invalid input
  O. tx.Worker()               -> worker=true  (内部自动接了同一个 AttestationService，走 s.pool)
  ```

  即：闸拦住了正门，侧门开着，而且侧门给出的是**完全一样的对象**。同理
  `H. tx.Worker() built = true` —— 一个绑在池子上的完整后台 worker（rollup / expiration / partition DDL /
  attestation）从一个事务回调里被造出来，没有任何提示。
- **最小复现**：`svc := ledger.New(pool, ledger.WithAttestor(a, v))` →
  `svc.RunInTx(ctx, func(tx *ledger.Service) error { _, err := tx.AttestationService(nil); /* err != nil */ w := tx.Worker(service.WorkerConfig{}); /* w != nil */ return nil })`
- **为什么现有机制没拦住**：同 M3 —— I-40 的名单是人工枚举的三个方法，没有机械断言
  「任何在 `s.tx != nil` 时仍然触碰 `s.pool` 的导出方法，要么被拒绝，要么在 godoc 里声明」。
  `ledger.go` 里当前还在这个集合里的还有 `Pool()`（已在 godoc 声明）和 `Ping()`（未声明，见 Minor）。

---

### [Major] `CHANGELOG` 的 0.6.0 破坏性清单漏掉了全部 Go API 层的破坏性变更，且已不再指向完整清单

- **位置**：`CHANGELOG.md:17-313`（`[0.6.0]` 整段）；被删掉的指针 `61177f9`；完整清单实际所在
  `docs/plans/2026-08-27-release-readiness.md:32-64`
- **判定**：CONFIRMED（机械 diff + grep）
- **失效场景**：`[0.6.0]` 段里带 `**Breaking**` 标记的共 5 条（`:39` 服务二进制、`:213` `NewReserverStore`、
  `:217` `journals.event_id`、`:225` migration 压平、`:274` `HolderTransaction.kind`）。
  `grep -niE 'idempotenc|release|finalize|rollupqueue|balancecheckpoint|transition'` 在
  `CHANGELOG.md:17-313` 上的命中：**0**（除了三处与此无关的散文）。

  实际 `v0.5.1..v0.6.0` 之间发生、且**编译期或运行期会打断既有消费方**的变更：

  | 变更 | v0.5.1 | v0.6.0 | CHANGELOG |
  |---|---|---|---|
  | `core.Reserver.Release` | `Release(ctx, reservationUID string) error` | `Release(ctx, input ReleaseInput) error` | 无 |
  | `core.Reserver.FinalizeSettlement` | `(ctx, reservationUID string)` | `(ctx, input FinalizeSettlementInput)` | 无 |
  | `core.SettleInput` | 无 `IdempotencyKey` | 新增且 `Validate()` 为空即拒 | 无 |
  | `core.TransitionInput` | 无 `IdempotencyKey` | 新增且 `Validate()` 为空即拒 | 无 |
  | `core.RollupQueueItem` | 存在 | 已移出 `core`（→ `service`） | 无 |
  | `core.BalanceCheckpoint` | `CurrencyID`/`ClassificationID int64` + `LastEntryID` | `CurrencyUID`/`ClassificationUID string`，无 `LastEntryID` | 无 |

  前两条是编译失败，中间两条是**编译通过、运行期被 `Validate()` 拒绝** —— 后者最危险：
  一个升级到 0.6.0 的消费方，`Settle`/`Transition` 全线在生产上开始返回 `ErrInvalidInput`。

  ⚠️ **归因说明（回查过 SoT）**：`61177f9` 删掉升级须知是 **Aaron 明确拍板**的（commit body 原文：
  「this package is being introduced from zero in Agentoon, and no consumer is carrying data from a
  previous version」）。**那条拍板针对的是「要改数据」的两条**（借贷方向、分数冲销），逻辑成立。
  但被删掉的那段里还有一句：

  > 完整的 23 条破坏性变更、按「消费方要做什么」分类：`docs/plans/2026-08-27-release-readiness.md` §3

  这是**整个仓库里指向那份清单的唯一指针**（现在 `grep -rn 'release-readiness' --include='*.md' .`
  除该文件自身外 = 0 命中）。所以现状不是「Aaron 决定不写升级须知」，而是「API 破坏性清单从
  CHANGELOG 里消失了，且没有任何路径能找回它」—— 这一条与 Aaron 的拍板不冲突。
- **为什么现有机制没拦住**：没有任何门禁比对「`v(N-1)..vN` 的导出符号 diff」与「CHANGELOG 的 Breaking 段」。
  `readme_api_surface_test.go` 证明了这类机械断言在这个仓库是可行且有效的（它就是这么抓 README 的），
  CHANGELOG 侧没有对应物。

---

### [Major] `examples/event-subscribe` 仍然教 at-most-once —— godoc 修好了，README 指过去的那个 example 没有

- **位置**：`examples/event-subscribe/main.go:95-97`；已修正的对照面 `service/worker.go:176-186`、
  `service/delivery/local.go:19-24`、`docs/RUNBOOK.md:348`
- **判定**：CONFIRMED
- **失效场景**：`Worker.Subscribe` 的 godoc 与 `LocalDispatcher` 的类型注释都已改成正确的 at-least-once
  （`worker.go:181-185`：「a handler that errors after doing partial work WILL be invoked again for the
  same event. Handlers must therefore be idempotent per event UID; do not write a handler that assumes
  "returns an error" means "had no effect"」），实现也确实走 `MarkRetry`（`local.go:74`）。

  但 README:794 指过去的那个 example 里，紧挨着 `worker.Subscribe(...)` 调用点的注释仍然是旧的错误说法：

  ```go
  // Subscribe to events. The handler receives every emitted core.Event.
  // If the handler returns an error the event is still marked delivered —
  // a buggy handler should not block the queue.
  ```

  这正是消费方会照抄的那一面。一个据此写的**非幂等** handler（「收到 deposit confirmed → 给用户加积分」）
  在第一次因下游 500 返回 error 后会被重投，积分加两次。

  README 自己也没有兜底：README:30 只写「In-process event subscription — `Worker.Subscribe` for
  library-mode event callbacks without a webhook server」，**不说交付语义**。`docs/RUNBOOK.md:348`
  说的是 webhook 投递，一个只读 README + example 的人不会走到 RUNBOOK。
- **为什么现有机制没拦住**：上一轮的修复对象是 godoc（因为发现是在 godoc 上提的），example 的注释
  不参与任何一致性检查；也没有门禁比对「同一命题在 godoc / example / README 三处是否一致」。

---

### [Major] 库模式的重试契约只接了一半：`core.IsRetryable` 有了，「重试要复用同一个 key」只写在 HTTP 那一侧

- **位置**：`core/errors.go:65-118`（`IsRetryable` 及其 godoc）、`idempotency.go:9-25`
  （`NewIdempotencyKey` godoc）；规则的唯一陈述点 `docs/api.md:135`；18 处反例见下
- **判定**：CONFIRMED
- **失效场景**：上一轮「库消费方没有任何『瞬时可重试』分类」的修复是新增 `core.IsRetryable`，做得很好 ——
  它的 godoc 甚至精确写了正确的用法：

  > "reports whether a caller may safely retry by resubmitting the **SAME request with the SAME
  > idempotency key** (see api-contract.md §9: retrying with a fresh key on a request that already
  > landed creates a duplicate side effect, not a no-op replay)"

  问题在另一半：**这条规则在库模式的消费方文档里从没被陈述过，而库提供的 key 生成器在所有示范里
  都是在调用点内联生成的。**

  - `grep -rn 'same idempotency key' README.md docs/COOKBOOK.md` = **0 命中**；唯一命中在
    `docs/api.md:135`（HTTP 模式，「Client-initiated writes generate one random UUID … per logical
    submission and **reuse it across retries**」）。
  - `ledger.NewIdempotencyKey` 自己的 godoc（`idempotency.go:9-16`）只说 key 的形状与 crypto/rand，
    **没有一个字**说「生成一次，在重试循环外面」。
  - `IdempotencyKey: ledger.NewIdempotencyKey(...)` 直接内联进 struct literal 的写法：
    **README 4 处（:110 :136 :146 :153）+ examples 10 处 + COOKBOOK 4 处（:168 :238 :252 :258）= 18 处**。

  组合起来的消费方代码是这样的：

  ```go
  for attempt := 0; attempt < 3; attempt++ {
      _, err = svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
          IdempotencyKey: ledger.NewIdempotencyKey("deposit-confirm"),   // <- README:136 原样
          ...
      })
      if err == nil || !core.IsRetryable(err) { break }                  // <- 库鼓励的重试判据
  }
  ```

  第一次调用超时但实际已提交 → 第二次带**全新 key** 重放 → **双倍入账**。两个都是库自己提供的
  primitive，都按各自的示范用法用，合起来就是重复记账。
- **最小复现**：上面那段循环，第一次调用被网络超时打断（journal 已落库）。
- **为什么现有机制没拦住**：`api-contract.md §9` 的合规检查只在 HTTP 边界做过一次
  （`docs/api.md:135`）。库边界补了 `IsRetryable`（错误分类），没补 key 生命周期。
- **移交**：「内联生成 key 在并发/重试下的实际风险窗口」偏 territory B，我这里只报消费方文档面。

---

### [Major] `core.ErrUnknownAuthKey` 在 `core.IsRetryable` 与 HTTP 映射之间口径相反；防止这件事的 pin 测试是一张漏了它的手抄表

- **位置**：`core/errors.go:33-45,113`（`IsRetryable` → `false`）、`pkg/httpx/response.go:203-238`
  （`resolveError` 的 switch 里没有这一支 → `default` → `19999`）、`pkg/bizcode/errors.go:206-221`
  （`Retryable(19999)` → `true`）；本该拦住它的 pin `pkg/httpx/response_test.go:127-158`
- **判定**：CONFIRMED（读完整调用链 + 逐个核对三处代码；未构造 HTTP 端到端请求）
- **失效场景**：`core.IsRetryable` 的 godoc 明写自己是「BOTH library and HTTP modes 的单一事实源」，
  并点名 `TestResolveError_AgreesWithCoreIsRetryable` 「pins the two from drifting apart」。

  那个测试（`response_test.go:128-145`）是一张**手写的 slice**：15 个 `core` 哨兵 + 1 个合成的
  未分类 error。而 `core/errors.go:6-63` 现在有 **16 个哨兵** —— 机械 diff 之后，唯一没被列进去的
  是 `core.ErrUnknownAuthKey`，而它恰好也是唯一一个两边口径不一致的：

  | | `core.IsRetryable` | `resolveError` → code | `bizcode.Retryable` |
  |---|---|---|---|
  | `ErrUnknownAuthKey` | `false`（`errors.go:113`） | `19999`（default 分支） | `true`（`errors.go:218`） |

  语义上这是「验签器不持有这个 key id」—— 密钥轮换掉了、或者这条 journal 属于别的部署
  （`core/errors.go:33-45`）。经 `pkg/httpx` 映射后它变成 HTTP 500 / 19999 /
  「An unexpected error occurred」/ **可重试**：客户端的通用退避循环会反复冲击一个永远不会变的结果，
  运维面板上看到的是普通 500 而不是「有 journal 的签名 key 不认识」。**这与上一轮已修的
  `ErrUnauthorizedJournal`（现在正确地映射到 14010 / 不可重试）是同一个形状，只是换了个哨兵。**
- **可达性说明（不夸大）**：`core.VerifyJournalAuth` 会把它包在 `ErrUnauthorizedJournal` 里一起返回
  （Go 多 `%w` 链），而 `resolveError` 的 switch 先命中 `ErrUnauthorizedJournal`（`response.go:223`），
  所以**走库内既有路径时映射是对的**。裸 `ErrUnknownAuthKey` 出现在：消费方自己实现的
  `core.AuthVerifier` 的 `Verify` 返回值（`errors.go:33-37` 就是这么规定的），以及
  `service.FullReconciliationService` 专门区分它的那条分支。而 `pkg/httpx` 是 `CLAUDE.md` 明确
  列为「给消费方在自己 handler 边界做映射」的 boundary adapter。
- **为什么现有机制没拦住**：pin 是手抄表。上一轮 README 的 API 表就是因为手抄漏了 11 个方法，
  修完人工补又漏 3 个，最后改成机械派生才收住（`docs/audits/2026-08-25-financial-engineering/README.md:44-46`
  的原话是「**手工补表就是会漏，两次为证**」）。同一个教训没有应用到这张表：它完全可以从
  `core/errors.go` 的 `var (...)` 块机械派生（就像 `internal/idschema` 从 migration 派生 id 列名那样）。

---

### [Major] `server.NewFromDeps` —— 为修「23 个位置参数 + panic」而加的构造函数 —— 全仓零使用，README / RUNBOOK / fullstack 仍然全部教旧的那个

- **位置**：`server/server.go:441-446`（`NewFromDeps`）、`server/server.go:400-433`（`Deps`）；
  仍在教旧构造函数的三处：README:188,192、`docs/RUNBOOK.md:780`、`examples/fullstack/backend/main.go:12,220-242`
- **判定**：CONFIRMED（`grep -rn 'NewFromDeps' --include='*.go' .` 命中只在 `server/server.go`
  自身与 `server/deps_test.go`）
- **失效场景**：`Deps` 的 doc comment 把问题说得很准：

  > "the twenty-one same-shaped interface parameters in a fixed positional order (New/NewWithConfig)
  > have no compiler help catching an accidental transposition -- interfaces don't carry field names,
  > so **two swapped arguments of matching interface shape compile clean and fail at runtime**"

  然后：
  - README 唯一的 HTTP 装配示例（README:192）是 `server.NewWithConfig(cfg, /* ... */)`；README 全文
    **0 次**提到 `NewFromDeps`。
  - `examples/fullstack/backend/main.go:220-242` —— 仓库自称的「complete assembly」—— 用的是
    23 个位置参数的版本，并在文件头注释（`:12`）里把它列为示范项。
  - 那个 example 因此也继承了 panic 语义：`NewWithConfig` 在 cfg 非法时 `panic`，
    `main.go:244-246` 只能跟着 `panic(err)`。

  结果：修复存在、有测试、且是正确的；而**没有一个消费方会看见它**。这就是上一轮 README 里
  总结的「机制存在，但没接到真实入口」，这一次发生在整改本身上。
- **为什么现有机制没拦住**：`server/deps_test.go` 证明 `NewFromDeps` 能用，没有任何东西问
  「文档和 example 是否改用了它」。

---

### [Major] `srv.SetReady(true)` 是必做的接线步骤，零文档；而 `docs/api.md` 声称这个探针会自动变绿

- **位置**：`server/server.go:484`（`ready: &atomic.Bool{}`，默认 false）、`:542-546`
  （`SetReady`/`IsReady`）、`server/handler_system.go:43-52`；唯一调用点
  `examples/fullstack/backend/main.go:243`；错误的文档 `docs/api.md:882-884`、README:885
- **判定**：CONFIRMED
- **失效场景**：`ready` 是一个默认 `false` 的 `atomic.Bool`，**库里没有任何代码会把它置 true** ——
  不观察 migration、不观察 worker。唯一的置位点是 example 里的一行 `srv.SetReady(true)`。

  而文档说的是相反的话：
  - `docs/api.md:884`：「Kubernetes-style readiness probe. **Returns 200 once migrations + worker
    have booted**; 503 with `{"status": "starting"}` otherwise.」
  - README:885：「`/api/v1/system/ready` returns 503 **until migrations + worker have booted**」
  - `handler_system.go:43` 的注释也是同一句：「returns 200 only after migrations + worker have booted」

  三处都把它描述成自动的。消费方按 README 挂上 `server/`、把 `/api/v1/system/ready` 配成 k8s
  readiness probe → **pod 永远不 ready，永远进不了 LB 轮转**，滚动发布卡死。整个过程没有任何错误
  提示，探针老老实实返回 503「starting up」，看起来就像还在启动。

  `SetReady` 在 README（0 命中）、`docs/api.md`（0 命中）、`docs/RUNBOOK.md`（0 命中）里都没有。
- **最小复现**：`server.NewWithConfig(...)`（不调 `SetReady`）→ `GET /api/v1/system/ready` → 恒 503。
- **附带**：`docs/api.md:884` 说失败响应体是 `{"status": "starting"}`，但 D-contract 已把探针失败
  改成统一信封（`handler_system.go:48` → `{"code":18101,...}`）。CHANGELOG 专门提醒过消费方要改监控脚本，
  `docs/api.md` 没跟上。**移交 territory H**。

---

### [Minor] `ledger.go` 的 `InstallExtendedPresets` godoc 列了 7 个 bundle，实际装 8 个（漏 FX）

- **位置**：`ledger.go:638-641` 对照 `presets/templates.go:344-353`
- **判定**：CONFIRMED
- 上一轮「preset bundle 数量在四处说了三个数」的修复把 README 全部统一到 8（README:129,265,284,661
  与 :271-278 的 8 行表格现在一致），**但 facade 的 godoc 没改** ——
  「deposit, withdrawal, transfer, fee, capital, settlement, and spread bundles」= 7，缺 `FXBundle()`。
  而 FX 是 `docs/COOKBOOK.md` Recipe 1 / 5（买 credits、cash out）的全部基础。只读 godoc 的人
  （IDE 悬浮提示看到的就是这一段）会以为要另外装 FX。

### [Minor] 三处「数量」型文档断言过期

- **位置与实测值**：
  | 断言 | 位置 | 实际 |
  |---|---|---|
  | 「The **34** invariants」 | README:778、`CLAUDE.md:150` | `grep -cE '^#+ *I-[0-9]+' docs/INVARIANTS.md` = **48**（I-1..I-48） |
  | `core.Metrics`「intentionally narrow (**~20** methods)」 | README:559 | 接口体内 **32** 个方法；`core/metrics.go:20` 自己的 godoc 又说「30」 |
  | openapi「**56** paths, **59** schemas」 | README:782 | **59** paths / **97** schemas |
- **判定**：CONFIRMED（机械计数）
- 上一轮把「preset bundle 数」这一条报成 Minor 并修了，同一形状的另外三处没被扫到。
  `core.NoopMetrics` 现在已导出（`core/metrics.go:136`），godoc 也给了 embed 用法 —— 上一轮那条
  Minor 的另一半修好了，只有 README 的数字没跟。

### [Minor] README 与 CLAUDE.md 都还列着已删除的 `deploy/helm/ledger/`

- **位置**：README:734（Architecture 目录树）、`CLAUDE.md:149`（File Layout 表）
- **判定**：CONFIRMED（对两份文档里的所有路径做了机械存在性检查，**只有这一条**不存在；`deploy/`
  在 `30bd872` 随服务二进制一起删掉）

### [Minor] README 的「Local Development with go.work」照抄会直接失败

- **位置**：README:50-51（`go 1.26.1`）、README:65、README:62（`rm ledger/go.work`）、README:66-67
- **判定**：CONFIRMED（实跑）
- 三个独立问题：
  1. `go.mod` 是 `go 1.26.6`，README 让消费方在 go.work 里写 `go 1.26.1`。实测：
     `go: module ... requires go >= 1.26.6, but go.work lists go 1.26.1`。**README 的第一个代码块就跑不动。**
  2. README 给的替代 go.work 只 `use` 了 `./ledger` 与 `./ledger/internal/postgrestest`，
     而仓库自带的 go.work 有**五个** module（还有 `chains/evm`、`anchors/r2`、
     `anchors/r2/internal/miniotest`）—— 照做之后碰到这三个 module 的构建会失败（PLAUSIBLE，未实跑，
     因为按 §1.1 我不能 `rm` 仓库里的 go.work）。
  3. README:66 说「The workspace file is git-ignored by convention」—— `go.work` 与 `go.work.sum`
     **都在 git 里**（`git ls-files go.work go.work.sum` 两个都返回），`.gitignore` 里没有 `go.work`。
     所以 README:62 的 `rm ledger/go.work` 会在仓库里留下一条未提交的删除 —— 正是上一轮
     「⚠️ 工作区异常」那一节记录过的现象，只是这次源头是 README 自己。
- 附带（PLAUSIBLE→CONFIRMED 的小观察）：另一条自然的本地路径
  `replace github.com/azex-ai/ledger => ../ledger` **跑不通** ——
  `go mod tidy` 会去解析根 `go.mod:28` 的
  `github.com/azex-ai/ledger/internal/postgrestest v0.0.0-00010101000000-000000000000`
  （它的 `replace` 是相对路径，不传递给消费方），报
  `invalid version: unknown revision 000000000000`。README 说「without publishing or `replace`
  directives」，但没说 `replace` 会**失败**，也没说原因。
  ✅ 对照组：`go get github.com/azex-ai/ledger@v0.6.0` + `go mod tidy` 在一个全新 module 里**是成功的**
  （我实跑过），所以这只影响本地开发路径，不影响已发布消费。

### [Minor] README 的「## Configuration」在描述一个不存在的服务入口

- **位置**：README:860-877（表头原文「The service entry point reads:」）对照
  `server/server.go:249-311`（`LoadConfig`）；README:184-186 自己刚说过「This repository ships **no
  server binary** … There is nothing here to deploy.」
- **判定**：CONFIRMED（grep 全仓 `os.Getenv`）
- 表里 **4 个变量库里没有任何代码读**：`DATABASE_URL`（只有 example / cli / test 自己读）、
  `HTTP_PORT`（只有 example 读）、`MIGRATE_MODE`（**全仓 0 命中**）、`EVM_WEBHOOK_SECRET`
  （只在 `channel/onchain/doc.go:8` 的注释里出现）。
  同时 **4 个 `LoadConfig` 真的读的变量不在表里**：`HOLDER_TOKEN_SECRET`（holder token 签名密钥）、
  `DEV_CREDIT_ENABLED`（凭空造余额的开关）、`PROTECTED_TEMPLATE_CODES`、`ALLOW_GENERIC_TEMPLATE_POST`
  （后两个直接决定 I-38 那道「write-scope key 不能铸造充值形状 journal」的闸口开在哪）。
  一个照 README 配置的部署方，会以为自己配全了。

### [Minor] `server.Config.AllowSystemClassificationPost` 的 godoc 声明了一个 `LoadConfig` 从不读的环境变量

- **位置**：`server/server.go:185`（doc 里写 `(ALLOW_SYSTEM_CLASSIFICATION_POST)`）对照
  `server/server.go:288-297`（`LoadConfig` 只读 `DEV_CREDIT_ENABLED` / `PROTECTED_TEMPLATE_CODES` /
  `ALLOW_GENERIC_TEMPLATE_POST`）
- **判定**：CONFIRMED
- 方向是 fail-closed（开关设不上 = 守卫保持开启），所以不是安全问题；但一个合法需要它的部署方
  设了环境变量、走 `server.New()`（内部用 `LoadConfig`），会在生产上收到一片 403 而找不到原因。
  README:886 的措辞是对的（说的是「sets `Config.AllowSystemClassificationPost`」，字段而非 env），
  是字段自己的 godoc 在说谎。

### [Minor] `docs/COOKBOOK.md:270` 的 `Release(rsv.UID)` 用的是已经改掉的签名

- **位置**：`docs/COOKBOOK.md:270` 对照 `core/interfaces.go:168`（`Release(ctx, input ReleaseInput)`）
- **判定**：CONFIRMED
- COOKBOOK 的 21 个 Go 代码块我逐个核过签名，其余都对（`ReverseJournal` / `ClosePeriod` /
  `TrialBalance` / `Settle` / `Reserve` 全部与 `core/interfaces.go` 一致）；**只有这一处**，
  而且因为它写在散文里（不是代码块），任何「抠出代码块编译」的门禁都抓不到。

### [Minor] `InstallPendingBundle` 在 README 与 docs 里仍是 0 命中，而 pending 是 README 的头牌能力

- **位置**：README:27（Features 列 "Pending two-phase deposits"）、README:602-603（API 表列出两个
  accessor）；约束现在诚实地写在 `ledger.go:295-301` 与 `postgres/pending_store.go:33-37`
- **判定**：CONFIRMED
- 上一轮 `PendingStore` 那两段自相矛盾的注释已经修好（现在统一说「per call 解析、构造期不会失败、
  必须先装 bundle，失败表现为第一次调用返回 `core.ErrNotFound`」），facade 的 godoc 也补上了。
  但**消费方文档一个字都没加**：`InstallDefaultPresets` 不含它、`InstallExtendedPresets` 也不含它
  （`presets/templates.go:344-353` 只有 8 个，pending / devcredit 都不在），README 却把它当头牌功能
  列在 Features 第一行。失败是响的（`ErrNotFound`）而不是静默的，所以维持 Minor。

### [Minor] `examples/credits-topup` 里两个 helper 仍把任意错误当 "not found"，而同文件第三个已经修好

- **位置**：`examples/credits-topup/main.go:258`、`:287`（`if _, err := ...; err == nil { return nil }`）
  对照同文件 `:311-318`（`ensureJournalType` 已改成 `errors.Is(err, core.ErrNotFound)`）
- **判定**：CONFIRMED
- 上一轮这条 Minor 覆盖了 5 个 example 的 `ensureCurrency` / `ensureJournalType` /
  `ensureClassification`。我逐个核过：`embed:145,182`、`tamper-evident:396,430`、
  `credits-topup:313` 全部已修成 `errors.Is(err, core.ErrNotFound)` 的正确形态；
  所有 `ensureCurrency` 也都加上了 `Exponent` 比对并在不匹配时报错。**只剩这两个模板 helper 是旧形态。**
  同一个文件里两种写法并存，是最容易被照抄错的那种残留。

### [Minor] 各 example 对同一个 USDT 声明的精度仍然不一致，现在会互相硬失败，而 README 没说要各用各的库

- **位置**：`examples/{embed,billing,event-subscribe,tx-compose,credits-topup}` 均 `Exponent: 18`；
  `examples/fullstack/backend/main.go:291` 是 `Exponent: 6`；README:787-800 的 example 清单
- **判定**：CONFIRMED（实跑）
- 上一轮报的是「静默接受先到先得的精度」，现在改成了显式报错 —— 方向对。副作用是两个 example
  不能共用一个数据库：

  ```
  $ DATABASE_URL=...(fullstack 用过的库) go run ./examples/billing
  currency USDT already exists with exponent 6, this example expects 18
  exit status 1
  ```

  README 的 example 清单里只有 `tamper-evident` 一条写了「Expects an empty database」，其余七条
  没有任何提示，各 example 头注释给的 `DATABASE_URL` 示例库名也不统一
  （`ledger_dev` / `ledger_example`）。八个 example 对同一个币种的精度**本身**也仍然不一致。

### [Minor] `examples/tx-compose` 的示范表存的是账本内部 `currency_id`，与 I-18 / README 自己的 uid-only 主张相反

- **位置**：`examples/tx-compose/main.go:82-89`（`currency_id BIGINT NOT NULL`）、
  `:130-133`（`VALUES ($1, $2, $3)` 传字面量 `1`）
- **判定**：CONFIRMED
- 这个 example 的全部意义是「教你怎么把自己的表和账本写进同一个事务」，所以它的表结构就是示范。
  它拿到的是 `currencyUID`（`:71`），却在自己的表里存了一个硬编码的内部数字 id。
  README:106 刚说过「every dimension … is referenced by uid, never by the internal BIGSERIAL id
  (api-contract.md §3: uid is the only identifier exposed anywhere, including this Go API)」。
  上一轮这个 example 的「用 `svc.DBTX()` 跑 DDL」已经修好了（`:77-80` 改用 `pool.Exec` 并写了理由），
  这一处没扫到。

### [Minor] `readme_api_surface_test.go` 的覆盖面比它的 doc comment 宣称的窄

- **位置**：`readme_api_surface_test.go:100`（`strings.Contains(doc, "`svc."+fn.Name.Name+"(")`）
- **判定**：CONFIRMED
- 两个缺口：
  1. 它匹配的是**整份 README 任意位置**出现过 `` `svc.X(` ``，不是「在 API Surface 表里」。
     我逐个核过 45 个方法当前**确实**全部落在 §API Surface（README:580+）内 —— 现状是好的，
     但这是巧合不是门禁保证：把某个方法从表里删掉、只留在 Quick Start 的散文里，测试照样绿。
  2. 它只看 `*Service` 的方法。**包级导出面完全不设防**：`ledger.New` / `ledger.Migrate` /
     `ledger.NewIdempotencyKey` / `WithLogger` / `WithMetrics` / `WithAttestor` 从 README 里
     消失也不会红。`WithAttestor` 是整个 P5 签名体系的唯一入口。

### [Minor] `PendingStore.ExpirePendingOlderThan` 用 `%v` 聚合子错误，整条路径的 `errors.Is` 失效

- **位置**：`postgres/pending_store.go:466`（`fmt.Errorf("pending: expire: %d errors: %v", len(errs), errs)`）
- **判定**：CONFIRMED（读代码）
- 里层用 `%w` 认真包了（`:431,436,456`），最外层用 `%v` 把链断掉。消费方拿到这个 error 之后
  `core.IsRetryable` 只能走 default（→ `true`），`errors.Is(err, core.ErrInsufficientBalance)`
  之类一律为假。这是 `svc.PendingTimeoutSweeper()` 唯一的返回面。

### [Minor] 保留下来的 clone 上，`Ping()` 与 `Pool()` 仍然报健康

- **位置**：`ledger.go:924-935`（`Ping` 用 `s.pool`）、`ledger.go:191`（`Pool`）；
  `RunInTx` 的告诫 `ledger.go:445-447`
- **判定**：CONFIRMED（实跑）
- 实测把 clone 存出回调外：

  ```
  K. escaped clone PostJournal = ...(先撞 Validate)
  L. escaped clone Ping        = <nil>   <-- 报健康
  M. escaped clone GetBalance  = 0 err=postgres: dims: list currencies: tx is closed
  ```

  数据面（M）失效得很干净、报错很清楚 —— 这是好的。但 `Ping` 走 `s.pool`，所以一个把 clone 误存进
  某个 health-check 结构里的消费方会拿到「连接正常」，而它的每一次实际读写都会 `tx is closed`。
  `RunInTx` 的 godoc 说的是「do not store it or use it after fn returns」，没说「存了之后有一部分
  方法会继续报成功」。与 M5 同源：`Ping` 也在「`s.tx != nil` 时仍然触碰 `s.pool`」那个集合里，
  且未在 godoc 声明。

### [Minor] `RegisterChannel` 的 godoc 说「Call before starting the HTTP server」，实际约束更早

- **位置**：`ledger.go:675`；调用点 `examples/fullstack/backend/main.go:231`（`svc.Channels()` 作为
  `NewWithConfig` 的第 11 个位置参数）
- **判定**：CONFIRMED（读代码）
- `server` 拿到的是 `svc.Channels()` 返回的**快照 map**（`ledger.go:697-705` 明确复制）。所以真实约束是
  「必须在 `svc.Channels()` 被求值之前」，不是「在 server 启动之前」。在 `NewWithConfig` 之后、
  `ListenAndServe` 之前调 `RegisterChannel`，godoc 字面上允许，实际无效。

### [Minor] 16 / 26 个 postgres store 不做错误归一，`core.IsRetryable` 的「单一事实源」在这些路径上靠 default 兜底

- **位置**：`postgres/errors.go:19-70`（`normalizeStoreError`）；引用它的只有
  `ledger_store.go` / `reserver_store.go` / `booking_store.go` / `classification_store.go` /
  `template_store.go` / `currency_store.go` / `account_policy_store.go` / `period_close_store.go` /
  `attestation_store.go` / `deposit_address_store.go`；未引用的 16 个 `*_store.go` 含
  `pending_store.go` / `reversal_fraction_store.go` / `checkpoint_integrity_store.go` /
  `verified_balance_store.go` / `holder_store.go`
- **判定**：PLAUSIBLE（覆盖率是 CONFIRMED 的机械事实；「哪一个具体的永久性错误会被误判为可重试」我没走完）
- `core.IsRetryable` 的 default 是 `true`，所以未归一路径上的**永久性** pg 错误（唯一约束、check 约束、
  数值溢出）会被分类成「可重试」，消费方的退避循环会一直撞。我确认了 pending 的重复幂等键那条
  **不受影响**（它经 `LedgerStore.PostJournal` 落地，那条路归一了）。剩下哪些真正逃逸，我没逐个 store 走完 ——
  见「我没能验证的」。

---

## 上轮修复复核

按契约要求，对落在本 territory 的上轮修复问三件事：接到真实路径了吗 / 拆掉 pin 会不会红 / 有没有在别处复制同形缺陷。

| 上轮发现 | 修复 | 复核结论 |
|---|---|---|
| **C5** `examples/billing` 一分钱不收 | `6eecc7c` | ✅ **真修好了**。实跑输出 `final balance: 84.25 USDT (expected 84.25)`，且 `main.go:181-185` 在不等时 `return error`（不是只打印）。Settle + `fee_charge` 进了同一个 `RunInTx`（`:144-160`）。同形缺陷已一并扫到 `credits-topup`（下一行）。 |
| **M12** `examples/credits-topup` 实扣在事务外（与 C5 同形） | — | ✅ **已修**。`main.go:181-198` 把 Settle 与 `credits_spend` 塞进一个 `RunInTx`，`:200-207` 断言余额 188 并在不等时报错。实跑通过。 |
| **M1** `Worker.Subscribe` 文档承诺 at-most-once | — | ⚠️ **半修**。godoc（`worker.go:176-186`）与 `local.go:19-24` 都改成正确的 at-least-once + 「handler 必须幂等」。**但 `examples/event-subscribe/main.go:95-97` 仍是旧的错误说法**，而那是 README 唯一指过去的示范面。见上方 Major。 |
| **M2** `svc.Worker` 静默关掉三个 job | — | ⚠️ **半修**。attestation 已自动接线（`ledger.go:855-857`）；webhook 投递与 full reconcile 仍需手动 Set*，但现在 README:171-178 讲清楚了、`Run` 会打启动报告、fullstack example 显式补齐。**残留：那份启动报告走 `s.logger`，默认 `NopLogger` 吞掉。** 见上方 Major #1。 |
| **M3** `Worker.SetAttestor` 零生产调用点 | — | ✅ **真接到真实路径**。`ledger.go:855-857` 在 `s.attestor != nil` 时自动 `SetAttestor`。I-40 的 pin `TestServiceWorker_AttestsAutomaticallyWhenAttestorConfigured` 走的是 `ledger.New` + `svc.Worker(cfg).Run` 的**消费方路径**并直接轮询 `ledger_attestations` 表 —— 删掉 `ledger.go` 那三行就会红，不是在测试里自己接线。这一条是本轮复核里最扎实的。⚠️ 附带缺陷：这条自动接线在 `RunInTx` clone 上也生效，绕过了 `AttestationService` 的 clone 闸（见 Major #5）。 |
| **M4** `mergeWorkerConfig` 漏 Attest 两项 | — | ✅ **已修**。`ledger.go:909-914` 补上了 `AttestInterval` / `AttestBatchSize`。⚠️ 但 `mergeWorkerConfig` 仍是**手写的逐字段 if 列表**（16 个字段），加新 config 字段依然没有任何机制强制同步 —— 上一轮那条「为什么现有机制没拦住」的根因没变，只是当前实例被修掉了。 |
| **M5** `RunInTx` 落的 journal 永久 `unsigned_tx_mode` | — | ✅ **文档面已修**：`RunInTx` 的 caveat 列表（`ledger.go:463-476`）现在完整写了这件事并指向 `Authorize` + `PostAuthorized`；README:481-492 也补齐；`docs/COOKBOOK.md` Recipe 4 的措辞改了。⚠️ **但没有任何 example 演示 `Authorize` + `PostAuthorized`**（`grep -rn 'PostAuthorized' examples/` = 0）。同时 `examples/credits-topup:189` 与 COOKBOOK Recipe 4 仍然在 `RunInTx` 里直接 `ExecuteTemplate` —— 即被推荐的写法仍然会毒化那个维度的 `VerifiedBalance`，只是现在 godoc 里承认了。 |
| **M6** 嵌套 `RunInTx` 静默另开事务 | — | ✅ **真修好了**。`ledger.go:488-490` 直接返回 error，godoc（`:459-462`）也写明。 |
| **M7** README 所有 Go 代码块编译不过 | — | ✅ **真修好了，我实测的**：把 README 的 20 个 Go 代码块逐字抠进一个临时 module（go.work 指向 `fd87cae` 干净 worktree）编译，**零错误**；并用「插入一个不存在的方法调用 → 报错 → 移除 → 通过」验证了这个编译检查本身不是空转。⚠️ 但「编译得过 ≠ 跑得通」：README:419-431 编译通过、运行必失败（见 Major #2）。 |
| **M8** README:38 幂等键 blanket 承诺与实现不符 | W1-A/W15-A | ✅ **已修**，方向是把实现补齐而不是弱化承诺：`Settle`/`Release`/`FinalizeSettlement`/`Transition` 全部要求幂等键，README:38 现在是真的。⚠️ 代价是 6 条破坏性变更没进 CHANGELOG（见 Major #6）。 |
| **M9** `ErrUnauthorizedJournal` 无 bizcode 映射 | W1-C | ✅ **已修**：`response.go:223-224` → 14010，`bizcode.Retryable(14010)` = false，用户文案也改成了「failed a security check … contact support」。⚠️ **同形缺陷在别处复制了一份**：`core.ErrUnknownAuthKey` 现在是新的漏网哨兵，且防它的 pin 是手抄表（见 Major #9）。 |
| **M10** 提款闸没有 HTTP 表面 | — | ✅ **已修**：`server/handler_reservations.go:26,108` 读 `require_verified_balance`，`docs/openapi.yaml:2143` 与 `web/.../schema.ts:3249` 都有了。⚠️ `docs/api.md` / `docs/COOKBOOK.md` 仍 0 命中（Minor，移交 H）。 |
| **M11** `examples/crypto-deposit` 手搓 ingest 绕过 ceiling + 内存地址表 | — | ✅ **真修好了**。实跑输出：`holder 1001 deposit address: 0x8eaA... (persisted in postgres, survives a restart)` / `over-ceiling deposit: ... status=review journal_uid="" (parked for human review, NOT auto-credited)`。改用了 `EnableOnchain` + `IngestDeposit`，过期的头注释也删了。 |
| **M13** README API Surface 缺 11 个方法 | `3f7b77f` | ✅ **已修且加了机械门禁**。我独立核过：`ledger.go` 的 **45 个**导出 `*Service` 方法，**全部 45 个**都在 README:580+ 的 API Surface 段里有 `` `svc.X(` ``。门禁本身有两个覆盖缺口（见 Minor）。 |
| **M14** 库消费方没有「瞬时可重试」分类 | W1-C + bus #24 | ⚠️ **半修**。`core.IsRetryable` + `core.ErrTransient` 落地，`postgres/errors.go:50-63` 真的把 SQLSTATE 40001/40P01 包成了 `ErrTransient`（不是只有哨兵没有包装点）。**残留两条**：归一只覆盖 10/26 个 store（Minor）；重试契约的另一半（复用同一个 key）在库模式文档里仍然缺席（Major #8）。 |
| **m1** `ensureCurrency` 把任意错误当 not found | — | ✅ **基本修好**：5 个 example 的 `ensureCurrency` 全部加了 `Exponent` 校验并在不匹配时报错；`ensureJournalType`/`ensureClassification` 全部改用 `errors.Is(core.ErrNotFound)`。⚠️ 剩 `credits-topup:258,287` 两个模板 helper（Minor）。 |
| **m2** `PendingStore` 自相矛盾的注释 | — | ✅ **已修**（`pending_store.go:33-37` 与 `:50-56` 现在一致）。⚠️ README 仍 0 提 `InstallPendingBundle`（Minor）。 |
| **m3** `server.NewWithConfig` 23 参数 + panic | D-contract | ⚠️ **修了但没人用**：`NewFromDeps` 存在且正确，全仓零引用（见 Major #10）。 |
| **m4** `core.Metrics` 30 方法 / `nopMetrics` 未导出 | — | ⚠️ **半修**：`core.NoopMetrics` 已导出为可嵌入的 struct，godoc 给了 embed 示例。README:559 的「~20 methods」没改（实际 32）。 |
| **m5** preset bundle 数量四处三个数 | — | ✅ README 已统一到 8。⚠️ `ledger.go:638-641` 的 godoc 还是 7（Minor）。 |
| **m6** 校验错误引用不存在的字段名 | W1-A | ✅ **已修**：`core/reserve.go:127` 现在是 `reservation_uid required`；`core/booking.go` 同理。 |
| **m7** `examples/tx-compose` 不验证回滚 | — | ✅ **真修好了**：`main.go:174-186` 真的 `SELECT count(*) FROM journals WHERE idempotency_key = $1` 并在非 0 时 `return error`。实跑输出 `verified: journal was rolled back — no row with that idempotency key exists`。 |
| **m8** facade 若干方法在 clone 上逃出事务（上轮 PLAUSIBLE） | — | ⚠️ **部分修，且我把 PLAUSIBLE 做成了 CONFIRMED**：`AttestationService`/`VerifyLedger`/`EnableOnchain` 三个装了闸（I-40）。**新验证出三个同族缺口**：`RegisterChannel`（Major #3）、`Onchain()`（Major #4）、`Worker()`（Major #5），外加 `Ping()`（Minor）。 |
| **m9** `svc.Worker()` 改写共享 eventStore（上轮 PLAUSIBLE） | — | ✅ **已修**：`ledger.go:835` 每次 `Worker()` 建一个独立的 `EventStore`，注释把 race 讲清楚了，并有 `-race` pin（I-40 第 5 条）。 |
| **m10** fullstack 重复实现 URL scheme 转换 | — | ✅ **已修**：`examples/fullstack/backend/main.go:73-79` 直接把 `dbURL` 交给 `ledger.Migrate` 并注明理由。 |
| **m11** examples 两处风格问题（`_ = confirmedEvent` / DBTX 跑 DDL） | — | ✅ **都修了**：`grep -rn '^\s*_ = [a-z]' examples/*/main.go` = 0；`tx-compose:77-80` 改用 `pool.Exec` 并写明「schema setup is not a ledger write」。 |

**一句话总结这张表**：上一轮 1 Critical + 14 Major + 11 Minor 共 26 条里，**15 条彻底修好
（含 3 条加了真实路径 pin —— `SetAttestor` 自动接线、`Worker()` 的 -race pin、`tx-compose` 的
回滚查询都是真会红的），9 条半修（修复只落在被点名的那一处，或走了一条默认静默的通道），
2 条修了但没人用**。半修与「没人用」那 11 条里，有 6 条重新升级成了本轮的 Major
（#1 #5 #6 #7 #9 #10）—— 这是本轮最值得记的一件事：**上一轮的教训「手工补表就是会漏」被
成功应用到了 README 的 API 表（改成机械派生，我实测 45/45 全覆盖），却没有被应用到
哨兵映射表、CHANGELOG 破坏性清单、以及「同一命题在 godoc / example / README 三处是否一致」。**

---

## 移交

- **territory H（结构与契约）**：`docs/api.md:884` 说 `/system/ready` 的 503 body 是
  `{"status": "starting"}`，实际是统一信封 `{"code":18101,...}`（`handler_system.go:48`）；
  `docs/api.md` 里两处 `"next_cursor": ""` 示例（D-contract 已改成字面 `null`）。
  两条都是 CHANGELOG 专门提醒消费方改代码的变更，长文档没跟上。另：README:782
  「56 paths, 59 schemas」实际 59 / 97。
- **territory C（tamper-evident）**：`svc.Worker` 的自动 `SetAttestor` **永远**传 nil anchor
  （`ledger.go:856`），所以库模式默认是「链在走、每批都签、但没有外部见证」；提示这件事的
  Warn（`worker.go:262-266`）走默认 `NopLogger` 被丢弃。「这个降级形态是否可接受」归 C 判。
  另：`CHANGELOG.md:20-21` 的「已知敞开项」说「外部锚定没有生产实现」，但 `anchors/r2` 已在
  `29b0c00`（v0.6.0 之后）落地且 README:815 把它列为生产载体 —— 同一 commit 上两份文档互相矛盾，
  且 CHANGELOG 没有 `[Unreleased]` 段记录 r2（`CHANGELOG.md:636` 只有一个链接引用，且 compare
  基准还停在 `v0.2.0`）。`docs/plans/2026-08-27-release-readiness.md:70-72` 同样过期。
- **territory F（测试可信度）**：`pkg/httpx/response_test.go:127-158` 的哨兵清单是手抄的
  15 元素 slice，漏 `core.ErrUnknownAuthKey`（本报告 Major #9）—— 建议改成从 `core/errors.go`
  的 `var` 块机械派生。另：`Run` 的启动报告（`worker.go:254-261`）没有任何测试断言它在**默认
  logger 下**可见，`Subscribe`-after-`Run` 的 Error 同理。
- **territory B（并发）**：`Worker.Subscribe`（`worker.go:198-206`）对 `w.localDeliverer` 是无同步写，
  `Run`（`worker.go:399`）对同一字段是读；`running` 是 atomic 但被保护的字段不是。文档要求
  「Subscribe 必须在 Run 之前」，但违反时是数据竞争而不只是「订阅不生效」。
- **territory G（链上）**：`examples/crypto-deposit` 已改用 `EnableOnchain`/`IngestDeposit` 并演示了
  ceiling 拦截（实跑确认），消费方表面这一侧我认为已闭合；ceiling 逻辑本身归 G。
- **territory I（可运维）**：本报告 Major #1（默认 `NopLogger` 吞掉所有 worker 可观测信号）
  在可运维维度可能更重，请 I 从「运维能不能知道 job 没跑」的角度再判一次定级。

---

## ⚠️ 工作区异常（需 team lead 处置，非审计发现）

审计过程中 `git status --porcelain` 出现：

```
 M core/onchain.go
?? docs/plans/2026-09-02-deep-audit-contract.md
```

`git diff core/onchain.go`：

```diff
 func (c TokenConfig) AutoCreditCeilingConfigured() bool {
-	return c.AutoCreditCeiling.IsPositive() || c.AutoCreditCeiling.Equal(UnboundedAutoCredit)
+	return true
 }
```

这是一次「拆掉再看」的证伪实验（`AutoCreditCeiling` fence 属 **territory G**），跑在**共享的主工作区**里，
未按本轮契约 §2 要求「跑完立即 `git checkout -- .` 恢复」。后果：

- `go test ./core/...` 在主工作区是**红的**（`TestTokenConfig_AutoCreditCeilingConfigured` 2/4 子测试失败）。
  同一条命令在 `fd87cae` 的干净 worktree 上是绿的 —— 我实测过，所以**基线本身没问题**。
- 任何在此期间读主工作区的 agent 都会看到一个被改过的 `core/onchain.go`。

**我没有动它**（避免打断 G 可能仍在进行的实验），也没有把它当成审计发现。
**收尾时复查：该改动已被恢复** —— 交付本报告时 `git status --porcelain` 只剩两个 untracked 项
（本审计目录 + 本轮契约文档），`go test ./core/...` 在主工作区已恢复绿。所以这条是**一次已闭合的
流程偏离**，记录下来是因为它在一段时间内让主工作区的 `go test ./core/...` 是红的，
任何在那个窗口里读主工作区的 agent 都会得到错误的基线判断。**处置建议：本轮契约 §2 的
「跑完立即恢复」应改成「在自己的 worktree 里跑」** —— 主工作区是十个 agent 共享的读取面。

**我自己的写盘足迹**：
- scratchpad 里四个临时 Go module，已全部重新指向 `/Users/aaron/projects/ledger`，可直接重跑：
  `readmecheck/`（README 20 个 Go 代码块编译）、`readmerun/`（README 分类示例运行时）、
  `clonecheck/`（RunInTx clone 逃逸探针 A–M / N–O）、`workerprobe/`（默认 logger 静默）。
  跑法：`cd <dir> && DATABASE_URL=postgres://postgres:pw@localhost:5443/<新库> go run .`
- 跑 example / 探针用的 postgres 容器 `auditE-pg`（端口 5443），**交付前已 `docker rm -f` 清掉**。
  要复核请重建（superuser 是 `ledger.Migrate` 首次跑的前提，见 README:72-79）：
  `docker run -d --name auditE-pg -e POSTGRES_PASSWORD=pw -p 5443:5432 postgres:17.2-alpine`，
  然后每个探针用一个新建的空库。
- 审计中途建过一个指向 `fd87cae` 的 detached worktree 用于在干净树上复验，**已
  `git worktree remove --force` + `git worktree prune` 清理**，`git worktree list` 现在只剩主工作区。
- 未修改仓库内任何文件，本报告除外。

---

## 我没能验证的

- **Minor「16/26 store 不做错误归一」我没走完。** 覆盖率是机械事实，但「哪一个具体的永久性 pg 错误
  在哪条未归一路径上会被 `core.IsRetryable` 误判成可重试」我只排除了 pending 的重复幂等键
  （那条经 `LedgerStore.PostJournal` 落地、归一了）。要坐实需要逐个 store 找出它直接暴露 pgx error
  的写路径。定级 PLAUSIBLE 就是因为这个。
- **`examples/fullstack` 的前端半边（`examples/fullstack/web`）我没跑。** 后端我起来了、探过
  `/`、`/api/v1/system/health`（返回正常信封）并确认了 worker 无输出，但没起 Next.js、没验证
  `@azex/ledger-react` 能不能对着它渲染。归 **territory J**，但「fullstack 这个 example 是否
  端到端可用」严格说是我的。
- **README:50-62「Local Development with go.work」的第 2 点是 PLAUSIBLE。** 我实测了
  `go 1.26.1` 会被拒（CONFIRMED），但「按 README 的 go.work 只列两个 module 会在碰到
  `chains/evm` / `anchors/r2` 时失败」没实跑 —— 因为要跑就得 `rm` 仓库里的 `go.work`，违反 §1.1。
- **`docs/frontend.md`(766 行) 我只做了 grep 级扫读。** 上一轮的报告也承认漏了这块。契约把
  `web/` 划给 territory J，但 `frontend.md` 作为「消费方文档」严格说横跨两边。
- **`Worker.Subscribe` 的数据竞争我没上 `-race` 实测**（已移交 B）。
- **`core.ErrUnknownAuthKey` 我没构造端到端 HTTP 请求。** 三处代码（`IsRetryable` 的 switch、
  `resolveError` 的 switch、`Retryable` 的区间）我逐行核过，结论是算术上的；
  但「一个真实部署里这个裸哨兵会不会走到 `pkg/httpx`」取决于消费方自己的 AuthVerifier 实现，
  我只能论证可达性、不能论证必然性。
- **`server/` 的 59 个端点我只看了 reservations 与 system 两个。** 上一轮报告结尾也留了这句 ——
  「其余端点的 DTO 是否遗漏了 core input 上的字段」这类同形问题我仍然没有系统核过（现在有
  `server/openapi_contract_test.go` 做双向反射比对，覆盖面比上一轮好，但那个门禁比的是
  「Go wire struct ↔ openapi」，比不了「core input 上有、DTO 上没有」—— 也就是
  `RequireVerifiedBalance` 当初那个形状）。
