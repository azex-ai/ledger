package core

import (
	"time"

	"github.com/shopspring/decimal"
)

// Event is an atomic record of a state transition on a booking.
// Events are the cause of journal postings.
type Event struct {
	ID                 int64           `json:"id"`
	ClassificationCode string          `json:"classification_code"`
	BookingID          int64           `json:"booking_id"`
	AccountHolder      int64           `json:"account_holder"`
	CurrencyID         int64           `json:"currency_id"`
	FromStatus         Status          `json:"from_status"`
	ToStatus           Status          `json:"to_status"`
	Amount             decimal.Decimal `json:"amount"`
	SettledAmount      decimal.Decimal `json:"settled_amount"`
	// JournalID is nullable: NULL means the event has not (yet) caused a journal
	// posting. Sentinel 0 cannot be used because Postgres enforces an FK on this
	// column to journals(id).
	JournalID          *int64          `json:"journal_id,omitempty"`
	Metadata           map[string]any  `json:"metadata"`
	OccurredAt         time.Time       `json:"occurred_at"`
	// ActorID is the user or system actor that triggered this transition.
	// 0 means unknown / system-initiated.
	ActorID            int64           `json:"actor_id"`
	// Source identifies the calling service or scope (e.g. "api", "worker", "webhook").
	// Empty string means unset.
	Source             string          `json:"source"`
	Attempts           int32           `json:"-"`
	MaxAttempts        int32           `json:"-"`
	NextAttemptAt      time.Time       `json:"-"`
}

// EventFilter is the filter for listing events.
type EventFilter struct {
	ClassificationCode string `json:"classification_code"`
	BookingID          int64  `json:"booking_id"`
	ToStatus           string `json:"to_status"`
	Cursor             int64  `json:"cursor"`
	Limit              int    `json:"limit"`
}
