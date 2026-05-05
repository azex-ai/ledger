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
	ID             int64             `json:"id"`
	AccountHolder  int64             `json:"account_holder"`
	CurrencyID     int64             `json:"currency_id"`
	ReservedAmount decimal.Decimal   `json:"reserved_amount"`
	SettledAmount  *decimal.Decimal  `json:"settled_amount,omitempty"`
	Status         ReservationStatus `json:"status"`
	JournalID      *int64            `json:"journal_id,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
	ExpiresAt      time.Time         `json:"expires_at"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

type ReserveInput struct {
	AccountHolder  int64           `json:"account_holder"`
	CurrencyID     int64           `json:"currency_id"`
	Amount         decimal.Decimal `json:"amount"`
	IdempotencyKey string          `json:"idempotency_key"`
	ExpiresIn      time.Duration   `json:"expires_in"`
}

func (i ReserveInput) Validate() error {
	if i.AccountHolder == 0 {
		return fmt.Errorf("core: reserve: account_holder required: %w", ErrInvalidInput)
	}
	if i.CurrencyID <= 0 {
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
