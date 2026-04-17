# Phase 2: Postgres Adapter — Store Implementation

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Implement all core interfaces in the PostgreSQL adapter using pgx+sqlc. This is the data layer that makes journals, balances, reservations, deposits, and withdrawals work against a real database.

**Architecture:** Each store implements one or more core interfaces. All writes are transactional. Reserve uses `pg_advisory_xact_lock`. Rollup queue uses `SKIP LOCKED`.

**Tech Stack:** Go 1.25+, pgx v5, sqlc, testcontainers-go (for integration tests)

**Depends on:** Phase 1 (core types + interfaces + schema)

**Can parallelize with:** Phase 3 (service layer — after Task 1-3 here are done)

---

### Task 1: sqlc Queries — Journals + Entries

**Files:**
- Create: `postgres/sql/queries/journals.sql`
- Generate: `postgres/sqlcgen/` (via `sqlc generate`)

**Step 1: Write queries**

```sql
-- postgres/sql/queries/journals.sql

-- name: InsertJournal :one
INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, reversal_of)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetJournal :one
SELECT * FROM journals WHERE id = $1;

-- name: GetJournalByIdempotencyKey :one
SELECT * FROM journals WHERE idempotency_key = $1;

-- name: ListJournals :many
SELECT * FROM journals
WHERE created_at < @cursor_at::timestamptz
ORDER BY created_at DESC
LIMIT @page_limit;

-- name: InsertJournalEntry :one
INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetEntriesByJournalID :many
SELECT * FROM journal_entries WHERE journal_id = $1 ORDER BY id;

-- name: ListEntriesByAccount :many
SELECT * FROM journal_entries
WHERE account_holder = $1 AND currency_id = $2
  AND id > @cursor_id
ORDER BY id ASC
LIMIT @page_limit;

-- name: SumEntriesSinceCheckpoint :many
SELECT
  account_holder,
  currency_id,
  classification_id,
  entry_type,
  SUM(amount) as total
FROM journal_entries
WHERE account_holder = $1
  AND currency_id = $2
  AND id > @since_entry_id
GROUP BY account_holder, currency_id, classification_id, entry_type;
```

**Step 2: Generate**

```bash
cd /Users/aaron/azex/ledger/postgres && sqlc generate
```

**Step 3: Verify compilation**

```bash
cd /Users/aaron/azex/ledger && go build ./...
```

**Step 4: Commit**

```bash
git add postgres/ && git commit -m "feat(postgres): add journal + entry sqlc queries"
```

---

### Task 2: sqlc Queries — Classifications, JournalTypes, Templates, Currencies

**Files:**
- Create: `postgres/sql/queries/classifications.sql`
- Create: `postgres/sql/queries/journal_types.sql`
- Create: `postgres/sql/queries/templates.sql`
- Create: `postgres/sql/queries/currencies.sql`

**Step 1: Write queries**

```sql
-- postgres/sql/queries/classifications.sql

-- name: InsertClassification :one
INSERT INTO classifications (code, name, normal_side, is_system)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: DeactivateClassification :exec
UPDATE classifications SET is_active = false WHERE id = $1;

-- name: ListClassifications :many
SELECT * FROM classifications WHERE (is_active = true OR @include_inactive::bool) ORDER BY code;

-- name: GetClassificationByCode :one
SELECT * FROM classifications WHERE code = $1;

-- name: GetClassificationByID :one
SELECT * FROM classifications WHERE id = $1;
```

```sql
-- postgres/sql/queries/journal_types.sql

-- name: InsertJournalType :one
INSERT INTO journal_types (code, name) VALUES ($1, $2) RETURNING *;

-- name: DeactivateJournalType :exec
UPDATE journal_types SET is_active = false WHERE id = $1;

-- name: ListJournalTypes :many
SELECT * FROM journal_types WHERE (is_active = true OR @include_inactive::bool) ORDER BY code;
```

```sql
-- postgres/sql/queries/templates.sql

-- name: InsertTemplate :one
INSERT INTO entry_templates (code, name, journal_type_id) VALUES ($1, $2, $3) RETURNING *;

-- name: InsertTemplateLine :one
INSERT INTO entry_template_lines (template_id, classification_id, entry_type, holder_role, amount_key, sort_order)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: DeactivateTemplate :exec
UPDATE entry_templates SET is_active = false WHERE id = $1;

-- name: GetTemplateByCode :one
SELECT * FROM entry_templates WHERE code = $1;

-- name: GetTemplateLines :many
SELECT * FROM entry_template_lines WHERE template_id = $1 ORDER BY sort_order;

-- name: ListTemplates :many
SELECT * FROM entry_templates WHERE (is_active = true OR @include_inactive::bool) ORDER BY code;
```

