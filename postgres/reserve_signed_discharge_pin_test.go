package postgres_test

// Pins for the signed half of the gated hold (docs/INVARIANTS.md I-65,
// remediation contract §7.18).
//
// I-49 left the gated hold crediting NO discharge claim, because every claim
// -- reservations.status, reservations.settled_amount, an appended
// settlement leg, an appended operation receipt -- is writable or appendable
// with the application's own credential, and in this threat model that
// credential is the attacker. It cost a settled or released reservation its
// full hold until expires_at, and it recorded the alternative it did not
// take: sign the claim, so the discharge becomes unforgeable rather than
// unnecessary.
//
// These pins hold BOTH sides of that:
//
//   - with signing configured, a forged or unverifiable claim still
//     discharges nothing (the C-1 attack, now aimed at the signed path), and
//   - a LEGITIMATE discharge gives the funds back immediately, which is the
//     whole product reason for doing this instead of leaving I-49's rule in
//     place.
//
// Every attack below is issued as ledger_app over a real socket, not as the
// test superuser, except where the pin is explicitly about a party ABOVE
// that credential (the guard-disabled UPDATE in
// TestReserve_SignedDischarge_RejectsTamperedAmount, which is called out
// there).

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

// setupSignedHoldFixture is setupHoldFixture with the reserver wired for
// signed discharge claims (postgres.ReserverStore.WithAuth). Deliberately a
// separate constructor rather than a flag on the existing one: the unsigned
// fixture is what every I-49 pin uses, and those pins are the control group
// for these -- they must keep exercising the un-wired store byte for byte.
func setupSignedHoldFixture(t *testing.T, pool *pgxpool.Pool, holder int64, keyID string, deposit int64) holdFixture {
	t.Helper()
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)

	attestor, verifier := newTestAttestor(t, keyID)
	ledger := postgres.NewLedgerStore(pool).WithAuth(attestor)
	_, err := ledger.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("signed-hold-deposit"), decimal.NewFromInt(deposit)))
	require.NoError(t, err)

	return holdFixture{
		f:      f,
		ledger: ledger,
		reserver: postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, verifier)).
			WithAuth(attestor, verifier),
		attacker: ledgerAppPool(t, ctx, pool, "w4-signed-holds-app-credential"),
		holder:   holder,
	}
}

