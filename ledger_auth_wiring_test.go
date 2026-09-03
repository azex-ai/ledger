package ledger_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/authdev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/service"
	"github.com/azex-ai/ledger/service/delivery"
)

func newTestAttestor(t *testing.T, keyID string) (core.Attestor, core.AuthVerifier) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	attestor, verifier, err := authdev.NewLocalAttestor(priv.Seed(), keyID)
	require.NoError(t, err)
	return attestor, verifier
}

// gateFixture is the smallest dimension the withdrawal gate can be pointed
// at: a holder with a funded balance_role='available' classification and its
// custodial system counterpart.
type gateFixture struct {
	holder       int64
	currencyUID  string
	walletUID    string
	custodialUID string
}

func seedGateFixture(t *testing.T, ctx context.Context, svc *ledger.Service, holder int64, amount decimal.Decimal) gateFixture {
	t.Helper()
	suffix := time.Now().UnixNano()

	cur, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{
		Code: fmt.Sprintf("VBU_%d", suffix), Name: "Verified Balance Unit", Exponent: 18,
	})
	require.NoError(t, err)
	wallet, err := svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: fmt.Sprintf("vb_main_%d", suffix), Name: "Verified Balance Main",
		NormalSide: core.NormalSideDebit, BalanceRole: core.BalanceRoleAvailable,
	})
	require.NoError(t, err)
	custodial, err := svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: fmt.Sprintf("vb_custodial_%d", suffix), Name: "Verified Balance Custodial",
		NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)
	jt, err := svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{
		Code: fmt.Sprintf("vb_fund_%d", suffix), Name: "Verified Balance Fund",
	})
	require.NoError(t, err)

	_, err = svc.JournalWriter().PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: postgrestest.UniqueKey("vb-fund"),
		Source:         "verified-balance-facade-test",
		ActorID:        holder,
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: cur.UID, ClassificationUID: wallet.UID, EntryType: core.EntryTypeDebit, Amount: amount},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: cur.UID, ClassificationUID: custodial.UID, EntryType: core.EntryTypeCredit, Amount: amount},
		},
	})
	require.NoError(t, err)

	return gateFixture{holder: holder, currencyUID: cur.UID, walletUID: wallet.UID, custodialUID: custodial.UID}
}

// TestService_WithAttestor_WiresTheWithdrawalGateVerifier pins ledger.go's
// `postgres.NewVerifiedBalanceStore(pool, s.authVerifier)` -- the one line
// that connects the verifier a consumer passes to ledger.WithAttestor to the
// gate that uses it.
//
// I-32/I-33's six existing pins cannot protect it: every one of them builds
// `postgres.NewVerifiedBalanceStore(pool, verifier)` by hand, which is the
// exact step a consumer never performs. Replacing that argument with nil left
// the whole suite green while turning the withdrawal gate from "verify and
// allow" into "reject every dimension that has ever been written to" for
// every consumer of the facade -- fail-closed, but permanently broken, and
// invisible to CI.
//
// Everything here goes through ledger.New. postgres.NewVerifiedBalanceStore
// is never named.
func TestService_WithAttestor_WiresTheWithdrawalGateVerifier(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	attestor, verifier := newTestAttestor(t, "withdrawal-gate-facade-key")
	svc, err := ledger.New(pool, ledger.WithAttestor(attestor, verifier))
	require.NoError(t, err)

	fx := seedGateFixture(t, ctx, svc, 9301, decimal.NewFromInt(100))

	// 1. The gate resolves the balance rather than refusing it. With
	//    s.authVerifier not reaching the store, this returns
	//    core.ErrUnauthorizedJournal instead: the funding journal above is
	//    signed, so the only way to fail here is having no verifier to check
	//    it with.
	balance, err := svc.VerifiedBalanceReader().VerifiedBalance(ctx, fx.holder, fx.currencyUID, fx.walletUID)
	require.NoError(t, err,
		"a signed journal posted through the facade must yield a verified balance; ledger.WithAttestor's "+
			"verifier is not reaching postgres.NewVerifiedBalanceStore")
	require.True(t, balance.Equal(decimal.NewFromInt(100)), "expected 100, got %s", balance)

	// 2. The same wiring seen from the withdrawal gate itself.
	res, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder:          fx.holder,
		CurrencyUID:            fx.currencyUID,
		Amount:                 decimal.NewFromInt(10),
		IdempotencyKey:         postgrestest.UniqueKey("vb-reserve"),
		ExpiresIn:              10 * time.Minute,
		RequireVerifiedBalance: true,
	})
	require.NoError(t, err,
		"Reserve with RequireVerifiedBalance must pass on a fully signed dimension when the facade wired the verifier")
	require.NotEmpty(t, res.UID)
}

