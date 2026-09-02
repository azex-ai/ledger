package postgres_test

// Pins for the hold half of Reserve's RequireVerifiedBalance gate
// (docs/INVARIANTS.md I-49 / I-11, docs/audits/2026-09-02-deep-audit/
// w3-review/money-path.md C-1).
//
// I-49 fixed the base of the availability expression -- min(V, E), neither
// term reading balance_checkpoints. It did not touch the term that gets
// SUBTRACTED from that base. The outstanding hold was read off
// reservations.status / reservations.settled_amount, and
// ledger_reservations_guard (001_baseline section 12) deliberately permits
// active -> settling/settled/released and permits settled_amount to grow.
// ledger_app holds UPDATE on the table. So one statement -- the very
// statement the guard was written to allow -- zeroed the hold, and the gate
// authorized the same balance twice.
//
// Sourcing the discharge from the append-only settlement record instead does
// not help: ledger_app keeps INSERT on both receipt tables because the
// application has to write them, so a forged INSERT discharges a hold just
// as cheaply (third variant below). The gate therefore credits NO discharge
// claim at all and trusts only expires_at, the one column the guard refuses
// to let anyone change. The conservative consequences of that -- a settled or
// released reservation goes on holding until it expires -- are pinned here
// too, as behavior, not as an accident.
//
// Every tamper below is issued over a real socket as ledger_app, not as the
// test superuser: the claim being pinned is about what the application's own
// credential can do, and a superuser statement would prove nothing about the
// grants.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// holdFixture is the shared setup for these pins: a signed deposit, a gated
// reserver, and a pool holding the application's own credential.
type holdFixture struct {
	f        vbFixture
	reserver *postgres.ReserverStore
	ledger   *postgres.LedgerStore
	attacker *pgxpool.Pool
	holder   int64
}

func setupHoldFixture(t *testing.T, pool *pgxpool.Pool, holder int64, keyID string, deposit int64) holdFixture {
	t.Helper()
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)

	attestor, verifier := newTestAttestor(t, keyID)
	ledger := postgres.NewLedgerStore(pool).WithAuth(attestor)
	_, err := ledger.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("hold-deposit"), decimal.NewFromInt(deposit)))
	require.NoError(t, err)

	return holdFixture{
		f:        f,
		ledger:   ledger,
		reserver: postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, verifier)),
		// ledgerAppPool is migration_024_test.go's helper -- the same
		// credential, deliberately shared rather than duplicated.
		attacker: ledgerAppPool(t, ctx, pool, "w3-holds-app-credential"),
		holder:   holder,
	}
}

func (h holdFixture) gatedReserve(ctx context.Context, amount int64, key string) (*core.Reservation, error) {
	return h.reserveFor(ctx, amount, key, 0, true)
}

func (h holdFixture) reserveFor(ctx context.Context, amount int64, key string, expiresIn time.Duration, gated bool) (*core.Reservation, error) {
	return h.reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:          h.holder,
		CurrencyUID:            h.f.CurrencyUID,
		Amount:                 decimal.NewFromInt(amount),
		IdempotencyKey:         postgrestest.UniqueKey(key),
		ExpiresIn:              expiresIn,
		RequireVerifiedBalance: gated,
	})
}

