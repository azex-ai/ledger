package postgres_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ledger "github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
)

// TestFX_LedgerDoesNotCheckTheRate turns a comment that was not true into a
// contract that is (2026-09-02 audit A-N5).
//
// presets/fx.go used to claim that keeping each journal single-currency "lets
// per-currency balance validation (DB trigger + Go validator) catch any
// rate-quote bug -- neither journal can be unbalanced and silently pass". The
// second half is true and the first does not follow from it: each journal
// balances within its own currency for ANY quantity, so the validation has
// nothing to say about the relationship between the two journals. A caller who
// read that sentence would reasonably believe the ledger was a backstop
// against a mispriced quote. It is not, and a mispriced FX conversion moves
// real money.
//
// So the test asserts the uncomfortable thing directly: quote the bought
// currency at 100x the correct figure and BOTH journals are accepted, no error,
// no warning. Rate correctness is the caller's, in full.
func TestFX_LedgerDoesNotCheckTheRate(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	svc, err := ledger.New(pool)
	require.NoError(t, err)
	require.NoError(t, svc.InstallExtendedPresets(ctx))

	curA := postgrestest.SeedCurrency(t, pool, "FXA", "FX Source")
	curB := postgrestest.SeedCurrency(t, pool, "FXB", "FX Target")
	const holder = int64(9601)

	_, err = svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID:       holder,
		CurrencyUID:    curA,
		IdempotencyKey: postgrestest.UniqueKey("fxrate-fund"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
		Source:         "fx_rate_test",
	})
	require.NoError(t, err)

	// Sell 100 A. At a rate of 0.9 the buy journal should be 90; quote 9000.
	_, err = svc.JournalWriter().ExecuteTemplate(ctx, "fx_sell", core.TemplateParams{
		HolderID:       holder,
		CurrencyUID:    curA,
		IdempotencyKey: postgrestest.UniqueKey("fxrate-sell"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
		Source:         "fx_rate_test",
	})
	require.NoError(t, err, "the sell journal balances within CCY-A")

	_, err = svc.JournalWriter().ExecuteTemplate(ctx, "fx_buy", core.TemplateParams{
		HolderID:       holder,
		CurrencyUID:    curB,
		IdempotencyKey: postgrestest.UniqueKey("fxrate-buy"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(9000)},
		Source:         "fx_rate_test",
	})
	require.NoError(t, err,
		"the buy journal balances within CCY-B whatever the quantity, so the ledger accepts a 100x-wrong quote; "+
			"if this ever starts failing, the ledger grew a rate check and presets/fx.go must stop disclaiming one")

	bought, err := svc.BalanceReader().GetBalance(ctx, holder, curB, classificationUID(ctx, t, pool, "main_wallet"))
	require.NoError(t, err)
	assert.True(t, bought.Equal(decimal.NewFromInt(9000)),
		"the holder is credited exactly what was quoted, right or wrong: got %s", bought)
}

func classificationUID(ctx context.Context, t *testing.T, pool *pgxpool.Pool, code string) string {
	t.Helper()
	var uid string
	require.NoError(t, pool.QueryRow(ctx, `SELECT uid FROM classifications WHERE code = $1`, code).Scan(&uid))
	return uid
}