// TestService_WithAttestor_GateRejectsAnUnverifiableJournal is the negative
// half: the gate must actually be a gate. A journal composed inside RunInTx
// can never be signed (core.AuthStatusUnsignedTxMode -- see RunInTx's doc
// comment), so it permanently poisons its dimension, and the gate has to say
// so rather than hand back a number.
//
// Without this, the positive test above could be satisfied by a gate that
// approves everything.
func TestService_WithAttestor_GateRejectsAnUnverifiableJournal(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	attestor, verifier := newTestAttestor(t, "withdrawal-gate-negative-key")
	svc, err := ledger.New(pool, ledger.WithAttestor(attestor, verifier))
	require.NoError(t, err)

	fx := seedGateFixture(t, ctx, svc, 9302, decimal.NewFromInt(100))

	// Control: signed-only, the gate resolves.
	_, err = svc.VerifiedBalanceReader().VerifiedBalance(ctx, fx.holder, fx.currencyUID, fx.walletUID)
	require.NoError(t, err)

	// Add one journal that cannot carry a signature.
	jt, err := svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{
		Code: fmt.Sprintf("vb_txmode_%d", time.Now().UnixNano()), Name: "Verified Balance Tx Mode",
	})
	require.NoError(t, err)
	require.NoError(t, svc.RunInTx(ctx, func(tx *ledger.Service) error {
		_, postErr := tx.JournalWriter().PostJournal(ctx, core.JournalInput{
			JournalTypeUID: jt.UID,
			IdempotencyKey: postgrestest.UniqueKey("vb-txmode"),
			Source:         "verified-balance-facade-test",
			ActorID:        fx.holder,
			Entries: []core.EntryInput{
				{AccountHolder: fx.holder, CurrencyUID: fx.currencyUID, ClassificationUID: fx.walletUID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1)},
				{AccountHolder: core.SystemAccountHolder(fx.holder), CurrencyUID: fx.currencyUID, ClassificationUID: fx.custodialUID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1)},
			},
		})
		return postErr
	}))

	_, err = svc.VerifiedBalanceReader().VerifiedBalance(ctx, fx.holder, fx.currencyUID, fx.walletUID)
	require.ErrorIs(t, err, core.ErrUnauthorizedJournal,
		"a dimension containing an unsigned (tx-mode) journal must come back UNDEFINED, never a number")
}

// ---------------------------------------------------------------------------
// I-R1 / B-m1 — EventStore.SetLogger, wired at last
// ---------------------------------------------------------------------------

// TestService_EventStoreClaimLostWarningsReachTheInjectedLogger pins the two
// lines this round adds to ledger.go. EventStore.SetLogger was added a round
// ago to fix "claim-lost warnings bypass the consumer's logger", and then
// nothing ever called it -- its own doc comment told the reader to wire it
// "from your composition root" while this library's composition root, the one
// place a facade consumer has, did not. Behaviour was unchanged by that fix:
// the three claim-lost warnings still went to slog.Default(), which in a
// structured-logging pipeline means nowhere.
//
// The claim-lost line is the only signal that a delivery lease was stolen and
// an outcome dropped; MarkDelivered still returns nil in that case, so the
// caller cannot tell.
func TestService_EventStoreClaimLostWarningsReachTheInjectedLogger(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	logger := &recordingLogger{}
	svc, err := ledger.New(pool, ledger.WithLogger(logger))
	require.NoError(t, err)

	// The EventStore ledger.New built and hands out via EventReader(). The
	// assertion is on that instance, so it fails if ledger.New stops wiring
	// the logger into it.
	poller, ok := svc.EventReader().(delivery.EventPoller)
	require.True(t, ok, "the facade's EventReader is the delivery poller; if that changes this pin needs a new route to it")

	// A claim token that matches nothing: the update touches zero rows, which
	// is exactly the "someone else re-claimed this event" case.
	require.NoError(t, poller.MarkDelivered(ctx, 987654321, time.Unix(1, 0).UTC()))

	require.True(t, logger.contains("claim lost", "987654321"),
		"the claim-lost warning must reach the logger passed to ledger.WithLogger, not slog.Default(); got %v", logger.snapshot())
}

