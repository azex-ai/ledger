package postgres_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service/delivery"
)

// seedPendingEvent creates a classification + booking + a "pending" ->
// "confirmed" transition, which produces a real event row. events.booking_id
// carries a FK to bookings (migration 018), so a bare INSERT INTO events
// isn't a legal fixture here — the event must originate from an actual
// booking transition. The resulting event's delivery_status defaults to
// 'pending' with next_attempt_at = now(), so it's immediately claimable by
// GetPendingEvents.
func seedPendingEvent(t *testing.T, pool *pgxpool.Pool, classCode string) {
	t.Helper()
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	bookingStore := postgres.NewBookingStore(pool)

	lifecycle := &core.Lifecycle{
		Initial:  "pending",
		Terminal: []core.Status{"confirmed"},
		Transitions: map[core.Status][]core.Status{
			"pending": {"confirmed"},
		},
	}
	cls, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code:       classCode,
		Name:       classCode,
		NormalSide: core.NormalSideCredit,
		Lifecycle:  lifecycle,
	})
	require.NoError(t, err)

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")

	booking, err := bookingStore.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: cls.Code,
		AccountHolder:      42,
		CurrencyID:         curID,
		Amount:             decimal.NewFromInt(100),
		IdempotencyKey:     postgrestest.UniqueKey("webhook-e2e-" + classCode),
		ChannelName:        "test",
		ExpiresAt:          time.Now().Add(time.Hour),
	})
	require.NoError(t, err)

	_, err = bookingStore.Transition(ctx, core.TransitionInput{
		BookingID: booking.ID,
		ToStatus:  "confirmed",
	})
	require.NoError(t, err)
}

func TestWebhookSubscriberStore_RecordDeliveryStatus(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	store := postgres.NewWebhookSubscriberStore(pool)

	var id int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO webhook_subscribers (name, url) VALUES ($1, $2) RETURNING id`,
		"sub-record-status", "http://example.invalid/hook",
	).Scan(&id))

	// New row starts at the No-NULL defaults.
	var statusCode int
	var lastError string
	var lastAttempt time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_status_code, last_error, last_attempt_at FROM webhook_subscribers WHERE id = $1`, id,
	).Scan(&statusCode, &lastError, &lastAttempt))
	assert.Equal(t, 0, statusCode)
	assert.Empty(t, lastError)
	assert.True(t, lastAttempt.Before(time.Now().Add(-24*time.Hour)), "default last_attempt_at must be the epoch sentinel")

	// Success: status code recorded, error cleared.
	require.NoError(t, store.RecordDeliveryStatus(ctx, id, 200, ""))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_status_code, last_error, last_attempt_at FROM webhook_subscribers WHERE id = $1`, id,
	).Scan(&statusCode, &lastError, &lastAttempt))
	assert.Equal(t, 200, statusCode)
	assert.Empty(t, lastError)
	assert.WithinDuration(t, time.Now(), lastAttempt, 10*time.Second)

	// Failure: status code + error text recorded (0 means no HTTP response).
	require.NoError(t, store.RecordDeliveryStatus(ctx, id, 0, "connection refused"))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_status_code, last_error, last_attempt_at FROM webhook_subscribers WHERE id = $1`, id,
	).Scan(&statusCode, &lastError, &lastAttempt))
	assert.Equal(t, 0, statusCode)
	assert.Equal(t, "connection refused", lastError)
}

// TestWebhookDeliverer_RecordsDeliveryStatus_EndToEnd wires the real
// WebhookSubscriberStore + EventStore + WebhookDeliverer against Postgres to
// confirm a live delivery attempt (success or failure) is reflected back on
// the subscriber row — this is the actual behavior operators rely on in
// docs/RUNBOOK.md §5.
func TestWebhookDeliverer_RecordsDeliveryStatus_EndToEnd(t *testing.T) {
	// Each subtest gets its own database — a shared one would leave the prior
	// subtest's subscriber row (and its now-closed httptest server) active,
	// so the second subtest's ProcessBatch would also try (and fail) to
	// deliver to it.
	t.Run("success", func(t *testing.T) {
		pool := postgrestest.SetupDB(t)
		ctx := context.Background()
		subscriberStore := postgres.NewWebhookSubscriberStore(pool)
		eventStore := postgres.NewEventStore(pool)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		var subID int64
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO webhook_subscribers (name, url) VALUES ($1, $2) RETURNING id`,
			"sub-success", srv.URL,
		).Scan(&subID))

		seedPendingEvent(t, pool, "webhook_e2e_success")

		deliverer := delivery.NewWebhookDeliverer(eventStore, subscriberStore, core.NopLogger())
		delivered, err := deliverer.ProcessBatch(ctx, 10)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, delivered, 1)

		var statusCode int
		var lastError string
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT last_status_code, last_error FROM webhook_subscribers WHERE id = $1`, subID,
		).Scan(&statusCode, &lastError))
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Empty(t, lastError)
	})

	t.Run("failure", func(t *testing.T) {
		pool := postgrestest.SetupDB(t)
		ctx := context.Background()
		subscriberStore := postgres.NewWebhookSubscriberStore(pool)
		eventStore := postgres.NewEventStore(pool)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		var subID int64
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO webhook_subscribers (name, url) VALUES ($1, $2) RETURNING id`,
			"sub-failure", srv.URL,
		).Scan(&subID))

		seedPendingEvent(t, pool, "webhook_e2e_failure")

		deliverer := delivery.NewWebhookDeliverer(eventStore, subscriberStore, core.NopLogger())
		_, err := deliverer.ProcessBatch(ctx, 10)
		require.NoError(t, err)

		var statusCode int
		var lastError string
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT last_status_code, last_error FROM webhook_subscribers WHERE id = $1`, subID,
		).Scan(&statusCode, &lastError))
		assert.Equal(t, http.StatusInternalServerError, statusCode)
		assert.Contains(t, lastError, "http status 500")
	})
}
