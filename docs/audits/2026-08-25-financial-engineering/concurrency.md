# 审计报告 — concurrency

## 摘要

**并发本身守得住，出问题的地方是「事务边界」和「幂等边界」。** Reserve 的 TOCTOU
是真的关上的（idempotency lock → balance lock → 锁内读余额 → 插入，全程在一个 tx
里），PostJournal 与 Reserve 共用同一把 `balance:<holder>:<currency>` advisory
lock，checkpoint 的 watermark 推进在并发下也是正确的（同一 (holder,currency) 的
journal 被 advisory lock 完全串行化，所以 entry id 顺序 == 提交顺序，不存在
序列空洞导致漏计的经典 bug）。

但有三类真实缺陷：

1. **一次性 `Settle` 根本不记账。** 它只翻 reservation 的状态，不发 journal ——
   而 `examples/billing`（这个库的事实文档）明确写着「Settle debits the actual
   amount」并打印「expected 84.25」。照这个例子实现计费的消费方会**释放冻结、
   一分不扣**。没有任何测试断言 Settle 之后的余额。
2. **幂等只覆盖了「创建」类写入口，没覆盖「状态机推进」类。** `Settle` /
   `Release` / `FinalizeSettlement` / `Booker.Transition` 完全没有
   idempotency_key，重放一律返回 `ErrInvalidTransition` —— 与 `docs/INVARIANTS.md`
   I-3 声称的「**每一个**状态变更操作都要求 idempotency_key，同 key 同 payload
   重放返回原结果」直接矛盾。
3. **三处 claim/lease 实现不一致。** `rollup_queue` 与 `events` 都有 claim-token
   guard 并写了注释解释为什么必须有；`registration_rescans` **一条都没有** ——
   过期 worker 能把游标改回去、能把已完成的 job 改回 pending、能抹掉活 worker 的 claim。

4. **锁键空间与锁顺序两处都能被调用方输入破坏**（都不需要恶意）：
   idempotency_key 与 balance 锁**共用一个 advisory lock 键空间**，
   传 `idempotency_key = "balance:1:1"` 就能构造 ABBA 死锁；
   `ExecuteTemplateBatch` 的锁排序只做在**单个 journal 内**，
   两个 holder 顺序相反的批次直接互锁。两处都是 40P01 而不是算错钱，
   但打在 money-path 上。

另外：事务内禁止外部调用这条红线在 `PostJournal` / `Authorize` 上是**真的守住了**
（签名严格在 `pool.Begin` 之前，且 `Authorize` 在 tx-bound store 上直接报错），
但 `Reserve` 的 `RequireVerifiedBalance` 闸抄了「放在事务前」的**位置**、没抄
那道**守卫**，在 tx 模式下会在调用方已开的事务里发起可能远程的 AuthVerifier 调用。

---

## 发现

### [Critical] 一次性 `Settle` 不发 journal：冻结被释放，钱一分没扣，而 canonical example 声称扣了

- **位置**：
  - `postgres/reserver_store.go:409-415`（`UpdateReservationSettle(..., JournalID: pgtype.Int8{}` + 注释 "no journal linked by the one-shot settle"）
  - `postgres/sql/queries/reservations.sql:21-22`（`UPDATE reservations SET status='settled', settled_amount=$2 ...` —— 全部动作）
  - `examples/billing/main.go:117-124`（注释：「Settle debits the actual amount and automatically releases the $4.25 remainder. Both operations happen atomically inside the adapter.」）
  - `examples/billing/main.go:140`（`fmt.Printf("final balance: %s USDT (expected 84.25)\n", balance)`）
- **判定**：CONFIRMED
- **失效场景**：
  1. 用户余额 100.00 USDT。`Reserve(20.00)` → `SumActiveReservations = 20`，
     available = 100 − 20 = 80。
  2. 算力跑完，实际成本 15.75。`Settle(uid, 15.75)` → reservation.status = `settled`，
     `settled_amount = 15.75`。
  3. `SumActiveReservations` 只统计 `status IN ('active','settling')`
     （`postgres/sql/queries/reservations.sql:78`），所以冻结**整笔消失**，
     available 回到 **100.00**。
  4. `journal_entries` 里**一条都没有**。用户被计费 15.75，账本上余额仍是 100.00。
     例子自己打印的「expected 84.25」永远不会出现 —— 实跑打印的是 100。
  5. 用户可以立刻再 `Reserve(100)`，无限白用。
- **最小复现**：`go run ./examples/billing`，看最后一行打印的数字。
- **为什么现有机制没拦住**：
  - `postgres/reserver_store_test.go:39-42` —— `TestReserverStore_Reserve_Settle`
    的最后一行是 `require.NoError(t, err)`，**没有任何余额断言**。把
    `settleWithQueries` 整个删空成 `return nil`，这个测试依然绿。
  - `examples/` 不进 CI，printf 不是断言，所以「expected 84.25 / 实际 100」这个
    矛盾没有任何门禁会发现。
  - `docs/INVARIANTS.md` I-4 只说「Settle and Release transition the same row
    under its own row lock」—— 措辞上其实是对的（它只 transition），但
    example 的措辞是错的，而 example 才是消费方会照抄的东西。
