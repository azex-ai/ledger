package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/authdev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
)

var errSignerUnavailable = errors.New("test signer unavailable")

func TestRun(t *testing.T) {
	// Exercise the documented executable path, including migrations, the
	// ledger_app runtime check, fixture setup, replay and the conservative gate.
	dbURL := postgrestest.SetupRawDB(t)
	runtimeURL, err := url.Parse(dbURL)
	require.NoError(t, err)
	query := runtimeURL.Query()
	query.Set("role", "ledger_app")
	runtimeURL.RawQuery = query.Encode()
	t.Setenv("MIGRATE_DATABASE_URL", dbURL)
	t.Setenv("DATABASE_URL", runtimeURL.String())
	require.NoError(t, run())
}

// Observes real PostgreSQL transaction state at the signer/verifier boundary.
// Each test owns its database, so any other open transaction is a violation.
type observedAuth struct {
	admin    *pgxpool.Pool
	attestor core.Attestor
	verifier core.AuthVerifier
	fail     bool
	corrupt  bool
	emptySig bool
	signs    int
	verifies int
}

func (a *observedAuth) outsideTransaction(ctx context.Context) error {
	var active int
	err := a.admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
		WHERE datname=current_database() AND pid<>pg_backend_pid() AND xact_start IS NOT NULL`).Scan(&active)
	if err != nil {
		return err
	}
	if active != 0 {
		return fmt.Errorf("signer/verifier called with %d open transactions", active)
	}
	return nil
}

func (a *observedAuth) Sign(ctx context.Context, digest []byte) ([]byte, string, error) {
	a.signs++
	if err := a.outsideTransaction(ctx); err != nil {
		return nil, "", err
	}
	if a.fail {
		return nil, "", errSignerUnavailable
	}
	sig, key, err := a.attestor.Sign(ctx, digest)
	if a.corrupt && len(sig) > 0 {
		sig[0] ^= 1
	}
	if a.emptySig {
		sig = nil
	}
	return sig, key, err
}

func (a *observedAuth) Verify(ctx context.Context, digest, signature []byte, key string) error {
	a.verifies++
	if err := a.outsideTransaction(ctx); err != nil {
		return err
	}
	return a.verifier.Verify(ctx, digest, signature, key)
}

type fixture struct {
	svc              *ledger.Service
	admin            *pgxpool.Pool
	auth             *observedAuth
	currency, wallet string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	admin := postgrestest.SetupDB(t)
	cfg := admin.Config().Copy()
	cfg.ConnConfig.RuntimeParams["role"] = "ledger_app"
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	seed := make([]byte, 32)
	_, err = rand.Read(seed)
	require.NoError(t, err)
	a, v, err := authdev.NewLocalAttestor(seed, "signed-capture-test")
	require.NoError(t, err)
	auth := &observedAuth{admin: admin, attestor: a, verifier: v}
	svc, err := ledger.New(pool, ledger.WithAttestor(auth, auth))
	require.NoError(t, err)
	require.NoError(t, svc.AssertRuntimeRole(t.Context()))
	currency, wallet, err := setup(t.Context(), svc)
	require.NoError(t, err)
	require.NoError(t, fund(t.Context(), svc, currency))
	return fixture{svc: svc, admin: admin, auth: auth, currency: currency, wallet: wallet}
}

func (f fixture) reserve(t *testing.T, amount int64, key string) (*core.Reservation, error) {
	t.Helper()
	return f.svc.Reserver().Reserve(t.Context(), core.ReserveInput{
		AccountHolder: holder, CurrencyUID: f.currency, Amount: decimal.NewFromInt(amount),
		IdempotencyKey: key, ExpiresIn: time.Hour, RequireVerifiedBalance: true,
	})
}

func submittedEvent(partial bool) usageEvent {
	return usageEvent{ID: "submitted-usage-1", Amount: decimal.NewFromInt(20), Partial: partial,
		RecordedAt: time.Date(2026, 10, 10, 0, 0, 0, 123456000, time.UTC)}
}

func (f fixture) assertBalance(t *testing.T, wantBalance, wantHeld int64) {
	t.Helper()
	plain, err := f.svc.BalanceReader().GetBalance(t.Context(), holder, f.currency, f.wallet)
	require.NoError(t, err)
	verified, err := f.svc.VerifiedBalanceReader().VerifiedBalance(t.Context(), holder, f.currency, f.wallet)
	require.NoError(t, err)
	held, err := f.svc.Reserver().HeldAmount(t.Context(), holder, f.currency)
	require.NoError(t, err)
	require.True(t, plain.Equal(decimal.NewFromInt(wantBalance)), "plain=%s", plain)
	require.True(t, verified.Equal(plain), "verified=%s plain=%s", verified, plain)
	require.True(t, held.Equal(decimal.NewFromInt(wantHeld)), "held=%s", held)
}

type rowCounts struct {
	Journals, Entries, Reservations, Receipts, Legs, Checkpoints, Rollups int
}

func (f fixture) counts(t *testing.T) rowCounts {
	t.Helper()
	var c rowCounts
	require.NoError(t, f.admin.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM journals), (SELECT count(*) FROM journal_entries),
		(SELECT count(*) FROM reservations), (SELECT count(*) FROM reservation_operation_receipts),
		(SELECT count(*) FROM reservation_settlement_legs), (SELECT count(*) FROM balance_checkpoints),
		(SELECT count(*) FROM rollup_queue)`).Scan(
		&c.Journals, &c.Entries, &c.Reservations, &c.Receipts, &c.Legs, &c.Checkpoints, &c.Rollups))
	return c
}

