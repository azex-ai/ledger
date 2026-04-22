package core

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Status represents a state in a classification lifecycle.
type Status string

// Lifecycle defines a finite state machine for a classification.
// Nil Lifecycle on a Classification means label-only (no state machine).
type Lifecycle struct {
	Initial     Status              `json:"initial"`
	Terminal    []Status            `json:"terminal"`
	Transitions map[Status][]Status `json:"transitions"`
}

// Validate checks that the lifecycle is well-formed.
func (l *Lifecycle) Validate() error {
	if l.Initial == "" {
		return fmt.Errorf("core: lifecycle: initial status must not be empty: %w", ErrInvalidInput)
	}

	// Build sets for lookup.
	terminalSet := make(map[Status]bool, len(l.Terminal))
	for _, s := range l.Terminal {
		terminalSet[s] = true
	}

	// Initial must have outgoing transitions.
	if _, ok := l.Transitions[l.Initial]; !ok {
		return fmt.Errorf("core: lifecycle: initial status %q must have outgoing transitions: %w", l.Initial, ErrInvalidInput)
	}

	// Terminal states must not have outgoing transitions.
	for _, s := range l.Terminal {
		if targets, ok := l.Transitions[s]; ok && len(targets) > 0 {
			return fmt.Errorf("core: lifecycle: terminal status %q must not have outgoing transitions: %w", s, ErrInvalidInput)
		}
	}

	// All transition targets must be defined (as a key in Transitions or in Terminal).
	definedSet := make(map[Status]bool, len(l.Transitions)+len(l.Terminal))
	for s := range l.Transitions {
		definedSet[s] = true
	}
	for _, s := range l.Terminal {
		definedSet[s] = true
	}
	for from, targets := range l.Transitions {
		for _, to := range targets {
			if !definedSet[to] {
				return fmt.Errorf("core: lifecycle: transition %q -> %q targets undefined status: %w", from, to, ErrInvalidInput)
			}
		}
	}

	return nil
}

// CanTransition reports whether the lifecycle allows from -> to.
func (l *Lifecycle) CanTransition(from, to Status) bool {
	for _, allowed := range l.Transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// IsTerminal reports whether s is a terminal state.
func (l *Lifecycle) IsTerminal(s Status) bool {
	for _, t := range l.Terminal {
		if t == s {
			return true
		}
	}
	return false
}

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
	ID   int64  `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
}

// Classification represents a dynamic account classification.
// Lifecycle is nil for label-only classifications (no state machine).
type Classification struct {
	ID         int64      `json:"id"`
	Code       string     `json:"code"`
	Name       string     `json:"name"`
	NormalSide NormalSide `json:"normal_side"`
	IsSystem   bool       `json:"is_system"`
	IsActive   bool       `json:"is_active"`
	Lifecycle  *Lifecycle `json:"lifecycle,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// JournalType represents a dynamic journal category.
type JournalType struct {
	ID        int64     `json:"id"`
	Code      string    `json:"code"`
	Name      string    `json:"name"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

// Balance represents a computed balance for an account dimension.
type Balance struct {
	AccountHolder    int64           `json:"account_holder"`
	CurrencyID       int64           `json:"currency_id"`
	ClassificationID int64           `json:"classification_id"`
	Balance          decimal.Decimal `json:"balance"`
}
