package core

import (
	"time"

	"github.com/shopspring/decimal"
)

type WithdrawStatus string

const (
	WithdrawStatusLocked     WithdrawStatus = "locked"
	WithdrawStatusReserved   WithdrawStatus = "reserved"
	WithdrawStatusReviewing  WithdrawStatus = "reviewing"
	WithdrawStatusProcessing WithdrawStatus = "processing"
	WithdrawStatusConfirmed  WithdrawStatus = "confirmed"
	WithdrawStatusFailed     WithdrawStatus = "failed"
	WithdrawStatusExpired    WithdrawStatus = "expired"
)

var withdrawTransitions = map[WithdrawStatus][]WithdrawStatus{
	WithdrawStatusLocked:     {WithdrawStatusReserved},
	WithdrawStatusReserved:   {WithdrawStatusReviewing, WithdrawStatusProcessing},
	WithdrawStatusReviewing:  {WithdrawStatusProcessing, WithdrawStatusFailed},
	WithdrawStatusProcessing: {WithdrawStatusConfirmed, WithdrawStatusFailed, WithdrawStatusExpired},
	WithdrawStatusFailed:     {WithdrawStatusReserved}, // retry
}

func (s WithdrawStatus) IsValid() bool {
	switch s {
	case WithdrawStatusLocked, WithdrawStatusReserved, WithdrawStatusReviewing,
		WithdrawStatusProcessing, WithdrawStatusConfirmed, WithdrawStatusFailed, WithdrawStatusExpired:
		return true
	}
	return false
}

func (s WithdrawStatus) CanTransitionTo(target WithdrawStatus) bool {
	for _, allowed := range withdrawTransitions[s] {
		if allowed == target {
			return true
		}
	}
	return false
}

type Withdrawal struct {
	ID             int64
	AccountHolder  int64
	CurrencyID     int64
	Amount         decimal.Decimal
	Status         WithdrawStatus
	ChannelName    string
	ChannelRef     *string
	ReservationID  *int64
	JournalID      *int64
	IdempotencyKey string
	Metadata       map[string]string
	ReviewRequired bool
	ExpiresAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type WithdrawInput struct {
	AccountHolder  int64
	CurrencyID     int64
	Amount         decimal.Decimal
	ChannelName    string
	IdempotencyKey string
	ReviewRequired bool
	Metadata       map[string]string
	ExpiresAt      *time.Time
}