func (f fixture) assertReservation(t *testing.T, uid, status string, settled int64) {
	t.Helper()
	var gotStatus, amount string
	require.NoError(t, f.admin.QueryRow(t.Context(),
		"SELECT status, coalesce(settled_amount, 0)::text FROM reservations WHERE uid=$1", uid).Scan(&gotStatus, &amount))
	require.Equal(t, status, gotStatus)
	require.True(t, decimal.RequireFromString(amount).Equal(decimal.NewFromInt(settled)), "settled=%s", amount)
}

func TestCaptureSignedReplayAndVerifiedHoldBoundary(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			f := newFixture(t)
			r, err := f.reserve(t, 60, "budget")
			require.NoError(t, err)
			event := submittedEvent(partial)
			signs, verifies := f.auth.signs, f.auth.verifies
			j, err := capture(t.Context(), f.svc, r, event)
			require.NoError(t, err)
			require.Equal(t, signs+1, f.auth.signs, "only the journal is signed; tx discharge is unsigned")
			require.Equal(t, verifies+1, f.auth.verifies, "authorization verified before transaction")
			require.Equal(t, core.AuthStatusSigned, j.AuthStatus)
			require.Equal(t, "signed-capture:charge:"+event.ID, j.IdempotencyKey)
			require.True(t, event.RecordedAt.Equal(j.EffectiveAt))
			var held int64
			status := "settled"
			if partial {
				held, status = 40, "settling"
			}
			f.assertBalance(t, 80, held)
			f.assertReservation(t, r.UID, status, 20)
			// Read the actual discharge row: there is deliberately no signature.
			table := "reservation_operation_receipts"
			if partial {
				table = "reservation_settlement_legs"
			}
			var digest, signature []byte
			var keyID string
			require.NoError(t, f.admin.QueryRow(t.Context(),
				"SELECT auth_digest, auth_signature, auth_key_id FROM "+table+" WHERE idempotency_key=$1",
				"signed-capture:settle:"+event.ID).Scan(&digest, &signature, &keyID))
			require.Empty(t, digest)
			require.Empty(t, signature)
			require.Empty(t, keyID)

			before := f.counts(t)
			signs = f.auth.signs
			replayed, err := capture(t.Context(), f.svc, r, event)
			require.NoError(t, err)
			require.Equal(t, signs, f.auth.signs, "Authorize reuses a committed journal without signing again")
			require.Equal(t, j.UID, replayed.UID)
			require.Equal(t, before, f.counts(t))
			f.assertBalance(t, 80, held)
			f.assertReservation(t, r.UID, status, 20)
			changed := event
			changed.Amount = decimal.NewFromInt(21)
			_, err = capture(t.Context(), f.svc, r, changed)
			require.ErrorIs(t, err, core.ErrConflict)
			require.Equal(t, before, f.counts(t))

			// VerifiedBalance sees signed entries (80), not spendability. The
			// gate counts the ORIGINAL unsigned hold (60), even after full settle.
			require.True(t, r.ExpiresAt.After(time.Now()))
			_, err = f.reserve(t, 21, "gated-too-much")
			require.ErrorIs(t, err, core.ErrInsufficientBalance)
			require.Equal(t, before, f.counts(t))
			_, err = f.reserve(t, 20, "gated-boundary")
			require.NoError(t, err)
			f.assertBalance(t, 80, held+20)
		})
	}
}