```sql
-- postgres/sql/queries/currencies.sql

-- name: InsertCurrency :one
INSERT INTO currencies (code, name) VALUES ($1, $2) RETURNING *;

-- name: ListCurrencies :many
SELECT * FROM currencies ORDER BY code;

-- name: GetCurrency :one
SELECT * FROM currencies WHERE id = $1;
```

**Step 2: Generate + verify**

```bash
cd /Users/aaron/azex/ledger/postgres && sqlc generate && cd .. && go build ./...
```

**Step 3: Commit**

```bash
git add postgres/ && git commit -m "feat(postgres): add classification, template, currency sqlc queries"
```

---

### Task 3: sqlc Queries — Checkpoints, Rollup, Reservations, Deposits, Withdrawals, Snapshots

**Files:**
- Create: `postgres/sql/queries/checkpoints.sql`
- Create: `postgres/sql/queries/reservations.sql`
- Create: `postgres/sql/queries/deposits.sql`
- Create: `postgres/sql/queries/withdrawals.sql`
- Create: `postgres/sql/queries/snapshots.sql`

**Step 1: Write queries**

```sql
-- postgres/sql/queries/checkpoints.sql

-- name: GetBalanceCheckpoint :one
SELECT * FROM balance_checkpoints
WHERE account_holder = $1 AND currency_id = $2 AND classification_id = $3;

-- name: UpsertBalanceCheckpoint :exec
INSERT INTO balance_checkpoints (account_holder, currency_id, classification_id, balance, last_entry_id, last_entry_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (account_holder, currency_id, classification_id)
DO UPDATE SET balance = $4, last_entry_id = $5, last_entry_at = $6, updated_at = now();

-- name: GetBalanceCheckpoints :many
SELECT * FROM balance_checkpoints
WHERE account_holder = $1 AND currency_id = $2;

-- name: BatchGetBalanceCheckpoints :many
SELECT * FROM balance_checkpoints
WHERE account_holder = ANY(@holder_ids::bigint[]) AND currency_id = $1;

-- name: EnqueueRollup :exec
INSERT INTO rollup_queue (account_holder, currency_id, classification_id)
VALUES ($1, $2, $3)
ON CONFLICT (account_holder, currency_id, classification_id) DO NOTHING;

-- name: DequeueRollupBatch :many
SELECT * FROM rollup_queue
WHERE processed_at IS NULL
ORDER BY created_at
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: MarkRollupProcessed :exec
UPDATE rollup_queue SET processed_at = now() WHERE id = $1;

-- name: CountPendingRollups :one
SELECT COUNT(*) FROM rollup_queue WHERE processed_at IS NULL;

-- name: GetMaxEntryID :one
SELECT COALESCE(MAX(id), 0)::bigint as max_id FROM journal_entries;
```

```sql
-- postgres/sql/queries/reservations.sql

-- name: InsertReservation :one
INSERT INTO reservations (account_holder, currency_id, reserved_amount, idempotency_key, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetReservation :one
SELECT * FROM reservations WHERE id = $1;

-- name: GetReservationForUpdate :one
SELECT * FROM reservations WHERE id = $1 FOR UPDATE;

-- name: UpdateReservationStatus :exec
UPDATE reservations SET status = $2, updated_at = now() WHERE id = $1;

-- name: UpdateReservationSettle :exec
UPDATE reservations SET status = 'settled', settled_amount = $2, journal_id = $3, updated_at = now() WHERE id = $1;

-- name: ListReservations :many
SELECT * FROM reservations
WHERE account_holder = $1 AND (status = @filter_status OR @filter_status = '')
ORDER BY created_at DESC
LIMIT @page_limit;

-- name: GetExpiredReservations :many
SELECT * FROM reservations WHERE status = 'active' AND expires_at < now() LIMIT $1;

-- name: CountActiveReservations :one
SELECT COUNT(*) FROM reservations WHERE status = 'active';
```

