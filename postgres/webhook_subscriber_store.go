package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/postgres/sqlcgen"
	"github.com/azex-ai/ledger/service/delivery"
)

var _ delivery.SubscriberLister = (*WebhookSubscriberStore)(nil)

// WebhookSubscriberStore lists active webhook subscribers for event delivery.
type WebhookSubscriberStore struct {
	q *sqlcgen.Queries
}

// NewWebhookSubscriberStore creates a new WebhookSubscriberStore.
func NewWebhookSubscriberStore(pool *pgxpool.Pool) *WebhookSubscriberStore {
	return &WebhookSubscriberStore{
		q: sqlcgen.New(pool),
	}
}

func (s *WebhookSubscriberStore) ListActiveSubscribers(ctx context.Context) ([]delivery.WebhookSubscriber, error) {
	rows, err := s.q.ListActiveWebhookSubscribers(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: list active webhook subscribers: %w", err)
	}

	subs := make([]delivery.WebhookSubscriber, len(rows))
	for i, row := range rows {
		subs[i] = delivery.WebhookSubscriber{
			ID:             row.ID,
			Name:           row.Name,
			URL:            row.Url,
			Secret:         row.Secret,
			FilterClass:    row.FilterClass,
			FilterToStatus: row.FilterToStatus,
			IsActive:       row.IsActive,
		}
	}
	return subs, nil
}

// RecordDeliveryStatus records the outcome of the most recent delivery
// attempt to a subscriber. statusCode is 0 when the request never received
// an HTTP response (e.g. connection refused, timeout). errMsg is empty on
// success.
func (s *WebhookSubscriberStore) RecordDeliveryStatus(ctx context.Context, subscriberID int64, statusCode int, errMsg string) error {
	if err := s.q.UpdateWebhookSubscriberDeliveryStatus(ctx, sqlcgen.UpdateWebhookSubscriberDeliveryStatusParams{
		ID:             subscriberID,
		LastStatusCode: int32(statusCode),
		LastError:      errMsg,
	}); err != nil {
		return fmt.Errorf("postgres: record webhook delivery status: %w", err)
	}
	return nil
}

// TryRecordNonce records an inbound webhook nonce (typically the request
// signature) and reports whether it was fresh. false = the nonce was already
// seen inside the retention window, i.e. the request is a replay and must be
// rejected. Expired nonces (older than 2x the signature timestamp window,
// which can never verify again) are pruned opportunistically on each call —
// this table is a replay cache, not ledger data.
func (s *WebhookSubscriberStore) TryRecordNonce(ctx context.Context, nonce string) (bool, error) {
	if err := s.q.DeleteExpiredWebhookNonces(ctx); err != nil {
		return false, fmt.Errorf("postgres: prune webhook nonces: %w", err)
	}
	rows, err := s.q.TryRecordWebhookNonce(ctx, nonce)
	if err != nil {
		return false, fmt.Errorf("postgres: record webhook nonce: %w", err)
	}
	return rows > 0, nil
}
