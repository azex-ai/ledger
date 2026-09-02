package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
	"github.com/azex-ai/ledger/service/delivery"
)

var _ delivery.SubscriberLister = (*WebhookSubscriberStore)(nil)

// WebhookSubscriberStore lists active webhook subscribers for event delivery.
type WebhookSubscriberStore struct {
	q *sqlcgen.Queries

	// logger is nil until SetLogger is called; nil falls back to
	// slog.Default(), matching EventStore's convention. pruneWarnOnce keeps a
	// permanently-degraded prune from writing a line per inbound webhook.
	logger        core.Logger
	pruneWarnOnce sync.Once
}

// NewWebhookSubscriberStore creates a new WebhookSubscriberStore.
func NewWebhookSubscriberStore(pool *pgxpool.Pool) *WebhookSubscriberStore {
	return &WebhookSubscriberStore{
		q: sqlcgen.New(pool),
	}
}

// SetLogger routes this store's own diagnostic warnings (currently just the
// nonce-prune degradation below) to the consumer's logger instead of
// slog.Default(). Returns the receiver so it can be chained onto the
// constructor.
func (s *WebhookSubscriberStore) SetLogger(logger core.Logger) *WebhookSubscriberStore {
	s.logger = logger
	return s
}

func (s *WebhookSubscriberStore) warn(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(msg, args...)
		return
	}
	slog.Warn(msg, args...)
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
//
// A prune refused for lack of privilege does not fail the call. Bounding a
// cache and rejecting a replay are different jobs, and only the second one is
// this request's. Before migration 002 granted ledger_app the DELETE, they
// were fused: the prune ran first and its error was returned, so every inbound
// webhook failed on a permission error in exactly the deployments that connect
// as ledger_app — which is the entire point of the role separation. Tolerating
// that one error keeps a database without the grant serving webhooks, at the
// cost of a cache that stops shrinking.
//
// Only insufficient_privilege is tolerated. A prune that fails for any other
// reason — deadlock, disk, a corrupted index — still fails the call, because
// those are not conditions this code knows how to survive.
func (s *WebhookSubscriberStore) TryRecordNonce(ctx context.Context, nonce string) (bool, error) {
	if err := s.q.DeleteExpiredWebhookNonces(ctx); err != nil {
		if !isInsufficientPrivilege(err) {
			return false, fmt.Errorf("postgres: prune webhook nonces: %w", err)
		}
		// Tolerated, but not silent. The doc comment above has always been
		// honest about the cost of this branch; what it had no way to say was
		// that the cost is paid invisibly -- a deployment missing migration
		// 002's grant behaves, in every observable way, exactly like one that
		// has it, right up until the replay cache fills the disk
		// (working-agreements §3: a degraded mode has to leave a trace).
		//
		// sync.Once because the condition is a missing GRANT: it is either
		// always true or never true, so a line per inbound webhook would be
		// noise rather than signal.
		s.pruneWarnOnce.Do(func() {
			s.warn("postgres: webhook nonce prune refused for lack of privilege; the inbound replay cache will grow without bound. "+
				"Apply migration 002 (GRANT DELETE ON webhook_nonces TO ledger_app) or grant the serving role DELETE on that table",
				"error", err)
		})
	}
	rows, err := s.q.TryRecordWebhookNonce(ctx, nonce)
	if err != nil {
		return false, fmt.Errorf("postgres: record webhook nonce: %w", err)
	}
	return rows > 0, nil
}

// isInsufficientPrivilege reports whether err is Postgres's SQLSTATE 42501,
// the code it raises uniformly for a refused privilege regardless of which
// privilege or object was involved.
func isInsufficientPrivilege(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}