- **注**：库的设计意图很可能是「记账由调用方编排（`RunInTx` 里 Settle + PostJournal）」
  —— `CLAUDE.md` 的 Event-Journal atomicity 段就是这么写的。问题不是设计，
  是**唯一一份演示 Reserve/Settle 的文档演示了错的用法**，而且没有任何机制
  强制/提示调用方必须自己补那笔 journal。移交 C 一并看「消费方表面」。

### [Major] `Settle` / `Release` / `FinalizeSettlement` / `Transition` 没有幂等 key，重放返回 `ErrInvalidTransition`

- **位置**：
  - `core/reserve.go:103-107`（`SettleInput` = {ReservationUID, Amount}，无 IdempotencyKey）
  - `core/reserve.go:19-22`（`reservationTransitions`：`settled` / `released` 没有出边）
  - `postgres/reserver_store.go:383-386`（`if !status.CanTransitionTo(settled) { return ErrInvalidTransition }`）
  - `postgres/reserver_store.go:697-700`（Release 同）
  - `postgres/reserver_store.go:668-671`（FinalizeSettlement 同）
  - `core/booking.go:65-80`（`TransitionInput` 无 IdempotencyKey）
  - `docs/INVARIANTS.md:100-105`（I-3：「Every state-changing operation requires an `idempotency_key`. Replaying the same key with the same payload returns the original result」）
- **判定**：CONFIRMED
- **失效场景**：
  1. 调用方 `Settle(uid, 15.75)`，DB 已提交，但响应在网络上丢了。
  2. 调用方重试同一个 `Settle(uid, 15.75)`。行锁取到，`status == settled`，
     `reservationTransitions[settled]` 为空 → `CanTransitionTo(settled) == false`
     → `ErrInvalidTransition`。
  3. 调用方**无法区分**三种情况：(a) 我自己刚才那次成功了；(b) 别人抢先结算了；
     (c) 这笔已经被 expiration worker 释放了（`released` 也走同一个
     `ErrInvalidTransition`）。
  4. 典型 retry 逻辑「Settle 成功后再 PostJournal」在这里会停在第 2 步，
     **journal 永远不会发** —— 叠加上一条 Critical，就是钱既没扣、冻结也没了。
- **最小复现**：`postgres/reserver_store_test.go:169-195`
  `TestReserverStore_Settle_InvalidTransition` **已经把这个行为固化成期望**了
  —— 它断言第二次 Settle 必须 `ErrorIs(ErrInvalidTransition)`。这不是缺测试，
  是**测试把与 I-3 矛盾的行为钉住了**。
- **为什么现有机制没拦住**：I-3 的 `Enforced by` 只列了 journals / reservations /
  bookings 三张表的 `UNIQUE(idempotency_key)` —— 那三个都是**创建**路径。
  状态机推进路径压根没有 key 可以 UNIQUE，I-3 的「every」是空头承诺。
  `SettlePartial` 是唯一做对的（`reservation_settlement_legs` 持久化每次应用，
  `postgres/reserver_store.go:498-517`），恰好证明作者知道该怎么做 ——
  只在「累加器」上做了，没在「状态机」上做。
- 移交 E：I-3 的 34 条 invariant 门禁只校验「引用的测试存在」，不校验
  「测试能否证伪该条」。这一条是活样本。

### [Major] `registration_rescans` 是三处 claim 机制里唯一没有 claim-token guard 的

- **位置**：
  - `postgres/registration_rescan_store.go:85-102`（`AdvanceRegistrationRescan`：`WHERE uid = $1`，**没有** `AND claimed_until = <token>`）
  - `postgres/registration_rescan_store.go:104-117`（`RetryRegistrationRescan`：同样只按 uid，且无条件 `status='pending', claimed_until=NULL`）
  - 对照组：`postgres/sql/queries/checkpoints.sql:110-121`（`MarkRollupProcessed ... WHERE id=$1 AND claimed_until=$2`，注释明确写了「a stale worker can never mark a claim it no longer owns」）
  - 对照组：`postgres/sql/queries/events.sql:51-58`（`UpdateEventDelivered ... WHERE id=$1 AND next_attempt_at=$2`，注释：「Without this, a worker whose callback outlived its lease could overwrite the result written by the worker that re-claimed the event」）
  - 消费方：`service/onchain.go:556-563`（失败分支用**外层 ctx**调 `RetryRegistrationRescan`，不是 `jobCtx`）
