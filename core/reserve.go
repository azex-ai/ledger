package core

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

type ReservationStatus string

const (
	ReservationStatusActive   ReservationStatus = "active"
	ReservationStatusSettling ReservationStatus = "settling"
	ReservationStatusSettled  ReservationStatus = "settled"
	ReservationStatusReleased ReservationStatus = "released"
)

var reservationTransitions = map[ReservationStatus][]ReservationStatus{
	ReservationStatusActive:   {ReservationStatusSettling, ReservationStatusSettled, ReservationStatusReleased},
	ReservationStatusSettling: {ReservationStatusSettled, ReservationStatusReleased},
}

func (s ReservationStatus) IsValid() bool {
	switch s {
	case ReservationStatusActive, ReservationStatusSettling, ReservationStatusSettled, ReservationStatusReleased:
		return true
	}
	return false
}

func (s ReservationStatus) CanTransitionTo(target ReservationStatus) bool {
	for _, allowed := range reservationTransitions[s] {
		if allowed == target {
			return true
		}
	}
	return false
}

type Reservation struct {
	UID            string            `json:"uid"`
	AccountHolder  int64             `json:"account_holder"`
	CurrencyUID    string            `json:"currency_uid"`
	ReservedAmount decimal.Decimal   `json:"reserved_amount"`
	SettledAmount  *decimal.Decimal  `json:"settled_amount,omitempty"`
	Status         ReservationStatus `json:"status"`
	JournalUID     string            `json:"journal_uid,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
	ExpiresAt      time.Time         `json:"expires_at"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

type ReserveInput struct {
	AccountHolder  int64           `json:"account_holder"`
	CurrencyUID    string          `json:"currency_uid"`
	Amount         decimal.Decimal `json:"amount"`
	IdempotencyKey string          `json:"idempotency_key"`
	ExpiresIn      time.Duration   `json:"expires_in"`
}

func (i ReserveInput) Validate() error {
	if i.AccountHolder == 0 {
		return fmt.Errorf("core: reserve: account_holder required: %w", ErrInvalidInput)
	}
	if i.CurrencyUID == "" {
		return fmt.Errorf("core: reserve: currency_id must be positive: %w", ErrInvalidInput)
	}
	if !i.Amount.IsPositive() {
		return fmt.Errorf("core: reserve: amount must be positive: %w", ErrInvalidInput)
	}
	if i.IdempotencyKey == "" {
		return fmt.Errorf("core: reserve: idempotency key required: %w", ErrInvalidInput)
	}
	return nil
}

// SettleInput is the input for a one-shot settlement of an active reservation.
type SettleInput struct {
	ReservationUID string          `json:"reservation_uid"`
	Amount         decimal.Decimal `json:"amount"`
}

func (i SettleInput) Validate() error {
	if i.ReservationUID == "" {
		return fmt.Errorf("core: settle: reservation_id must be positive: %w", ErrInvalidInput)
	}
	if !i.Amount.IsPositive() {
		return fmt.Errorf("core: settle: amount must be positive: %w", ErrInvalidInput)
	}
	return nil
}

// SettlePartialInput is the input for one increment of a partial settlement.
//
// IdempotencyKey is REQUIRED (I-3): SettlePartial is an accumulator
// (settled_amount += Amount), so without a durable dedup record a client
// retry of a lost response would double-apply the amount. A replayed key
// with the same amount succeeds without re-applying; a replayed key with a
// different amount is ErrConflict.
type SettlePartialInput struct {
	ReservationUID string          `json:"reservation_uid"`
	Amount         decimal.Decimal `json:"amount"`
	IdempotencyKey string          `json:"idempotency_key"`
}

func (i SettlePartialInput) Validate() error {
	if i.ReservationUID == "" {
		return fmt.Errorf("core: settle partial: reservation_id must be positive: %w", ErrInvalidInput)
	}
	if !i.Amount.IsPositive() {
		return fmt.Errorf("core: settle partial: amount must be positive: %w", ErrInvalidInput)
	}
	if i.IdempotencyKey == "" {
		return fmt.Errorf("core: settle partial: idempotency key required: %w", ErrInvalidInput)
	}
	return nil
}
