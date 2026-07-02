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
)
