package core

import (
	"time"

	"github.com/shopspring/decimal"
)

// Holder-scoped wallet read surface (docs/plans/2026-07-08-holder-scoped-wallet-surface.md).
// These are read-model projections of ledger data for ONE account holder,
// translated to end-user concepts: no entry sides, no counterpart accounts,
// no internal ids. The projection rules live in the store implementation
// (grain = (journal, holder, currency) net aggregation, §3.3 of the plan).

// HolderTransactionDirection is the sign of a holder's net balance change in
// one transaction view row.
type HolderTransactionDirection string

const (
	HolderTransactionIn  HolderTransactionDirection = "in"
	HolderTransactionOut HolderTransactionDirection = "out"
)

// HolderTransaction is one row of the holder-facing transaction view: the
// net effect of one journal on one (holder, currency), in user language.
type HolderTransaction struct {
	// UID is the journal uid — the idempotent trace anchor, safe to display.
	UID string `json:"uid"`
	// Kind is the stable machine code (journal type code); the anchor for
	// product-side i18n / label overrides.
	Kind string `json:"kind"`
	// KindLabel is the library-side default display label (see
	// Classification.DisplayLabel / JournalType.DisplayLabel fallback chain).
	KindLabel    string                     `json:"kind_label"`
	Direction    HolderTransactionDirection `json:"direction"`
	Amount       decimal.Decimal            `json:"amount"` // absolute net amount
	CurrencyUID  string                     `json:"currency_uid"`
	CurrencyCode string                     `json:"currency_code"`
	// OccurredAt is the journal's effective_at (business time, I-14).
	OccurredAt    time.Time `json:"occurred_at"`
	ReversalOfUID string    `json:"reversal_of_uid,omitempty"`
	// Memo is journal.metadata["memo"] — the well-known key hosts write
	// user-readable copy into at post time. Empty when absent.
	Memo string `json:"memo"`
}

// HolderMemoMetadataKey is the journal metadata key the holder transaction
// view surfaces as Memo.
const HolderMemoMetadataKey = "memo"

// HolderHold is the user-facing summary of one active reservation: what is
// locked, in what currency, and until when. No reservation state machine.
type HolderHold struct {
	UID string `json:"uid"`
	// Amount is the outstanding hold: full reserved_amount while active, the
	// unsettled remainder while settling.
	Amount       decimal.Decimal `json:"amount"`
	CurrencyUID  string          `json:"currency_uid"`
	CurrencyCode string          `json:"currency_code"`
	CreatedAt    time.Time       `json:"created_at"`
	ExpiresAt    time.Time       `json:"expires_at"`
}

// HolderBalance is BalanceBreakdown plus the display currency code — one row
// per currency the holder has ever touched.
type HolderBalance struct {
	CurrencyUID  string          `json:"currency_uid"`
	CurrencyCode string          `json:"currency_code"`
	Available    decimal.Decimal `json:"available"`
	Pending      decimal.Decimal `json:"pending"`
	Locked       decimal.Decimal `json:"locked"`
	Total        decimal.Decimal `json:"total"` // = Available + Pending + Locked
}
