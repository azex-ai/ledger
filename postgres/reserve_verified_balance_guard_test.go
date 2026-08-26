package postgres_test

// Pin test for concurrency.md's Major: Reserve(RequireVerifiedBalance: true)
// used to run its verified-balance gate unconditionally, even on a
// transaction-bound store (postgres.ReserverStore.WithDB -- reachable from
// inside ledger.Service.RunInTx's callback). The gate may call a remote
// core.AuthVerifier (core.AuthVerifier's doc comment), so running it there is
// exactly the "external call inside an open DB transaction" financial.md
// forbids -- the same violation LedgerStore.Authorize already guards
// against (see authorize_pin_test.go's
// TestAuthorize_RejectsOnTransactionBoundStore, which this test mirrors).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// TestReserve_RequireVerifiedBalance_RejectsOnTransactionBoundStore pins the
// guard itself: the tx-bound store must refuse BEFORE ever reaching
// requireVerifiedAvailableBalance, not merely fail once it gets there. The
// two cases below share one funded holder + nil core.AuthVerifier so the
// error identity draws a clean before/after line:
//   - pool mode: the gate runs (unaffected by this fix) and, with no
//     AuthVerifier configured, VerifiedBalanceReader's own doc comment says
//     any dimension with a contributing journal comes back
//     core.ErrUnauthorizedJournal -- see postgres.VerifiedBalanceStore.
//   - tx mode: the guard now refuses with core.ErrInvalidInput before the
//     gate runs at all. Reverting the guard (see reserver_store.go's
//     RequireVerifiedBalance block) would make the tx-mode case ALSO return
//     core.ErrUnauthorizedJournal instead -- proceeding into the gate from
//     inside an open transaction, the exact violation this test exists to
//     prevent.
func TestReserve_RequireVerifiedBalance_RejectsOnTransactionBoundStore(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
	seedReservableBalance(t, ctx, ledger, pool, 601, curID, decimal.NewFromInt(50))

	// Pool mode: gate runs, no AuthVerifier configured -> ErrUnauthorizedJournal.
	_, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:          601,
		CurrencyUID:            curID,
		Amount:                 decimal.NewFromInt(1),
		IdempotencyKey:         postgrestest.UniqueKey("reserve-vb-pool"),
		ExpiresIn:              10 * time.Minute,
		RequireVerifiedBalance: true,
	})
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrUnauthorizedJournal, "pool mode must still reach the gate (unaffected by this fix)")

	// Tx mode: guard must refuse before the gate runs at all.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	txLedger := ledger.WithDB(tx)
	txStore := store.WithDB(tx, txLedger)

	_, err = txStore.Reserve(ctx, core.ReserveInput{
		AccountHolder:          601,
		CurrencyUID:            curID,
		Amount:                 decimal.NewFromInt(1),
		IdempotencyKey:         postgrestest.UniqueKey("reserve-vb-tx"),
		ExpiresIn:              10 * time.Minute,
		RequireVerifiedBalance: true,
	})
	require.Error(t, err, "Reserve must refuse RequireVerifiedBalance on a transaction-bound store")
	require.ErrorIs(t, err, core.ErrInvalidInput)
	require.False(t, errors.Is(err, core.ErrUnauthorizedJournal),
		"must be rejected by the guard, not by reaching the gate from inside the transaction")
}