- **判定**：CONFIRMED（机制缺失）／PLAUSIBLE（是否能造成漏扫）
- **失效场景**（多副本，`registrationRescanTimeout` 默认 10m）：
  1. Worker A 于 t0 claim job J（chain 1 / 0xabc / next_block=100），
     DB 置 `claimed_until = now()+10m`。
  2. A 的 `FetchDeposits` 卡住。t0+10m，DB 侧租约到期。
  3. Worker B claim 到 J（`claimed_until < now()` 命中），读到 next_block=100，
     扫 [100,199]，`Advance(J, 200, false)` → 行变成 pending / next_block=200。
     B 下一 tick 再 claim，扫到 300，甚至 `completed`。
  4. A 的 `jobCtx` 到期（`context.WithTimeout(ctx, timeout)`，起点比 DB 的
     `claimed_until` 晚一个 round-trip，所以 A 的 deadline **晚于** DB 租约）
     → `processRegistrationRescan` 返回 deadline exceeded → 走失败分支
     `RetryRegistrationRescan(ctx=外层, ...)`。**这个调用用的不是 jobCtx，
     所以它一定能写进去。**
  5. A 的 retry 无条件把 J 打回 `status='pending', claimed_until=NULL,
     available_at=now()+delay` —— **抹掉了 B 正在持有的活 claim**，
     并把一个已经 `completed` 的 job 重新变成 pending。
  6. 结果：同一 (chain, address) 可以被两个 worker 同时扫；游标能回退；
     已完成的历史扫描被无限重开。
- **最小复现**：把 `registrationRescanTimeout` 设成 1s，让 `Reader.FetchDeposits`
  sleep 3s，起两个 `runRegistrationRescansOnce`，观察 `registration_rescans`
  行的 `next_block` 回退与 `status` 从 `completed` 回到 `pending`。
- **为什么现有机制没拦住**：`postgres/registration_rescan_store_test.go` 存在，
  但 claim-token guard 这个机制**不存在**，所以没有测试可以覆盖它。
  rollup / events 两处都为这件事写了长注释，说明团队清楚这个 failure mode ——
  第三处只是漏了，属于 working-agreements §5「能被结构强制的不靠记忆」的反例。
- **不是 Critical 的理由**：`IngestDeposit` 的幂等 key 是
  `deposit-{chain_id}-{tx_hash}-{txlog_seq}`（`service/onchain.go:683-685`），
  重复摄入会被 `bookings.idempotency_key` 挡住，所以**重复不会重复入账**。
  我**没能验证**是否存在真正的「漏扫某段区块」路径 —— 所有 `Advance` 写入的
  `to+1` 都不超过写入者自己扫过的范围，游标只会回退不会前跳，所以看起来不漏。
  见「我没能验证的」。

### [Major] 调用方可控的 idempotency_key 与 balance 锁共用同一把 advisory lock 键空间 → 可构造 ABBA 死锁

- **位置**：
  - `postgres/sql/queries/journals.sql:146-152`（`AcquireBalanceLock`：`pg_advisory_xact_lock(hashtextextended($1::text, 0))`，key = `"balance:<holder>:<currency_id>"`）
  - `postgres/sql/queries/journals.sql:154-158`（`AcquireIdempotencyLock`：**同一个函数、同一个单参 bigint 键空间**，key = 调用方原样传入的 idempotency_key）
  - `postgres/ledger_store.go:132-140` / `142-147`（两个 helper）
  - `postgres/ledger_store.go:812`（先取 idempotency lock）→ `postgres/ledger_store.go:889`（再取 balance locks）
  - `server/handler_journals.go:39` + `:200`（`idempotency_key` 直接来自请求 body，**无格式校验**）
- **判定**：CONFIRMED
- **失效场景**：
  - Tx A：`POST /journals` with `idempotency_key = "balance:2:1"`，entries 打
    holder=1 / currency_id=1 →
    先锁 `h("balance:2:1")`，再锁 `h("balance:1:1")`。
  - Tx B：`POST /journals` with `idempotency_key = "balance:1:1"`，entries 打
    holder=2 / currency_id=1 →
    先锁 `h("balance:1:1")`，再锁 `h("balance:2:1")`。
  - 经典 ABBA。Postgres 在 `deadlock_timeout`（默认 1s）后检测并 abort 其中一个
    （SQLSTATE 40P01）。
  - 后果不是算错钱，是**任何调用方都能对 money-path 制造 1s 级延迟尖刺和随机
    40P01 失败**，而且被牵连的是**别人的 holder**（A 的死锁对手方是 holder 2 的
    正常记账）。
- **最小复现**：两个并发 `PostJournal`，idempotency_key 互相交叉引用对方的
  `balance:<holder>:<currency>` 字符串（holder/currency_id 是内部 id，
  可以从 `balance:1:1` 开始暴力枚举小整数）。
