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

// PendingEvent pairs an event with the internal storage id the delivery
// bookkeeping (MarkDelivered/MarkRetry/MarkDead) operates on. The internal id
// never reaches a payload or header — consumers see only Event.UID.
type PendingEvent struct {
	InternalID int64
	core.Event
}

// EventPoller reads pending events from the store.
type EventPoller interface {
	GetPendingEvents(ctx context.Context, limit int) ([]PendingEvent, error)
	MarkDelivered(ctx context.Context, id int64) error
	MarkRetry(ctx context.Context, id int64, nextAttempt time.Time) error
	MarkDead(ctx context.Context, id int64) error
}

// SubscriberLister loads active webhook subscribers and records the outcome
// of delivery attempts against them.
type SubscriberLister interface {
	ListActiveSubscribers(ctx context.Context) ([]WebhookSubscriber, error)
	// RecordDeliveryStatus persists the result of the most recent delivery
	// attempt to subscriberID. statusCode is 0 when no HTTP response was
	// received (e.g. connection refused, timeout). errMsg is empty on success.
	RecordDeliveryStatus(ctx context.Context, subscriberID int64, statusCode int, errMsg string) error
}

// maxRecordedDeliveryErrorLen bounds how much of a delivery error string is
// persisted per subscriber, so a verbose upstream error can't bloat the row.
const maxRecordedDeliveryErrorLen = 500

// WebhookDeliverer delivers events to webhook subscribers via HTTP POST.
type WebhookDeliverer struct {
	poller      EventPoller
	subscribers SubscriberLister
	client      *http.Client
	logger      core.Logger
	metrics     core.Metrics
}

// NewWebhookDeliverer creates a new WebhookDeliverer.
func NewWebhookDeliverer(poller EventPoller, subscribers SubscriberLister, logger core.Logger, metrics core.Metrics) *WebhookDeliverer {
	if metrics == nil {
		metrics = core.NopMetrics()
	}
	return &WebhookDeliverer{
		poller:      poller,
		subscribers: subscribers,
		client:      &http.Client{Timeout: 30 * time.Second},
		logger:      logger,
		metrics:     metrics,
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
			if err := d.poller.MarkDelivered(ctx, evt.InternalID); err != nil {
				d.logger.Error("delivery: webhook: mark delivered (no subscribers)", "event_id", evt.InternalID, "error", err)
			} else {
				d.metrics.EventDelivered()
			}
		}
		return len(events), nil
	}

	delivered := 0
	for _, evt := range events {
		if err := d.deliverEvent(ctx, evt, subs); err != nil {
			d.logger.Error("delivery: webhook: deliver event", "event_id", evt.InternalID, "error", err)
		} else {
			delivered++
		}
	}
	return delivered, nil
}

func (d *WebhookDeliverer) deliverEvent(ctx context.Context, evt PendingEvent, subs []WebhookSubscriber) error {
	matched := d.matchSubscribers(evt, subs)
	if len(matched) == 0 {
		err := d.poller.MarkDelivered(ctx, evt.InternalID)
		if err == nil {
			d.metrics.EventDelivered()
		}
		return err
	}

	allOK := true
	for _, sub := range matched {
		statusCode, err := d.sendHTTP(ctx, evt, sub)
		errMsg := ""
		if err != nil {
			d.logger.Warn("delivery: webhook: send failed",
				"subscriber", sub.Name,
				"url", sub.URL,
				"error", err,
			)
			errMsg = truncateError(err.Error(), maxRecordedDeliveryErrorLen)
			allOK = false
		}
		if recErr := d.subscribers.RecordDeliveryStatus(ctx, sub.ID, statusCode, errMsg); recErr != nil {
			d.logger.Error("delivery: webhook: record delivery status",
				"subscriber", sub.Name,
				"error", recErr,
			)
		}
	}

	if allOK {
		err := d.poller.MarkDelivered(ctx, evt.InternalID)
		if err == nil {
			d.metrics.EventDelivered()
		}
		return err
	}

	// At least one subscriber failed — schedule retry with exponential backoff.
	// The store increments attempts and transitions the event to dead when max_attempts is exceeded;
	// mirror that same threshold here so EventDead reflects the DB's own decision.
	d.metrics.EventDeliveryFailed()
	if evt.MaxAttempts > 0 && evt.Attempts+1 >= evt.MaxAttempts {
		d.metrics.EventDead()
	}
	return d.poller.MarkRetry(ctx, evt.InternalID, time.Now().Add(retryDelay(evt.Attempts)))
}

func (d *WebhookDeliverer) matchSubscribers(evt PendingEvent, subs []WebhookSubscriber) []WebhookSubscriber {
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

// sendHTTP delivers evt to sub and returns the HTTP status code received
// (0 if none, e.g. a connection error) alongside any error.
func (d *WebhookDeliverer) sendHTTP(ctx context.Context, evt PendingEvent, sub WebhookSubscriber) (int, error) {
	payload, err := json.Marshal(evt.Event)
	if err != nil {
		return 0, fmt.Errorf("delivery: webhook: marshal: %w", err)
	}

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.URL, bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("delivery: webhook: create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ledger-Event-UID", evt.UID)
	req.Header.Set("X-Ledger-Timestamp", timestamp)

	if sub.Secret != "" {
		sig := computeSignature(payload, timestamp, sub.Secret)
		req.Header.Set("X-Ledger-Signature", fmt.Sprintf("t=%s,v1=%s", timestamp, sig))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("delivery: webhook: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("delivery: webhook: http status %d", resp.StatusCode)
}

func computeSignature(payload []byte, timestamp, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// truncateError bounds an error string to at most max bytes so a verbose
// upstream error can't bloat the recorded delivery status.
func truncateError(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
