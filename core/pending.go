package core

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// PendingBalanceWriter implements the two-phase pending-balance pattern:
//
//	AddPending     → DR suspense (system)   CR pending (user)
//	ConfirmPending → DR pending (user)  + DR main_wallet (user)
//	                 CR suspense (system) + CR custodial (system)
//	CancelPending  → DR pending (user)      CR suspense (system)   (compensating)
//
// This interface is intentionally decoupled from Booker — callers without
// bookings (e.g. raw on-chain deposit pipelines) may use it directly.
type PendingBalanceWriter interface {
	// AddPending moves funds from the suspense (system) classification into the
	// pending (user) classification, signalling that a deposit is in-flight.
	AddPending(ctx context.Context, in AddPendingInput) (*Journal, error)

	// ConfirmPending settles a pending deposit: clears the pending classification
	// and credits the user's main wallet.  Must be called with the same
	// (AccountHolder, CurrencyID) that was used in the matching AddPending call.
	ConfirmPending(ctx context.Context, in ConfirmPendingInput) (*Journal, error)

	// CancelPending reverses a pending deposit by posting a compensating journal
	// (DR pending, CR suspense).  The original AddPending journal is never mutated.
	// Returns ErrInsufficientBalance if the pending classification balance is zero.
	CancelPending(ctx context.Context, in CancelPendingInput) (*Journal, error)
}

// PendingTimeoutSweeper expires pending deposits that have been sitting in the
// pending classification for longer than the supplied threshold.
//
// Implementations should process a bounded batch per call (e.g. 1 000 rows)
// and be safe to call concurrently — each cancellation uses its own idempotency
// key derived from the originating journal ID.
type PendingTimeoutSweeper interface {
	ExpirePendingOlderThan(ctx context.Context, threshold time.Duration) (int, error)
}

// AddPendingInput is the input to AddPending.
type AddPendingInput struct {
	// AccountHolder is the positive user ID.  The system counterpart (-AccountHolder)
	// is derived automatically.
	AccountHolder  int64           `json:"account_holder"`
	CurrencyID     int64           `json:"currency_id"`
	Amount         decimal.Decimal `json:"amount"`
	IdempotencyKey string          `json:"idempotency_key"`
	ActorID        int64           `json:"actor_id"`
	Source         string          `json:"source"`
	Metadata       map[string]string `json:"metadata"`
}

func (i AddPendingInput) Validate() error {
	if i.AccountHolder == 0 {
		return fmt.Errorf("pending: add: account_holder required: %w", ErrInvalidInput)
	}
	if i.CurrencyID <= 0 {
		return fmt.Errorf("pending: add: currency_id must be positive: %w", ErrInvalidInput)
	}
	if !i.Amount.IsPositive() {
		return fmt.Errorf("pending: add: amount must be positive: %w", ErrInvalidInput)
	}
	if i.IdempotencyKey == "" {
		return fmt.Errorf("pending: add: idempotency_key required: %w", ErrInvalidInput)
	}
	return nil
}

// ConfirmPendingInput is the input to ConfirmPending.
type ConfirmPendingInput struct {
	AccountHolder  int64           `json:"account_holder"`
	CurrencyID     int64           `json:"currency_id"`
	Amount         decimal.Decimal `json:"amount"`
	IdempotencyKey string          `json:"idempotency_key"`
	ActorID        int64           `json:"actor_id"`
	Source         string          `json:"source"`
	Metadata       map[string]string `json:"metadata"`
}

func (i ConfirmPendingInput) Validate() error {
	if i.AccountHolder == 0 {
		return fmt.Errorf("pending: confirm: account_holder required: %w", ErrInvalidInput)
	}
	if i.CurrencyID <= 0 {
		return fmt.Errorf("pending: confirm: currency_id must be positive: %w", ErrInvalidInput)
	}
	if !i.Amount.IsPositive() {
		return fmt.Errorf("pending: confirm: amount must be positive: %w", ErrInvalidInput)
	}
	if i.IdempotencyKey == "" {
		return fmt.Errorf("pending: confirm: idempotency_key required: %w", ErrInvalidInput)
	}
	return nil
}

// CancelPendingInput is the input to CancelPending.
type CancelPendingInput struct {
	AccountHolder  int64           `json:"account_holder"`
	CurrencyID     int64           `json:"currency_id"`
	Amount         decimal.Decimal `json:"amount"`
	Reason         string          `json:"reason"`
	IdempotencyKey string          `json:"idempotency_key"`
	ActorID        int64           `json:"actor_id"`
	Source         string          `json:"source"`
	Metadata       map[string]string `json:"metadata"`
}

func (i CancelPendingInput) Validate() error {
	if i.AccountHolder == 0 {
		return fmt.Errorf("pending: cancel: account_holder required: %w", ErrInvalidInput)
	}
	if i.CurrencyID <= 0 {
		return fmt.Errorf("pending: cancel: currency_id must be positive: %w", ErrInvalidInput)
	}
	if !i.Amount.IsPositive() {
		return fmt.Errorf("pending: cancel: amount must be positive: %w", ErrInvalidInput)
	}
	if i.IdempotencyKey == "" {
		return fmt.Errorf("pending: cancel: idempotency_key required: %w", ErrInvalidInput)
	}
	return nil
}
