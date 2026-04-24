package core

import (
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
	ID             int64
	AccountHolder  int64
	CurrencyID     int64
	ReservedAmount decimal.Decimal
	SettledAmount  *decimal.Decimal
	Status         ReservationStatus
	JournalID      *int64
	IdempotencyKey string
	ExpiresAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type ReserveInput struct {
	AccountHolder  int64
	CurrencyID     int64
	Amount         decimal.Decimal
	IdempotencyKey string
	ExpiresIn      time.Duration
}
