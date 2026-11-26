package postgres_test

import (
	"context"
	"sync"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// pausingVerifier wraps a real AuthVerifier and, on its first Verify call,
// signals entered and blocks until release is closed -- the window between
// VerifiedBalance listing the contributing entries and returning V.
type pausingVerifier struct {
	inner   core.AuthVerifier
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newPausingVerifier(inner core.AuthVerifier) *pausingVerifier {
	return &pausingVerifier{inner: inner, entered: make(chan struct{}), release: make(chan struct{})}
}

func (v *pausingVerifier) Verify(ctx context.Context, digest, signature []byte, keyID string) error {
	v.once.Do(func() {
		close(v.entered)
		<-v.release
	})
	return v.inner.Verify(ctx, digest, signature, keyID)
}

// TestVerifiedBalance_ForgeryInsideVerifyWindowIsNotCounted pins I-49's "V
// is the sum of what was verified".
//
// VerifiedBalance listed the contributing journals, verified them (possibly
// through a remote AuthVerifier), and then returned a fresh, unbounded
// RecomputeBalance -- a separate statement with its own snapshot. An unsigned
// journal committed while the verifier ran was summed into V without ever
// being verified; E counted it too, so min(V, E) included it and a gated
// Reserve locked forged money.
func TestVerifiedBalance_ForgeryInsideVerifyWindowIsNotCounted(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)
	const holder int64 = 9031

	attestor, verifier := newTestAttestor(t, "ed25519-vb-window")
	ledger := postgres.NewLedgerStore(pool).WithAuth(attestor)
	_, err := ledger.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("vb-window-deposit"), decimal.NewFromInt(100)))
	require.NoError(t, err)

	pausing := newPausingVerifier(verifier)
	vb := postgres.NewVerifiedBalanceStore(pool, pausing)

	type result struct {
		v   decimal.Decimal
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := vb.VerifiedBalance(ctx, holder, f.CurrencyUID, f.AvailableUID)
		done <- result{v, err}
	}()

	<-pausing.entered
	insertForgedBalancedJournal(t, pool, ctx, f, holder, "5000", postgrestest.UniqueKey("vb-window-forged"))
	close(pausing.release)

	got := <-done
	require.NoError(t, got.err)
	require.True(t, got.v.Equal(decimal.NewFromInt(100)),
		"V must be the sum of the verified entries only; the forgery committed inside the window must not be counted, got %s", got.v)

	// The forgery is now a contributor: the next gated read fails closed.
	_, err = postgres.NewVerifiedBalanceStore(pool, verifier).VerifiedBalance(ctx, holder, f.CurrencyUID, f.AvailableUID)
	require.ErrorIs(t, err, core.ErrUnauthorizedJournal)
}

// TestReserve_RequireVerifiedBalance_ForgeryInsideVerifyWindow is the same
// window through the gated Reserve: E counts the forgery (entries only,
// unauthenticated), V must not, so min(V, E) = 100 and a 1000 reservation is
// refused.
func TestReserve_RequireVerifiedBalance_ForgeryInsideVerifyWindow(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)
	const holder int64 = 9032

	attestor, verifier := newTestAttestor(t, "ed25519-reserve-window")
	ledger := postgres.NewLedgerStore(pool).WithAuth(attestor)
	_, err := ledger.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("reserve-window-deposit"), decimal.NewFromInt(100)))
	require.NoError(t, err)

	pausing := newPausingVerifier(verifier)
	reserver := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, pausing))

	done := make(chan error, 1)
	go func() {
		_, err := reserver.Reserve(ctx, core.ReserveInput{
			AccountHolder:          holder,
			CurrencyUID:            f.CurrencyUID,
			Amount:                 decimal.NewFromInt(1000),
			IdempotencyKey:         postgrestest.UniqueKey("reserve-window-gated"),
			RequireVerifiedBalance: true,
		})
		done <- err
	}()

	<-pausing.entered
	insertForgedBalancedJournal(t, pool, ctx, f, holder, "5000", postgrestest.UniqueKey("reserve-window-forged"))
	close(pausing.release)

	err = <-done
	require.ErrorIs(t, err, core.ErrInsufficientBalance,
		"the forged 5000 committed while the verifier ran must not back a gated reservation")

	var active int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM reservations WHERE account_holder = $1`, holder).Scan(&active))
	require.Zero(t, active, "no reservation may exist")
}
