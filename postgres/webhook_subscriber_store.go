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
