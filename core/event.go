package core

import (
	"time"

	"github.com/shopspring/decimal"
)

// Event is an atomic record of a state transition on a booking.
// Events are the cause of journal postings.
type Event struct {
	UID                string          `json:"uid"`
	ClassificationCode string          `json:"classification_code"`
	BookingUID         string          `json:"booking_uid"`
	AccountHolder      int64           `json:"account_holder"`
	CurrencyUID        string          `json:"currency_uid"`
	FromStatus         Status          `json:"from_status"`
	ToStatus           Status          `json:"to_status"`
	Amount             decimal.Decimal `json:"amount"`
	SettledAmount      decimal.Decimal `json:"settled_amount"`
	// JournalUID is empty when the event has not (yet) caused a journal
	// posting. Sentinel 0 cannot be used because Postgres enforces an FK on this
	// column to journals(id).
	JournalUID string            `json:"journal_uid,omitempty"`
	Metadata   map[string]string `json:"metadata"`
	OccurredAt time.Time         `json:"occurred_at"`
	// ActorID is the user or system actor that triggered this transition.
	// 0 means unknown / system-initiated.
	ActorID int64 `json:"actor_id"`
	// Source identifies the calling service or scope (e.g. "api", "worker", "webhook").
	// Empty string means unset.
	Source        string    `json:"source"`
	Attempts      int32     `json:"-"`
	MaxAttempts   int32     `json:"-"`
	NextAttemptAt time.Time `json:"-"`
}

// EventFilter is the filter for listing events.
type EventFilter struct {
	ClassificationCode string `json:"classification_code"`
	BookingUID         string `json:"booking_uid"`
	ToStatus           string `json:"to_status"`
	// Cursor is the opaque keyset cursor from the previous page ("" = start).
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}
