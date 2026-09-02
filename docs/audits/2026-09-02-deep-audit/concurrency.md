# 审计报告 — concurrency（第二轮深度审计 · territory B）

> 基线 commit `fd87cae`。契约：`docs/plans/2026-09-02-deep-audit-contract.md` +
> `docs/plans/2026-08-25-financial-audit-contract.md` §1/§3。
> 所有 `file:line` 以 `fd87cae` 工作树为准。

## 摘要

**上一轮 B territory 的八条 Major 有六条真修了，且修得比文档声称的更实**——终态操作幂等
（`reservation_operation_receipts`）、`Transition` 幂等（`booking_transition_receipts`）、
advisory lock 命名空间前缀（`bal:` / `idem:`）、`Reserve` 的 tx 模式 fail-closed 守卫、
`registration_rescans` 的 `attempts` claim-token、`expiration` 进 `LockedJob`、
`cleanupContext` 铺到每个「退出时释放 claim/锁」的点。**I-4/I-11 的空洞 pin 也真补上了**：
我把 `reserveWithQueries` 的 `acquireBalanceLocks` 拆成 `if false` 实跑，
`TestReserverStore_Reserve_Concurrent_RejectsOverCommit` **红**（10 路并发预留总额 135 > 余额 100），
跑完已 `git checkout --` 还原。

**但同一个失效形态在三个没被点名的兄弟路径上原样留着**，这是本轮 B 的主线：

1. **`ConfirmPending`/`CancelPending` 的 balance lock 取序与全仓相反**——先锁用户 holder、
   再由 `PostJournal` 按全局序补锁系统对手方（`-holder` 排在前）。任意两个普通的
   pending 操作（`AddPending` ⨯ `ConfirmPending`，同一用户）就能互锁。**我实跑复现了真实
   40P01**，不是推演。上一轮给 `ExecuteTemplateBatch` 写下「一个 canonical order 不花钱」的
   结论时，没有回头核这条路径。
2. **`VerifiedBalanceReader()` 在 RunInTx clone 上完全没有守卫**——`Reserve` 那条
   `RequireVerifiedBalance` 闸加了 `s.pool == nil → fail closed`，但**同一个 store 的直接
   accessor 没加**，而它才是设计稿里的提现闸。消费方在 `RunInTx` 里「校验已授权余额 + 记提现账」
   （最自然的组合方式）就会在已开事务里发起可能远程的 `AuthVerifier.Verify`。
3. **`ExecuteTemplateBatch` 的 batch 级全局锁序只加在 pool 模式**，tx 模式
   （`executeTemplateBatchWithQueries`）原样是 per-journal 取锁。而**声称钉住这条修复的
   回归测试根本没有调用 `ExecuteTemplateBatch`**——它在测试里自己重写了一遍修复逻辑，
   把 `ledger_store.go` 的整段预锁删掉它依然全绿（契约 §1.4 的活样本）。

另外发现一条**上一轮八份报告都没问的问题**：`ClosePeriod` 不取任何锁、不设任何屏障，
而 `postJournalWithQueries` 的期末关账检查在 READ COMMITTED 下是一次普通读——
**I-15 声称的「硬写屏障」在并发下不成立**，且没有任何对账检查能事后发现越线的 journal。
在 `RunInTx` 下这个窗口的长度等于调用方回调的长度，不是微秒级。

无 Critical。**6 Major / 8 Minor。**

> ⚠️ **流程告警（不是代码发现）**：审计期间工作树被其他 territory 的 agent 反复投毒又还原
> ——`service/rollup.go:211` 曾被改成 `currentBalance.Sub(delta)`（10:27:34），随后
> `core/onchain.go` 出现改动（10:35 前后）。任何人在这些窗口里跑 `go test ./...`
> 会拿到被污染的结果并可能误判为真实发现。请 lead 收口 mutation-test 的写盘纪律
> （建议：mutation 只在 `wt` 开出的 worktree 里做）。我自己的两次写盘（`postgres/` 下的
> 临时探针文件、`postgres/reserver_store.go` 的 `if false` 变异）**都已在跑完后立即删除 /
> `git checkout --` 还原**，本报告落盘前 `git status` 中我的部分为空。

---

## 发现

### [Major] `ConfirmPending` / `CancelPending` 的 balance lock 取序与全仓相反 → 两个普通 pending 操作可互锁（实跑复现真实 40P01）

- **位置**：
  - `postgres/pending_store.go:248-253`（`checkPendingBalanceAndPost` 的 `run`：
    `acquireBalanceLocks(ctx, qtx, []balancePair{{holder: holder, currencyID: cur.ID}})`
    —— **只锁用户 holder 这一个 pair**）
  - `postgres/pending_store.go:277` / `:279`（随后 `ledger.PostAuthorized` / `ledger.PostJournal`）
  - `postgres/ledger_store.go:968`（`postJournalWithQueries` → `acquireBalanceLocks(balancePairsFromEntries(resolved))`，
    按 `(holder, currency_id)` **全局升序**）
  - `postgres/pending_store.go:315-354`（`buildConfirmPendingJournalInput`：四条腿分别打
    `in.AccountHolder` 与 `core.SystemAccountHolder(in.AccountHolder)`）
  - `core/types.go:146-148`（`SystemAccountHolder(u) = -u`，恒为负 → 排序时**排在用户 holder 之前**）
- **判定**：**CONFIRMED（实跑）**
- **失效场景**：
  - Tx A = `ConfirmPending(holder=H)`：锁序 `bal(H,C)` → 然后 `PostJournal` 按全局序要
    `bal(-H,C)`（先）→ `bal(H,C)`（已持有，可重入）。**实际持锁顺序 = H, 然后 -H。**
  - Tx B = `AddPending(holder=H)`（或任何同时打 `(H,C)` 与 `(-H,C)` 的模板，
    如 `deposit_confirm` / `transfer` / `fee`）：直接走 `PostJournal`，锁序 `bal(-H,C)` → `bal(H,C)`。
  - A 持 `bal(H)` 等 `bal(-H)`；B 持 `bal(-H)` 等 `bal(H)` → 经典 ABBA，
    Postgres `deadlock_timeout` 后 abort 一方，SQLSTATE **40P01**。
  - 后果不是算错钱（40P01 经 `normalizeStoreError` 归成 `core.ErrTransient`，可重试），
    是**充值确认这条 money-path 上的随机失败 + 秒级延迟尖刺**，且两个参与者都是完全正常的
    业务调用，**不需要任何恶意或异常输入**。
- **最小复现**（我实跑过，用完即删）：
  1. 装 `presets.InstallPendingBundle`，`AddPending(H=90001, 500)`。
  2. 第三个事务 T 先取 `bal(-90001, C)`（用 `postgres.AcquireBalanceLocksForTest`）。
  3. goroutine 里调 `ps.ConfirmPending(H=90001, 100)` —— 它取到 `bal(90001)` 后阻塞在 `bal(-90001)`。
  4. sleep 1.5s 后，T 再取 `bal(90001, C)`。
  - **实跑输出**：
    `T err = postgres: post journal: advisory lock (90001,1): transient postgres error 40P01: ERROR: deadlock detected (SQLSTATE 40P01): transient failure, safe to retry`
    —— 若 `ConfirmPending` 遵守全局序，步骤 4 会立刻成功、不可能出现 40P01。
