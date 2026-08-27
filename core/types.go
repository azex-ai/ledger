package core

import (
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// Status represents a state in a classification lifecycle.
type Status string

// Lifecycle defines a finite state machine for a classification.
// Nil Lifecycle on a Classification means label-only (no state machine).
type Lifecycle struct {
	// Version of the lifecycle JSON shape. 0 (absent in pre-v0.3 rows) and 1
	// are equivalent today; the field exists so a future breaking change to
	// this structure can distinguish old rows instead of guessing. Bump only
	// with a documented migration path.
	Version     int                 `json:"version,omitempty"`
	Initial     Status              `json:"initial"`
	Terminal    []Status            `json:"terminal"`
	Transitions map[Status][]Status `json:"transitions"`
}

// Validate checks that the lifecycle is well-formed.
func (l *Lifecycle) Validate() error {
	if l.Version < 0 || l.Version > 1 {
		return fmt.Errorf("core: lifecycle: unsupported version %d (this build understands version 1): %w", l.Version, ErrInvalidInput)
	}
	if l.Initial == "" {
		return fmt.Errorf("core: lifecycle: initial status must not be empty: %w", ErrInvalidInput)
	}

	// Build sets for lookup.
	terminalSet := make(map[Status]bool, len(l.Terminal))
	for _, s := range l.Terminal {
		terminalSet[s] = true
	}

	// Initial must have outgoing transitions. An empty slice still counts as
	// "no transitions" — the FSM would have nowhere to go from initial.
	if outs, ok := l.Transitions[l.Initial]; !ok || len(outs) == 0 {
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

	// Every declared status must be reachable from Initial by walking
	// Transitions — an island state can never be entered (or, if it's the
	// dangling target of nothing, exited), so it can never be observed.
	visited := map[Status]bool{l.Initial: true}
	queue := []Status{l.Initial}
	for len(queue) > 0 {
		from := queue[0]
		queue = queue[1:]
		for _, to := range l.Transitions[from] {
			if !visited[to] {
				visited[to] = true
				queue = append(queue, to)
			}
		}
	}
	var unreachable []Status
	for s := range definedSet {
		if !visited[s] {
			unreachable = append(unreachable, s)
		}
	}
	if len(unreachable) > 0 {
		sort.Slice(unreachable, func(i, k int) bool { return unreachable[i] < unreachable[k] })
		return fmt.Errorf("core: lifecycle: unreachable status(es) from initial %q: %v: %w", l.Initial, unreachable, ErrInvalidInput)
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
//
// Exponent is the maximum number of decimal places an entry amount in this
// currency may carry (JPY=0, USD=2, USDT=6, wei=18). It bounds business
// precision; NUMERIC(30,18) in Postgres is only storage precision. Write
// paths reject (never round) amounts that exceed it — see
// core.ErrPrecisionExceeded.
type Currency struct {
	UID      string `json:"uid"`
	Code     string `json:"code"`
	Name     string `json:"name"`
	IsActive bool   `json:"is_active"`
	Exponent int32  `json:"exponent"`
}

// BalanceRole is the semantic liquidity tag of a classification in the
// holder-facing balance breakdown. It decouples the breakdown (and the
// Reserve availability base) from hardcoded classification codes — presets
// tag their classifications, consumers can tag their own.
type BalanceRole string

const (
	// BalanceRoleNone excludes the classification from the holder's
	// spendable-money view. Legitimate for is_system classifications (they
	// are never part of the holder-facing breakdown by construction), and
	// for legacy non-system rows created before ClassificationInput.Validate
	// started requiring an explicit role. ClassificationInput.Validate
	// refuses "" on any NEW non-system classification (M-4 fix,
	// docs/INVARIANTS.md I-37 addendum): "" used to mean both "this is a
	// deliberate memo/cost account" (fee_expense) and "nobody tagged this
	// yet" -- the same value carrying two different intents made the second
	// one silently invisible to SolvencyReport.Liability. Non-system memo
	// accounts must now declare BalanceRoleMemo explicitly instead of
	// leaving this blank.
	BalanceRoleNone BalanceRole = ""
	// BalanceRoleAvailable marks immediately spendable funds (main_wallet).
	BalanceRoleAvailable BalanceRole = "available"
	// BalanceRolePending marks inbound funds awaiting confirmation.
	BalanceRolePending BalanceRole = "pending"
	// BalanceRoleLocked marks journal-locked funds (withdrawal in flight).
	BalanceRoleLocked BalanceRole = "locked"
	// BalanceRoleMemo marks a non-system, user-side classification that is
	// deliberately NOT part of the holder's spendable-money view and NOT a
	// liability the platform owes back (fee_expense and friends: money the
	// user already paid, tracked per-holder for reporting only). Exists so a
	// classification can say "excluded on purpose" instead of leaving
	// BalanceRoleNone to do double duty as both that AND "unset" -- see the
	// BalanceRoleNone doc and docs/INVARIANTS.md I-37's addendum.
	BalanceRoleMemo BalanceRole = "memo"
)

func (r BalanceRole) IsValid() bool {
	switch r {
	case BalanceRoleNone, BalanceRoleAvailable, BalanceRolePending, BalanceRoleLocked, BalanceRoleMemo:
		return true
	}
	return false
}

// HolderTxKind is the small, deployment-stable product vocabulary the
// holder-facing transaction view (HolderTransaction.Kind) is drawn from. It
// exists to fix M-7 (`.local/independent-review-2026-08-26.md`,
// docs/INVARIANTS.md I-44): the holder wallet surface's `kind` field
// previously carried journal_types.code (e.g. "deposit_confirm" -- an
// internal accounting-engine identifier that narrates *how the ledger
// produced the balance*, a `~/.claude/rules/user-facing-surfaces.md`
// violation), and a first attempt at fixing that switched it to
// journal_types.uid (compliant, but opaque, per-deployment-random, and
// unwriteable as a literal -- @azex/ledger-react's `kindLabels` prop, which
// is keyed by a stable string a host app hardcodes, went silently dead
// against it).
//
// HolderTxKind is deliberately small and coarse -- a handful of values
// every deployment's transactions bucket into, stable across deployments,
// writable as a literal, and describing what the transaction IS to the
// holder rather than which internal template produced it. It is NOT meant
// to be exhaustive: HolderTxKindOther is the explicit escape hatch for a
// journal type that legitimately doesn't fit any bucket below, and adding a
// new named value later is cheap (an additive, expand-safe change) --
// changing what an existing value means is not
// (`~/.claude/rules/deployment.md`: "field semantics are never reused").
type HolderTxKind string

const (
	// HolderTxKindNone is JournalType.HolderKind's zero value. It means
	// "nobody has tagged this journal type yet" -- NOT a deliberate product
	// decision, unlike HolderTxKindOther below. Every preset this package
	// installs (presets/templates.go and friends) declares an explicit,
	// non-none HolderKind; this value only appears on legacy rows written
	// before this field existed, or on a consumer's own journal type they
	// have not yet retagged via JournalTypeStore.SetHolderKind. Unlike
	// BalanceRoleNone (core/interfaces.go's ClassificationInput.Validate
	// refuses it outright for a new non-system classification, because an
	// untagged real liability is silently miscounted in
	// SolvencyReport.Liability -- a financial-correctness failure), an
	// untagged journal type has no financial consequence: the holder
	// transaction view's read path (postgres/sql/queries/holder.sql) never
	// emits "" on the wire -- it reads HolderTxKindNone as
	// HolderTxKindOther, a legitimate, disclosed, generic bucket, not a
	// silent miscalculation. That is why this value is validated (rejects
	// garbage strings) but not forbidden at creation time the way
	// BalanceRoleNone is.
	HolderTxKindNone HolderTxKind = ""
	// HolderTxKindDeposit marks funds arriving from outside the platform
	// (crypto deposits, dev-mode simulated credit, merchant checkout
	// settlement landing in the holder's spendable balance).
	HolderTxKindDeposit HolderTxKind = "deposit"
	// HolderTxKindWithdrawal marks funds leaving the platform to the
	// holder's own external destination, including the lock/unlock legs of
	// that lifecycle.
	HolderTxKindWithdrawal HolderTxKind = "withdrawal"
	// HolderTxKindTransfer marks a movement between two holders on the same
	// platform (no funds cross the platform boundary).
	HolderTxKindTransfer HolderTxKind = "transfer"
	// HolderTxKindFee marks a charge the platform levies against the
	// holder's balance (withdrawal fee, standalone fee charge, ...).
	HolderTxKindFee HolderTxKind = "fee"
	// HolderTxKindAdjustment marks a correction to a holder's balance that
	// is not itself a new deposit, withdrawal, transfer, or fee (deposit
	// overage record/resolve/release, simulated dev-mode credit).
	HolderTxKindAdjustment HolderTxKind = "adjustment"
	// HolderTxKindOther is the explicit "genuinely does not fit any bucket
	// above" declaration -- for a journal type whose author considered the
	// vocabulary and chose this on purpose (capital injection/withdrawal,
	// FX conversion legs), as opposed to HolderTxKindNone's "nobody has
	// looked at this yet". See this type's doc comment.
	HolderTxKindOther HolderTxKind = "other"
)

func (k HolderTxKind) IsValid() bool {
	switch k {
	case HolderTxKindNone, HolderTxKindDeposit, HolderTxKindWithdrawal, HolderTxKindTransfer,
		HolderTxKindFee, HolderTxKindAdjustment, HolderTxKindOther:
		return true
	}
	return false
}

// Classification represents a dynamic account classification.
// Lifecycle is nil for label-only classifications (no state machine).
type Classification struct {
	UID        string     `json:"uid"`
	Code       string     `json:"code"`
	Name       string     `json:"name"`
	NormalSide NormalSide `json:"normal_side"`
	IsSystem   bool       `json:"is_system"`
	IsActive   bool       `json:"is_active"`
	// DisplayLabel is the user-facing wording for the holder transaction
	// view's kind translation (empty = not configured; the projection falls
	// back to the journal type's label, then its Name).
	DisplayLabel string      `json:"display_label"`
	BalanceRole  BalanceRole `json:"balance_role"`
	Lifecycle    *Lifecycle  `json:"lifecycle,omitempty"`
	CreatedAt    time.Time   `json:"created_at"`
}

// JournalType represents a dynamic journal category.
type JournalType struct {
	UID      string `json:"uid"`
	Code     string `json:"code"`
	Name     string `json:"name"`
	IsActive bool   `json:"is_active"`
	// DisplayLabel is the user-facing wording for the holder transaction
	// view's kind translation (empty = not configured; the projection falls
	// back to Name).
	DisplayLabel string `json:"display_label"`
	// HolderKind is this journal type's bucket in the small, stable
	// HolderTxKind vocabulary the holder transaction view's `kind` field is
	// drawn from (M-7 fix, docs/INVARIANTS.md I-44). HolderTxKindNone ("")
	// on a row read from storage means "untagged" -- see HolderTxKindNone's
	// doc comment for why that is tolerated here where it is refused for
	// Classification.BalanceRole.
	HolderKind HolderTxKind `json:"holder_kind"`
	CreatedAt  time.Time    `json:"created_at"`
}

// Balance represents a computed balance for an account dimension.
type Balance struct {
	AccountHolder     int64           `json:"account_holder"`
	CurrencyUID       string          `json:"currency_uid"`
	ClassificationUID string          `json:"classification_uid"`
	Balance           decimal.Decimal `json:"balance"`
}

// BalanceBreakdown is the holder-facing liquidity view of one
// (holder, currency) pair, aggregated from classification balance roles plus
// reservation holds:
//
//	Pending   = Σ balance(role=pending)                 — deposits awaiting confirmation
//	Locked    = Σ balance(role=locked) + reservation holds
//	Available = Σ balance(role=available) − reservation holds
//	Total     = Available + Locked + Pending
//
// Classifications with an empty role (fees, suspense, custodial, ...) are not
// part of this view. Reserve enforces its availability check against the same
// Available figure, so a consumer reading Available can rely on Reserve
// accepting up to exactly that amount (barring concurrent writes).
type BalanceBreakdown struct {
	AccountHolder int64           `json:"account_holder"`
	CurrencyUID   string          `json:"currency_uid"`
	Available     decimal.Decimal `json:"available"`
	Pending       decimal.Decimal `json:"pending"`
	Locked        decimal.Decimal `json:"locked"`
	Total         decimal.Decimal `json:"total"`
}
