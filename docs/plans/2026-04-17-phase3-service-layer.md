# Phase 3: Service Layer — Rollup, Reconciliation, Snapshots

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build the business orchestration layer that runs background jobs (rollup, expiration cleanup) and provides reconciliation + snapshot services.

**Architecture:** Services compose core interfaces + store implementations. Each service is independently testable with mock stores.

**Tech Stack:** Go 1.25+, errgroup (concurrent rollup)

**Depends on:** Phase 1 (core), Phase 2 Tasks 1-5 (store basics)

**Can parallelize with:** Phase 4 (HTTP API — after this Phase's Tasks 1-2)

---

### Task 1: RollupService

**Files:**
- Create: `service/rollup.go`
- Test: `service/rollup_test.go`

**Responsibilities:**
- `ProcessBatch(ctx, batchSize)`: dequeue from rollup_queue → for each item: get checkpoint → sum entries since → compute new balance (respecting normal_side) → upsert checkpoint → mark processed
- Emit metrics: `RollupProcessed`, `RollupLatency`, `PendingRollups`, `CheckpointAge`, `BalanceDrift`
- Log warnings on drift detection

**Tests:**
- Process single rollup item → checkpoint updated correctly
- Debit-normal vs credit-normal balance computation
- Empty queue → returns 0, no error
- Drift detection → metrics emitted

**Commit:** `feat(service): implement RollupService with drift detection`

---

### Task 2: ReconciliationService

**Files:**
- Create: `service/reconcile.go`
- Test: `service/reconcile_test.go`

**Responsibilities:**
- `CheckAccountingEquation(ctx)`: SUM(all debits) == SUM(all credits) across all journals. Also verify per-currency: Assets = Liabilities + Equity + Revenue (requires classification → category mapping)
- `ReconcileAccount(ctx, holder, currencyID)`: checkpoint balance vs actual SUM(entries) for each classification
- Emit metrics: `ReconcileCompleted`, `ReconcileGap`

**Tests:**
- Balanced system → result.Balanced = true, gap = 0
- Inject imbalance → result.Balanced = false, gap reported
- Per-account reconcile detects checkpoint drift

**Commit:** `feat(service): implement ReconciliationService with accounting equation check`

---

### Task 3: SnapshotService

**Files:**
- Create: `service/snapshot.go`
- Test: `service/snapshot_test.go`

**Responsibilities:**
- `CreateDailySnapshot(ctx, date)`: read all balance_checkpoints → insert into balance_snapshots for the given date
- `GetSnapshotBalance(ctx, holder, currencyID, date)`: read from balance_snapshots
- Emit metrics: `SnapshotLatency`

**Tests:**
- Create snapshot → query returns correct balances
- Duplicate snapshot for same date → idempotent (ON CONFLICT DO NOTHING)
- Query non-existent date → empty result, no error

**Commit:** `feat(service): implement SnapshotService for daily balance snapshots`

---

### Task 4: SystemRollupService

**Files:**
- Create: `service/system_rollup.go`
- Test: `service/system_rollup_test.go`

**Responsibilities:**
- `RefreshSystemRollups(ctx)`: aggregate balance_checkpoints by (currency_id, classification_id) → upsert system_rollups
- Provides O(1) platform-wide balance queries

**Tests:**
- Multiple accounts → aggregated correctly
- Refresh after new journal → updated

**Commit:** `feat(service): implement SystemRollupService for platform-wide balance aggregation`

---

### Task 5: ExpirationService

**Files:**
- Create: `service/expiration.go`
- Test: `service/expiration_test.go`

**Responsibilities:**
- `ExpireStaleReservations(ctx, batchSize)`: find expired active reservations → release each (posts reversal journal)
- `ExpireStaleDeposits(ctx, batchSize)`: find expired pending/confirming deposits → fail each
- `ExpireStaleWithdrawals(ctx, batchSize)`: find expired processing withdrawals → fail each

**Tests:**
- Active reservation past expires_at → released + journal posted
- Non-expired reservation → untouched
- Expired deposit → status = expired, Suspense/Pending reversed

**Commit:** `feat(service): implement ExpirationService for stale reservation/deposit/withdrawal cleanup`

---

### Task 6: Worker — Background Job Runner

**Files:**
- Create: `service/worker.go`
- Test: `service/worker_test.go`

**Responsibilities:**
- Runs RollupService, ExpirationService, ReconciliationService, SnapshotService, SystemRollupService on configurable intervals
- Uses `errgroup` with `ctx.Done()` exit path for each goroutine
- Configurable via `WorkerConfig`:
  ```go
  type WorkerConfig struct {
      RollupInterval     time.Duration  // default: 5s
      RollupBatchSize    int            // default: 100
      ExpirationInterval time.Duration  // default: 30s
      ReconcileCron      string         // default: "0 */6 * * *"
      SnapshotCron       string         // default: "0 2 * * *"
      SystemRollupInterval time.Duration // default: 1m
  }
  ```

**Tests:**
- Worker starts and stops cleanly on context cancellation
- Rollup runs at configured interval

**Commit:** `feat(service): implement Worker background job runner with configurable intervals`
