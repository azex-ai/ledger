package delivery

import (
	"context"
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

type mockSubscriberLister struct {
	subs []WebhookSubscriber
}

func (m *mockSubscriberLister) ListActiveSubscribers(_ context.Context) ([]WebhookSubscriber, error) {
	return m.subs, nil
}

func TestWebhookDeliverer_ProcessBatch_NilSubscriberLister(t *testing.T) {
	deliverer := NewWebhookDeliverer(&mockEventPoller{}, nil, core.NopLogger())

	_, err := deliverer.ProcessBatch(context.Background(), 10)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subscriber lister is nil")
}

func TestWebhookDeliverer_ProcessBatch_NoSubscribersMarksDelivered(t *testing.T) {
	poller := &mockEventPoller{
		events: []core.Event{{ID: 42, ClassificationCode: "deposit", ToStatus: "confirmed"}},
	}
	deliverer := NewWebhookDeliverer(poller, &mockSubscriberLister{}, core.NopLogger())

	delivered, err := deliverer.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, delivered)
	assert.Equal(t, []int64{42}, poller.delivered)
	assert.Empty(t, poller.retried)
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
