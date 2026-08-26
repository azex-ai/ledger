package ledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/presets"
	"github.com/azex-ai/ledger/service"
)

// TestServiceWorker_SubscribeWorksWithoutManualWiring pins the wiring a
// consumer actually gets, rather than the wiring a test can arrange for
// itself.
//
// Worker.Subscribe shipped broken: it built its dispatcher with a nil poller
// and required a separate SetLocalPoller call before Run. Nothing outside the
// test suite ever made that call, so an in-process subscription silently
// received nothing while logging a failure every tick. ledger.Service.Worker
// now wires the poller, and that is the line this test exists to protect.
//
// The four tests in service/worker_subscribe_test.go do not protect it. Every
// one of them builds a Worker by hand and calls SetLocalPoller itself -- the
// same manual step whose absence was the original bug. Delete the wiring from
// ledger.go and all four still pass. This test goes through the facade a
// consumer calls, so it fails when the wiring is gone.
func TestServiceWorker_SubscribeWorksWithoutManualWiring(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	svc, err := ledger.New(pool)
	require.NoError(t, err)

	cfg := service.DefaultWorkerConfig()
	cfg.EventDeliveryInterval = 50 * time.Millisecond
	worker := svc.Worker(cfg)

	received := make(chan core.Event, 4)
	// The only call a consumer makes. No SetLocalPoller.
	worker.Subscribe(func(_ context.Context, evt core.Event) error {
		received <- evt
		return nil
	})

	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancelWorker()
		<-workerDone
	})

	// Produce one event through the ordinary path: a booking transition.
	deps := seedSubscribeFixture(t, ctx, svc)
	booking, err := svc.Booker().CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: "deposit",
		AccountHolder:      9101,
		CurrencyUID:        deps.currencyUID,
		Amount:             decimal.NewFromInt(25),
		IdempotencyKey:     postgrestest.UniqueKey("subscribe-wiring"),
	})
	require.NoError(t, err)

	_, err = svc.Booker().Transition(ctx, core.TransitionInput{
		BookingUID:     booking.UID,
		ToStatus:       "confirming",
		Source:         "subscribe-wiring-test",
		IdempotencyKey: postgrestest.UniqueKey("subscribe-wiring-transition"),
	})
	require.NoError(t, err)

	select {
	case evt := <-received:
		require.Equal(t, core.Status("confirming"), evt.ToStatus,
			"the handler must receive the transition event")
	case <-time.After(5 * time.Second):
		t.Fatal("no event reached the handler within 5s (100 poll cycles at the configured 50ms interval) -- " +
			"ledger.Service.Worker is no longer wiring the event poller, so Subscribe delivers nothing. " +
			"This is the exact regression the facade wiring exists to prevent.")
	}
}

type subscribeFixture struct{ currencyUID string }

// seedSubscribeFixture installs the minimum a booking needs: a currency, the
// accounting templates, and a lifecycle on the seeded label-only "deposit"
// classification (the preset bundles install templates, not lifecycles).
func seedSubscribeFixture(t *testing.T, ctx context.Context, svc *ledger.Service) subscribeFixture {
	t.Helper()

	cur, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{
		Code: "SWU", Name: "Subscribe Wiring Unit", Exponent: 2,
	})
	require.NoError(t, err)

	require.NoError(t, svc.InstallDefaultPresets(ctx))

	class, err := svc.Classifications().GetByCode(ctx, "deposit")
	require.NoError(t, err)
	require.NoError(t, svc.Classifications().SetLifecycleIfEmpty(ctx, class.UID, presets.DepositLifecycle))

	return subscribeFixture{currencyUID: cur.UID}
}
