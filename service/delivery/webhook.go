package delivery

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/wirejson"
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
//
// ClaimToken is the lease timestamp GetPendingEvents stamped on the row
// (next_attempt_at). Every Mark* call passes it back; the store only applies
// the outcome if the lease is still current, so a worker whose callback
// outlived its lease can never overwrite the re-claiming worker's result.
type PendingEvent struct {
	InternalID int64
	ClaimToken time.Time
	core.Event
}

// EventPoller reads pending events from the store.
type EventPoller interface {
	GetPendingEvents(ctx context.Context, limit int) ([]PendingEvent, error)
	// MarkDelivered/MarkRetry/MarkDead are claim-token scoped: they no-op
	// (without error) when claimToken no longer matches the row's lease.
	MarkDelivered(ctx context.Context, id int64, claimToken time.Time) error
	MarkRetry(ctx context.Context, id int64, claimToken time.Time, nextAttempt time.Time) error
	MarkDead(ctx context.Context, id int64, claimToken time.Time) error
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

	// signer, when set, replaces webhook_subscribers.secret as the source of
	// the outbound HMAC. See SetSigner.
	signer          core.WebhookSigner
	dbSecretWarnOne sync.Once
}

// SetSigner installs a core.WebhookSigner, moving the outbound HMAC key out of
// the database.
//
// Why this exists: webhook_subscribers.secret is the last key this schema
// stores, and ledger_app can read it. Migration 007 revoked exactly that
// column from ledger_ro and wrote down the reason -- reading it "hands a
// read-only credential the ability to forge signed event deliveries to any
// subscriber" -- and the same sentence is true of the credential the threat
// model assumes is leaked. So a leaked application DB credential does not just
// expose this ledger: it lets the holder send any downstream subscriber an
// HMAC-valid "deposit confirmed, amount X".
//
// Fully closing that means revoking the column, which is a migration for a
// deployment that has moved its keys, not a change this library can make on
// its behalf (deployment.md: expand, migrate, contract). This is the expand
// step -- both paths work, the port wins where present -- and the fallback
// warns once, so a deployment still on the column knows it is on it rather
// than finding out from a threat model.
func (d *WebhookDeliverer) SetSigner(signer core.WebhookSigner) *WebhookDeliverer {
	d.signer = signer
	return d
}

