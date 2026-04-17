package core

import (
	"time"

	"github.com/shopspring/decimal"
)

type DepositStatus string

const (
	DepositStatusPending    DepositStatus = "pending"
	DepositStatusConfirming DepositStatus = "confirming"
	DepositStatusConfirmed  DepositStatus = "confirmed"
	DepositStatusFailed     DepositStatus = "failed"
	DepositStatusExpired    DepositStatus = "expired"
)

var depositTransitions = map[DepositStatus][]DepositStatus{
	DepositStatusPending:    {DepositStatusConfirming, DepositStatusFailed, DepositStatusExpired},
	DepositStatusConfirming: {DepositStatusConfirmed, DepositStatusFailed, DepositStatusExpired},
}

func (s DepositStatus) IsValid() bool {
	switch s {
	case DepositStatusPending, DepositStatusConfirming, DepositStatusConfirmed, DepositStatusFailed, DepositStatusExpired:
		return true
	}
	return false
}

func (s DepositStatus) CanTransitionTo(target DepositStatus) bool {
	for _, allowed := range depositTransitions[s] {
		if allowed == target {
			return true
		}
	}
	return false
}

type Deposit struct {
	ID             int64
	AccountHolder  int64
	CurrencyID     int64
	ExpectedAmount decimal.Decimal
	ActualAmount   *decimal.Decimal
	Status         DepositStatus
	ChannelName    string
	ChannelRef     *string
	JournalID      *int64
	IdempotencyKey string
	Metadata       map[string]string
	ExpiresAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type DepositInput struct {
	AccountHolder  int64
	CurrencyID     int64
	ExpectedAmount decimal.Decimal
	ChannelName    string
	IdempotencyKey string
	Metadata       map[string]string
	ExpiresAt      *time.Time
}

type ConfirmDepositInput struct {
	DepositID    int64
	ActualAmount decimal.Decimal
	ChannelRef   string
}