- **为什么现有机制没拦住**：
  - SQL 注释断言「**Collisions in the hash only reduce concurrency; they do not
    affect correctness**」（`journals.sql:149-150` 与 `:156-157` 两处都这么写）
    —— 这个断言对**降并发**成立，对**死锁**不成立：两个命名空间共用一个键空间时，
    「先 idem 后 balance」这个固定顺序会因为 key 的角色互换而被反转。
  - `postgres/reserver_store.go:212-215` 的注释说「the lock space (**two-arg
    int4 form**) stays consistent」—— 与 `journals.sql:151`（「**Single-arg
    bigint form** is used」）**直接矛盾**。读到前者的人会以为两个命名空间是分开的。
  - 无任何测试覆盖跨命名空间的锁顺序。
- **修法方向（不实施）**：balance 锁用双参形式 `pg_advisory_xact_lock(int4,int4)`
  或给 key 加不可被用户构造的前缀 + 拒绝以 `balance:` 开头的 idempotency_key。

### [Major] `Reserve(RequireVerifiedBalance: true)` 在 tx 模式下会在调用方已开的事务里发起外部调用

- **位置**：
  - `postgres/reserver_store.go:108-115`（`RequireVerifiedBalance` 闸，注释：「**Deliberately BEFORE any transaction is opened**, on the same placement rule Authorize follows -- an AuthVerifier implementation is permitted to be a remote call ... so financial.md's "no external calls inside a transaction" applies here exactly as it does to Attestor.Sign」）
  - `postgres/reserver_store.go:56-73`（`WithDB`：「verifiedBalance is intentionally **NOT re-bound to db**」）
  - `postgres/ledger_store.go:315-318`（对照组：`Authorize` 在 `s.pool == nil` 时**直接报错**，「Authorize must run before opening a transaction, not from inside RunInTx」）
  - `ledger.go:498`（`reserverStore: s.reserverStore.WithDB(tx, ls)` —— tx 模式经 `RunInTx` 对消费方**可达**）
- **判定**：CONFIRMED（外部调用发生在已开事务内 + 读在调用方快照之外）／PLAUSIBLE（连接池耗尽死锁）
- **失效场景**：
  ```go
  svc.RunInTx(ctx, func(tx *ledger.Service) error {
      // 事务已经开了
      _, err := tx.Reserver().Reserve(ctx, core.ReserveInput{
          ..., RequireVerifiedBalance: true,   // ← 闸在这里跑
      })
      ...
  })
  ```
  1. `Reserve` 走到 `requireVerifiedAvailableBalance`，它在 `s.pool == nil`
     分支**之前**，所以 tx 模式也会执行。
  2. `s.verifiedBalance` 仍绑在 **pool** 上（`WithDB` 故意不重绑），于是它
     **从连接池再取一条连接**，在调用方事务之外读 —— 读不到调用方自己未提交的写，
     语义上就已经和「锁内读余额」的前提不一致。
  3. `VerifiedBalance` 内部可能调 `core.AuthVerifier`，而该接口的 doc
     明确允许远程实现 → **在调用方已开的事务里做远程调用**，正是
     `financial.md` 的红线，也正是这段注释声称自己避免了的东西。
  4. 连接池大小 N 时，N 个并发 `RunInTx` 都走这条路 → N 条连接被事务持有，
     N 个 goroutine 都在等第 N+1 条 → 阻塞到 pgx acquire 超时 / ctx deadline。
- **最小复现**：pool `MaxConns=2`，起 2 个并发 `RunInTx`，各自在回调里
  `Reserve(RequireVerifiedBalance:true)`。
- **为什么现有机制没拦住**：`Authorize` 和 `AuthorizeReversal` 都加了
  `s.pool == nil → ErrInvalidInput` 的守卫（`postgres/ledger_store.go:317`、
  `postgres/reversal_fraction_store.go:401`），`Reserve` 的这条闸**照抄了位置、
  没照抄守卫**。没有测试断言 tx 模式下这个闸的行为。
- **这条直接回答 team lead 提醒 #2**：设计稿声称的「uid-space digest 在事务外算」
  在 `PostJournal` / `Authorize` / `PostAuthorized` / `ExecuteTemplateBatch` /
  `postDepositConfirmedJournal` 上**是真的做到了**（我逐条走了调用链：签名严格在
  `pool.Begin` 之前，tx 模式一律标 `unsigned_tx_mode` 而不偷偷签）。**没做到的
  是 Reserve 的这条闸** —— 它不是「挪到另一个事务」，是压根没挪。

### [Major] `ConfirmPending` / `CancelPending` 的 journal 在配置了 Attestor 的 pool 模式下也永远不签名

- **位置**：
  - `postgres/pending_store.go:268`（pool 模式：`run(s.q.WithTx(tx), s.ledger.WithDB(tx))`）
  - `postgres/pending_store.go:254`（`return ledger.PostJournal(ctx, input)` —— 这里的 `ledger` 已经是 tx-bound）
  - `postgres/ledger_store.go:412-428`（tx 模式分支：`journalAuth{status: core.AuthStatusUnsignedTxMode}`，「Signing is deliberately NOT attempted in this mode」）
  - `ledger.go:157-158`（`s.ledgerStore = s.ledgerStore.WithAuth(s.attestor)`）＋ `ledger.go:177`（`NewPendingStore(pool, s.ledgerStore, ...)`）
