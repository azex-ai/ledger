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

// Sign is the sole authority for interpreting a classification's
// normal_side anywhere in this codebase: it reports whether entryType,
// posted against an account whose classification has the given normal
// side, increases (+1) or decreases (-1) that account's balance.
//
//	debit-normal accounts:  debit increases, credit decreases
//	credit-normal accounts: credit increases, debit decreases
//
// Equivalently: entryType == normalSide increases, anything else decreases.
// Every other function in this file, and every balance/delta/direction
// computation across core/, service/, postgres/ and the postgres/sql/
// queries/*.sql sign expressions (via the ledger_signed_amount() SQL
// function migration 009 installs, which mirrors this exact rule) must
// route through Sign or one of its wrappers below — not reimplement the
// comparison. See I-43.
//
// entryType and normalSide are both restricted to {debit, credit} by DB
// CHECK constraints (001_baseline.up.sql), and normal_side is immutable
// once set (the classifications_normal_side_immutable trigger), so an
// unknown value is unreachable in a healthy deployment. Sign still refuses
// rather than defaults on one: prior to this function, the 17 independent
// reimplementations of this same comparison disagreed on what an unknown
// normal_side means — some rejected it, one defaulted to debit-normal, and
// one (checkpoints.sql's ListComputedBalancesForHolders) silently excluded
// the entry from the balance (`ELSE 0`) instead of erring at all. A caller
// that manages to construct an invalid value — a bug, a corrupted read, a
// future write path that skips validation — must find out immediately, not
// have funds silently miscounted or dropped. See
// docs/audits/2026-08-25-financial-engineering/financial-correctness.md
// ("同一个符号语义有 17 处独立实现").
func Sign(normalSide NormalSide, entryType EntryType) (int, error) {
	if !normalSide.IsValid() {
		return 0, fmt.Errorf("core: unknown normal_side %q: %w", normalSide, ErrInvalidInput)
	}
	if !entryType.IsValid() {
		return 0, fmt.Errorf("core: unknown entry_type %q: %w", entryType, ErrInvalidInput)
	}
	if string(entryType) == string(normalSide) {
		return 1, nil
	}
	return -1, nil
}

// SignedAmount applies Sign to a single entry's amount, returning +amount
// if the entry increases the normalSide-normal balance it targets, or
// -amount if it decreases it.
func SignedAmount(normalSide NormalSide, entryType EntryType, amount decimal.Decimal) (decimal.Decimal, error) {
	sign, err := Sign(normalSide, entryType)
	if err != nil {
		return decimal.Decimal{}, err
	}
	if sign < 0 {
		return amount.Neg(), nil
	}
	return amount, nil
}

// Delta applies Sign to a classification's pre-bucketed debit-type and
// credit-type entry sums — the shape GetBalance, the rollup worker and the
// reconciliation suite all consume — and returns the net balance change
// those entries represent. It is exactly
// SignedAmount(normalSide, EntryTypeDebit, debitSum) +
// SignedAmount(normalSide, EntryTypeCredit, creditSum), by linearity of the
// same sign rule Sign encodes.
func Delta(normalSide NormalSide, debitSum, creditSum decimal.Decimal) (decimal.Decimal, error) {
	debitPart, err := SignedAmount(normalSide, EntryTypeDebit, debitSum)
	if err != nil {
		return decimal.Decimal{}, err
	}
	creditPart, err := SignedAmount(normalSide, EntryTypeCredit, creditSum)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return debitPart.Add(creditPart), nil
}

// EntryDirection reports whether entryType, posted against an account whose
// classification has the given normal side, increases or decreases that
// account's balance. This is the sole authority account-policy enforcement
// uses to classify an entry as "consumption" (decrease) vs "deposit"
// (increase) — see I-17. It is a thin wrapper over Sign; see Sign's
// documentation for the rule itself and why an unknown normal_side or
// entry_type is refused rather than defaulted.
func EntryDirection(entryType EntryType, normalSide NormalSide) (BalanceDirection, error) {
	sign, err := Sign(normalSide, entryType)
	if err != nil {
		return 0, err
	}
	if sign > 0 {
		return BalanceDirectionIncrease, nil
	}
	return BalanceDirectionDecrease, nil
}
