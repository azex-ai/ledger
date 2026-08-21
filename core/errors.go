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
	// ErrRollupPending is returned by CheckpointIntegrityStore.RebuildCheckpoint
	// when a rollup_queue item is still pending or claimed for the dimension
	// being rebuilt (see docs/INVARIANTS.md I-23). A rollup worker may have
	// already read the (possibly poisoned) checkpoint into memory; overwriting
	// it now would only be re-clobbered with poisoned-base-plus-delta once
	// that worker's write lands. Drain or wait for the item first.
	ErrRollupPending = errors.New("rollup queue item pending for dimension")
)
