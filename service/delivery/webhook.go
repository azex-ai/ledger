package delivery

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/azex-ai/ledger/core"
)

// WebhookSubscriber represents a registered webhook endpoint.
type WebhookSubscriber struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	URL            string `json:"url"`
	Secret         string `json:"secret"`
	FilterClass    string `json:"filter_class"`
	FilterToStatus string `json:"filter_to_status"`
	IsActive       bool   `json:"is_active"`
}

// EventPoller reads pending events from the store.
type EventPoller interface {
	GetPendingEvents(ctx context.Context, limit int) ([]core.Event, error)
	MarkDelivered(ctx context.Context, id int64) error
	MarkRetry(ctx context.Context, id int64, nextAttempt time.Time) error
	MarkDead(ctx context.Context, id int64) error
}

// SubscriberLister loads active webhook subscribers.
type SubscriberLister interface {
	ListActiveSubscribers(ctx context.Context) ([]WebhookSubscriber, error)
}

// WebhookDeliverer delivers events to webhook subscribers via HTTP POST.
type WebhookDeliverer struct {
	poller      EventPoller
	subscribers SubscriberLister
	client      *http.Client
	logger      core.Logger
}

// NewWebhookDeliverer creates a new WebhookDeliverer.
func NewWebhookDeliverer(poller EventPoller, subscribers SubscriberLister, logger core.Logger) *WebhookDeliverer {
	return &WebhookDeliverer{
		poller:      poller,
		subscribers: subscribers,
		client:      &http.Client{Timeout: 30 * time.Second},
		logger:      logger,
	}
}

// retryIntervals defines exponential backoff: 1m, 5m, 30m, 2h, 24h.
var retryIntervals = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	24 * time.Hour,
}

func retryDelay(attempts int32) time.Duration {
	if attempts <= 0 {
		return retryIntervals[0]
	}
	idx := int(attempts)
	if idx >= len(retryIntervals) {
		idx = len(retryIntervals) - 1
	}
	return retryIntervals[idx]
}

// ProcessBatch polls pending events and delivers them to subscribers.
// Returns the number of events successfully delivered.
func (d *WebhookDeliverer) ProcessBatch(ctx context.Context, batchSize int) (int, error) {
	if d.poller == nil {
		return 0, fmt.Errorf("delivery: webhook: event poller is nil")
	}
	if d.subscribers == nil {
		return 0, fmt.Errorf("delivery: webhook: subscriber lister is nil")
	}

	events, err := d.poller.GetPendingEvents(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("delivery: webhook: poll: %w", err)
	}
	if len(events) == 0 {
		return 0, nil
	}

	subs, err := d.subscribers.ListActiveSubscribers(ctx)
	if err != nil {
		return 0, fmt.Errorf("delivery: webhook: list subscribers: %w", err)
	}
	if len(subs) == 0 {
		// No subscribers — mark all as delivered (nobody to notify).
		for _, evt := range events {
			if err := d.poller.MarkDelivered(ctx, evt.ID); err != nil {
				d.logger.Error("delivery: webhook: mark delivered (no subscribers)", "event_id", evt.ID, "error", err)
			}
		}
		return len(events), nil
	}

	delivered := 0
	for _, evt := range events {
		if err := d.deliverEvent(ctx, evt, subs); err != nil {
			d.logger.Error("delivery: webhook: deliver event", "event_id", evt.ID, "error", err)
		} else {
			delivered++
		}
	}
	return delivered, nil
}

func (d *WebhookDeliverer) deliverEvent(ctx context.Context, evt core.Event, subs []WebhookSubscriber) error {
	matched := d.matchSubscribers(evt, subs)
	if len(matched) == 0 {
		return d.poller.MarkDelivered(ctx, evt.ID)
	}

	allOK := true
	for _, sub := range matched {
		if err := d.sendHTTP(ctx, evt, sub); err != nil {
			d.logger.Warn("delivery: webhook: send failed",
				"subscriber", sub.Name,
				"url", sub.URL,
				"error", err,
			)
			allOK = false
		}
	}

	if allOK {
		return d.poller.MarkDelivered(ctx, evt.ID)
	}

	// At least one subscriber failed — schedule retry with exponential backoff.
	// The store increments attempts and transitions the event to dead when max_attempts is exceeded.
	return d.poller.MarkRetry(ctx, evt.ID, time.Now().Add(retryDelay(evt.Attempts)))
}

func (d *WebhookDeliverer) matchSubscribers(evt core.Event, subs []WebhookSubscriber) []WebhookSubscriber {
	var matched []WebhookSubscriber
	for _, sub := range subs {
		if sub.FilterClass != "" && sub.FilterClass != evt.ClassificationCode {
			continue
		}
		if sub.FilterToStatus != "" && sub.FilterToStatus != string(evt.ToStatus) {
			continue
		}
		matched = append(matched, sub)
	}
	return matched
}

func (d *WebhookDeliverer) sendHTTP(ctx context.Context, evt core.Event, sub WebhookSubscriber) error {
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("delivery: webhook: marshal: %w", err)
	}

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.URL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("delivery: webhook: create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ledger-Event-ID", strconv.FormatInt(evt.ID, 10))
	req.Header.Set("X-Ledger-Timestamp", timestamp)

	if sub.Secret != "" {
		sig := computeSignature(payload, timestamp, sub.Secret)
		req.Header.Set("X-Ledger-Signature", fmt.Sprintf("t=%s,v1=%s", timestamp, sig))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("delivery: webhook: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("delivery: webhook: http status %d", resp.StatusCode)
}

func computeSignature(payload []byte, timestamp, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