- **判定**：CONFIRMED
- **失效场景**：部署配了 `ledger.WithAttestor(...)`，消费方从**顶层 Service**
  调 `PendingBalanceWriter().ConfirmPending(...)`（pool 模式，不在任何 RunInTx 里）。
  `checkPendingBalanceAndPost` 自己 `pool.Begin`，然后把 ledger 降级成
  `WithDB(tx)` 去发 journal → `PostJournal` 命中 tx 模式分支 → 该 journal
  的 `auth_digest` / `auth_signature` 为空、`auth_status = 'unsigned_tx_mode'`。
  「把 pending 余额转成可用余额」这一整类 money-path journal **全部不在防篡改
  覆盖范围内**，而同一个 Service 上的 `PostJournal` / `ExecuteTemplate` 都是签的。
  消费方没有任何迹象可以看出这个差别。
- **为什么现有机制没拦住**：全仓只有 `pending_store.go:268` 这一处
  「pool 模式内部降级成 tx 模式」（我 grep 过 `ledger.WithDB(tx)`，唯一命中）。
  正确姿势就在隔壁：`service/onchain.go:930-957` 的
  `postDepositConfirmedJournal` 先 `AuthorizeTemplate`（事务外）再
  `RunInTx` + `PostAuthorized`。`PendingStore` 没有做这一步。
  没有测试断言「配了 Attestor 时 ConfirmPending 产出的 journal 是 signed」。
- 移交 D：防篡改覆盖率的完整清单该由 D 收口；我只报这一处成因（事务边界）。

### [Major] I-4 / I-11 的 TOCTOU pin 无法证伪它自己 —— 全仓没有一个并发超额预留测试

- **位置**：
  - `postgres/reserver_store_test.go:127-167`（`TestReserverStore_Reserve_Concurrent`：余额 **100**，并发预留 **50 + 30 = 80**）
  - `docs/INVARIANTS.md:166-169`（I-4 的 `Pinned by` 列了它）
  - `docs/INVARIANTS.md:347-354`（I-11 的 `Pinned by` 也列了它）
- **判定**：CONFIRMED
- **失效场景**：这个测试的两笔预留**加起来还没超过余额**，所以「两个并发 Reserve
  各自读到余额够、都插入、holder 被超额占用」这个 I-4 存在的唯一理由**从未被触发**。
  把 `reserveWithQueries` 里的 `acquireBalanceLocks`（`postgres/reserver_store.go:216`）
  整行删掉，这个测试依然全绿 —— 满足契约 §1.4 的「空洞性」判据。
  I-4 另外两个 pin（`core.TestReservationStatus_AllTransitions` /
  `..._TerminalStatesAreSticky`）是纯 map 表遍历，`core/reserve_stress_test.go`
  里连 goroutine 都没有（文件名叫 stress，内容不是）。
  I-11 剩下的 5 个 pin（`postgres/balance_breakdown_test.go`、
  `reserver_partial_test.go:85` 等）全是单线程顺序断言。
- **该有的测试**：余额 100，并发发起 N 笔各 60 的 Reserve，断言
  「恰好 1 笔成功、N−1 笔 `ErrInsufficientBalance`」且
  `SumActiveReservations ≤ 100`。这个形状在
  `postgres/invariants_test.go:79`（`TestIdempotency_ConcurrentSameKey`，
  100 goroutine + 余额终值断言）已经有现成模板，只是没用在 Reserve 上。
- **为什么现有机制没拦住**：INVARIANTS.md 的门禁只校验「引用的测试名存在」，
  不校验「该测试能否证伪该条」。移交 E。

### [Major] `svc.Worker(service.WorkerConfig{})` + `SetAttestor` → 防篡改链静默永不推进

- **位置**：
  - `ledger.go:742-786`（`mergeWorkerConfig`：逐个字段补默认值，**唯独没有 `AttestInterval` 和 `AttestBatchSize`** 的分支）
  - `ledger.go:711-712`（doc：「Any zero-valued field on cfg is filled in from service.DefaultWorkerConfig so callers get a **safe-by-default** Worker even when they pass a partially populated config or `service.WorkerConfig{}`」）
  - `service/worker.go:324-329`（`runLoop`：`interval <= 0` → `Warn` + `<-ctx.Done()`，**整个 job 不跑**）
  - `service/worker.go:67-68`（`DefaultWorkerConfig` 确实设了 60s / 1000，所以只有走 merge 这条路才中招）
  - `postgres/sql/queries/integrity_attestations.sql:47`（`LIMIT sqlc.arg(batch_size)::int`）
