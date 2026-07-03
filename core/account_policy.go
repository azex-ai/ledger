package core

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// AccountPolicyStatus is the freeze/close state of an account dimension.
type AccountPolicyStatus string

const (
	AccountPolicyStatusActive AccountPolicyStatus = "active"
	AccountPolicyStatusFrozen AccountPolicyStatus = "frozen"
	AccountPolicyStatusClosed AccountPolicyStatus = "closed"
)

func (s AccountPolicyStatus) IsValid() bool {
	switch s {
	case AccountPolicyStatusActive, AccountPolicyStatusFrozen, AccountPolicyStatusClosed:
		return true
	}
	return false
}

// accountPolicyNoteMaxLen bounds the free-text audit note. This is an
// operational safety valve (avoid unbounded payloads riding into an
// append-only audit table), not a business rule.
const accountPolicyNoteMaxLen = 2000

// AccountPolicy is an optional override on the otherwise implicit
// (account_holder, currency, classification) account dimension. A
// dimension with no AccountPolicy row behaves exactly as it does today:
// active, unconstrained. CurrencyUID == "" means "all currencies for this
// holder"; ClassificationUID == "" means "all classifications for this
// holder/currency". See docs/INVARIANTS.md I-17 for the enforcement contract.
type AccountPolicy struct {
	UID               string              `json:"uid"`
	AccountHolder     int64               `json:"account_holder"`
	CurrencyUID       string              `json:"currency_uid,omitempty"`
	ClassificationUID string              `json:"classification_uid,omitempty"`
	Status            AccountPolicyStatus `json:"status"`
	MinBalance        decimal.Decimal     `json:"min_balance"`
	EnforceMinBalance bool                `json:"enforce_min_balance"`
	Note              string              `json:"note"`
	UpdatedAt         time.Time           `json:"updated_at"`
	CreatedAt         time.Time           `json:"created_at"`
}

// AccountPolicyInput is the input to AccountPolicyStore.SetPolicy. Setting a
// policy is an operational/config write (not a funds movement), so unlike
// journal/reservation writes it carries no idempotency key — SetPolicy is a
// plain UPSERT keyed on (account_holder, currency_uid, classification_uid).
type AccountPolicyInput struct {
	AccountHolder     int64               `json:"account_holder"`
	CurrencyUID       string              `json:"currency_uid,omitempty"`
	ClassificationUID string              `json:"classification_uid,omitempty"`
	Status            AccountPolicyStatus `json:"status"`
	MinBalance        decimal.Decimal     `json:"min_balance"`
	EnforceMinBalance bool                `json:"enforce_min_balance"`
	Note              string              `json:"note"`
	ActorID           int64               `json:"actor_id"`
}

func (i AccountPolicyInput) Validate() error {
	if i.AccountHolder == 0 {
		return fmt.Errorf("core: account policy: account_holder must not be zero: %w", ErrInvalidInput)
	}
	if i.ClassificationUID != "" && i.CurrencyUID == "" {
		// A classification-specific policy without a currency has no defined
		// dimension to match (specificity ladder is holder -> +currency ->
		// +classification).
		return fmt.Errorf("core: account policy: classification_uid requires currency_uid: %w", ErrInvalidInput)
	}
	if !i.Status.IsValid() {
		return fmt.Errorf("core: account policy: invalid status %q: %w", i.Status, ErrInvalidInput)
	}
	if len(i.Note) > accountPolicyNoteMaxLen {
		return fmt.Errorf("core: account policy: note exceeds %d characters: %w", accountPolicyNoteMaxLen, ErrInvalidInput)
	}
	return nil
}

// BalanceDirection classifies whether posting an entry increases or
// decreases the balance of the account dimension it targets.
type BalanceDirection int

const (
	BalanceDirectionIncrease BalanceDirection = iota
	BalanceDirectionDecrease
)

// EntryDirection reports whether entryType, posted against an account whose
// classification has the given normal side, increases or decreases that
// account's balance. This mirrors the delta computation used by balance
// queries (see postgres.LedgerStore.getBalanceWithQueries):
//
//	debit-normal accounts:  debit increases, credit decreases
//	credit-normal accounts: credit increases, debit decreases
//
// This is the sole authority account-policy enforcement uses to classify an
// entry as "consumption" (decrease) vs "deposit" (increase) — see I-17.
func EntryDirection(entryType EntryType, normalSide NormalSide) BalanceDirection {
	increases := (entryType == EntryTypeDebit && normalSide == NormalSideDebit) ||
		(entryType == EntryTypeCredit && normalSide == NormalSideCredit)
	if increases {
		return BalanceDirectionIncrease
	}
	return BalanceDirectionDecrease
}
