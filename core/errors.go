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
	// configured AuthVerifier rejects (see docs/INVARIANTS.md I-26). Also
	// returned (wrapped) by VerifiedBalanceReader.VerifiedBalance when ANY
	// journal contributing an entry to the dimension fails that same check
	// -- the balance is UNDEFINED in that case, never a number computed by
	// excluding the failing journal (contracts §W2-1; see
	// docs/INVARIANTS.md I-32).
	ErrUnauthorizedJournal = errors.New("journal missing or has invalid authorization signature")
	// ErrRollupPending is returned by CheckpointIntegrityStore.RebuildCheckpoint
	// when a rollup_queue item is still pending or claimed for the dimension
	// being rebuilt (see docs/INVARIANTS.md I-23). A rollup worker may have
	// already read the (possibly poisoned) checkpoint into memory; overwriting
	// it now would only be re-clobbered with poisoned-base-plus-delta once
	// that worker's write lands. Drain or wait for the item first.
	ErrRollupPending = errors.New("rollup queue item pending for dimension")
	// ErrTransient marks a failure caused by momentary contention or an
	// external dependency hiccup (serialization conflict, deadlock victim,
	// connection reset, ...) rather than a business-rule violation or a
	// malformed request. It carries no HTTP or bizcode knowledge itself --
	// adapters that observe a driver-specific transient condition (e.g. a
	// pgx SQLSTATE 40001/40P01) should wrap it into the error they return
	// (fmt.Errorf("...: %w: %w", causeErr, ErrTransient)) so IsRetryable
	// classifies it correctly without the caller needing to import a
	// driver-specific error type.
	ErrTransient = errors.New("transient failure, safe to retry")
)

// IsRetryable reports whether err represents a condition a caller may
// safely retry by resubmitting the SAME request with the SAME idempotency
// key (see api-contract.md §9: retrying with a fresh key on a request that
// already landed creates a duplicate side effect, not a no-op replay).
//
// This is the single source of truth for retry classification, consumed by
// BOTH library and HTTP modes so the two can never disagree on the same
// underlying error:
//   - library mode: call IsRetryable(err) directly -- this package has no
//     HTTP or bizcode dependency, so a consumer never needs to import pgx
//     or inspect a SQLSTATE to decide whether to retry.
//   - HTTP mode: pkg/httpx.resolveError maps every sentinel referenced
//     below to a bizcode drawn from a range whose bizcode.Retryable()
//     agrees with this function for that same error -- see
//     pkg/httpx/response_test.go TestResolveError_AgreesWithCoreIsRetryable,
//     which pins the two from drifting apart.
//
// A nil err has nothing to retry and reports false. ErrTransient,
// ErrAttestorUnavailable (the configured signer/KMS is momentarily
// unreachable), and ErrRollupPending (a concurrent rollup worker is
// mid-flight; retry once it drains) are retryable. Every other named
// sentinel above describes a business-rule outcome or a malformed
// request -- replaying the identical input reproduces the identical
// result, so it is never retryable. An error that matches none of the
// known sentinels defaults to retryable, mirroring bizcode.Retryable's
// default: an unclassified failure is assumed to be a transient
// dependency hiccup rather than a permanent defect.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrTransient),
		errors.Is(err, ErrAttestorUnavailable),
		errors.Is(err, ErrRollupPending):
		return true
	case errors.Is(err, ErrNotFound),
		errors.Is(err, ErrInvalidInput),
		errors.Is(err, ErrInsufficientBalance),
		errors.Is(err, ErrDuplicateJournal),
		errors.Is(err, ErrUnbalancedJournal),
		errors.Is(err, ErrInvalidTransition),
		errors.Is(err, ErrConflict),
		errors.Is(err, ErrPrecisionExceeded),
		errors.Is(err, ErrAccountFrozen),
		errors.Is(err, ErrAccountClosed),
		errors.Is(err, ErrPeriodClosed),
		errors.Is(err, ErrUnauthorizedJournal):
		return false
	default:
		return true
	}
}