- **判定**：CONFIRMED
- **失效场景**：
  - **A（interval=0）**：消费方 `w := svc.Worker(service.WorkerConfig{});
    w.SetAttestor(attestSvc); go w.Run(ctx)` → `AttestInterval` 仍为 0 →
    `runLoop` 打一条 Warn 就去 `<-ctx.Done()` → **attestation job 一次都不跑**。
    一切看起来接好了（`w.attestation != nil`，goroutine 起来了），
    `integrity_attestations` 表永远空。
  - **B（interval 设了、batch 没设）**：`AttestBatchSize = 0` →
    `ListUncoveredEntries` 走 `LIMIT 0` → 每批覆盖 **0 条 entry** →
    `RunAttestBatch` 照样成功、seq 照样自增、签名照样发生，
    **但 `entry_attestations` 永远不长一行**。链在推进，覆盖率恒为 0。
    这一档比 A 更毒：它有产物、有日志、看起来在工作。
- **最小复现**：`svc.Worker(service.WorkerConfig{})` + `SetAttestor`，
  跑 5 分钟后 `SELECT count(*) FROM integrity_attestations;` = 0。
- **为什么现有机制没拦住**：`mergeWorkerConfig` 的形状是「每个字段一个 if」，
  加 `AttestInterval` 时漏了一个 if 就静默失效 —— 正是
  working-agreements §5 说的「靠人记忆的规则默认会被违反」。
  `RunAttestBatch(ctx, 0)` 也没有对 `batchSize <= 0` 的入参校验
  （`service/attestation.go:120-141`）。
  与 §3「未运行 ≠ 通过」直接冲突：这里「未运行」= 一条 Warn 日志。

### [Major] `expiration` job 无 leader 选举、`GetExpiredReservations` 无 claim；且过期能在 in-flight Settle 下抽走冻结

- **位置**：
  - `service/worker.go:204-213`（`expiration` 是**裸 `runLoop`**，不是 `NewLockedJob` —— 对照 reconcile / system_rollup / partition / attestation 四个都包了 `LockedJob`）
  - `postgres/sql/queries/reservations.sql:49-59`（`GetExpiredReservations`：`SELECT ... WHERE status IN ('active','settling') AND expires_at < now() ORDER BY expires_at LIMIT $1` —— **没有 `FOR UPDATE SKIP LOCKED`，没有 claim 列**）
  - `postgres/sql/queries/bookings.sql:39-47`（`ListExpiredBookings` 同）
  - `postgres/reserver_store.go:303`（`reservationDefaultExpiresIn = 15 * time.Minute`）
  - `service/worker.go:57`（`ExpirationInterval: 30s`）
- **判定**：CONFIRMED
- **失效场景**：
  - **多副本空转**：K 个副本每 30s 各自 `SELECT` 到**同一批**过期 reservation，
    各自调 `Release(uid)`。行锁串行化，第一个成功，其余 K−1 个拿到
    `ErrInvalidTransition` → `service/expiration.go` 记 error 日志。
    不丢钱，但错误日志会被真实故障淹没（噪声掩盖信号）。
  - **抽走 in-flight 的冻结**：`ExpiresIn` 默认 15 分钟。一个跑了 16 分钟的
    操作在完成时调 `Settle` → 此时 expiration worker 已 `Release`，
    `reservationTransitions[released]` 为空 → `ErrInvalidTransition`。
    调用方**服务已经交付了**，但结算被拒 —— 而且拿到的错误和「我自己刚才
    结算成功了」（上一条 Major）**是同一个错误**，无法区分。
    这是「幂等缺失」与「过期回收」两个缺陷叠加出的一个不可归因失败。
- **为什么现有机制没拦住**：`service/expiration_test.go` 存在但是单实例的；
  「多副本下 expiration 的行为」没有任何断言。四个 job 包了 `LockedJob`、
  两个没包，这个不一致本身没有任何机制会发现（同 §5）。

### [Major] `ExecuteTemplateBatch` 的 balance lock 只在**单个 journal 内**排序，跨 journal 无全局序 → 调用方可控的 ABBA 死锁

- **位置**：
  - `postgres/ledger_store.go:551-558`（pool 模式：一个 tx 里 `for i, input := range inputs { postJournalWithQueries(...) }`）
  - `postgres/ledger_store.go:889`（每个 journal 各自 `acquireBalanceLocks(balancePairsFromEntries(resolved))`）
  - `postgres/ledger_store.go:110-127`（`balancePairsFromEntries` 的注释：「Sorted order is required to take advisory locks in the same global order across concurrent transactions, otherwise deadlocks become possible (tx A locks pair P1 then P2 while tx B locks P2 then P1)」—— 排序**只覆盖单个 journal 的 entries**）
  - `ledger.go:237-238` + `core/interfaces.go:91-92`（`ExecuteTemplateBatch(requests []TemplateExecutionRequest)` 是导出 API，**slice 顺序由调用方给**）