// TestReserve_SignedDischarge_ForgedClaimDischargesNothing is C-1's attack
// re-aimed at the signed path: the credential that can append a claim row
// cannot produce a signature over it, so appending one buys nothing.
//
// Three shapes, because "unsigned" and "wrongly signed" are different rows
// and a naive implementation can get one right and the other wrong:
//
//	unsigned:  INSERT a 'release' receipt with the auth_* columns left at
//	           their defaults ('' / '' / ''). This is what a forged row looks
//	           like, and it is also what every row written before migration
//	           028 looks like -- so the gate's answer has to be the same for
//	           both: no discharge.
//	garbage:   INSERT one with plausible-looking but invalid signature bytes.
//	replayed:  INSERT one carrying the digest/signature/key_id COPIED
//	           verbatim from a genuinely signed claim on the same reservation.
//	           This is the attack a digest that covered too little would let
//	           through: the signature is real, so a verifier that only asked
//	           "does this signature check out against the stored digest"
//	           would accept it. It fails because the digest is recomputed
//	           from the row's own uid/operation/amount/key/created_at, which
//	           are not the ones that were signed.
func TestReserve_SignedDischarge_ForgedClaimDischargesNothing(t *testing.T) {
	// Deposit 1001, reserve 1000: the spare 1 lets the "replayed" variant
	// mint a genuine donor claim of its own without competing with the
	// reservation under test for the balance, and is small enough that the
	// control assertions below (a second 1000 must be refused) still hold.
	const (
		deposit = 1001
		reserve = 1000
	)

	variants := []struct {
		name    string
		holder  int64
		keyID   string
		tampers func(t *testing.T, h holdFixture, reservationUID string)
	}{
		{
			name:   "unsigned release receipt",
			holder: 9501,
			keyID:  "ed25519-signed-hold-unsigned",
			tampers: func(t *testing.T, h holdFixture, reservationUID string) {
				forgeReleaseReceipt(t, h, reservationUID, "forged-unsigned", nil, nil, "")
			},
		},
		{
			name:   "release receipt with garbage signature",
			holder: 9502,
			keyID:  "ed25519-signed-hold-garbage",
			tampers: func(t *testing.T, h holdFixture, reservationUID string) {
				forgeReleaseReceipt(t, h, reservationUID, "forged-garbage",
					[]byte("not-a-digest-but-32-bytes-long!!"), []byte("not-a-signature"), "ed25519-signed-hold-garbage")
			},
		},
		{
			name:   "release receipt replaying a genuine signature from another claim",
			holder: 9503,
			keyID:  "ed25519-signed-hold-replay",
			tampers: func(t *testing.T, h holdFixture, reservationUID string) {
				ctx := context.Background()
				// A genuine, signed claim to steal the signature from: a
				// finalize on a DIFFERENT reservation of the same holder, so
				// the bytes really were minted by the real key.
				donor, err := h.reserveFor(ctx, 1, "signed-hold-replay-donor", time.Hour, false)
				require.NoError(t, err)
				require.NoError(t, h.reserver.SettlePartial(ctx, core.SettlePartialInput{
					ReservationUID: donor.UID,
					Amount:         decimal.NewFromInt(1),
					IdempotencyKey: postgrestest.UniqueKey("signed-hold-replay-donor-leg"),
				}))
				require.NoError(t, h.reserver.FinalizeSettlement(ctx, core.FinalizeSettlementInput{
					ReservationUID: donor.UID,
					IdempotencyKey: postgrestest.UniqueKey("signed-hold-replay-donor-final"),
				}))

				var digest, signature []byte
				var keyID string
				require.NoError(t, h.attacker.QueryRow(ctx, `
					SELECT o.auth_digest, o.auth_signature, o.auth_key_id
					FROM reservation_operation_receipts o
					JOIN reservations r ON r.id = o.reservation_id
					WHERE r.uid = $1 AND o.operation = 'finalize_settlement'`, donor.UID).
					Scan(&digest, &signature, &keyID))
				require.NotEmpty(t, signature, "test setup: the donor claim must actually be signed, or this variant proves nothing")

				forgeReleaseReceipt(t, h, reservationUID, "forged-replay", digest, signature, keyID)
			},
		},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			pool := postgrestest.SetupDB(t)
			ctx := context.Background()
			h := setupSignedHoldFixture(t, pool, v.holder, v.keyID, deposit)

			first, err := h.reserveFor(ctx, reserve, "signed-hold-first", time.Hour, true)
			require.NoError(t, err)
			require.NotNil(t, first)

			// Control: with the hold intact the gate refuses a second bite at
			// the same 1000, so a green pin below cannot be "it refuses
			// everything".
			_, err = h.reserveFor(ctx, reserve, "signed-hold-control", time.Hour, true)
			require.ErrorIs(t, err, core.ErrInsufficientBalance, "control: the untampered hold must already refuse the second reservation")

			v.tampers(t, h, first.UID)

			_, err = h.reserveFor(ctx, reserve, "signed-hold-after-forgery", time.Hour, true)
			require.ErrorIs(t, err, core.ErrInsufficientBalance,
				"a discharge claim the application's credential appended without a valid signature must discharge nothing")

			// Control 2: the forged row really is in the table, so the pin is
			// about the gate ignoring it rather than about the INSERT failing.
			var forged int
			require.NoError(t, pool.QueryRow(ctx, `
				SELECT count(*) FROM reservation_operation_receipts o
				JOIN reservations r ON r.id = o.reservation_id
				WHERE r.uid = $1 AND o.idempotency_key LIKE 'forged-%'`, first.UID).Scan(&forged))
			require.Equal(t, 1, forged, "sanity: the forged receipt must actually be in the table")
		})
	}
}