// waitUntilExpired blocks until the database itself says the reservation has
// passed its expires_at, using the same clock_timestamp() predicate the
// settle path uses. Polling the predicate rather than probing with a real
// Settle/SettlePartial matters: those mutate on success, so a probe issued
// before the expiry would settle the reservation and every later refusal
// would be "already terminal", not "expired" -- a pin that passes for the
// wrong reason.
func waitUntilExpired(t *testing.T, pool *pgxpool.Pool, reservationUID string) {
	t.Helper()
	ctx := context.Background()
	require.Eventually(t, func() bool {
		var expired bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT expires_at <= clock_timestamp() FROM reservations WHERE uid = $1`, reservationUID).Scan(&expired))
		return expired
	}, 30*time.Second, 100*time.Millisecond, "the reservation should have expired by now")
}

// TestReserve_RequireVerifiedBalance_HoldSurvivesStatusTamper is C-1's pin,
// plus the sibling attack that the first attempt at fixing C-1 left open.
//
// Each variant is ONE statement that the schema accepts by design, and each
// used to make the outstanding hold report as zero for a reservation that is
// still live:
//
//	settling: status='settling', settled_amount=reserved_amount
//	          -- SumActiveReservations' CASE arm reports reserved - settled = 0
//	released: status='released'
//	          -- the row drops out of the WHERE clause entirely
//	receipt:  INSERT a 'release' row into reservation_operation_receipts
//	          -- append-only stops rewriting a settlement record, not
//	             appending one
//
// Pointing the gated hold at any of those signals turns the second Reserve in
// each variant green, which is the attack: 2000 authorized against 1000.
//
// The three variants are red against different code, and saying which is the
// point of listing them together. The first two are red against the code this
// audit found (the hold read status/settled_amount). The third is red against
// the first attempt at fixing it -- sourcing the discharge from the
// append-only settlement record -- which looked airtight because those tables
// refuse UPDATE and DELETE, and was defeated by an INSERT the application's
// own grants allow. It is here so that attempt cannot be reintroduced as an
// optimization for the conservative rule that replaced it.
func TestReserve_RequireVerifiedBalance_HoldSurvivesStatusTamper(t *testing.T) {
	variants := []struct {
		name   string
		holder int64
		tamper string
	}{
		{
			name:   "settling with settled_amount raised to the full reserved amount",
			holder: 9407,
			tamper: `UPDATE reservations SET status = 'settling', settled_amount = reserved_amount WHERE uid = $1`,
		},
		{
			name:   "released outright",
			holder: 9408,
			tamper: `UPDATE reservations SET status = 'released' WHERE uid = $1`,
		},
		{
			name:   "forged release receipt appended to the append-only table",
			holder: 9413,
			tamper: `INSERT INTO reservation_operation_receipts (reservation_id, operation, idempotency_key, amount)
			         SELECT id, 'release', 'forged-release-' || id, 0 FROM reservations WHERE uid = $1`,
		},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			pool := postgrestest.SetupDB(t)
			ctx := context.Background()
			h := setupHoldFixture(t, pool, v.holder, "ed25519-hold-tamper", 1000)

			first, err := h.gatedReserve(ctx, 1000, "hold-tamper-first")
			require.NoError(t, err)
			require.NotNil(t, first)

			// Control: with the hold intact the gate refuses a second bite at
			// the same 1000. Without this the pin could pass by refusing
			// everything.
			_, err = h.gatedReserve(ctx, 1000, "hold-tamper-control")
			require.ErrorIs(t, err, core.ErrInsufficientBalance, "control: the untampered hold must already refuse the second reservation")

			// The attack, issued with the application's own credential.
			tag, err := h.attacker.Exec(ctx, v.tamper, first.UID)
			require.NoError(t, err, "the schema permits this statement by design -- if it starts failing, the guards changed and this pin needs rewriting, not deleting")
			require.EqualValues(t, 1, tag.RowsAffected())

			// The pin: the gated hold must not move.
			_, err = h.gatedReserve(ctx, 1000, "hold-tamper-gated")
			require.ErrorIs(t, err, core.ErrInsufficientBalance,
				"the gate must credit no discharge claim a leaked ledger_app credential can write or append")

			// Control 2: the tamper really landed. The first two variants are
			// visible to the ungated path (which keeps the state machine's own
			// answer) and really would have paid out; the receipt variant is
			// invisible there by construction, so assert the row instead.
			if v.name == "forged release receipt appended to the append-only table" {
				var receipts int
				require.NoError(t, pool.QueryRow(ctx, `
					SELECT count(*) FROM reservation_operation_receipts o
					JOIN reservations r ON r.id = o.reservation_id WHERE r.uid = $1`, first.UID).Scan(&receipts))
				require.Equal(t, 1, receipts, "sanity: the forged receipt must actually be in the table")
				return
			}
			ungated, err := h.reserver.Reserve(ctx, core.ReserveInput{
				AccountHolder:  h.holder,
				CurrencyUID:    h.f.CurrencyUID,
				Amount:         decimal.NewFromInt(1000),
				IdempotencyKey: postgrestest.UniqueKey("hold-tamper-ungated"),
			})
			require.NoError(t, err, "sanity: the tampered hold must actually be gone from the ungated path, or the pin above proves nothing")
			require.NotNil(t, ungated)
		})
	}
}

// TestReserve_RequireVerifiedBalance_HoldClearsOnlyAtExpiry pins the
// conservative semantics themselves, so that "the gate refuses more than a
// perfect reader would" stays a decision on the record rather than something
// a later reader mistakes for a bug and "fixes" back into C-1.
//
// Settling or releasing a reservation does NOT give its funds back to gated
// calls: both claims are writable with the application's credential, so the
// gate ignores them. Only expires_at -- which no credential can change --
// discharges the hold.
func TestReserve_RequireVerifiedBalance_HoldClearsOnlyAtExpiry(t *testing.T) {
	t.Run("a legitimate Release does not give the funds back before expiry", func(t *testing.T) {
		pool := postgrestest.SetupDB(t)
		ctx := context.Background()
		h := setupHoldFixture(t, pool, 9409, "ed25519-hold-release", 1000)

		res, err := h.reserveFor(ctx, 1000, "hold-release-reserve", time.Hour, true)
		require.NoError(t, err)

		require.NoError(t, h.reserver.Release(ctx, core.ReleaseInput{
			ReservationUID: res.UID,
			IdempotencyKey: postgrestest.UniqueKey("hold-release-op"),
		}), "Release itself must still work -- only the gate's view of it changes")

		_, err = h.gatedReserve(ctx, 1000, "hold-release-after")
		require.ErrorIs(t, err, core.ErrInsufficientBalance,
			"a released reservation must keep holding under the gate until it expires: 'released' is a claim ledger_app can write")

		// The ungated path, which trusts the state machine, does give it back.
		// This is the deliberate divergence, not a bug in either query.
		ungated, err := h.reserveFor(ctx, 1000, "hold-release-ungated", 0, false)
		require.NoError(t, err, "the ordinary path still honors Release immediately")
		require.NotNil(t, ungated)
	})

	t.Run("a legitimate Settle does not give the remainder back before expiry", func(t *testing.T) {
		pool := postgrestest.SetupDB(t)
		ctx := context.Background()
		h := setupHoldFixture(t, pool, 9410, "ed25519-hold-settle", 1000)

		res, err := h.reserveFor(ctx, 1000, "hold-settle-reserve", time.Hour, true)
		require.NoError(t, err)

		// Settle 400 and post the matching charge: the balance is now 600 and
		// the reservation is terminal, but the gate still counts 1000 held.
		_, err = h.ledger.PostJournal(ctx, h.f.spendInput(h.holder, postgrestest.UniqueKey("hold-settle-charge"), decimal.NewFromInt(400)))
		require.NoError(t, err)
		require.NoError(t, h.reserver.Settle(ctx, core.SettleInput{
			ReservationUID: res.UID,
			Amount:         decimal.NewFromInt(400),
			IdempotencyKey: postgrestest.UniqueKey("hold-settle-op"),
		}))

		_, err = h.gatedReserve(ctx, 1, "hold-settle-after")
		require.ErrorIs(t, err, core.ErrInsufficientBalance,
			"the settled portion is deliberately double-counted until expiry: it left through its own journal AND is still held")
	})

	t.Run("expiry gives the funds back", func(t *testing.T) {
		pool := postgrestest.SetupDB(t)
		ctx := context.Background()
		h := setupHoldFixture(t, pool, 9411, "ed25519-hold-expiry", 1000)

		_, err := h.reserveFor(ctx, 1000, "hold-expiry-reserve", 2*time.Second, true)
		require.NoError(t, err)

		// Nothing is released, nothing is settled, nothing is tampered with:
		// the hold ends because time passed, which is the only discharge this
		// path accepts. Polled rather than slept so a slow or clock-skewed
		// container cannot make it flaky in the failing direction.
		require.Eventually(t, func() bool {
			_, err := h.gatedReserve(ctx, 1000, "hold-expiry-after")
			return err == nil
		}, 30*time.Second, 200*time.Millisecond,
			"an expired reservation must stop holding, or a gated caller's balance is locked forever")
	})
}

// TestReserverStore_Settle_RefusesExpiredReservation is the other half of the
// expiry rule, and the reason the hold above may drop at expires_at at all:
// once the gate has stopped counting a reservation, that reservation must not
// still be able to take money. Without this, no tampering is needed --
// reserve 1000, wait out the expiry, reserve 1000 again (the hold is gone),
// settle both.
//
// Release and FinalizeSettlement are deliberately NOT refused: neither
// records a new amount, and both are what service.ExpirationService calls to
// wind an expired reservation down. Refusing them would leave every expired
// settling reservation stuck in settling, failing on every sweep.
func TestReserverStore_Settle_RefusesExpiredReservation(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	h := setupHoldFixture(t, pool, 9414, "ed25519-hold-expired-settle", 10000)

	// Control: a live reservation settles normally.
	live, err := h.reserveFor(ctx, 100, "expired-settle-live", time.Hour, false)
	require.NoError(t, err)
	require.NoError(t, h.reserver.Settle(ctx, core.SettleInput{
		ReservationUID: live.UID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("expired-settle-live-op"),
	}), "control: settling a reservation that has not expired must still work")

	expiring, err := h.reserveFor(ctx, 100, "expired-settle-target", 2*time.Second, false)
	require.NoError(t, err)

	waitUntilExpired(t, pool, expiring.UID)

	// The reservation is still 'active' -- nothing has settled or released it,
	// it has only run out of time -- so a refusal here can only be the expiry
	// rule.
	err = h.reserver.Settle(ctx, core.SettleInput{
		ReservationUID: expiring.UID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("expired-settle-op"),
	})
	require.ErrorIs(t, err, core.ErrInvalidTransition, "an expired reservation must no longer be settleable")

	err = h.reserver.SettlePartial(ctx, core.SettlePartialInput{
		ReservationUID: expiring.UID,
		Amount:         decimal.NewFromInt(10),
		IdempotencyKey: postgrestest.UniqueKey("expired-settle-partial"),
	})
	require.ErrorIs(t, err, core.ErrInvalidTransition, "SettlePartial records new spend too, so it is refused on the same terms")

	// The sweeper's own path still works on the same expired reservation.
	require.NoError(t, h.reserver.Release(ctx, core.ReleaseInput{
		ReservationUID: expiring.UID,
		IdempotencyKey: "expire-release-" + expiring.UID,
	}), "service.ExpirationService releases expired reservations through this exact call")
}

// TestReserverStore_FinalizeSettlement_AllowedAfterExpiry pins the carve-out
// above from the other side: an expired, partially-settled reservation must
// still be finalizable, because that is how service.ExpirationService winds
// one down (expiration.go: settling rows get FinalizeSettlement, not
// Release, so the settled portion is not recorded as a release). Refusing it
// would strand every such reservation in settling forever.
func TestReserverStore_FinalizeSettlement_AllowedAfterExpiry(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	h := setupHoldFixture(t, pool, 9415, "ed25519-hold-expired-finalize", 10000)

	res, err := h.reserveFor(ctx, 100, "expired-finalize-target", 3*time.Second, false)
	require.NoError(t, err)

	// Partially settle while it is still live.
	require.NoError(t, h.reserver.SettlePartial(ctx, core.SettlePartialInput{
		ReservationUID: res.UID,
		Amount:         decimal.NewFromInt(40),
		IdempotencyKey: postgrestest.UniqueKey("expired-finalize-leg"),
	}))

	waitUntilExpired(t, pool, res.UID)

	require.NoError(t, h.reserver.FinalizeSettlement(ctx, core.FinalizeSettlementInput{
		ReservationUID: res.UID,
		IdempotencyKey: "expire-finalize-" + res.UID,
	}), "the expiration sweeper's finalize path must keep working on an expired reservation")
}

// TestReserve_RequireVerifiedBalance_HoldRecomputedUnderLock is the hold
// counterpart of TestReserve_RequireVerifiedBalance_RechecksUnderLock: the
// hold must be computed inside the (holder, currency) advisory lock, not
// alongside the pre-transaction verification. A reservation that commits in
// the gate's window is exactly the over-sell race I-4/I-11 exist to close.
func TestReserve_RequireVerifiedBalance_HoldRecomputedUnderLock(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	h := setupHoldFixture(t, pool, 9412, "ed25519-hold-lock", 1000)

	lockHeld := make(chan struct{})
	committed := make(chan error, 1)
	go func() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			committed <- err
			close(lockHeld)
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		// Reserve in tx mode takes the same (holder, currency) advisory lock
		// the gated Reserve below needs, and holds it until this transaction
		// ends. Ungated because the gate refuses to run inside a caller's
		// transaction (I-32); what matters here is the hold it writes.
		if _, err := h.reserver.WithDB(tx, h.ledger.WithDB(tx)).Reserve(ctx, core.ReserveInput{
			AccountHolder:  h.holder,
			CurrencyUID:    h.f.CurrencyUID,
			Amount:         decimal.NewFromInt(600),
			IdempotencyKey: postgrestest.UniqueKey("hold-lock-concurrent"),
		}); err != nil {
			committed <- err
			close(lockHeld)
			return
		}
		close(lockHeld)

		if err := waitForBlockedAdvisoryLock(ctx, pool); err != nil {
			committed <- err
			return
		}
		committed <- tx.Commit(ctx)
	}()

	<-lockHeld

	_, err := h.gatedReserve(ctx, 600, "hold-lock-gated")
	require.NoError(t, <-committed, "test setup: the concurrent reservation must commit")
	require.ErrorIs(t, err, core.ErrInsufficientBalance,
		"the hold must be recomputed under the balance advisory lock; a reservation that commits in the gate's window is otherwise invisible")
}
