package core

import (
	"time"

	"github.com/shopspring/decimal"
)

// Event is an atomic record of a state transition on an operation.
// Events are the cause of journal postings.
type Event struct {
	ID                 int64           `json:"id"`
	ClassificationCode string          `json:"classification_code"`
	OperationID        int64           `json:"operation_id"`
	AccountHolder      int64           `json:"account_holder"`
	CurrencyID         int64           `json:"currency_id"`
	FromStatus         Status          `json:"from_status"`
	ToStatus           Status          `json:"to_status"`
	Amount             decimal.Decimal `json:"amount"`
	SettledAmount      decimal.Decimal `json:"settled_amount"`
	JournalID          int64           `json:"journal_id"`
	Metadata           map[string]any  `json:"metadata"`
	OccurredAt         time.Time       `json:"occurred_at"`
}

// EventFilter is the filter for listing events.
type EventFilter struct {
	ClassificationCode string `json:"classification_code"`
	OperationID        int64  `json:"operation_id"`
	ToStatus           string `json:"to_status"`
	Cursor             int64  `json:"cursor"`
	Limit              int    `json:"limit"`
}
