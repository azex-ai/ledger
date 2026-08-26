package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// TestJournalEntries_DuplicateIDAcrossPartitions_SplitsBooks verifies (does
// not fix -- the fix is a schema/DB-role change outside this task's file
// ownership) the PLAUSIBLE consequence half of a Minor from the 2026-08-25
// financial-engineering audit
// (postgres/sql/migrations/001_baseline.up.sql:325-338;
// financial-correctness.md): journal_entries' primary key is
// (id, created_at), not id alone, because a partitioned table's PK must
// include the partition key (created_at). That means id is only guaranteed
// unique *within* a partition (month), not across the whole table.
//
// This test confirms the claimed consequence is real, not merely plausible:
// under the audit's already-established threat model (a raw SQL write using
// the ledger_app credential -- see docs/audits/.../threat-model.md and C2's
// disposition), an attacker can INSERT a balanced two-entry pair whose ids
// duplicate an already-used pair in a different month's partition. Because
// every per-account balance path filters strictly on `id > checkpoint.
// last_entry_id`, a duplicated id that is <= the current watermark is
// invisible to GetBalance forever, while SumGlobalDebitCreditByCurrency (which
// sums every row, not id-filtered) counts it -- and because the forged pair
// is itself internally balanced, the global debit==credit sanity check does
// not flag anything either. The two views of the same ledger permanently
// disagree without either individually looking wrong.
//
// This is a verification pin, not a fix: preventing raw-SQL id forgery is a
// DB-role/grant hardening concern (see D-threat's task in
// docs/plans/2026-08-26-audit-remediation-contracts.md), not something
// expressible in postgres/sql/queries/checkpoints.sql or platform_balances.sql
// (this task's file ownership).
func TestJournalEntries_DuplicateIDAcrossPartitions_SplitsBooks(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	adapter := postgres.NewRollupAdapter(pool)

	curID := postgrestest.SeedCurrency(t, pool, "USDT-IDDUP", "Tether USD ID Dup")
	jtID := postgrestest.SeedJournalType(t, pool, "transfer_iddup", "Transfer ID Dup")
	wallet := postgrestest.SeedClassification(t, pool, "wallet_iddup", "Wallet ID Dup", "debit", false)
	custodial := postgrestest.SeedClassification(t, pool, "custodial_iddup", "Custodial ID Dup", "credit", true)

	holder := int64(9302)

	// 1. A normal, legitimate journal: wallet +100 / custodial +100.
	j, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("iddup-anchor"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	})
	require.NoError(t, err)

	var journalID, currencyID, walletID, custodialID int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM journals WHERE uid = $1::uuid`, j.UID).Scan(&journalID))
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM currencies WHERE uid = $1::uuid`, curID).Scan(&currencyID))
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM classifications WHERE uid = $1::uuid`, wallet).Scan(&walletID))
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM classifications WHERE uid = $1::uuid`, custodial).Scan(&custodialID))

	var debitEntryID, creditEntryID int64
	rows, err := pool.Query(ctx, `SELECT id, entry_type FROM journal_entries WHERE journal_id = $1 ORDER BY id`, journalID)
	require.NoError(t, err)
	for rows.Next() {
		var id int64
		var entryType string
		require.NoError(t, rows.Scan(&id, &entryType))
		if entryType == "debit" {
			debitEntryID = id
		} else {
			creditEntryID = id
		}
	}
	rows.Close()
	require.NotZero(t, debitEntryID)
	require.NotZero(t, creditEntryID)

	// 2. Simulate the rollup worker having already materialized a checkpoint
	// covering this anchor journal -- last_entry_id == debitEntryID, the
	// legitimate current watermark for (holder, currency, wallet).
	require.NoError(t, adapter.UpsertCheckpoint(ctx, service.BalanceCheckpoint{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: walletID,
		Balance:          decimal.NewFromInt(100),
		LastEntryID:      debitEntryID,
		LastEntryAt:      time.Now(),
	}))

	// 3. Forge a balanced pair reusing the SAME ids as the anchor entries,
	// landing in a different (much older) partition -- the composite
	// (id, created_at) primary key does not stop this, only a same-partition
	// exact duplicate would. This is the raw-SQL / compromised-credential
	// path the audit's threat model already assumes elsewhere in this wave.
	// Both legs are inserted in the same transaction: the per-journal-currency
	// balance constraint is checked against the whole transaction's effect on
	// journal 1 (which already has its own balanced legitimate legs), so a
	// lone forged debit or credit would trip it even though it has nothing to
	// do with the id-uniqueness gap under test.
	forgedAt := time.Now().AddDate(0, -2, 0)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (id, journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at)
		VALUES ($1, $2, $3, $4, $5, 'debit', 999, $6, $6)`,
		debitEntryID, journalID, holder, currencyID, walletID, forgedAt)
	require.NoError(t, err, "forging a duplicate-id row in a different partition must succeed under the ledger_app raw-SQL threat model -- if this now fails, the PLAUSIBLE consequence no longer holds and this test (not the fix) should be revisited")
	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (id, journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at)
		VALUES ($1, $2, $3, $4, $5, 'credit', 999, $6, $6)`,
		creditEntryID, journalID, core.SystemAccountHolder(holder), currencyID, custodialID, forgedAt)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	// 4. Per-account balance: unaffected. Both forged ids are <= the
	// watermark, so the delta filter (id > last_entry_id) excludes them --
	// this is the "permanently invisible" half of the split.
	balance, err := ledgerStore.GetBalance(ctx, holder, curID, wallet)
	require.NoError(t, err)
	assert.True(t, balance.Equal(decimal.NewFromInt(100)), "GetBalance must not see the duplicate-id forged entry: got %s, want 100", balance)

	// 5. Global debit/credit totals: DO see the forged pair, and because it
	// is itself balanced (999 debit == 999 credit), the mismatch is
	// undetectable from this aggregate alone -- this is the "counted
	// elsewhere, without violating any id-range invariant" half of the
	// split. reconcile.sql's global check draws from the same unfiltered
	// SUM(amount) shape as SumGlobalDebitCreditByCurrency, so this generalizes.
	totals, err := adapter.SumGlobalDebitCreditByCurrency(ctx)
	require.NoError(t, err)
	var found bool
	for _, tot := range totals {
		if tot.CurrencyID != currencyID {
			continue
		}
		found = true
		want := decimal.NewFromInt(100).Add(decimal.NewFromInt(999))
		assert.True(t, tot.Debit.Equal(want), "global debit total must include the forged entry: got %s, want %s", tot.Debit, want)
		assert.True(t, tot.Credit.Equal(want), "global credit total must include the forged entry: got %s, want %s", tot.Credit, want)
		assert.True(t, tot.Debit.Equal(tot.Credit), "the forged pair is itself balanced, so the global debit==credit check stays green despite the split -- this is exactly what makes the divergence undetectable from this aggregate alone")
	}
	assert.True(t, found, "expected a global total row for currency id %d", currencyID)
}