- **判定**：CONFIRMED
- **失效场景**：
  - Tx A：`ExecuteTemplateBatch([]{ req(holder=1), req(holder=2) })` →
    锁序 = idem(A0), balance(1,cur), idem(A1), balance(2,cur)
  - Tx B：`ExecuteTemplateBatch([]{ req(holder=2), req(holder=1) })` →
    锁序 = idem(B0), balance(2,cur), idem(B1), balance(1,cur)
  - A 持 balance(1) 等 balance(2)，B 持 balance(2) 等 balance(1) → ABBA。
    Postgres `deadlock_timeout` 后 abort 一方（40P01）。
  - 「原子批量执行模板」正是**批量结算/批量返佣**这类会同时打多个 holder 的场景，
    两个批次的 holder 顺序不同是完全正常的调用方行为 —— **不需要恶意构造**。
- **最小复现**：两个并发 `ExecuteTemplateBatch`，requests 里的 holder 顺序互为
  倒序，模板各自只打一个 holder。
- **为什么现有机制没拦住**：`balancePairsFromEntries` 的注释精确描述了这个
  failure mode，然后只在**单个 journal 的粒度**上解决了它。批量路径把 N 个
  journal 串进一个事务时，「全局锁序」这个前提就被打破了 ——
  正确做法是先把整个 batch 的所有 `balancePair` 收齐、去重、排序、一次性取完，
  再逐个 post。`postgres/ledger_store.go:577-593` 的 tx-mode 版本
  （`executeTemplateBatchWithQueries`）有完全相同的问题。
  没有任何测试并发跑两个顺序相反的 batch。

---

### [Minor] 三套互不相干的哈希共用一个 advisory lock 键空间，且注释自相矛盾

- **位置**：
  - `postgres/sql/queries/journals.sql:151` —— balance 锁：`hashtextextended(text, 0)`，单参 bigint 形式
  - `postgres/sql/queries/journals.sql:158` —— idempotency 锁：同上，同一键空间
  - `service/snapshot.go:25-26` —— `advisoryLockKey(name)` 用 **FNV-64a**，喂给
    `service/locked_job.go:40` 的 `pg_try_advisory_lock($1)`：**也是**单参 bigint 形式，
    **还是同一个键空间**
  - `postgres/reserver_store.go:213-214` —— 注释说「the lock space (**two-arg int4 form**)」，与 `journals.sql:151` 的「**Single-arg bigint form** is used」直接矛盾
- **判定**：CONFIRMED
- **后果**：三套哈希（hashtextextended×2 语义 + FNV-64a）共享一个 64-bit 空间，
  没有任何命名空间分区（Postgres 的双参 int4 形式是**独立**空间，本可以用来分区，
  但没用）。64-bit 随机碰撞概率可忽略，所以我把它定 Minor 而不是 Major ——
  **但**：一个 job 的 session 级 `pg_advisory_lock` 若碰撞到某个 balance key，
  `pg_advisory_xact_lock` 会**无限期阻塞**（不是超时失败），
  记账会挂到 job 跑完。加上上面那条可构造的 ABBA，整体键空间设计是脆的。
  矛盾的注释会直接误导下一个读代码的人得出「命名空间是分开的」这个错误结论。

### [Minor] `event_store` 的「claim lost」用包级 `slog` 报，消费方注入的 logger 看不到

- **位置**：`postgres/event_store.go:174`、`:189`、`:205`（`slog.Warn("... claim lost, outcome dropped", "event_id", id)`）
- **判定**：CONFIRMED
- **后果**：这三处是 lease 竞争唯一的可观测信号。库里其他地方一律走注入的
  `core.Logger`（`engine.Logger()`），只有这里用包级 `slog` 默认 handler。
  库模式消费方把日志接到自己的 handler 之后，**这三条 Warn 落到进程默认
  stderr**，在结构化日志管线里等于不存在。事件投递的丢弃是静默的
  （working-agreements §3：降级必须落痕 —— 落了，但落在别人看不到的地方）。
  另：`n == 0` 之后 `return nil` —— 调用方无法知道自己的 outcome 被丢了。
- 移交 F：`postgres` 是 adapter 层，import slog 不违反 golang.md 的领域层禁令，
  但「注入 logger vs 包级 slog」的不一致属结构问题。

### [Minor] `RebuildCheckpoint` 会被它要修的东西永久挡住

- **位置**：
  - `postgres/checkpoint_integrity_store.go:145-156`（`pending > 0 → ErrRollupPending`）
  - `postgres/sql/queries/integrity_checkpoint.sql:47-52`（`CountPendingRollupForDimension`：`WHERE ... processed_at IS NULL`）
  - `postgres/sql/queries/checkpoints.sql:91-97`（`DequeueRollupBatch`：`AND failed_attempts < 10`）
- **判定**：CONFIRMED
- **后果**：一个 rollup 队列项失败 10 次后被 dequeue 永久排除，但
  `processed_at` 永远是 NULL → `CountPendingRollupForDimension > 0` 永远成立
  → 该维度的 `RebuildCheckpoint` **永久返回 `ErrRollupPending`**。
  而「rollup 反复失败」恰恰是「checkpoint 被投毒」最可能的伴生现象 ——
  修复路径被它要修的故障本身堵住。注释说「the operator drains or waits for
  the item first」，但没有任何 API 能 drain 一个 `failed_attempts >= 10` 的项。

