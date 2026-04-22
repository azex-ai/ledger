package core

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Journal is a persisted balanced journal.
type Journal struct {
	ID             int64             `json:"id"`
	JournalTypeID  int64             `json:"journal_type_id"`
	IdempotencyKey string            `json:"idempotency_key"`
	TotalDebit     decimal.Decimal   `json:"total_debit"`
	TotalCredit    decimal.Decimal   `json:"total_credit"`
	Metadata       map[string]string `json:"metadata"`
	ActorID        int64             `json:"actor_id"`
	Source         string            `json:"source"`
	ReversalOf     int64             `json:"reversal_of"`
	EventID        int64             `json:"event_id"`
	CreatedAt      time.Time         `json:"created_at"`
}

// Entry is a single debit or credit line in a journal.
type Entry struct {
	ID               int64           `json:"id"`
	JournalID        int64           `json:"journal_id"`
	AccountHolder    int64           `json:"account_holder"`
	CurrencyID       int64           `json:"currency_id"`
	ClassificationID int64           `json:"classification_id"`
	EntryType        EntryType       `json:"entry_type"`
	Amount           decimal.Decimal `json:"amount"`
	CreatedAt        time.Time       `json:"created_at"`
}

// EntryInput is the input for a single entry line.
type EntryInput struct {
	AccountHolder    int64           `json:"account_holder"`
	CurrencyID       int64           `json:"currency_id"`
	ClassificationID int64           `json:"classification_id"`
	EntryType        EntryType       `json:"entry_type"`
	Amount           decimal.Decimal `json:"amount"`
}

// JournalInput is the input to post a journal.
type JournalInput struct {
	JournalTypeID  int64             `json:"journal_type_id"`
	IdempotencyKey string            `json:"idempotency_key"`
	Entries        []EntryInput      `json:"entries"`
	Metadata       map[string]string `json:"metadata"`
	ActorID        int64             `json:"actor_id"`
	Source         string            `json:"source"`
	ReversalOf     int64             `json:"reversal_of"`
	EventID        int64             `json:"event_id"`
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
		return fmt.Errorf("core: journal: idempotency key required: %w", ErrInvalidInput)
	}
	if len(j.Entries) == 0 {
		return fmt.Errorf("core: journal: entries must not be empty: %w", ErrInvalidInput)
	}
	for i, e := range j.Entries {
		if !e.EntryType.IsValid() {
			return fmt.Errorf("core: journal: entry[%d]: invalid entry type %q: %w", i, e.EntryType, ErrInvalidInput)
		}
		if !e.Amount.IsPositive() {
			return fmt.Errorf("core: journal: entry[%d]: amount must be positive: %w", i, ErrInvalidInput)
		}
	}
	debit, credit := j.Totals()
	if !debit.Equal(credit) {
		return fmt.Errorf("core: journal: unbalanced — debit=%s credit=%s: %w", debit, credit, ErrUnbalancedJournal)
	}
	return nil
}