// forgeReleaseReceipt appends a 'release' claim for the reservation using the
// application's own credential -- the one statement C-1 is about -- with
// whatever signature material the caller wants to try.
func forgeReleaseReceipt(t *testing.T, h holdFixture, reservationUID, keyPrefix string, digest, signature []byte, keyID string) {
	t.Helper()
	ctx := context.Background()
	tag, err := h.attacker.Exec(ctx, `
		INSERT INTO reservation_operation_receipts
			(reservation_id, operation, idempotency_key, amount, auth_digest, auth_signature, auth_key_id)
		SELECT id, 'release', $2 || '-' || id, 0, COALESCE($3::bytea, ''::bytea), COALESCE($4::bytea, ''::bytea), $5
		FROM reservations WHERE uid = $1`, reservationUID, keyPrefix, digest, signature, keyID)
	require.NoError(t, err, "the schema permits this statement by design -- if it starts failing, the grants changed and this pin needs rewriting, not deleting")
	require.EqualValues(t, 1, tag.RowsAffected())
}

// TestReserve_SignedDischarge_RestoresAvailabilityImmediately is the reason
// I-65 exists rather than leaving I-49's conservative rule in place: a
// LEGITIMATE discharge, signed as it is written, gives the funds back at
// once instead of at expires_at.
//
// The three legitimate shapes, each asserted against the exact amount that
// should be available afterwards (not merely "something succeeds"), so a
// hold that is discharged by too much would be just as red as one
// discharged by too little:
//
//	release:            hold -> 0, base unchanged (nothing was spent)
//	settle + journal:   hold -> 0, base fell by the settled amount
//	partial + finalize: hold -> unsettled remainder, then -> 0
func TestReserve_SignedDischarge_RestoresAvailabilityImmediately(t *testing.T) {
	t.Run("a signed Release returns the whole amount at once", func(t *testing.T) {
		pool := postgrestest.SetupDB(t)
		ctx := context.Background()
		h := setupSignedHoldFixture(t, pool, 9504, "ed25519-signed-hold-release", 1000)

		res, err := h.reserveFor(ctx, 1000, "signed-release-reserve", time.Hour, true)
		require.NoError(t, err)

		_, err = h.reserveFor(ctx, 1, "signed-release-before", time.Hour, true)
		require.ErrorIs(t, err, core.ErrInsufficientBalance, "control: everything is held before the release")

		require.NoError(t, h.reserver.Release(ctx, core.ReleaseInput{
			ReservationUID: res.UID,
			IdempotencyKey: postgrestest.UniqueKey("signed-release-op"),
		}))

		// The full 1000 is available again immediately -- no expiry wait,
		// which is exactly what I-49 could not offer.
		again, err := h.reserveFor(ctx, 1000, "signed-release-after", time.Hour, true)
		require.NoError(t, err, "a signed release must discharge the hold immediately")
		require.NotNil(t, again)
	})

	t.Run("a signed Settle discharges the hold and the spend shows in the base", func(t *testing.T) {
		pool := postgrestest.SetupDB(t)
		ctx := context.Background()
		h := setupSignedHoldFixture(t, pool, 9505, "ed25519-signed-hold-settle", 1000)

		res, err := h.reserveFor(ctx, 1000, "signed-settle-reserve", time.Hour, true)
		require.NoError(t, err)

		_, err = h.ledger.PostJournal(ctx, h.f.spendInput(h.holder, postgrestest.UniqueKey("signed-settle-charge"), decimal.NewFromInt(400)))
		require.NoError(t, err)
		require.NoError(t, h.reserver.Settle(ctx, core.SettleInput{
			ReservationUID: res.UID,
			Amount:         decimal.NewFromInt(400),
			IdempotencyKey: postgrestest.UniqueKey("signed-settle-op"),
		}))

		// 400 left through its own journal, so 600 -- and exactly 600 -- is
		// available. The settled portion is counted once now, not twice as
		// I-49 had to.
		_, err = h.reserveFor(ctx, 601, "signed-settle-over", time.Hour, true)
		require.ErrorIs(t, err, core.ErrInsufficientBalance, "the discharge must not credit more than the reservation held")
		again, err := h.reserveFor(ctx, 600, "signed-settle-after", time.Hour, true)
		require.NoError(t, err, "the whole reservation is discharged by a signed settle; the spend is visible in the base")
		require.NotNil(t, again)
	})

	t.Run("signed legs discharge only the settled part until finalize", func(t *testing.T) {
		pool := postgrestest.SetupDB(t)
		ctx := context.Background()
		h := setupSignedHoldFixture(t, pool, 9506, "ed25519-signed-hold-partial", 1000)

		res, err := h.reserveFor(ctx, 1000, "signed-partial-reserve", time.Hour, true)
		require.NoError(t, err)

		_, err = h.ledger.PostJournal(ctx, h.f.spendInput(h.holder, postgrestest.UniqueKey("signed-partial-charge"), decimal.NewFromInt(400)))
		require.NoError(t, err)
		require.NoError(t, h.reserver.SettlePartial(ctx, core.SettlePartialInput{
			ReservationUID: res.UID,
			Amount:         decimal.NewFromInt(400),
			IdempotencyKey: postgrestest.UniqueKey("signed-partial-leg"),
		}))

		// Base 600, hold 600 (the unsettled remainder): nothing spare yet.
		// This is the leg being credited exactly, not the whole reservation.
		_, err = h.reserveFor(ctx, 1, "signed-partial-mid", time.Hour, true)
		require.ErrorIs(t, err, core.ErrInsufficientBalance,
			"a settlement leg discharges its own amount only -- the unsettled remainder is still held")

		require.NoError(t, h.reserver.FinalizeSettlement(ctx, core.FinalizeSettlementInput{
			ReservationUID: res.UID,
			IdempotencyKey: postgrestest.UniqueKey("signed-partial-final"),
		}))

		again, err := h.reserveFor(ctx, 600, "signed-partial-after", time.Hour, true)
		require.NoError(t, err, "a signed finalize discharges the remainder immediately")
		require.NotNil(t, again)
	})
}