// NewWebhookDeliverer creates a new WebhookDeliverer.
func NewWebhookDeliverer(poller EventPoller, subscribers SubscriberLister, logger core.Logger, metrics core.Metrics) *WebhookDeliverer {
	if metrics == nil {
		metrics = core.NopMetrics()
	}
	return &WebhookDeliverer{
		poller:      poller,
		subscribers: subscribers,
		client: &http.Client{
			Timeout: 30 * time.Second,
			// Do not follow redirects: subscriber URLs come from the database
			// (writable by ledger_app under the threat model), and following a
			// 302 would re-send the signed X-Ledger-Signature + event payload
			// to the redirect target — a blind SSRF that can reach internal
			// addresses (e.g. cloud metadata). Treat the 3xx as the response.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger:  logger,
		metrics: metrics,
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
		// No active subscribers. Do NOT mark the batch delivered: "nobody was
		// listening" must stay distinguishable from "everyone was notified".
		// Marking here silently laundered every event that fired while the
		// subscriber table was empty (fresh environment, operator toggled
		// is_active off, migration window) into delivered — indistinguishable
		// from success, never retried once a subscriber appeared. Leaving the
		// claim untouched lets the lease expire and the batch re-poll.
		// (matchSubscribers returning empty for an event is different: a
		// subscriber existed and its filter chose not to receive it — that IS
		// a completed delivery decision, and deliverEvent marks it.)
		d.logger.Warn("delivery: webhook: no active subscribers; leaving batch pending for redelivery", "batch", len(events))
		return 0, nil
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
		// Detached ctx: "a subscriber existed and its filter chose not to
		// receive this event" is a completed delivery decision, so the
		// outcome must be recorded even if the parent was cancelled between
		// the poll and here. See cleanupContext.
		markCtx, cancel := cleanupContext(ctx)
		err := d.poller.MarkDelivered(markCtx, evt.InternalID, evt.ClaimToken)
		cancel()
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
		// Detached ctx: the POST above already left the process, so the
		// subscriber's health record must survive a shutdown that landed
		// while it was in flight. Losing it makes a failing endpoint look
		// healthy. See cleanupContext.
		recordCtx, cancel := cleanupContext(ctx)
		recErr := d.subscribers.RecordDeliveryStatus(recordCtx, sub.ID, statusCode, errMsg)
		cancel()
		if recErr != nil {
			d.logger.Error("delivery: webhook: record delivery status",
				"subscriber", sub.Name,
				"error", recErr,
			)
		}
	}

	if allOK {
		// Detached ctx: every matched subscriber has already been POSTed to.
		// Dropping this mark redelivers events that all landed.
		markCtx, cancel := cleanupContext(ctx)
		err := d.poller.MarkDelivered(markCtx, evt.InternalID, evt.ClaimToken)
		cancel()
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
	// Detached ctx for the same reason: the attempt happened, so the attempt
	// COUNT must advance. Losing it means the backoff never progresses and a
	// permanently broken subscriber is retried at the lease interval forever
	// instead of reaching max_attempts.
	markCtx, cancel := cleanupContext(ctx)
	defer cancel()
	return d.poller.MarkRetry(markCtx, evt.InternalID, evt.ClaimToken, time.Now().Add(retryDelay(evt.Attempts)))
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
	// wirejson, not encoding/json (H-M4): the outbound payload must obey the
	// same wire rules as an HTTP response -- above all RFC3339 UTC on every
	// _at field. With encoding/json a TZ=Asia/Singapore deployment sent
	// `occurred_at: ...+08:00` to subscribers while serving `...Z` over HTTP
	// for the very same event, because pgx decodes timestamptz into
	// time.Local and the stdlib marshaller preserves that offset.
	payload, err := wirejson.Marshal(evt.Event)
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

	sig, err := d.sign(ctx, sub, payload, timestamp)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Ledger-Signature", fmt.Sprintf("t=%s,v1=%s", timestamp, sig))

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

// sign produces the hex-encoded HMAC for one delivery, preferring the injected
// core.WebhookSigner over the subscriber's stored secret.
//
// Either way this fails closed rather than delivering unsigned. A subscriber
// the ledger cannot sign for is a subscriber that cannot verify authenticity,
// and a receiver that only checks "is a signature present" would accept forged
// events -- so the delivery becomes an error (retried, visible in last_error)
// instead of an unsigned POST.
func (d *WebhookDeliverer) sign(ctx context.Context, sub WebhookSubscriber, payload []byte, timestamp string) (string, error) {
	signingInput := make([]byte, 0, len(timestamp)+1+len(payload))
	signingInput = append(signingInput, timestamp...)
	signingInput = append(signingInput, '.')
	signingInput = append(signingInput, payload...)

	if d.signer != nil {
		mac, err := d.signer.Sign(ctx, sub.Name, signingInput)
		if err != nil {
			return "", fmt.Errorf("delivery: webhook: sign for subscriber %q: %w", sub.Name, err)
		}
		if len(mac) == 0 {
			return "", fmt.Errorf("delivery: webhook: signer returned an empty MAC for subscriber %q; refusing to deliver unsigned: %w", sub.Name, core.ErrInvalidInput)
		}
		return hex.EncodeToString(mac), nil
	}

	// No signer: the pre-port path, reading the key out of the database.
	// Warned once, because a deployment on this path is on it for every
	// delivery and the fact worth surfacing is the configuration, not the
	// request.
	d.dbSecretWarnOne.Do(func() {
		d.logger.Warn("delivery: webhook: signing outbound events with webhook_subscribers.secret; " +
			"ledger_app can read that column, so a leaked database credential can forge signed deliveries to every subscriber. " +
			"Install a core.WebhookSigner (WebhookDeliverer.SetSigner) to keep the key out of the database")
	})

	if sub.Secret == "" {
		return "", fmt.Errorf("delivery: webhook: subscriber %d has no signing secret; refusing to deliver unsigned: %w", sub.ID, core.ErrInvalidInput)
	}
	return computeSignature(payload, timestamp, sub.Secret), nil
}

func computeSignature(payload []byte, timestamp, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// truncateError bounds an error string to at most max bytes so a verbose
// upstream error can't bloat the recorded delivery status. The cut backs up
// to a rune boundary: slicing mid-rune would persist invalid UTF-8, which
// Postgres text columns reject — turning a long error message into a failed
// status write.
func truncateError(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
