package postgres

import (
	"errors"

	"github.com/azex-ai/ledger/core"
)

// classifyJournalFailureReason maps a PostJournal/ExecuteTemplate error to a
// bounded reason label for JournalFailed's "reason" metric label (I-M1).
// This is the single source of truth for those label values -- keep
// docs/RUNBOOK.md's reason table (§7) in sync with this list rather than the
// other way around (I-M3: the RUNBOOK table used to name reasons no code
// ever produced).
//
// The switch order matters where sentinels can co-occur via multi-%w
// wrapping (e.g. ErrUnauthorizedJournal wraps ErrUnknownAuthKey): the more
// specific / more actionable label is checked first.
func classifyJournalFailureReason(err error) string {
	switch {
	case errors.Is(err, core.ErrUnbalancedJournal):
		return "unbalanced"
	case errors.Is(err, core.ErrAccountFrozen), errors.Is(err, core.ErrAccountClosed):
		return "account_policy"
	case errors.Is(err, core.ErrInsufficientBalance):
		return "insufficient_balance"
	case errors.Is(err, core.ErrPeriodClosed):
		return "period_closed"
	case errors.Is(err, core.ErrUnauthorizedJournal),
		errors.Is(err, core.ErrUnknownAuthKey),
		errors.Is(err, core.ErrAttestorUnavailable):
		return "unauthorized"
	case errors.Is(err, core.ErrDuplicateJournal):
		return "duplicate"
	case errors.Is(err, core.ErrConflict):
		return "conflict"
	case errors.Is(err, core.ErrNotFound):
		return "not_found"
	case errors.Is(err, core.ErrPrecisionExceeded), errors.Is(err, core.ErrInvalidInput):
		return "validation"
	default:
		return "db_error"
	}
}