```sql
-- postgres/sql/queries/deposits.sql

-- name: InsertDeposit :one
INSERT INTO deposits (account_holder, currency_id, expected_amount, channel_name, idempotency_key, metadata, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetDeposit :one
SELECT * FROM deposits WHERE id = $1;

-- name: GetDepositForUpdate :one
SELECT * FROM deposits WHERE id = $1 FOR UPDATE;

-- name: UpdateDepositStatus :exec
UPDATE deposits SET status = $2, updated_at = now() WHERE id = $1;

-- name: UpdateDepositConfirm :exec
UPDATE deposits SET status = 'confirmed', actual_amount = $2, channel_ref = $3, journal_id = $4, updated_at = now() WHERE id = $1;

-- name: GetDepositByChannelRef :one
SELECT * FROM deposits WHERE channel_ref = $1;

-- name: ListDeposits :many
SELECT * FROM deposits
WHERE account_holder = $1
ORDER BY created_at DESC
LIMIT @page_limit;

-- name: GetExpiredDeposits :many
SELECT * FROM deposits WHERE status IN ('pending', 'confirming') AND expires_at IS NOT NULL AND expires_at < now() LIMIT $1;
```

```sql
-- postgres/sql/queries/withdrawals.sql

-- name: InsertWithdrawal :one
INSERT INTO withdrawals (account_holder, currency_id, amount, channel_name, idempotency_key, metadata, review_required, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetWithdrawal :one
SELECT * FROM withdrawals WHERE id = $1;

-- name: GetWithdrawalForUpdate :one
SELECT * FROM withdrawals WHERE id = $1 FOR UPDATE;

-- name: UpdateWithdrawalStatus :exec
UPDATE withdrawals SET status = $2, updated_at = now() WHERE id = $1;

-- name: UpdateWithdrawalProcess :exec
UPDATE withdrawals SET status = 'processing', channel_ref = $2, updated_at = now() WHERE id = $1;

-- name: UpdateWithdrawalReservation :exec
UPDATE withdrawals SET reservation_id = $2, status = 'reserved', updated_at = now() WHERE id = $1;

-- name: UpdateWithdrawalConfirm :exec
UPDATE withdrawals SET status = 'confirmed', journal_id = $2, updated_at = now() WHERE id = $1;

-- name: ListWithdrawals :many
SELECT * FROM withdrawals
WHERE account_holder = $1
ORDER BY created_at DESC
LIMIT @page_limit;
```

```sql
-- postgres/sql/queries/snapshots.sql

-- name: InsertSnapshot :exec
INSERT INTO balance_snapshots (account_holder, currency_id, classification_id, snapshot_date, balance)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (account_holder, currency_id, classification_id, snapshot_date) DO NOTHING;

-- name: GetSnapshotBalances :many
SELECT * FROM balance_snapshots
WHERE account_holder = $1 AND currency_id = $2 AND snapshot_date = $3;

-- name: ListSnapshotsByDateRange :many
SELECT * FROM balance_snapshots
WHERE account_holder = $1 AND currency_id = $2
  AND snapshot_date BETWEEN @start_date AND @end_date
ORDER BY snapshot_date;

-- name: UpsertSystemRollup :exec
INSERT INTO system_rollups (currency_id, classification_id, total_balance)
VALUES ($1, $2, $3)
ON CONFLICT (currency_id, classification_id)
DO UPDATE SET total_balance = $3, updated_at = now();

-- name: GetSystemRollups :many
SELECT * FROM system_rollups ORDER BY currency_id, classification_id;
```

**Step 2: Generate + verify**

```bash
cd /Users/aaron/azex/ledger/postgres && sqlc generate && cd .. && go build ./...
```

**Step 3: Commit**

```bash
git add postgres/ && git commit -m "feat(postgres): add checkpoint, reservation, deposit, withdrawal, snapshot queries"
```

---

### Task 4: Test Infrastructure — testcontainers

**Files:**
- Create: `postgres/testutil_test.go`

**Step 1: Set up test helper**

```go
// postgres/testutil_test.go
package postgres_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	ledgerpg "github.com/azex-ai/ledger/postgres"
)

func setupTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:17",
		tcpostgres.WithDatabase("ledger_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithLogger(testcontainers.TestLogger(t)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { container.Terminate(ctx) })

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	err = ledgerpg.Migrate(ctx, pool)
	require.NoError(t, err)

	return pool
}
```

**Step 2: Add dependency**

```bash
cd /Users/aaron/azex/ledger
go get github.com/testcontainers/testcontainers-go
go get github.com/testcontainers/testcontainers-go/modules/postgres
go mod tidy
```

**Step 3: Commit**

```bash
git add . && git commit -m "test(postgres): add testcontainers test infrastructure"
```

---

### Task 5: LedgerStore — PostJournal + GetBalance

**Files:**
- Create: `postgres/ledger_store.go`
- Test: `postgres/ledger_store_test.go`

This is the most critical store — implements `JournalWriter` and `BalanceReader`.