// TestReserve_SignedDischarge_RejectsTamperedAmount pins that the digest
// binds the AMOUNT, not just the fact that a claim exists.
//
// Unlike the other pins here the tamper is issued as the migration runner
// with the append-only trigger temporarily disabled, because ledger_app
// cannot UPDATE these tables at all (migration 006) -- so this is
// deliberately a claim about a party ABOVE the application credential: even
// someone who can rewrite the row cannot make the rewritten amount verify.
// That is the property the signature adds over append-only-ness, and the
// reason the whole reservation (not just the tampered claim) falls back to
// holding in full.
func TestReserve_SignedDischarge_RejectsTamperedAmount(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	h := setupSignedHoldFixture(t, pool, 9507, "ed25519-signed-hold-tampered", 1000)

	res, err := h.reserveFor(ctx, 1000, "signed-tamper-reserve", time.Hour, true)
	require.NoError(t, err)

	_, err = h.ledger.PostJournal(ctx, h.f.spendInput(h.holder, postgrestest.UniqueKey("signed-tamper-charge"), decimal.NewFromInt(100)))
	require.NoError(t, err)
	require.NoError(t, h.reserver.SettlePartial(ctx, core.SettlePartialInput{
		ReservationUID: res.UID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("signed-tamper-leg"),
	}))

	// Control: the genuine, signed leg IS credited -- base 900, hold 900, so
	// 900 is exactly what a second gated reservation may not exceed but may
	// reach only after the remainder is discharged. Assert the credit
	// directly instead: 100 of the hold is gone, so the balance drop is
	// matched and nothing spare appeared.
	_, err = h.reserveFor(ctx, 1, "signed-tamper-control", time.Hour, true)
	require.ErrorIs(t, err, core.ErrInsufficientBalance, "control: base and hold both fell by 100, so nothing is spare")

	// Now rewrite the signed leg's amount to the full reservation. Under a
	// gate that trusted the row, this would discharge the whole hold.
	for _, stmt := range []string{
		`ALTER TABLE reservation_settlement_legs DISABLE TRIGGER reservation_settlement_legs_no_update`,
		`UPDATE reservation_settlement_legs SET amount = 1000 WHERE reservation_id = (SELECT id FROM reservations WHERE uid = $1)`,
		`ALTER TABLE reservation_settlement_legs ENABLE TRIGGER reservation_settlement_legs_no_update`,
	} {
		if _, err := pool.Exec(ctx, stmt, res.UID); err != nil {
			// The UPDATE takes the uid parameter; the two ALTERs do not.
			_, err = pool.Exec(ctx, stmt)
			require.NoError(t, err)
		}
	}

	var tamperedAmount decimal.Decimal
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT l.amount FROM reservation_settlement_legs l
		JOIN reservations r ON r.id = l.reservation_id WHERE r.uid = $1`, res.UID).Scan(&tamperedAmount))
	require.True(t, tamperedAmount.Equal(decimal.NewFromInt(1000)), "test setup: the tamper must have landed, got %s", tamperedAmount)

	// The pin: the amount no longer matches the digest, so the claim is not
	// trusted, so the reservation holds its FULL 1000 again -- strictly less
	// available than before the tamper, never more.
	_, err = h.reserveFor(ctx, 1, "signed-tamper-after", time.Hour, true)
	require.ErrorIs(t, err, core.ErrInsufficientBalance,
		"a signed claim whose amount was rewritten must discharge nothing; the reservation falls back to holding in full")
}

// TestReserve_SignedDischarge_NoAttestorKeepsConservativeHold is the control
// group for every pin above, and the guarantee that turning this feature ON
// is the only way to change behaviour.
//
// The identical script -- reserve everything, release it legitimately, try
// again -- is run against two stores that differ in exactly one call:
// WithAuth. The unsigned one must still refuse (I-49's rule, which is what
// every deployment that never configures an Attestor keeps getting); the
// signed one must allow. Asserting the divergence rather than tolerating it
// is what makes "no Attestor => zero behaviour change" a checked claim
// instead of a comment.
func TestReserve_SignedDischarge_NoAttestorKeepsConservativeHold(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	run := func(t *testing.T, h holdFixture) error {
		res, err := h.reserveFor(ctx, 1000, "conservative-reserve", time.Hour, true)
		require.NoError(t, err)
		require.NoError(t, h.reserver.Release(ctx, core.ReleaseInput{
			ReservationUID: res.UID,
			IdempotencyKey: postgrestest.UniqueKey("conservative-release"),
		}))
		_, err = h.reserveFor(ctx, 1000, "conservative-after", time.Hour, true)
		return err
	}

	unsigned := setupHoldFixture(t, pool, 9508, "ed25519-conservative-unsigned", 1000)
	require.ErrorIs(t, run(t, unsigned), core.ErrInsufficientBalance,
		"without WithAuth the gate must keep I-49's rule: a released reservation holds until expiry")

	signed := setupSignedHoldFixture(t, pool, 9509, "ed25519-conservative-signed", 1000)
	require.NoError(t, run(t, signed),
		"with WithAuth the same legitimate release discharges the hold immediately")
}

// TestReserve_SignedDischarge_RecomputedUnderLock is the signed-path
// counterpart of TestReserve_RequireVerifiedBalance_HoldRecomputedUnderLock.
// Signing moved the DISCHARGE outside the transaction; it must not have
// moved the reservations themselves out with it. A reservation that commits
// while a second transaction holds the (holder, currency) advisory lock has
// no discharge claim at all, so it must be visible to the gated Reserve
// queued behind that lock and must hold in full -- the over-sell race
// I-4/I-11 exist to close.
func TestReserve_SignedDischarge_RecomputedUnderLock(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	h := setupSignedHoldFixture(t, pool, 9510, "ed25519-signed-hold-lock", 1000)

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

		if _, err := h.reserver.WithDB(tx, h.ledger.WithDB(tx)).Reserve(ctx, core.ReserveInput{
			AccountHolder:  h.holder,
			CurrencyUID:    h.f.CurrencyUID,
			Amount:         decimal.NewFromInt(600),
			IdempotencyKey: postgrestest.UniqueKey("signed-hold-lock-concurrent"),
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

	_, err := h.reserveFor(ctx, 600, "signed-hold-lock-gated", time.Hour, true)
	require.NoError(t, <-committed, "test setup: the concurrent reservation must commit")
	require.ErrorIs(t, err, core.ErrInsufficientBalance,
		"the per-reservation hold must be re-read under the balance advisory lock; only the discharge may come from outside it")
}

// TestReserverStore_SignedDischarge_RefusesExpiredSettlement keeps I-49's
// other half honest under signing. The gate stops counting a reservation at
// expires_at, and that is only sound while an expired reservation can no
// longer take money -- a rule that has nothing to do with signatures and
// must therefore survive them. Signing changes which discharges count, never
// whether an expired reservation may settle.
func TestReserverStore_SignedDischarge_RefusesExpiredSettlement(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	h := setupSignedHoldFixture(t, pool, 9511, "ed25519-signed-hold-expired", 10000)

	live, err := h.reserveFor(ctx, 100, "signed-expired-live", time.Hour, false)
	require.NoError(t, err)
	require.NoError(t, h.reserver.Settle(ctx, core.SettleInput{
		ReservationUID: live.UID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("signed-expired-live-op"),
	}), "control: a live reservation still settles under a configured Attestor")

	expiring, err := h.reserveFor(ctx, 100, "signed-expired-target", 2*time.Second, false)
	require.NoError(t, err)
	waitUntilExpired(t, pool, expiring.UID)

	require.ErrorIs(t, h.reserver.Settle(ctx, core.SettleInput{
		ReservationUID: expiring.UID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("signed-expired-op"),
	}), core.ErrInvalidTransition, "an expired reservation must not become settleable just because signing is on")

	require.NoError(t, h.reserver.Release(ctx, core.ReleaseInput{
		ReservationUID: expiring.UID,
		IdempotencyKey: "signed-expire-release-" + expiring.UID,
	}), "the expiration sweeper's path must keep working, and its receipt must still be signable")
}

// TestReserve_SignedDischarge_ReplayDoesNotResign pins that a retried
// discharge (same key, same payload) short-circuits on its existing claim
// row without asking the Attestor for a second signature. attestJournal
// makes the identical pre-check for the identical reason: an Attestor may be
// a remote signer, and a lost-response retry loop must not turn into a
// signing-rate problem.
func TestReserve_SignedDischarge_ReplayDoesNotResign(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)
	holder := int64(9512)

	inner, verifier := newTestAttestor(t, "ed25519-signed-hold-replay-count")
	counter := &countingAttestor{inner: inner}
	ledger := postgres.NewLedgerStore(pool).WithAuth(inner)
	_, err := ledger.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("signed-replay-deposit"), decimal.NewFromInt(1000)))
	require.NoError(t, err)

	reserver := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, verifier)).
		WithAuth(counter, verifier)

	res, err := reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:  holder,
		CurrencyUID:    f.CurrencyUID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("signed-replay-reserve"),
		ExpiresIn:      time.Hour,
	})
	require.NoError(t, err)

	key := postgrestest.UniqueKey("signed-replay-release")
	for i := 0; i < 3; i++ {
		require.NoError(t, reserver.Release(ctx, core.ReleaseInput{
			ReservationUID: res.UID,
			IdempotencyKey: key,
		}), "replay %d must return the original success", i)
	}
	require.EqualValues(t, 1, counter.signCalls.Load(),
		"a replayed discharge must reuse its existing claim row, not mint a second signature")
}