- **为什么现有机制没拦住**：
  - `postgres/ledger_store.go:602-631`（`ExecuteTemplateBatch` 的 batch 预锁）为**完全相同的
    形态**写了长注释，并明确定下「一个 canonical order 不花钱」的原则——但那次修复只覆盖了
    自己那一个调用点，没有回头扫「还有谁在 `postJournalWithQueries` 之外自己取 balance lock」。
    全仓自己取 balance lock 的非 `postJournalWithQueries` 调用点共三处：
    `pending_store.go:248`（**本条，逆序**）、`reserver_store.go:232`（单 pair，无序问题）、
    `account_policy_store.go:100`（单 pair，无序问题）、
    `checkpoint_integrity_store.go:156`（单 pair，无序问题）。**只有 pending 这一处会取到
    两个 pair 且顺序错。**
  - `postgres/pending_store.go` 全部测试都是单线程顺序断言；`TestPendingStore_*_Concurrent`
    只并发同一 key（走幂等短路），从不并发两个方向相反的 pair。
- **修法方向（不实施）**：把预锁改成 `sortedUniquePairs(balancePairsFromEntries(...))`
  的完整集合（即和 `PostJournal` 取同一组、同一序），而不是只锁用户那一个 pair。

---

### [Major] `VerifiedBalanceReader()` 在 RunInTx clone 上无守卫 —— 已开事务里发起（设计上允许远程的）`AuthVerifier` 调用

- **位置**：
  - `postgres/verified_balance_store.go:82-92`（`WithDB`：`verifier: s.verifier` 原样带过来，
    **没有任何 tx 模式标记的消费点**）
  - `postgres/verified_balance_store.go:159-161`（`s.verifyJournalsNaively(ctx, journalIDs)`
    → `core.AuthVerifier.Verify`，**无 `s.pool == nil` 守卫**）
  - `ledger.go:606`（`verifiedBalanceStore: s.verifiedBalanceStore.WithDB(tx)` —— clone 上可达）
  - `ledger.go:559`（`func (s *Service) VerifiedBalanceReader() core.VerifiedBalanceReader`
    —— 公开 accessor，**没有 `s.tx != nil` 守卫**）
  - 对照组（都守了）：`postgres/reserver_store.go:123-128`（`Reserve` 的
    `RequireVerifiedBalance`，错误文案就叫「the verified-balance gate may call a remote
    AuthVerifier and financial.md forbids that inside an open transaction」）、
    `postgres/ledger_store.go:315-318`（`Authorize`）、
    `ledger.go:340`（`AttestationService`）、`ledger.go:390`（`VerifyLedger`）、
    `ledger.go:731`（`EnableOnchain`）
  - `core/interfaces.go:647-649`（`AuthVerifier` 的 doc：「verification can run entirely
    outside the database host -- that independence is the whole point」）
- **判定**：**CONFIRMED**（代码级：调用链上不存在任何 tx 模式守卫）
- **失效场景**：
  ```go
  svc.RunInTx(ctx, func(tx *ledger.Service) error {
      // 事务已开，可能已持有 bal(H,C) 的 advisory lock
      bal, err := tx.VerifiedBalanceReader().VerifiedBalance(ctx, holder, curUID, clsUID)
      if err != nil { return err }            // ← 这里会打远程 verifier
      if bal.LessThan(amount) { return ErrX }
      _, err = tx.JournalWriter().PostJournal(ctx, withdrawalInput)
      return err
  })
  ```
  1. 这是**设计稿里提现闸的最自然写法**（「校验已授权余额」与「记提现账」必须原子），
     也是 `core.VerifiedBalanceReader` doc 里 "the withdrawal gate" 的字面用法。
  2. `VerifiedBalance` 对每一笔贡献 journal 都跑 live check（`verified_balance_store.go:118-146`
     的注释明确说 T4 的缓存优化在这条路径上被**故意放弃**了），所以 verifier 调用次数
     ∝ 该维度的 journal 数——一个远程 verifier 下这是**在已开事务里的 N 次网络往返**。
  3. 直接违反 `financial.md`「DB 事务内禁止外部调用」，而这正是 `Reserve` 那条闸上一轮被判
     Major 并修掉的东西。修复只落在 `Reserve` 的入口，**没落在 store 自己的方法上**。
  4. 叠加效应：调用方若已在同一事务里发过 journal，`bal(H,C)` 的 advisory lock 会被
     整个网络往返期间一直持有 —— 同一 holder 的所有记账排队等一个远程 RPC。
- **最小复现**：注入一个 `AuthVerifier` 实现，`Verify` 里 `time.Sleep(3s)` + 记录
  `pool.Stat().AcquiredConns()`；在 `RunInTx` 回调里先 `PostJournal` 再
  `VerifiedBalanceReader().VerifiedBalance(...)`，观察事务在 sleep 期间保持 open 且持锁。
- **为什么现有机制没拦住**：上一轮的修复是「给 `Reserve` 补上 `Authorize` 已有的守卫」，
  按**发现的那个入口**修，没按**能到达同一外部调用的所有入口**修。没有任何测试断言
  「tx 模式下 `VerifiedBalance` 的行为」——`postgres/verified_balance_store_test.go`
  全部走 pool 模式。
- **移交 C（tamper-evident）**：这条同时是防篡改面的问题（提现闸的正确用法边界），
  但成因是事务边界，所以我报在这里；覆盖率清单请 C 收口。

---

### [Major] `ExecuteTemplateBatch` 的 batch 级全局锁序只加在 pool 模式，tx 模式原样未修

- **位置**：
  - `postgres/ledger_store.go:602-631`（pool 模式：idem keys 排序取完 + `sortedUniquePairs(allPairs)`
    一次取完 —— **已修**）
  - `postgres/ledger_store.go:656-676`（`executeTemplateBatchWithQueries`：
    `for i, input := range inputs { s.postJournalWithQueries(...) }`
    —— **零 batch 级预锁，逐 journal 取 idem → bal**）
  - `postgres/ledger_store.go:542-548`（`if s.pool == nil { return s.executeTemplateBatchWithQueries(...) }`
    —— tx 模式唯一分支）
  - `ledger.go:589`（`ledgerStore: ls` = `s.ledgerStore.WithDB(tx)`，clone 上 `JournalWriter()` 可达）
- **判定**：**CONFIRMED**（代码级；死锁机制本身已由
  `postgres/lock_order_test.go:42-93` 的 `TestAcquireBalanceLocks_RealDeadlock_WrapsErrTransient` 钉住）
- **失效场景**：
  - Tx A：`svc.RunInTx(... tx.JournalWriter().ExecuteTemplateBatch([]{req(holder=1), req(holder=2)}) ...)`
    → 锁序 `idem(A0), bal(-1), bal(1), idem(A1), bal(-2), bal(2)`
  - Tx B：同上但 requests 顺序倒置 `[]{req(holder=2), req(holder=1)}`
    → 锁序 `idem(B0), bal(-2), bal(2), idem(B1), bal(-1), bal(1)`
  - A 持 `bal(1)` 等 `bal(-2)`；B 持 `bal(2)` 等 `bal(-1)` → ABBA → 40P01。
  - 「批量结算 / 批量返佣」正是会同时打多个 holder 的场景，两批 holder 顺序不同是
    **完全正常的调用方行为**——和上一轮 pool 模式那条 Major 一字不差，只是走了 `RunInTx`。
- **为什么现有机制没拦住**：见下一条——那个声称钉住这条修复的测试根本没跑到这个函数，
  更不可能跑到它的 tx 分支。`ExecuteTemplateBatch` 的 doc comment（`:520-536`）
  也只讨论了 pool 模式的签名位置，没提 tx 模式的锁序。