---

## 移交

- **A（financial-correctness）**：`SumActiveReservations`（`reservations.sql:73-78`）
  对 `settling` 用 `reserved_amount - settled_amount`，而 `settled_amount` 是
  `SettlePartial` 累加出来的、**与任何 journal 无关**的数 —— 「held」这个口径
  与账本实际发生额是否守恒，需要 A 从金额守恒角度核一遍。
- **A**：`GetBalance`（`postgres/ledger_store.go:1119`）用 `normalSide` 的
  `default:` 分支静默按 debit-normal 算（`:1178-1180`），而
  `service/rollup.go:207` 的同一个判断把未知 `normal_side` 当**致命错误**。
  同一个语义两处不同处置，其中一处是静默默认值。
- **C（consumer-surface）**：`examples/billing` 的注释与 printf 声称
  Settle 会扣钱（见第一条 Critical）。这是「example 即文档」的直接反例。
- **C**：`Settle` / `Release` / `FinalizeSettlement` / `Transition` 的
  `ErrInvalidTransition` 无法区分「我重放了」/「别人抢了」/「过期了」——
  哨兵错误面不够用。
- **D（threat-model）**：`ConfirmPending` / `CancelPending` 产出 unsigned journal
  （见对应 Major）。防篡改覆盖率的完整清单请 D 收口 ——
  建议逐条列出「哪些 auth_status 值在生产里会出现」。
- **E（test-credibility）**：`TestReserverStore_Reserve_Concurrent` 是 I-4/I-11
  的空洞 pin；`TestReserverStore_Reserve_Settle` 只 `require.NoError` 不断言余额；
  `TestReserverStore_Settle_InvalidTransition` 把与 I-3 矛盾的行为钉成了期望。
  更普遍的：INVARIANTS.md 的门禁只校验引用存在。
- **E**：`core/reserve_stress_test.go` 文件名含 stress，内容是纯 map 表遍历，
  零 goroutine。命名会让人以为并发被压过。
- **F（structure）**：`postgres/event_store.go` 用包级 `slog` 而非注入的
  `core.Logger`（见 Minor）。
- **F**：`postgres/registration_rescan_store.go` 用手写 raw SQL 字符串
  （`:34`、`:52`、`:90`、`:105`），全仓其他 store 都走 sqlc `.sql` 文件
  —— golang.md 明确「所有查询写 .sql 文件，不手写 raw SQL string」。
  这**可能就是它漏了 claim-token guard 的结构性原因**：它不在那批被集中
  review 过的 `.sql` 文件里。

---

## 我没能验证的

1. **registration_rescan 是否存在真正的「漏扫区块」路径。** 我推演了游标回退、
   un-complete、活 claim 被抹三种后果，但没能构造出「某段区块永远没被扫过」的
   序列 —— 所有 `Advance` 写的 `to+1` 都不超过写入者自己扫过的上界。
   需要有人从 `available_at` / `attempts` 退避与 `maxBlocksPerScan` 的交互
   再推一遍，特别是 `RetryRegistrationRescan` 把 `available_at` 推到未来时，
   一个正在被 B 处理的 job 被 A 推后会发生什么。
2. **没有实跑任何测试或 SQL。** 全部结论来自源码与 SQL 通读。
   Critical #1（Settle 不扣钱）我有 100% 的代码级把握，但**没有实跑
   `go run ./examples/billing` 确认打印出来的是 100 而不是 84.25** ——
   建议 Team Lead 先跑这一条，它是最便宜的验证（一条命令）。
3. **连接池耗尽死锁（Reserve tx 模式）标了 PLAUSIBLE。** 「在已开事务里再取
   一条池连接」是 CONFIRMED 的，但是否真会打满取决于 pgx pool 的
   `MaxConns` 与 acquire 超时行为，我没有验证 pgx v5 在这种自我等待下
   是否会返回错误而不是永久阻塞。
4. **advisory lock 键空间碰撞的实际影响面**我只推到「可构造 ABBA 死锁」。
   `service/locked_job.go` 的 session 级锁与 balance 锁的真实碰撞概率
   我按 64-bit 随机估的，没有算 FNV-64a 在短字符串（`"job:reconcile"` 这类）
   上的分布是否有偏。
5. **`registration_rescans` / `chain_cursors` / `ingest_dead_letters` /
   `webhook_subscribers` 的其余并发面**没有逐个走完 —— 我只深挖了
   team lead 点名的三处 claim 机制。
6. **partition DDL 与并发写入的交互**（`service/partition.go`）没看。
   月度分区在创建瞬间与正在插入 `journal_entries` 的事务是否会互锁，未验证。