// TestServiceWorker_EventPollerClaimLostWarningsReachTheInjectedLogger pins
// the second half of the same wiring: the dedicated *postgres.EventStore
// (*Service).Worker builds for the delivery loop. That instance is the one
// that actually races other replicas for leases, so it is the one whose
// claim-lost warnings matter most -- and it is invisible from outside the
// Worker, which is why this drives it end to end instead of asserting on a
// field.
//
// The sequence is deterministic, not timing-dependent: the handler blocks
// until the test has re-claimed the very event the worker is holding, so the
// worker's MarkDelivered is guaranteed to arrive with a stale claim token.
func TestServiceWorker_EventPollerClaimLostWarningsReachTheInjectedLogger(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	logger := &recordingLogger{}
	svc, err := ledger.New(pool, ledger.WithLogger(logger))
	require.NoError(t, err)

	cfg := service.DefaultWorkerConfig()
	cfg.EventDeliveryInterval = 50 * time.Millisecond
	// A lease this short means the worker's own claim has already expired by
	// the time the handler returns, so the test below can re-claim the row.
	cfg.EventClaimLease = time.Millisecond
	worker, err := svc.Worker(cfg)
	require.NoError(t, err)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	require.NoError(t, worker.Subscribe(func(context.Context, core.Event) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	}))

	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancel()
		releaseOnce()
		<-done
	})

	// Produce one event through the ordinary path.
	deps := seedSubscribeFixture(t, ctx, svc)
	booking, err := svc.Booker().CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: "deposit",
		AccountHolder:      9401,
		CurrencyUID:        deps.currencyUID,
		Amount:             decimal.NewFromInt(25),
		IdempotencyKey:     postgrestest.UniqueKey("claim-lost"),
	})
	require.NoError(t, err)
	_, err = svc.Booker().Transition(ctx, core.TransitionInput{
		BookingUID:     booking.UID,
		ToStatus:       "confirming",
		Source:         "claim-lost-test",
		IdempotencyKey: postgrestest.UniqueKey("claim-lost-transition"),
	})
	require.NoError(t, err)

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("the worker never delivered the event to the handler; logs: %v", logger.snapshot())
	}

	// Steal the lease while the handler is still blocked. The worker's
	// in-flight MarkDelivered will then find zero rows -- "claim lost,
	// outcome dropped".
	thief, ok := svc.EventReader().(delivery.EventPoller)
	require.True(t, ok)
	require.Eventually(t, func() bool {
		pending, pollErr := thief.GetPendingEvents(ctx, 10)
		return pollErr == nil && len(pending) > 0
	}, 10*time.Second, 50*time.Millisecond, "the worker's 1ms lease should have expired, making the event re-claimable")

	releaseOnce()

	require.Eventually(t, func() bool { return logger.contains("claim lost") }, 10*time.Second, 50*time.Millisecond,
		"the Worker's event poller must route its claim-lost warning through the logger passed to "+
			"ledger.WithLogger; without that wiring it goes to slog.Default() and a stolen lease is invisible. Logs: %v",
		logger.snapshot())
}

