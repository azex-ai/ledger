package core

import "errors"

// Domain sentinel errors. These carry no HTTP or bizcode knowledge.
var (
	ErrNotFound            = errors.New("not found")
	ErrInvalidInput        = errors.New("invalid input")
	ErrInsufficientBalance = errors.New("insufficient balance")
	ErrDuplicateJournal    = errors.New("duplicate journal")
	ErrUnbalancedJournal   = errors.New("unbalanced journal")
	ErrInvalidTransition   = errors.New("invalid state transition")
	ErrConflict            = errors.New("conflict")
	ErrPrecisionExceeded   = errors.New("amount exceeds currency precision")
	ErrAccountFrozen       = errors.New("account frozen")
	ErrAccountClosed       = errors.New("account closed")
	// ErrPeriodClosed is returned when a journal's effective_at falls before
	// the active accounting period close line (see docs/INVARIANTS.md I-15).
	ErrPeriodClosed = errors.New("accounting period is closed")
	// ErrAttestorUnavailable is returned (wrapped) by PostJournal when a
	// configured Attestor's Sign call errors (see docs/INVARIANTS.md I-26).
	ErrAttestorUnavailable = errors.New("attestor unavailable")
	// ErrUnauthorizedJournal is returned (wrapped) by VerifyJournalAuth when
	// a journal has no stored signature, a stored digest that does not
	// match its own recomputed canonical digest, or a signature/key_id the
	// configured AuthVerifier rejects (see docs/INVARIANTS.md I-26).
	ErrUnauthorizedJournal = errors.New("journal missing or has invalid authorization signature")
)
