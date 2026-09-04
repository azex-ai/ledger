package postgres_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// TestSettlementNettingViolations_ReportsTheSameSignAsGetBalance pins A-N1
// from the 2026-09-02 audit.
//
// SettlementNettingViolations computed its net_balance with a bare
// debit-minus-credit CASE, ignoring normal_side. The zero test is immune to
// that -- non-zero is non-zero whichever way round the terms go -- so the
// check never misfired and nothing failed. What it produced was a Finding
// whose Detail string ("net=%s") handed the on-call engineer the OPPOSITE
// sign to what GetBalance reports for the same classification, because
// settlement is credit-normal. Somebody chasing a settlement leak reads that
// number and looks in the wrong direction.
//
// The assertion is agreement with GetBalance rather than a literal, because
// "the reconciler and the balance reader describe the same position the same
// way" is the property that has to hold.
func TestSettlementNettingViolations_ReportsTheSameSignAsGetBalance(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	adapter := postgres.NewReconcileAdapter(pool)

	curUID := postgrestest.SeedCurrency(t, pool, "USD-NETSIGN", "US Dollar Netting Sign")
	jtUID := postgrestest.SeedJournalType(t, pool, "netsign_jt", "Netting Sign")
	settlement := postgrestest.SeedClassification(t, pool, "settlement", "Settlement", "credit", true)
	wallet := postgrestest.SeedClassificationWithRole(t, pool, "main_wallet", "Main Wallet", "debit", false, "available")

	const holder = int64(9501)
	sys := core.SystemAccountHolder(holder)

	// A transfer_in-shaped journal with no matching transfer_out: settlement is
	// left holding a real, non-zero position -- credit-normal, credited, so
	// GetBalance reads it POSITIVE.
	_, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtUID,
		IdempotencyKey: postgrestest.UniqueKey("netsign"),
		Entries: []core.EntryInput{
			{AccountHolder: sys, CurrencyUID: curUID, ClassificationUID: settlement, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(40)},
			{AccountHolder: holder, CurrencyUID: curUID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(40)},
		},
	})
	require.NoError(t, err)

	balance, err := ledgerStore.GetBalance(ctx, sys, curUID, settlement)
	require.NoError(t, err)
	require.True(t, balance.Equal(decimal.NewFromInt(40)),
		"fixture: settlement must hold a non-zero position, got %s", balance)

	// windowMinutes = 0: nothing is excluded as in-flight.
	violations, err := adapter.SettlementNettingViolations(ctx, "settlement", 0)
	require.NoError(t, err)
	require.Len(t, violations, 1, "a non-zero settlement position must be reported")

	assert.Truef(t, violations[0].NetBalance.Equal(balance),
		"the reconciler reports net=%s for a settlement position GetBalance reads as %s; an operator "+
			"following the Finding would look for the leak in the opposite direction",
		violations[0].NetBalance, balance)
}
