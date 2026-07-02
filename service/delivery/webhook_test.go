package delivery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

type mockEventPoller struct {
	events      []core.Event
	delivered   []int64
	retried     []int64
	lastRetryAt time.Time
}

func (m *mockEventPoller) GetPendingEvents(_ context.Context, _ int) ([]core.Event, error) {
	return m.events, nil
}

func (m *mockEventPoller) MarkDelivered(_ context.Context, id int64) error {
	m.delivered = append(m.delivered, id)
	return nil
}

func (m *mockEventPoller) MarkRetry(_ context.Context, id int64, nextAttempt time.Time) error {
	m.retried = append(m.retried, id)
	m.lastRetryAt = nextAttempt
	return nil
}

func (m *mockEventPoller) MarkDead(_ context.Context, _ int64) error { return nil }

type recordedDeliveryStatus struct {
	subscriberID int64
	statusCode   int
	errMsg       string
}

type mockSubscriberLister struct {
	subs             []WebhookSubscriber
	recordedStatuses []recordedDeliveryStatus
}

func (m *mockSubscriberLister) ListActiveSubscribers(_ context.Context) ([]WebhookSubscriber, error) {
	return m.subs, nil
}

func (m *mockSubscriberLister) RecordDeliveryStatus(_ context.Context, subscriberID int64, statusCode int, errMsg string) error {
	m.recordedStatuses = append(m.recordedStatuses, recordedDeliveryStatus{subscriberID: subscriberID, statusCode: statusCode, errMsg: errMsg})
	return nil
}

// recordingMetrics captures delivery-metric calls for testing.
type recordingMetrics struct {
	core.Metrics
	delivered      int
	deliveryFailed int
	dead           int
}

func (m *recordingMetrics) EventDelivered()      { m.delivered++ }
func (m *recordingMetrics) EventDeliveryFailed() { m.deliveryFailed++ }
func (m *recordingMetrics) EventDead()           { m.dead++ }

func TestWebhookDeliverer_ProcessBatch_NilSubscriberLister(t *testing.T) {
	deliverer := NewWebhookDeliverer(&mockEventPoller{}, nil, core.NopLogger(), core.NopMetrics())

	_, err := deliverer.ProcessBatch(context.Background(), 10)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subscriber lister is nil")
}

func TestWebhookDeliverer_ProcessBatch_NoSubscribersMarksDelivered(t *testing.T) {
	poller := &mockEventPoller{
		events: []core.Event{{ID: 42, ClassificationCode: "deposit", ToStatus: "confirmed"}},
	}
	metrics := &recordingMetrics{}
	deliverer := NewWebhookDeliverer(poller, &mockSubscriberLister{}, core.NopLogger(), metrics)

	delivered, err := deliverer.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, delivered)
	assert.Equal(t, []int64{42}, poller.delivered)
	assert.Empty(t, poller.retried)
	assert.Equal(t, 1, metrics.delivered, "EventDelivered must be emitted")
}

func TestWebhookDeliverer_ProcessBatch_NilMetricsDefaultsToNop(t *testing.T) {
	poller := &mockEventPoller{
		events: []core.Event{{ID: 1, ClassificationCode: "deposit", ToStatus: "confirmed"}},
	}
	deliverer := NewWebhookDeliverer(poller, &mockSubscriberLister{}, core.NopLogger(), nil)

	assert.NotPanics(t, func() {
		_, err := deliverer.ProcessBatch(context.Background(), 10)
		require.NoError(t, err)
	})
}

func TestWebhookDeliverer_DeliverEvent_SubscriberFailureEmitsFailedMetric(t *testing.T) {
	poller := &mockEventPoller{
		events: []core.Event{{ID: 7, ClassificationCode: "deposit", ToStatus: "confirmed", Attempts: 0, MaxAttempts: 10}},
	}
	// A subscriber whose URL will fail to dial — sendHTTP returns an error.
	subs := &mockSubscriberLister{subs: []WebhookSubscriber{
		{ID: 1, Name: "unreachable", URL: "http://127.0.0.1:0/webhook", IsActive: true},
	}}
	metrics := &recordingMetrics{}
	deliverer := NewWebhookDeliverer(poller, subs, core.NopLogger(), metrics)

	_, err := deliverer.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, []int64{7}, poller.retried)
	assert.Equal(t, 1, metrics.deliveryFailed, "EventDeliveryFailed must be emitted on send failure")
	assert.Equal(t, 0, metrics.dead, "not yet at max attempts, so EventDead must not fire")
}

func TestWebhookDeliverer_DeliverEvent_LastAttemptEmitsDeadMetric(t *testing.T) {
	poller := &mockEventPoller{
		events: []core.Event{{ID: 8, ClassificationCode: "deposit", ToStatus: "confirmed", Attempts: 9, MaxAttempts: 10}},
	}
	subs := &mockSubscriberLister{subs: []WebhookSubscriber{
		{ID: 1, Name: "unreachable", URL: "http://127.0.0.1:0/webhook", IsActive: true},
	}}
	metrics := &recordingMetrics{}
	deliverer := NewWebhookDeliverer(poller, subs, core.NopLogger(), metrics)

	_, err := deliverer.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, metrics.deliveryFailed)
	assert.Equal(t, 1, metrics.dead, "attempts+1 >= max_attempts must emit EventDead")
}

func TestWebhookDeliverer_ProcessBatch_RecordsSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	poller := &mockEventPoller{
		events: []core.Event{{ID: 1, ClassificationCode: "deposit", ToStatus: "confirmed"}},
	}
	lister := &mockSubscriberLister{subs: []WebhookSubscriber{{ID: 7, Name: "sub", URL: srv.URL}}}
	deliverer := NewWebhookDeliverer(poller, lister, core.NopLogger(), core.NopMetrics())

	delivered, err := deliverer.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, delivered)
	require.Len(t, lister.recordedStatuses, 1)
	assert.Equal(t, int64(7), lister.recordedStatuses[0].subscriberID)
	assert.Equal(t, http.StatusOK, lister.recordedStatuses[0].statusCode)
	assert.Empty(t, lister.recordedStatuses[0].errMsg)
}

func TestWebhookDeliverer_ProcessBatch_RecordsFailureStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	poller := &mockEventPoller{
		events: []core.Event{{ID: 2, ClassificationCode: "deposit", ToStatus: "confirmed"}},
	}
	lister := &mockSubscriberLister{subs: []WebhookSubscriber{{ID: 9, Name: "sub", URL: srv.URL}}}
	deliverer := NewWebhookDeliverer(poller, lister, core.NopLogger(), core.NopMetrics())

	_, err := deliverer.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, lister.recordedStatuses, 1)
	assert.Equal(t, int64(9), lister.recordedStatuses[0].subscriberID)
	assert.Equal(t, http.StatusInternalServerError, lister.recordedStatuses[0].statusCode)
	assert.Contains(t, lister.recordedStatuses[0].errMsg, "http status 500")
	assert.Equal(t, []int64{2}, poller.retried)
}

func TestRetryDelay(t *testing.T) {
	tests := []struct {
		name     string
		attempts int32
		want     time.Duration
	}{
		{name: "first failure", attempts: 0, want: time.Minute},
		{name: "second failure", attempts: 1, want: 5 * time.Minute},
		{name: "third failure", attempts: 2, want: 30 * time.Minute},
		{name: "caps at max interval", attempts: 99, want: 24 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, retryDelay(tt.attempts))
		})
	}
}