func TestCaptureSigningFailureLeavesHoldAndBalanceUntouched(t *testing.T) {
	for _, failure := range []string{"unavailable", "bad-signature", "empty-signature", "not-configured"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			r, err := f.reserve(t, 60, "budget")
			require.NoError(t, err)
			before := f.counts(t)
			svc := f.svc
			wantErr := core.ErrUnauthorizedJournal
			switch failure {
			case "unavailable":
				f.auth.fail, wantErr = true, errSignerUnavailable
			case "bad-signature":
				f.auth.corrupt = true
			case "empty-signature":
				f.auth.emptySig = true
			case "not-configured":
				svc, err = ledger.New(f.svc.Pool())
				require.NoError(t, err)
			}
			j, err := capture(t.Context(), svc, r, submittedEvent(false))
			require.ErrorIs(t, err, wantErr)
			require.Nil(t, j)
			require.Equal(t, before, f.counts(t))
			f.assertBalance(t, 100, 60)
			f.assertReservation(t, r.UID, "active", 0)
			// Recover signing and retry the same submitted event / keys.
			f.auth.fail, f.auth.corrupt, f.auth.emptySig = false, false, false
			_, err = capture(t.Context(), f.svc, r, submittedEvent(false))
			require.NoError(t, err)
			f.assertBalance(t, 80, 0)
		})
	}
}

func TestCaptureSettlementFailureRollsBackSignedCharge(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			f := newFixture(t)
			r, err := f.reserve(t, 60, "budget")
			require.NoError(t, err)
			before := f.counts(t)
			event := submittedEvent(partial)
			event.Amount = decimal.NewFromInt(61) // charge can post; settlement exceeds hold
			j, err := capture(t.Context(), f.svc, r, event)
			require.ErrorIs(t, err, core.ErrInvalidInput)
			require.ErrorContains(t, err, "exceeds reserved")
			require.Nil(t, j, "never return a receipt for a rolled-back journal")
			require.Equal(t, before, f.counts(t))
			f.assertBalance(t, 100, 60)
			f.assertReservation(t, r.UID, "active", 0)
		})
	}
}

func TestCapturePartialIncrementsUseDistinctEventKeys(t *testing.T) {
	f := newFixture(t)
	r, err := f.reserve(t, 60, "budget")
	require.NoError(t, err)
	event := submittedEvent(true)
	_, err = capture(t.Context(), f.svc, r, event)
	require.NoError(t, err)
	next := event
	next.ID, next.Amount = "submitted-usage-2", decimal.NewFromInt(10)
	next.RecordedAt = next.RecordedAt.Add(time.Second)
	j, err := capture(t.Context(), f.svc, r, next)
	require.NoError(t, err)
	before := f.counts(t)
	replayed, err := capture(t.Context(), f.svc, r, next)
	require.NoError(t, err)
	require.Equal(t, j.UID, replayed.UID)
	require.Equal(t, before, f.counts(t))
	require.Equal(t, 2, before.Legs)
	f.assertBalance(t, 70, 30)
	f.assertReservation(t, r.UID, "settling", 30)
}

func TestCaptureReplayRejectsUnverifiableHistory(t *testing.T) {
	for _, history := range []string{"unsigned-tx", "bad-signature"} {
		t.Run(history, func(t *testing.T) {
			f := newFixture(t)
			r, err := f.reserve(t, 60, "budget")
			require.NoError(t, err)
			event := submittedEvent(false)
			_, err = capture(t.Context(), f.svc, r, event)
			require.NoError(t, err)
			// A separate balanced +1 journal with an absent or invalid signature
			// makes the dimension undefined. The original capture is still signed.
			post := func(svc *ledger.Service) error {
				_, err := svc.JournalWriter().ExecuteTemplate(t.Context(), "deposit_confirm", core.TemplateParams{
					HolderID: holder, CurrencyUID: f.currency, IdempotencyKey: "unverifiable-extra-credit",
					Amounts: map[string]decimal.Decimal{"amount": decimal.NewFromInt(1)},
				})
				return err
			}
			if history == "unsigned-tx" {
				require.NoError(t, f.svc.RunInTx(t.Context(), post))
			} else {
				f.auth.corrupt = true
				require.NoError(t, post(f.svc))
				f.auth.corrupt = false
			}
			before := f.counts(t)
			plain, err := f.svc.BalanceReader().GetBalance(t.Context(), holder, f.currency, f.wallet)
			require.NoError(t, err)
			require.True(t, plain.Equal(decimal.NewFromInt(81)))
			_, err = f.svc.VerifiedBalanceReader().VerifiedBalance(t.Context(), holder, f.currency, f.wallet)
			require.ErrorIs(t, err, core.ErrUnauthorizedJournal)
			_, err = f.reserve(t, 1, "gate-unverifiable-history")
			require.ErrorIs(t, err, core.ErrUnauthorizedJournal)
			j, err := capture(t.Context(), f.svc, r, event)
			require.ErrorIs(t, err, core.ErrUnauthorizedJournal)
			require.Nil(t, j)
			require.Equal(t, before, f.counts(t))
			f.assertReservation(t, r.UID, "settled", 20)
			held, err := f.svc.Reserver().HeldAmount(t.Context(), holder, f.currency)
			require.NoError(t, err)
			require.True(t, held.IsZero())
		})
	}
}
