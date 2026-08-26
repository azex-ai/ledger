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

// TestJournalEntries_DuplicateIDAcrossPartitions_Rejected pins migration
// 008's fix for the PLAUSIBLE-turned-CONFIRMED consequence this test used to
// demonstrate directly: journal_entries' primary key is (id, created_at),
// not id alone, because a partitioned table's primary key must include the
// partition key (created_at). id is therefore only guaranteed unique
// *within* a partition (month), not across the whole table -- and every
// per-account balance path filters strictly on `id > checkpoint.
// last_entry_id`, so a duplicated id at or below the watermark is
// permanently invisible to GetBalance forever, while
// SumGlobalDebitCreditByCurrency (which sums every row, no id filter) counts
// it anyway. See postgres/sql/migrations/008_journal_entries_id_sequence_only.up.sql
// for the full argument and docs/INVARIANTS.md's I-5 "Known gap" note (now
// closed) for the invariant-level framing.
//
// git history carries the original form of this test (pre-migration-008):
// under the audit's already-established ledger_app threat model, it forged
// a balanced pair reusing an already-used id in a different partition and
// showed the resulting split directly -- GetBalance blind to it,
// SumGlobalDebitCreditByCurrency counting it, both individually looking
// correct. This version pins the fix instead: the same forged INSERT, run
// with the same ledger_app credential the original test only narrated (it
// connected through postgrestest.SetupDB's admin pool; this version connects
// as ledger_app for real, via roles_test.go's newAppPool), must now be
// refused outright.
func TestJournalEntries_DuplicateIDAcrossPartitions_Rejected(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	appPool := newAppPool(t, pool, "roles-test-app-iddup-not-a-real-secret") //nolint:gosec // test-only credential

	ledgerStore := postgres.NewLedgerStore(pool)
	adapter := postgres.NewRollupAdapter(pool)

	curID := postgrestest.SeedCurrency(t, pool, "USDT-IDDUP", "Tether USD ID Dup")
	jtID := postgrestest.SeedJournalType(t, pool, "transfer_iddup", "Transfer ID Dup")
	wallet := postgrestest.SeedClassification(t, pool, "wallet_iddup", "Wallet ID Dup", "debit", false)
	custodial := postgrestest.SeedClassification(t, pool, "custodial_iddup", "Custodial ID Dup", "credit", true)

	holder := int64(9303)

	// 1. A normal, legitimate journal: wallet +100 / custodial +100.
	j, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("iddup-anchor-rejected"),
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
	// legitimate current watermark for (holder, currency, wallet). This is
	// what makes the attack this test attempts dangerous if it ever
	// succeeded: both forged ids would fall at-or-below the watermark and
	// be permanently invisible to GetBalance.
	require.NoError(t, adapter.UpsertCheckpoint(ctx, service.BalanceCheckpoint{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: walletID,
		Balance:          decimal.NewFromInt(100),
		LastEntryID:      debitEntryID,
		LastEntryAt:      time.Now(),
	}))

	// 3. Attempt the forged pair -- same ids as the anchor entries, landing
	// in a different (much older) partition -- AS ledger_app, the credential
	// migration 008's column-level GRANT restricts. Both legs go in one
	// transaction, exactly like the pre-fix form of this test: a single leg
	// inserted alone is unbalanced by itself and would be refused by the
	// deferred per-journal balance trigger (23514) regardless of the id
	// column -- confirmed by running this test against migration 007 alone,
	// where the two forged rows succeed and commit together, and only THEN
	// do steps 4/5 below show the split this migration exists to prevent.
	// With migration 008 applied, the very first statement inside the
	// transaction is refused at the ACL layer (42501) before the second leg
	// is ever attempted, so nothing here ever reaches a commit.
	forgedAt := time.Now().AddDate(0, -2, 0)
	tx, err := appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (id, journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at)
		VALUES ($1, $2, $3, $4, $5, 'debit', 999, $6, $6)`,
		debitEntryID, journalID, holder, currencyID, walletID, forgedAt)
	assertPermissionDenied(t, err)

	// 4. Per-account balance: unaffected, because neither forged row ever
	// committed.
	balance, err := ledgerStore.GetBalance(ctx, holder, curID, wallet)
	require.NoError(t, err)
	assert.True(t, balance.Equal(decimal.NewFromInt(100)), "GetBalance must reflect only the legitimate anchor journal: got %s, want 100", balance)

	// 5. Global debit/credit totals: also unaffected, for the same reason --
	// nothing the attack attempted was ever persisted, so the two views of
	// the ledger that migration 008 exists to keep from splitting never had
	// anything to disagree about.
	totals, err := adapter.SumGlobalDebitCreditByCurrency(ctx)
	require.NoError(t, err)
	var found bool
	for _, tot := range totals {
		if tot.CurrencyID != currencyID {
			continue
		}
		found = true
		assert.True(t, tot.Debit.Equal(decimal.NewFromInt(100)), "global debit total must show only the legitimate 100, no forged 999: got %s", tot.Debit)
		assert.True(t, tot.Credit.Equal(decimal.NewFromInt(100)), "global credit total must show only the legitimate 100, no forged 999: got %s", tot.Credit)
	}
	assert.True(t, found, "expected a global total row for currency id %d", currencyID)
}