- **修法方向（不实施）**：把 `:604-631` 那段（idem keys 排序 + `sortedUniquePairs(allPairs)`）
  提成一个 helper，两个分支共用。

---

### [Major] `TestExecuteTemplateBatch_GlobalLockOrder_PreventsCrossJournalDeadlock` 从不调用 `ExecuteTemplateBatch` —— 删掉修复它照样绿

- **位置**：
  - `postgres/lock_order_test.go:172-222`（测试体：`runBatch` 自己 `pool.Begin` →
    `AcquireBalanceLocksForTest(ctx, q, SortedUniquePairsForTest(batch))` → `tx.Commit`。
    **全文没有 `ExecuteTemplateBatch` 这个标识符**）
  - `postgres/lock_order_test.go:155-171`（doc comment：「pins the fix for
    ExecuteTemplateBatch's cross-journal lock-order Major: pre-union every journal's
    balance pairs across the whole batch, sort ONCE, and acquire before posting any journal
    (see ExecuteTemplateBatch's doc comment in ledger_store.go)」）
  - `postgres/ledger_store.go:602-631`（真正被声称钉住的那段实现）
- **判定**：**CONFIRMED**
- **失效场景**（契约 §1.4「这个测试如果被测代码整个不执行，会失败吗？」）：
  测试在自己的 `runBatch` 里**重新实现了一遍修复**（自己调 `SortedUniquePairsForTest`），
  所以把 `ledger_store.go:604-631` 整段删掉——即把 `ExecuteTemplateBatch` 退回到
  pre-fix 的 per-journal 取锁——**这个测试依然全绿**。它证明的是
  「`sortedUniquePairs` + `acquireBalanceLocks` 这两个 primitive 组合起来不死锁」，
  不是「`ExecuteTemplateBatch` 用了它们」。
- **为什么现有机制没拦住**：`docs/INVARIANTS.md` 的门禁只校验「引用的测试名存在」，
  不校验「测试是否驱动了被声称的实现」。这条与上一轮 E territory 的同一结论同源，
  但这次是**一条 Major 修复的唯一 pin**，不是 invariant 的旁证。
- **对比组（做对了的）**：`postgres/reserver_store_test.go:227-280`
  （`TestReserverStore_Reserve_Concurrent_RejectsOverCommit`）**真的调用 `store.Reserve`**，
  所以我把 `reserveWithQueries` 的锁包成 `if false` 之后它**立刻红**
  （实跑：`combined reserved amount 135 (from 9/10 successful concurrent Reserve(15) calls)
  must never exceed the funded balance 100`）。同一个文件夹里两种写法并存，
  只有后一种能证伪。
- **移交 F（test-credibility）**：建议加一条门禁——「声称钉住某个导出符号的测试，
  必须在其源码里出现该符号」。这条比人工 review 便宜得多。

---

### [Major] `ClosePeriod` 不取任何锁也不设屏障 → I-15 的「硬写屏障」在并发下不成立，且无任何事后检查

- **位置**：
  - `postgres/ledger_store.go:951-961`（期末关账闸：`q.GetActivePeriodClose(ctx)` 一次普通读，
    随后比较 `effectiveAt.Before(activeClose.CloseBefore)`）
  - `postgres/ledger_store.go:968`（balance advisory lock 在**这个检查之后**才取）
  - `postgres/period_close_store.go:50-65`（`ClosePeriod`：只有一条 `InsertPeriodClose`，
    **无 advisory lock、无 `FOR UPDATE`、无对 in-flight journal 的屏障**）
  - `postgres/sql/queries/periods.sql:8-13`（`GetActivePeriodClose`：`ORDER BY created_at DESC, id DESC LIMIT 1`）
  - `docs/INVARIANTS.md:609-614`（I-15：「**There is no journal** whose `effective_at` is
    earlier than the currently active period-close line」——全称、无并发例外）
- **判定**：**CONFIRMED**（代码级；两边都不取锁，隔离级别默认 READ COMMITTED）
- **失效场景**：
  1. T1（调用方的 `RunInTx`）：`PostJournal(effective_at = 2026-08-15, 金额 1000)`。
     `GetActivePeriodClose` 返回「从未关账」或一条更早的线 → 闸放行 → 取 bal 锁 → 写 entries。
     **T1 还没 commit**（回调里还有别的业务写，或在等一个慢调用）。
  2. T2：运维在月末执行 `POST /periods/close {close_before: "2026-09-01T00:00:00Z"}` → INSERT → COMMIT。
  3. T1 COMMIT。
  4. 结果：账上存在一笔 `effective_at = 2026-08-15` 的 journal，而当前活跃关账线是 `2026-09-01`。
     **I-15 的全称命题被违反。**「上个月的账已定稿」这个承诺失效，8 月的历史报表被事后改动。
  - **窗口不是微秒级**：`RunInTx` 的事务由调用方持有，窗口长度 = 调用方回调的执行时长
    （`ledger.go:462` 的 doc 明确鼓励在回调里做调用方自己的写）。
  - 反向同样成立：T2 先 commit、T1 的**读**发生在 T2 commit 之后就会被拒——
    所以这不是「总是漏」，是「取决于两个事务的相对时序」，即一个真正的 TOCTOU。
- **最小复现**：
  ```
  T1: BEGIN; -- PostJournal(effective_at=8/15) 走完到写 entries，不 commit
  T2: BEGIN; INSERT INTO period_closes(close_before='2026-09-01',...); COMMIT;
  T1: COMMIT;
  SELECT count(*) FROM journals j
    WHERE j.effective_at < (SELECT close_before FROM period_closes ORDER BY created_at DESC, id DESC LIMIT 1);
  -- > 0
  ```
- **为什么现有机制没拦住**：
  - I-15 的 `Enforced by` 只说「reads the active close line **inside the same transaction as
    every write path**」——「在同一个事务里读」不等于「与 `ClosePeriod` 互斥」。
    `ClosePeriod` 不参与任何锁，所以同一事务读根本没有排他效果。
  - I-15 的 6 条 `Pinned by`（`docs/INVARIANTS.md:631-643`）**全部是单线程顺序断言**：
    先关账、再 post、断言被拒。没有一条并发。
  - `service/reconcile.go` 的全套检查里**没有任何一条**比对
    `journals.effective_at` 与 `period_closes.close_before`（我 grep 过 `period`，
    唯一命中是 `:270` 的 `SettlementWindow` 注释，与关账无关）。
    **所以越线的 journal 落地之后，没有任何机制会发现它。**
- **修法方向（不实施）**：`postJournalWithQueries` 取
  `pg_advisory_xact_lock_shared(periodKey)`，`ClosePeriod` 取
  `pg_advisory_xact_lock(periodKey)` 排他——共享锁不影响记账并发度，排他锁让
  `ClosePeriod` 天然等到所有 in-flight journal 落地。另外**无论如何都该补一条对账检查**
  （越线 journal 计数），因为屏障再严也需要一个能证伪它的观测量。
- **注**：这是我在契约 §0「上一轮共同盲区」意义上的候选——上一轮八份报告没有一份问过
  「关账与 in-flight 写的相对顺序」。

---

### [Major] `Service.Worker()` 是唯一不守卫 `s.tx != nil` 的构造器 —— 后台 worker 被接到一个即将销毁的事务上

- **位置**：
  - `ledger.go:817-860`（`func (s *Service) Worker(cfg service.WorkerConfig) *service.Worker`
    —— **无 `if s.tx != nil` 分支**）
  - `ledger.go:839`（`expirationSvc := service.NewExpirationService(rollupAdapter,
    **s.reserverStore, s.reserverStore, s.bookingStore, s.bookingStore**, engine)`
    —— 在 clone 上这四个都是 `WithDB(tx)` 绑定的 tx 模式 store，见 `ledger.go:589-590`）
  - `ledger.go:857`（`w.SetAttestor(s.attestationServiceUnchecked(nil))` ——
    绕过了 `AttestationService` 自己的 `s.tx` 守卫，见 `ledger.go:349-353` 的
    `attestationServiceUnchecked` 注释「callers ... that have already established both
    preconditions」——`Worker` 恰恰**没有**建立 `s.tx == nil` 这个前提）
  - 对照组（三个兄弟全守了）：`ledger.go:340`（`AttestationService`）、
    `ledger.go:390`（`VerifyLedger`）、`ledger.go:731`（`EnableOnchain`，错误文案还专门解释了
    「would be set on a clone that RunInTx discards when the callback returns」）
  - 半守的：`ledger.go:412`/`:424`/`:287`（`Reconciler` / `FullReconciler` /
    `SnapshotBackfiller` 都做了 `if s.tx != nil { ...WithDB(s.tx) }` 的正确重绑）
- **判定**：**CONFIRMED**（接线事实）／**PLAUSIBLE**（具体运行期症状我没实跑）
- **失效场景**：
  ```go
  err := svc.RunInTx(ctx, func(tx *ledger.Service) error {
      w := tx.Worker(service.WorkerConfig{})   // 无任何报错
      go w.Run(ctx)                            // 或存到外面，回调返回后再 Run
      return nil
  })
  ```
  1. `Worker()` 返回的对象是**半在事务里半在池上**的嵌合体：`rollupAdapter` / `eventPoller` /
     `partitionStore` / `SetPool` 都取自 `s.pool`，而 `expiration` 的四个 store 是 tx-bound。
  2. `RunInTx` 提交（或回滚）后那个 `pgx.Tx` 就废了。expiration job 每 30s 一次，
     `LockedJob` 拿到 pool 锁、跑 `ExpireStaleReservations` →
     `Release`/`FinalizeSettlement` 打在已关闭的 tx 上 → 报错 →
     `service/worker.go:298-301` **只 log 不返回**。
  3. 结果：worker 起来了、`w.SetAttestor` 接好了、日志里有 "worker: started"，
     **过期 reservation 与 booking 永远不会被回收**，而唯一的信号是一条被淹没的 Error 行。
     这正是 working-agreements §3「未运行 ≠ 通过 / 降级必须落痕」的形态。
  4. 更糟的变体：在回调**内部**就 `go w.Run(ctx)` → 两个 goroutine 并发用同一个 `pgx.Tx`
     （`ledger.go:445-448` 的 doc 明说不支持），`-race` 下是真数据竞争。
- **最小复现**：`RunInTx` 里 `w := tx.Worker(service.WorkerConfig{ExpirationInterval: 50*time.Millisecond})`，
  回调返回后 `go w.Run(ctx)`；预先插一条 `expires_at` 已过期的 reservation，
  跑 1s 后查它是否仍是 `active`（预期：仍是 active，且日志里有 tx 相关错误）。
- **为什么现有机制没拦住**：三个兄弟构造器各自加守卫时，是**逐个按发现修**的，
  没有一条机器规则要求「任何返回长生命周期对象的 `*Service` 方法必须处理 `s.tx`」。
  `readme_api_surface_test.go` 只校验方法出现在 README 表里，不校验 tx 语义。
  没有任何测试在 clone 上调 `Worker()`。
- **移交 E（consumer-surface）**：`RunInTx` 的 doc comment（`ledger.go:459-482`）逐条列了
  「哪些方法在 clone 上会被拒」，**`Worker` 不在列**——读文档的人有理由认为它是安全的。

---

### [Minor] `EventStore.SetLogger` 全仓零调用点 —— 上一轮的 Minor「加了机制、没接到线」

- **位置**：
  - `postgres/event_store.go:64-82`（`SetLogger` 的 doc：「Wire it from your composition root:
    `eventStore.SetLogger(engine.Logger())`」）
  - `postgres/event_store.go:86-92`（`warn`：`s.logger == nil` → `slog.Warn`）
  - `ledger.go:169`（`s.eventStore = postgres.NewEventStore(pool)` —— **没接**）
  - `ledger.go:835`（`eventPoller := postgres.NewEventStore(s.pool)` —— **没接**）
  - `examples/fullstack/backend/main.go:119`（`postgres.NewEventStore(pool)` —— **没接**）
- **判定**：**CONFIRMED**（`grep -rn "SetLogger" --include='*.go' .` 的全部非注释命中都在
  `event_store.go` 自身）
- **后果**：`ledger.WithLogger(myLogger)` 的消费方仍然看不到 `MarkDelivered` /
  `MarkRetry` / `MarkDead` 的三条 "claim lost, outcome dropped" 警告——它们落到
  `slog.Default()` 的进程默认 handler，在结构化日志管线里等于不存在。
  这是 lease 竞争唯一的可观测信号（`n == 0` 之后仍 `return nil`，调用方无从得知 outcome 被丢）。
  **本库自己的 composition root（`ledger.New`）就是那个「your composition root」**，
  它没调；`examples/` 作为事实文档也没示范。
- **修法方向**：`ledger.go:169` / `:835` 各加一行 `.SetLogger(...)`，
  并加一条断言「`ledger.WithLogger` 配置的 logger 收得到 claim-lost 行」。

---

### [Minor] event delivery 的 `MarkDelivered` / `MarkRetry` 用的是可能已被取消的 ctx，未走 `cleanupContext`

- **位置**：
  - `service/delivery/local.go:74`（`d.poller.MarkRetry(ctx, ...)`）、`:82`（`MarkDelivered(ctx, ...)`）
  - `service/delivery/webhook.go:172`（`d.poller.MarkDelivered(ctx, evt.InternalID, evt.ClaimToken)`）
  - 对照组：`service/cleanup_context.go:18-22`（「**Use this at every "release a claim / lock
    on the way out" call site** instead of the ctx that was just cancelled」）
  - 已按这条办的三处：`service/rollup.go:125`、`service/locked_job.go:114`、
    `service/snapshot.go:106`、`service/onchain.go:562`
- **判定**：**CONFIRMED**
- **后果**：优雅关停时 ctx 被取消，一条 HTTP 投递可能刚刚成功返回，紧接着的
  `MarkDelivered` 因 `ctx.Err() != nil` 立即失败 → outcome 丢失 → lease 过期后**重投**。
  投递语义本来就是 at-least-once，所以不是正确性缺陷；但它与 `cleanup_context.go` 自己
  写下的「every ... call site」规则不一致，而这条不一致**没有任何机制会发现**
  （working-agreements §5）。
- **移交 I（operability）**：重复投递在关停窗口里会集中出现，值得在 RUNBOOK 里点名。

---

### [Minor] `journals.sql` 的「残余风险」注释已过时：`migrate.go` 用的是**阻塞式** session 锁

- **位置**：
  - `postgres/sql/queries/journals.sql:183-199`（残余风险段：「**every one of those other
    callers uses the non-blocking `pg_try_advisory_lock`**, so the only possible effect is
    one skipped/delayed lock-wait, **never a deadlock** (`pg_try_advisory_lock` cannot
    participate in a wait-for cycle)」）
  - `postgres/migrate.go:147`（`SELECT pg_advisory_lock($1)` —— **阻塞式、session 级**，
    key = `clusterMigrationLockKey = 2573143714`，`postgres/migrate.go:24`）
  - `postgres/migrate.go:145-146`（注释自认「Blocks indefinitely until acquired」）
- **判定**：**CONFIRMED**（注释的全称断言有反例）
- **后果**：实际风险为零——`acquireClusterLock` 连的是集群的 `postgres` **维护库**
  （`postgres/migrate.go:133-137`），而 PostgreSQL 的 advisory lock **按 database 隔离**
  （I-47 自己就是靠这条性质成立的），所以它和 `bal:` / `idem:` / `job:` 三套 key
  根本不在同一个锁空间。**但那段注释给出的理由（"都是 try 变体"）是错的**，
  真正的理由（database 隔离）它一个字没提。下一个读这段的人会得出
  「全仓没有阻塞式 session 锁」这个错误结论，从而在应用库里引入一个真的会成环的阻塞 session 锁。
  边界情况：若有人把账本装进 `postgres` 维护库本身（不推荐但没有任何东西阻止），
  两套锁就真的共享 64-bit 空间了——碰撞概率可忽略，但那时「never a deadlock」不再成立。
- **修法方向**：把那段的理由改成「按 database 隔离」，并把
  `migrate.go` 的阻塞式 session 锁显式列为已知的唯一例外。

---

### [Minor] `acquireClusterLock` 无超时、无 ctx —— 一次卡住的 Migrate 让整个集群的 Migrate 永久挂起且无诊断

- **位置**：
  - `postgres/migrate.go:136`（`ctx := context.Background()`）
  - `postgres/migrate.go:147`（`pg_advisory_lock` 阻塞式，无 `lock_timeout`、无 `statement_timeout`）
  - 对照组：`postgres/migrate.go:50`（`waitForDatabase(databaseURL, 10*time.Second)` —— 有超时）
  - `func Migrate(databaseURL string) error`（`postgres/migrate.go:47`）签名里根本没有 ctx
- **判定**：**CONFIRMED**
- **后果**：持锁方进程被 `SIGKILL` 且 TCP 连接进入半开状态时，Postgres 要等到
  `tcp_keepalives_idle` 才会回收 session、释放锁。这期间**同一集群上每一个**
  `ledger.Migrate` 调用（包括其他项目的库——I-47 的设计就是要跨库序列化）
  都会静默阻塞在 `migrate.go:147`，无日志、无超时、无法取消。
  在 Aaron 的本地共享 `dev-postgres`（`infra.md`：一个集群、db-per-project）上，
  这意味着一个卡住的项目会挂住所有项目的启动。
- **修法方向**：`SET lock_timeout` + 有界重试循环（每次失败打一条 Info 说明在等谁），
  或改用 `pg_try_advisory_lock` 轮询。至少要有一条「我在等集群迁移锁」的日志。
- **移交 I（operability）**：另有一条相关的运维前置——`Migrate` 现在**硬依赖**对
  `postgres` 维护库的 CONNECT 权限（`postgres/migrate.go:133`）。托管 Postgres 上
  这未必总成立，而失败信息是 `connect to maintenance database`，不指向根因。

---

### [Minor] `dimCache` 会缓存未提交事务里创建的配置行；回滚后进程级共享缓存被污染且永不自愈

- **位置**：
  - `postgres/dims.go:53-56`（`var dimCaches sync.Map // *pgxpool.Pool -> *dimCache`
    —— **按 pool 共享，所有 tx clone 继承同一指针**，见 `:55-56` 注释）
  - `postgres/dims.go:112-128`（`dimLookup`：**只在 miss 时 refresh**，hit 永不复核）
  - `postgres/dims.go:70-109`（`refresh(ctx, q)`：`q` 在 tx 模式下就是**调用方的事务**）
  - `ledger.go:594`（`currencyStore: s.currencyStore.WithDB(tx)` —— 配置写在 clone 上可达）
  - `postgres/dims.go:6-9`（包注释的 "Cache safety" 只论证了**字段不可变**，
    **完全没提未提交行的可见性**）
- **判定**：**CONFIRMED（机制）／PLAUSIBLE（真实触达）**
- **失效场景**：
  1. T1 = `RunInTx`：`tx.Currencies().CreateCurrency(...)` 插入货币 C（未提交，拿到 id=N）。
  2. 同一回调里任何按 uid 解析 C 的动作（如紧接着 `PostJournal`）→ cache miss →
     `refresh(q = T1 的事务)` → **C 被写进按 pool 共享的全局 cache**。
  3. T1 回滚（回调返回 error）。C 在库里不存在，`currencies` 的 BIGSERIAL 把 N 烧掉了。
  4. 之后任何请求（**别的连接、别的 goroutine**）用 C 的 uid → **cache hit** → 拿到 id=N →
     写 `journal_entries` 时外键失败。因为是 hit，**永远不会触发 refresh 自愈**，
     一直坏到进程重启。
  - 好消息：`refresh` 是整表重读、只增不减（已提交行在 READ COMMITTED 下都能看到），
    所以污染是**加法性的**——只会多出一个幻影条目，不会把已存在的 uid 映射错。
    且 BIGSERIAL 不复用，所以不会出现「uid 映射到另一个货币」这种更坏的情形。
- **为什么现有机制没拦住**：包注释把「安全性」论证成了「缓存的字段不可变」，
  这个前提是对的，但缓存的**存在性**同样需要论证，而它没有。
  没有任何测试在 `RunInTx` 里创建配置行然后回滚再从池上读。
- **修法方向**：tx 模式下 refresh 走 pool（另开只读连接）而非调用方事务；
  或 tx-bound clone 用**自己的**一次性 dimCache，不写回共享实例。

---

### [Minor] bookings 行锁与 balance advisory lock 的取序在两条路径上相反

- **位置**：
  - `service/onchain.go:976-991`（`postDepositConfirmedJournal` 的 `RunInTx` 回调：
    `booker.Transition(...)` 先取 bookings 行锁 → 之后 `journals.PostAuthorized(...)` 取
    idem → balance locks）
  - `postgres/booking_store.go:212`（`Transition` → `GetBookingForUpdateByUID`，`FOR UPDATE` 行锁）
  - `postgres/ledger_store.go:968`（`postJournalWithQueries` 先取 balance locks）
  - `postgres/ledger_store.go:1123`（**之后**才 `q.LinkBookingJournal(...)` —— bookings 行的 UPDATE）
  - `server/handler_journals.go:191` / `:263`（`EventUID` 是**调用方可给的 wire 字段**，
    `core/journal.go:84-86`）
- **判定**：**CONFIRMED（锁序反向）／PLAUSIBLE（真实触达）**
- **失效场景**：
  - Tx A（本库自己的存款确认，或任何遵循 `CLAUDE.md`「Event-Journal atomicity」写法的消费方）：
    bookings 行锁(B) → bal(H,C)
  - Tx B（`POST /journals` 带 `event_uid` = booking B 的某个 event，entries 打 holder H；
    典型场景是补账 / 修复作业事后把 journal 补挂到一个早先事务创建的 event 上）：
    idem → bal(H,C) → bookings 行锁(B)
  - A 持行锁等 bal，B 持 bal 等行锁 → 40P01。
- **为什么不是 Major**：需要调用方在**没有先行锁 booking** 的情况下 post 一个带 `event_uid`
  的 journal。文档推荐的写法（Transition 与 PostJournal 同一个 `RunInTx`）天然先取行锁，
  不会成环。但 HTTP 面把这条路径直接开放给了调用方，没有任何提示。
- **修法方向**：`postJournalWithQueries` 在解析 `EventUID` 时就把对应 booking 行
  `FOR UPDATE`（取在 balance locks 之前），让两条路径的取序一致。

---

### [Minor] 链上 watcher（`scanChainOnce`）无 leader 选举，`chain_cursors` 也无单调保护

- **位置**：
  - `service/onchain.go:1926-1935`（`Run` 里每条链一个 `runLoop("onchain_watch", ...)`，
    **裸 runLoop，不是 `NewLockedJob`**；对照 `:1952` 的 sweep 就包了 `newSweepLockedJob`）
  - `service/onchain.go:1200`（读 cursor）→ `:1252`（`SetCursor(ctx, chainID, to)`，
    读写之间无锁、无 CAS）
  - `postgres/sql/queries/chain_cursors.sql:4-9`（`SetChainCursor` 的注释自认
    「The watcher is expected to call this monotonically ... **this query does not enforce
    monotonicity -- that is an orchestration-layer invariant (service/)**」）
  - `postgres/chain_cursor_store.go:53`（编排层同样没有实现那条 invariant）
- **判定**：**CONFIRMED**
- **后果**：K 个副本每 tick 各自扫同一段区块——K 倍 RPC 成本、K 倍 `IngestDeposit`
  （靠 `deposit-{chain}-{tx}-{seq}` 幂等键去重，不会重复入账）。
  更明确的缺陷是**游标可回退**：慢副本 A 读到 cursor=100 后卡住，快副本 B 已推进到 300，
  A 恢复后 `SetCursor(200)` 把游标拉回 200 → 重扫。不丢块，只是白干。
  「不丢块」这个结论**依赖于没有任何路径会让游标前跳**，而这一点没有任何断言在守。
  声明「这是 service/ 层的 invariant」但 service/ 层没实现它，是典型的
  working-agreements §5 反例。
- **修法方向**：`SetChainCursor` 加 `WHERE last_scanned_block < EXCLUDED.last_scanned_block`
  （和 `UpsertBalanceCheckpoint`（`postgres/sql/queries/checkpoints.sql:6-18`）
  已经在用的单调保护同款），并把 watcher 包进 `NewLockedJob`。
- **移交 G（onchain-money-path）**：真正危险的是同一函数里
  `service/onchain.go:1246-1250` —— `IngestDeposit` 失败**只 log 不中断**，
  紧接着 `:1252` 照样 `SetCursor(to)` 推进游标。单副本下这就是一笔充值被永久跳过
  （fail-open）。这属于 G 的资金路径，我不深挖，但它与本条叠加。

---

### [Minor] `chains/evm.Sweeper.lastFee` 无界增长

- **位置**：`chains/evm/sweeper.go:45`（`lastFee map[int64]map[uint64]feeQuote`）、
  `:302-308`（`recordFee`：只写不删，全仓无任何 `delete(s.lastFee...)`）
- **判定**：**CONFIRMED**
- **后果**：每笔 sweep 交易在进程生命周期内永久留下一个 `nonce -> feeQuote`
  条目（两个 `*big.Int`）。长驻进程按每分钟一次 sweep 计，一年约 50 万条——
  内存量级不大，但增长无上限、无 TTL、无按已确认 nonce 的裁剪。
  已确认（nonce 低于链上 `PendingNonceAt`）的条目在语义上已经无用
  （`priorFeeFloor`（`:286-300`）只会查当前 pending 的那个 nonce）。
- **修法方向**：`recordFee` 时顺手裁掉低于当前 pending nonce 的条目，或直接按 chain 限长。

---

## 上轮修复复核

按契约 §0 的三问逐条核（① 修复是否接到真实路径 ② 拆掉 pin 会不会红 ③ 有没有在别处复制出同形缺陷）。

| 上轮发现 | 现状 | ① 接到真实路径 | ② pin 可证伪 | ③ 同形缺陷是否被复制/遗留 |
|---|---|---|---|---|
| `Settle`/`Release`/`FinalizeSettlement` 无幂等键 | **真修** —— `core/reserve.go:129-192` 三个 Input 都 `IdempotencyKey` 必填；`postgres/reserver_store.go:350-364` `ensureReservationOperationReceiptMatches` 三态齐全（同 key 同 payload → nil；异 reservation / 异 operation / 异 amount → `ErrConflict`）；检查点在**行锁之后、状态机闸之前**（`:453-458`、`:715-720`、`:759-764`）；`reservation_operation_receipts.idempotency_key` UNIQUE（migration `005`）+ `recordReservationOperationReceipt`（`:376-390`）把 23505 竞争译成 `ErrConflict` | ✅ `service/expiration.go:87-99` 的 worker 自己也走这条路（`expire-finalize-<uid>` / `expire-release-<uid>` 确定性键，api-contract §9） | 未拆验（DB 唯一索引 + 行锁双保险，形状清晰） | ✅ 未发现遗留写路径。我逐个核了 `pending_store`（Add/Confirm/Cancel 三个键齐）、dev credit（`server/handler_devcredit.go:65-72` 走 `ExecuteTemplate` → journal 幂等）、webhook 入站（`server/handler_webhooks.go:122` 派生键 + `TryRecordNonce` 原子 nonce）、onchain credit（`service/onchain.go:696-698` `deposit-{chain}-{tx}-{seq}`）、holder 面（`EnsureDepositAddress` = upsert + `ON CONFLICT DO NOTHING`；mint token 不写库）。**唯一仍无幂等键的是配置写**（`SetPolicy` / `ClosePeriod` / `Create*` / `Deactivate*`），全是 upsert 或 append-only，语义上自幂等——但 I-3 的「every state-changing operation」在字面上仍不成立 |
| I-3 `Transition` 幂等键必填（Wave 1.5） | **真修** —— `core/booking.go:80-118` 必填 + doc 明确警告「键不能是 `<uid>-<status>`」；`postgres/booking_store.go:220-229` receipt 检查在行锁之后、lifecycle 闸之前；`idempotentTransitionEvent`（`:260-283`）作为次级保障保留 | ✅ 所有系统侧调用点都派生了键：`service/expiration.go:151`、`service/onchain.go:984`、`server/handler_webhooks.go:122` | 未拆验 | ✅ 未发现漏网 |
| Reserve TOCTOU pin「有锁无锁同样为真」 | **真修，且我实跑证伪过** | ✅ `postgres/reserver_store_test.go:227-280` 真的调 `store.Reserve` | ✅ **实跑**：把 `postgres/reserver_store.go:232-237` 的 `acquireBalanceLocks` 包成 `if false`，`TestReserverStore_Reserve_Concurrent_RejectsOverCommit` **红**——`combined reserved amount 135 (from 9/10 successful concurrent Reserve(15) calls) must never exceed the funded balance 100`。跑完立即 `git checkout -- postgres/reserver_store.go`。同一变异下旧的 `TestReserverStore_Reserve_Concurrent` **仍绿**，与它自己的 doc comment（`:143-153`）一致 | ⚠️ **同形缺陷在别处**：`TestExecuteTemplateBatch_GlobalLockOrder_...` 是同一个「pin 不驱动被测实现」的形态（见上文 Major #4），而且它是那条 Major 的**唯一** pin |
| advisory lock 键空间共用 / 可构造 ABBA | **真修** —— `postgres/sql/queries/journals.sql:200`（`'bal:' \|\| key`）与 `:211`（`'idem:' \|\| key`）字面前缀，首字节不同 → 两个字符串集合**构造性不相交**，任何调用方给的 `idempotency_key` 都进不了 balance 空间。M-6 复审又把 32-bit `hashtext` 退回 64-bit `hashtextextended` | ✅ 走真实 `AcquireBalanceLock` / `AcquireIdempotencyLock` | ✅ `postgres/lock_order_test.go:114-152` 与 `:234-300` 都驱动真实 primitive 且是真 40P01 | ⚠️ 残余风险注释（`journals.sql:183-199`）的理由**已过时**——见 Minor「`migrate.go:147` 是阻塞式 session 锁」 |
| `ExecuteTemplateBatch` 跨 journal 无全局锁序 | **半修** —— pool 模式（`postgres/ledger_store.go:602-631`）真加了 idem 排序 + `sortedUniquePairs(allPairs)` 一次取完；**tx 模式（`:656-676`）原样未修** | ❌ tx 模式没接 | ❌ pin 根本不调 `ExecuteTemplateBatch`（Major #4） | ⚠️ **同形缺陷已存在于 `pending_store`**（Major #1）——同一次修复定下的「一个 canonical order 不花钱」原则没有横向扫过其余自取 balance lock 的调用点 |
| `Reserve(RequireVerifiedBalance:true)` 事务内外部调用 | **半修** —— `postgres/reserver_store.go:123-128` fail closed（`s.pool == nil` → `ErrInvalidInput`，错误文案指路「call before RunInTx」） | ✅ `Reserve` 这个入口真守住了 | 未拆验 | ❌ **同一外部调用的另一个入口没守**：`Service.VerifiedBalanceReader()`（`ledger.go:559`）在 clone 上返回 tx-bound store，`VerifiedBalance`（`postgres/verified_balance_store.go:159`）无守卫 → Major #2 |
| `registration_rescans` 无 claim-token guard | **真修** —— `postgres/registration_rescan_store.go:93-107` / `:110-124` 都带 `AND attempts = $4`；`ClaimRegistrationRescans`（`:56`）每次 claim `attempts = attempts + 1` 充当 token；`RowsAffected()==0` → `pgx.ErrNoRows` 告知调用方写入未生效 | ✅ `service/onchain.go:565` 的失败分支传 `job.Attempts` | 未拆验 | ✅ 三处 claim 机制（rollup / events / rescans）现在都有 token |
| `expiration` 无 leader 选举 | **真修** —— `service/worker.go:290-307` 包进 `NewLockedJob("expiration", ...)`，`ledger.go:848` `w.SetPool(s.pool)` 真接了池；`service/worker_expiration_test.go:74-92` 用真实 `pg_try_advisory_lock` 占锁验证跳过 | ✅ | ✅（该测试驱动真实 `Worker.Run` 分支并对着真锁断言） | ⚠️ 「过期能在 in-flight Settle 下抽走冻结」这半条**靠幂等键改善了归因**（现在能区分「我重放了」与「被过期回收了」），但**冻结仍会被抽走**——`reservationDefaultExpiresIn = 15m`（`postgres/reserver_store.go:317`）不变，超时业务的 `Settle` 仍会拿到 `ErrInvalidTransition`。这是产品语义问题，不是并发缺陷，我不重复报 |
| `mergeWorkerConfig` 漏 `AttestInterval` / `AttestBatchSize` | **真修** —— `ledger.go:865-916` 十六个字段全覆盖，含 `AttestInterval`（`:895`）与 `AttestBatchSize`（`:898`） | ✅ `ledger.go:856-858` 还自动接了 `SetAttestor` | 未拆验 | ⚠️ 形状仍是「每字段一个 if」，加第十七个字段照样会漏。可用反射断言「合并后无零值字段」，属 F |
| `ConfirmPending`/`CancelPending` 永不签名 | **真修** —— `postgres/pending_store.go:283-297`：pool 模式 `s.ledger.Authorize(ctx, input)` 严格在 `pool.Begin` **之前**，之后 `PostAuthorized`；tx 模式明确保留 `unsigned_tx_mode` 并在 doc 里说明 | ✅ | 未拆验 | ⚠️ 这次修复引入的**新**缺陷正是 Major #1（预锁位置）——修签名的时候把 balance lock 提到了 `PostJournal` 之前，却只锁了一个 pair |
| `RebuildCheckpoint` 被它要修的东西永久挡住 | **真修** —— `postgres/sql/queries/integrity_checkpoint.sql:57` 加了 `AND failed_attempts < 10`，与 `DequeueRollupBatch`（`checkpoints.sql:93`）口径一致，注释直接引用了上轮这条 Minor | ✅ | 未拆验 | ⚠️ 遗留：`failed_attempts >= 10` 的队列项**仍然没有任何 API 能 drain 或复位**，且 `CountPendingRollups`（`checkpoints.sql:129-130`，喂 `s.metrics.PendingRollups`）**不带这个过滤**，所以它会永久 > 0 —— 一个永远告警的指标。移交 I |
| `event_store` claim-lost 用包级 `slog` | **半修** —— 加了 `SetLogger`，**但全仓零调用点**（Minor #1） | ❌ | — | — |
| `cleanupContext` | **真修且铺开** —— `service/locked_job.go:114`、`service/snapshot.go:106`、`service/rollup.go:104/126`、`service/onchain.go:562` | ✅ | `service/locked_job_integration_test.go:46-48` 断言跑完后 `pg_locks` 里 `locktype='advisory'` 计数为 0——真实断言 | ❌ `service/delivery/` 三处没铺（Minor #2） |
| `Migrate` 集群级序列化（I-47） | **真修** —— `postgres/migrate.go:129-152` 在集群 `postgres` 维护库上取 session 级 `pg_advisory_lock`，覆盖整个 `Up()`；I-47 的 pin `TestMigrate_ConcurrentAcrossDatabases` 并发装 8 个库 | ✅ `postgres.Migrate` 是唯一入口，`ledger.Migrate`（`ledger.go:620`）只是转发 | 未拆验（I-47 自述「fails reliably before this fix」） | ⚠️ 两条遗留：注释过时（Minor #3）、无超时（Minor #4） |

**关于「`Migrate` 的锁与其他锁是否同键空间」（lead 点名）**：**不同空间，但理由不是那段注释写的。**
`clusterMigrationLockKey = 2573143714` 是 `postgres` 维护库上的锁；`bal:`/`idem:`（`pg_advisory_xact_lock`）
与 `job:`/`snapshot:`（FNV-64a，`service/snapshot.go:26`）都在**应用库**上。PostgreSQL 的 advisory lock
按 database 隔离（I-47 的整个论证就建立在这条性质上），所以两组 key 物理上不可能相遇——
**除非把账本装进 `postgres` 维护库本身**，那时它们就共享同一个 64-bit 空间了（碰撞概率可忽略，
但「阻塞式 session 锁不可能成环」这个断言会失效）。

**关于「两个不同 database 的 Migrate 是否真被序列化、以及该不该」**：**是，且应该。**
`001_baseline` 与 `007` 写 `pg_authid` / `pg_auth_members`——集群级共享目录，
不按 database 隔离，所以跨库并发确实会撞（I-47 记录的实测是 `tuple concurrently updated`）。
代价是跨项目的 `ledger.Migrate` 互相排队（Aaron 的共享 `dev-postgres` 上尤其明显），
这是正确的取舍；真正的问题是**排队没有上限**（Minor #4）。

---

## 移交

- **A（financial-correctness）**：`ClosePeriod` 的 TOCTOU（我的 Major #5）会让一笔
  `effective_at` 早于关账线的 journal 落地。请从「历史报表可否被事后改动」的角度评估
  资金层面的影响，以及是否值得补一条对账检查（目前 `service/reconcile.go` 里**一条都没有**）。
- **A**：`service/rollup.go:169` 的 `SumEntriesSince` 是按 `(holder, currency)` 汇总的，
  但 checkpoint 的 `LastEntryID` 写的是**跨全部 classification** 的 max
  （`service/rollup.go:270`）。我论证过在「每个 journal 写入前都持 `bal(holder,currency)`
  advisory lock」的前提下这仍然安全（同维度 entry id 顺序 == 提交顺序，无序列空洞），
  且 `postgres/rollup_adapter.go:196` 用 REPEATABLE READ 把两次读钉在同一快照上——
  但这个安全性**完全依赖那个前提**，而它只写在注释里（`postgres/ledger_store.go:962-966`）。
  建议 A 从数学侧确认我的推理，并考虑是否值得一条机器断言。
- **C（tamper-evident）**：Major #2（`VerifiedBalance` 在事务内调 `AuthVerifier`）同时是
  提现闸的正确用法边界问题；防篡改覆盖面的完整清单请 C 收口。另：
  `service/attestation.go:195` 的 `Attestor.Sign` 与 `:227` 的 `Anchor.Publish` 我都核过
  **不在任何 pgx 事务内**（`InsertAttestation` 自开自提交），这条红线守住了。
- **D（threat-model）**：`server/handler_webhooks.go:53-62` 的 nonce 是
  `sha256(channel ‖ X-Timestamp ‖ X-Signature ‖ body)`，`TryRecordNonce`
  （`postgres/webhook_subscriber_store.go:84-93`）是 `INSERT ... ON CONFLICT` 原子操作，
  并发重放安全。但 `DeleteExpiredWebhookNonces` 的 42501 容忍（`:85`）意味着
  **没有 DELETE 权限的部署里这张表只增不减**——replay cache 无界增长，属你的攻击面。
- **E（consumer-surface）**：`RunInTx` 的 doc（`ledger.go:459-482`）逐条列了哪些方法在
  clone 上被拒，**`Worker` 和 `VerifiedBalanceReader` 都不在列，而它们都该在**（我的 Major #2/#6）。
- **F（test-credibility）**：① `postgres/lock_order_test.go:172-222` 是一条不驱动被测实现的
  pin（Major #4），建议加门禁「测试若在 doc 里声称钉住某导出符号，其源码必须出现该符号」；
  ② `postgres/registration_rescan_store.go` 仍是手写 raw SQL 字符串（`:34`、`:57`、`:97`、`:117`），
  全仓其他 store 都走 sqlc `.sql` 文件——上轮就报过，没改；
  ③ `mergeWorkerConfig`（`ledger.go:865`）的「每字段一个 if」可用反射断言替代。
- **G（onchain-money-path）**：`service/onchain.go:1246-1250` —— `IngestDeposit` 失败只
  log 不中断，`:1252` 照样推进 cursor。单副本下这是一笔充值被永久跳过（fail-open），
  且只有 `ErrConflict` 会进 dead letter（`:686-690`），其他错误（DB 抖动、超时）**什么都不留**。
  这是我这轮看到的最像「丢钱」的东西，但属你的 territory。
- **I（operability）**：① `CountPendingRollups`（`postgres/sql/queries/checkpoints.sql:129-130`）
  不排除 `failed_attempts >= 10`，而这类项永远不会被 dequeue → `PendingRollups` 指标
  永久 > 0 = 永远告警；且**没有任何 API 能 drain 或复位它们**。
  ② `acquireClusterLock` 无超时且无日志（Minor #4），启动期卡死无诊断。
  ③ `Migrate` 现在硬依赖 `postgres` 维护库的 CONNECT。
  ④ event delivery 在关停窗口会集中重投（Minor #2）。

---

## 我没能验证的

1. **Major #6（`Worker()` 逃逸）的运行期症状没有实跑。** 接线事实是代码级 CONFIRMED
   （`ledger.go:839` 在 clone 上取的就是 tx-bound store），但「过期 reservation 从此不再被回收、
   且只留一条 Error 日志」这个具体表现我是从 `service/worker.go:298-301` 推的，没构造。
2. **Major #3（tx 模式 batch 锁序）没有端到端实跑。** 我实跑的是 `ConfirmPending` 那条
   （Major #1）。tx 模式 batch 的锁序缺失是从 `postgres/ledger_store.go:656-676` 直接读出来的，
   死锁机制本身由既有的 `TestAcquireBalanceLocks_RealDeadlock_WrapsErrTransient` 钉住——
   两者拼起来是可靠的，但不是一次实跑。
3. **Major #5（关账 TOCTOU）没有实跑。** 两侧都不取锁、隔离级别是 READ COMMITTED，
   这两点都是代码级确定的，但我没有真的构造出一笔越线 journal。
   **建议 lead 优先实跑这一条**——它便宜（两个事务 + 一条 SQL 计数），
   而且它推翻的是一条被写成全称命题的 invariant。
4. **`LockedJob` 的锁丢失（连接断开）后果我只核到「有没有 DB 层兜底」这一层。**
   attestation 有（`ledger_attestations.seq` UNIQUE + `entry_attestations` PK on `entry_id`，
   `001_baseline.up.sql:1000/1021`，双跑会报错而非污染链）；expiration 有（幂等 receipt）；
   snapshot / system_rollup 是 upsert；partition 是 DDL。**唯独 sweep 没有**——
   `newSweepLockedJob`（`service/onchain.go:1997-2007`）的注释自己说「两个 sweepTick 抢同一个
   nonce 序列会各自读到同一个 next nonce、在同一 nonce 上广播两笔、一笔静默覆盖另一笔」，
   而 advisory lock 是唯一的防线，且 `LockedJob.Run`（`service/locked_job.go:96-127`）
   **在 fn 执行期间从不复核自己是否还持有锁**。我没能评估 pgx pool 在连接被服务端切断时
   `conn.Release()` 的行为，也没构造这个场景。资金后果移交 G。
5. **没有跑全量测试套件。** 只跑了 `./postgres/` 的两个 `-run` 过滤子集
   （我的临时探针、以及 Reserve 变异复核）。原因：审计期间工作树被其他 agent 的
   mutation test 反复污染（见摘要的流程告警），全量跑的结果不可信。
6. **`server/middleware_ratelimit.go` 与 `middleware_body.go` 的并发面没看**
   （限流器的共享状态、body 缓冲的复用）。时间用在了锁序与事务边界上。
7. **`anchors/r2` 子模块的并发面没看**（`Publish` 的并发安全性——I-48 明说 port
   不承诺并发安全，但 R2 实现是否真的只被单个重试队列调用，我没追）。移交 C。
8. **没有评估 `deadlock_timeout` 在生产配置下的实际值。** 本报告里所有「秒级延迟尖刺」
   的说法基于 Postgres 默认的 `1s`，我没有查任何部署侧配置（本仓也不再有部署面）。
