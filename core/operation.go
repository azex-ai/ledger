package core

import (
	"time"

	"github.com/shopspring/decimal"
)

// Operation is the unified record replacing Deposit/Withdrawal.
// Its lifecycle is governed by the classification's state machine.
type Operation struct {
	ID               int64              `json:"id"`
	ClassificationID int64              `json:"classification_id"`
	AccountHolder    int64              `json:"account_holder"`
	CurrencyID       int64              `json:"currency_id"`
	Amount           decimal.Decimal    `json:"amount"`
	SettledAmount    decimal.Decimal    `json:"settled_amount"`
	Status           Status             `json:"status"`
	ChannelName      string             `json:"channel_name"`
	ChannelRef       string             `json:"channel_ref"`
	ReservationID    int64              `json:"reservation_id"`
	JournalID        int64              `json:"journal_id"`
	IdempotencyKey   string             `json:"idempotency_key"`
	Metadata         map[string]any     `json:"metadata"`
	ExpiresAt        time.Time          `json:"expires_at"`
	CreatedAt        time.Time          `json:"created_at"`
	UpdatedAt        time.Time          `json:"updated_at"`
}

// CreateOperationInput is the input to create a new operation.
type CreateOperationInput struct {
	ClassificationCode string          `json:"classification_code"`
	AccountHolder      int64           `json:"account_holder"`
	CurrencyID         int64           `json:"currency_id"`
	Amount             decimal.Decimal `json:"amount"`
	IdempotencyKey     string          `json:"idempotency_key"`
	ChannelName        string          `json:"channel_name"`
	Metadata           map[string]any  `json:"metadata"`
	ExpiresAt          time.Time       `json:"expires_at"`
}

// TransitionInput is the input to advance an operation's state.
type TransitionInput struct {
	OperationID int64           `json:"operation_id"`
	ToStatus    Status          `json:"to_status"`
	ChannelRef  string          `json:"channel_ref"`
	Amount      decimal.Decimal `json:"amount"`
	Metadata    map[string]any  `json:"metadata"`
	ActorID     int64           `json:"actor_id"`
}

// OperationFilter is the filter for listing operations.
type OperationFilter struct {
	AccountHolder    int64  `json:"account_holder"`
	ClassificationID int64  `json:"classification_id"`
	Status           string `json:"status"`
	Cursor           int64  `json:"cursor"`
	Limit            int    `json:"limit"`
}