**Step 1: Write failing integration test**

```go
// postgres/ledger_store_test.go
package postgres_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

func TestLedgerStore_PostJournal_Balanced(t *testing.T) {
	pool := setupTestDB(t)
	store := NewLedgerStore(pool)
	ctx := context.Background()

	// Seed classification + journal type + currency
	// ... (seed helpers)

	input := core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: "test-001",
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyID: curID, ClassificationID: clsDebitID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsCreditID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	}

	journal, err := store.PostJournal(ctx, input)
	require.NoError(t, err)
	assert.True(t, journal.TotalDebit.Equal(decimal.NewFromInt(100)))

	// Verify idempotency — same key returns same journal
	journal2, err := store.PostJournal(ctx, input)
	require.NoError(t, err)
	assert.Equal(t, journal.ID, journal2.ID)
}

func TestLedgerStore_GetBalance(t *testing.T) {
	pool := setupTestDB(t)
	store := NewLedgerStore(pool)
	ctx := context.Background()

	// Post a journal, then check balance
	// Balance = checkpoint(0) + SUM(entries) = 100
	bal, err := store.GetBalance(ctx, 1, curID, clsDebitID)
	require.NoError(t, err)
	assert.True(t, bal.Equal(decimal.NewFromInt(100)))
}
```

**Step 2: Implement LedgerStore**

Key implementation details:
- `PostJournal`: BEGIN TX → check idempotency key → insert journal → insert entries → enqueue rollup → COMMIT
- `GetBalance`: get checkpoint → SUM entries since checkpoint → compute based on normal_side
- Compile-time interface assertion: `var _ core.JournalWriter = (*LedgerStore)(nil)`

**Step 3: Run integration tests**

```bash
cd /Users/aaron/azex/ledger && go test ./postgres/ -v -run TestLedgerStore -count=1
```

**Step 4: Commit**

```bash
git add postgres/ && git commit -m "feat(postgres): implement LedgerStore — PostJournal + GetBalance"
```

---

### Task 6: ReserverStore — Reserve / Settle / Release

**Files:**
- Create: `postgres/reserver_store.go`
- Test: `postgres/reserver_store_test.go`

Key implementation: `pg_advisory_xact_lock(account_holder)` for concurrency.

**Tests must cover:**
- Happy path: reserve → settle
- Reserve → release
- Idempotency on reserve
- Insufficient balance rejection
- Concurrent reserve on same account (advisory lock serialization)
- Expire stale reservations

**Commit:** `feat(postgres): implement ReserverStore — Reserve/Settle/Release with advisory lock`

---

### Task 7: DepositStore + WithdrawStore

**Files:**
- Create: `postgres/deposit_store.go`
- Create: `postgres/withdraw_store.go`
- Test: `postgres/deposit_store_test.go`
- Test: `postgres/withdraw_store_test.go`

**Deposit tests must cover:**
- Full happy path: init → confirming → confirm (actual amount)
- Amount mismatch: expected 100, actual 95 → Suspense adjustment
- Channel ref idempotency (duplicate confirm → return success, no double entry)
- State machine enforcement (confirmed → pending = error)

**Withdraw tests must cover:**
- Happy path: init → reserve → review → process → confirm
- Skip review: init → reserve → process (review_required=false)
- Retry: process → fail → retry → reserve
- Expired cannot retry

**Commit:** `feat(postgres): implement DepositStore + WithdrawStore with state machine enforcement`

---

### Task 8: MetadataStore — Classifications, JournalTypes, Templates, Currencies

**Files:**
- Create: `postgres/classification_store.go`
- Create: `postgres/template_store.go`
- Create: `postgres/currency_store.go`
- Test: `postgres/metadata_store_test.go`

**Tests must cover:**
- CRUD for each entity
- Deactivation prevents new journal usage
- Deactivated entities still appear in history queries

**Commit:** `feat(postgres): implement metadata stores — Classification, JournalType, Template, Currency`

---

### Task 9: Integration Test — Full End-to-End Flow

**Files:**
- Create: `postgres/integration_test.go`

**Test scenario:**
1. Create currency (USDT), classifications (MainWallet, Locked, Custodial, Suspense, Pending, Fees)
2. Create journal type (deposit_confirm) + template
3. Execute deposit: init → confirming → confirm via template
4. Verify balances across all classifications
5. Reserve → Settle flow
6. Verify rollup queue populated
7. Run reconciliation check (accounting equation)

**Commit:** `test(postgres): add end-to-end integration test covering full ledger flow`
