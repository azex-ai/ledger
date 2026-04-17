package core

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Journal is a persisted balanced journal.
type Journal struct {
	ID             int64
	JournalTypeID  int64
	IdempotencyKey string
	TotalDebit     decimal.Decimal
	TotalCredit    decimal.Decimal
	Metadata       map[string]string
	ActorID        *int64
	Source         string
	ReversalOf     *int64
	CreatedAt      time.Time
}

// Entry is a single debit or credit line in a journal.
type Entry struct {
	ID               int64
	JournalID        int64
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	EntryType        EntryType
	Amount           decimal.Decimal
	CreatedAt        time.Time
}

// EntryInput is the input for a single entry line.
type EntryInput struct {
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	EntryType        EntryType
	Amount           decimal.Decimal
}

// JournalInput is the input to post a journal.
type JournalInput struct {
	JournalTypeID  int64
	IdempotencyKey string
	Entries        []EntryInput
	Metadata       map[string]string
	ActorID        *int64
	Source         string
	ReversalOf     *int64
}

func (j *JournalInput) Totals() (debit, credit decimal.Decimal) {
	debit = decimal.Zero
	credit = decimal.Zero
	for _, e := range j.Entries {
		switch e.EntryType {
		case EntryTypeDebit:
			debit = debit.Add(e.Amount)
		case EntryTypeCredit:
			credit = credit.Add(e.Amount)
		}
	}
	return debit, credit
}

func (j *JournalInput) Validate() error {
	if j.IdempotencyKey == "" {
		return fmt.Errorf("core: journal: idempotency key required")
	}
	if len(j.Entries) == 0 {
		return fmt.Errorf("core: journal: entries must not be empty")
	}
	for i, e := range j.Entries {
		if !e.EntryType.IsValid() {
			return fmt.Errorf("core: journal: entry[%d]: invalid entry type %q", i, e.EntryType)
		}
		if !e.Amount.IsPositive() {
			return fmt.Errorf("core: journal: entry[%d]: amount must be positive", i)
		}
	}
	debit, credit := j.Totals()
	if !debit.Equal(credit) {
		return fmt.Errorf("core: journal: unbalanced — debit=%s credit=%s", debit, credit)
	}
	return nil
}
