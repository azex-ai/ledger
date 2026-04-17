package core

import (
	"time"

	"github.com/shopspring/decimal"
)

// EntryType represents debit or credit.
type EntryType string

const (
	EntryTypeDebit  EntryType = "debit"
	EntryTypeCredit EntryType = "credit"
)

func (e EntryType) IsValid() bool {
	return e == EntryTypeDebit || e == EntryTypeCredit
}

// NormalSide indicates default balance direction.
type NormalSide string

const (
	NormalSideDebit  NormalSide = "debit"
	NormalSideCredit NormalSide = "credit"
)

func (n NormalSide) IsValid() bool {
	return n == NormalSideDebit || n == NormalSideCredit
}

// SystemAccountHolder returns the system counterpart for a user.
// Positive = user, negative = system.
func SystemAccountHolder(userID int64) int64 {
	return -userID
}

func IsSystemAccount(holder int64) bool {
	return holder < 0
}

// Currency represents a tradeable currency.
type Currency struct {
	ID   int64
	Code string
	Name string
}

// Classification represents a dynamic account classification.
type Classification struct {
	ID         int64
	Code       string
	Name       string
	NormalSide NormalSide
	IsSystem   bool
	IsActive   bool
	CreatedAt  time.Time
}

// JournalType represents a dynamic journal category.
type JournalType struct {
	ID        int64
	Code      string
	Name      string
	IsActive  bool
	CreatedAt time.Time
}

// Balance represents a computed balance for an account dimension.
type Balance struct {
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	Balance          decimal.Decimal
}