// TestService_WithAttestor_WiresTheSignedDischargeHold pins the OTHER line
// ledger.New's WithAttestor branch owns: `s.reserverStore =
// s.reserverStore.WithAuth(s.attestor, s.authVerifier)` (I-65).
//
// It exists for the identical reason the verifier pin above does, and against
// the identical blind spot. Every I-65 pin in the postgres package builds
// `postgres.NewReserverStore(...).WithAuth(attestor, verifier)` by hand --
// the one step a consumer of the facade never performs. Deleting that line
// from ledger.go leaves the whole postgres suite green while silently giving
// every facade consumer I-49's conservative hold instead of the signed one
// they configured an Attestor to get: safe, but not what they asked for, and
// invisible to CI.
//
// Everything here goes through ledger.New. postgres.NewReserverStore and
// ReserverStore.WithAuth are never named.
func TestService_WithAttestor_WiresTheSignedDischargeHold(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	attestor, verifier := newTestAttestor(t, "signed-discharge-facade-key")
	svc, err := ledger.New(pool, ledger.WithAttestor(attestor, verifier))
	require.NoError(t, err)

	fx := seedGateFixture(t, ctx, svc, 9302, decimal.NewFromInt(100))

	gatedReserve := func(amount int64, key string) (*core.Reservation, error) {
		return svc.Reserver().Reserve(ctx, core.ReserveInput{
			AccountHolder:          fx.holder,
			CurrencyUID:            fx.currencyUID,
			Amount:                 decimal.NewFromInt(amount),
			IdempotencyKey:         postgrestest.UniqueKey(key),
			ExpiresIn:              time.Hour,
			RequireVerifiedBalance: true,
		})
	}

	res, err := gatedReserve(100, "signed-discharge-facade-reserve")
	require.NoError(t, err)

	// Control: the whole balance is held, so the pin below cannot pass by
	// the gate being permissive.
	_, err = gatedReserve(100, "signed-discharge-facade-control")
	require.ErrorIs(t, err, core.ErrInsufficientBalance)

	require.NoError(t, svc.Reserver().Release(ctx, core.ReleaseInput{
		ReservationUID: res.UID,
		IdempotencyKey: postgrestest.UniqueKey("signed-discharge-facade-release"),
	}))

	// The pin: the release was signed on the way in and verified on the way
	// out, so the funds are back at once. Without the WithAuth line this is
	// core.ErrInsufficientBalance until the reservation expires an hour from
	// now.
	again, err := gatedReserve(100, "signed-discharge-facade-after")
	require.NoError(t, err,
		"a release issued through the facade must discharge the gated hold immediately; ledger.WithAttestor's "+
			"attestor/verifier are not reaching the reserver")
	require.NotEmpty(t, again.UID)
}

// TestService_UnverifiableDischargeWarningReachesTheInjectedLogger pins the
// other half of ledger.New's reserver wiring: `s.reserverStore.SetLogger`.
//
// A discharge claim that FAILS verification is tamper evidence, and the
// gate's reaction to it -- keep holding the funds -- is correct but
// invisible: the caller just sees a smaller available balance. That is the
// shape working-agreements §3 calls out (a degradation indistinguishable
// from nothing having happened), and a Warn line is the whole signal. If it
// goes to a NopLogger because the composition root forgot to wire one, the
// signal does not exist.
//
// Drives it end to end: a forged 'release' receipt appended straight to the
// append-only table (the C-1 statement) makes the gate verify a claim that
// carries no signature.
func TestService_UnverifiableDischargeWarningReachesTheInjectedLogger(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	attestor, verifier := newTestAttestor(t, "discharge-warning-facade-key")
	logger := &recordingLogger{}
	svc, err := ledger.New(pool, ledger.WithAttestor(attestor, verifier), ledger.WithLogger(logger))
	require.NoError(t, err)

	fx := seedGateFixture(t, ctx, svc, 9303, decimal.NewFromInt(100))

	res, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder:          fx.holder,
		CurrencyUID:            fx.currencyUID,
		Amount:                 decimal.NewFromInt(100),
		IdempotencyKey:         postgrestest.UniqueKey("discharge-warning-reserve"),
		ExpiresIn:              time.Hour,
		RequireVerifiedBalance: true,
	})
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		INSERT INTO reservation_operation_receipts
			(reservation_id, operation, idempotency_key, amount)
		SELECT id, 'release', 'facade-forged-release-' || id, 0
		FROM reservations WHERE uid = $1`, res.UID)
	require.NoError(t, err)

	// The gate runs the verification during this call; the answer it gives
	// the caller is unchanged (the funds stay held), which is exactly why the
	// log line is the only evidence.
	_, err = svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder:          fx.holder,
		CurrencyUID:            fx.currencyUID,
		Amount:                 decimal.NewFromInt(100),
		IdempotencyKey:         postgrestest.UniqueKey("discharge-warning-after"),
		ExpiresIn:              time.Hour,
		RequireVerifiedBalance: true,
	})
	require.ErrorIs(t, err, core.ErrInsufficientBalance, "control: the forged claim must not discharge the hold")

	require.True(t, logger.contains("discharge claim does not verify", res.UID),
		"a claim that fails verification must be reported through the logger passed to ledger.WithLogger; got %v", logger.snapshot())
}
