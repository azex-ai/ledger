package bizcode

import (
	"fmt"
	"sync"
)

// AppError is a structured business error with a numeric code.
type AppError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Err     error  `json:"-"`
}

func (e *AppError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("bizcode %d: %s: %s", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("bizcode %d: %s", e.Code, e.Message)
}

func (e *AppError) Unwrap() error { return e.Err }

func (e *AppError) Is(target error) bool {
	t, ok := target.(*AppError)
	return ok && t.Code == e.Code
}

// New creates a new AppError.
func New(code int, message string) *AppError {
	return &AppError{Code: code, Message: message}
}

// Wrap creates an AppError wrapping an underlying error.
func Wrap(code int, message string, err error) *AppError {
	return &AppError{Code: code, Message: message, Err: err}
}

// HTTPStatus derives an HTTP status code from the business code range.
func (e *AppError) HTTPStatus() int {
	switch {
	case e.Code >= 10000 && e.Code <= 10099:
		return 400 // Bad Request — input validation
	case e.Code >= 10100 && e.Code <= 10149:
		return 401 // Unauthorized
	case e.Code >= 10150 && e.Code <= 10199:
		return 403 // Forbidden
	case e.Code >= 10200 && e.Code <= 10299:
		return 404 // Not Found
	case e.Code >= 10300 && e.Code <= 10399:
		return 409 // Conflict — already exists
	case e.Code >= 10400 && e.Code <= 10499:
		return 429 // Too Many Requests — rate limiting
	case e.Code >= 10900 && e.Code <= 10999:
		return 409 // Conflict — state conflict
	case e.Code >= 14000 && e.Code <= 14999:
		return 422 // Unprocessable Entity — ledger errors
	case e.Code >= 18100 && e.Code <= 18199:
		return 503 // Service Unavailable
	default:
		return 500
	}
}

// --- Standard sentinels ---

var (
	InvalidInput  = New(10001, "invalid input")
	Unauthorized  = New(10101, "unauthorized")
	Forbidden     = New(10150, "forbidden")
	NotFound      = New(10201, "not found")
	AlreadyExists = New(10301, "already exists")
	Conflict      = New(10901, "conflict")
	Internal      = New(19999, "internal error")
)

// --- Ledger domain (14xxx) ---

var (
	InsufficientBalance = New(14001, "insufficient balance")
	DuplicateJournal    = New(14002, "duplicate journal (idempotency)")
	UnbalancedJournal   = New(14003, "journal debit/credit not balanced")
	InvalidTransition   = New(14004, "invalid state transition")
	ReservationExpired  = New(14005, "reservation expired")
	PrecisionExceeded   = New(14006, "amount exceeds currency precision")
	AccountFrozen       = New(14007, "account frozen")
	AccountClosed       = New(14008, "account closed")
	PeriodClosed        = New(14009, "accounting period is closed")
	// UnauthorizedJournal maps core.ErrUnauthorizedJournal: a journal has no
	// stored signature, a stored digest that does not match its recomputed
	// canonical digest, a signature the configured AuthVerifier rejects, or
	// (via VerifiedBalanceReader) a dimension where ANY contributing journal
	// fails that check. This is the tamper-detection system's only signal --
	// it must never fall through to the 19999 default, which both mislabels
	// it as an unclassified 500 and (via Retryable's default) tells clients
	// to retry a rejection that will never change (2026-08-26 audit
	// remediation, see docs/plans/2026-08-26-audit-remediation-contracts.md
	// §2).
	UnauthorizedJournal = New(14010, "journal missing or has invalid authorization signature")
)

// --- Feature availability (18xxx, 503) ---

var (
	// FeatureNotEnabled is returned by an optional add-on's routes (e.g. the
	// crypto-deposit address issuance / sighting-ingestion endpoints) when
	// the consumer's composition root never wired the corresponding service
	// in (nil until the relevant Set* call on server.Server). Distinct from
	// 18101 "starting up" -- this is a permanent configuration state, not a
	// transient one, but still maps to 503 since the caller's remedy (ask an
	// operator to enable it) looks the same as "try again later" from the
	// client's perspective.
	FeatureNotEnabled = New(18102, "feature not enabled")
)

// --- Transient / dependency-availability failures (18xxx, 503, retryable) ---
//
// Every code in this block corresponds 1:1 to a core sentinel that
// core.IsRetryable classifies as retryable. Keeping them in the existing
// 18100-18199 band (rather than inventing a new range) means
// bizcode.Retryable's existing range check already returns true for them
// with no switch-statement edit -- see
// pkg/httpx/response_test.go TestResolveError_AgreesWithCoreIsRetryable,
// which pins core.IsRetryable and bizcode.Retryable from disagreeing on the
// same error.

var (
	// RollupPending maps core.ErrRollupPending: a rollup worker is
	// mid-flight for this dimension. Retry once it drains.
	RollupPending = New(18103, "rollup queue item pending for this dimension")
	// AttestorUnavailable maps core.ErrAttestorUnavailable: the configured
	// signer/KMS used to authorize a journal was momentarily unreachable.
	AttestorUnavailable = New(18104, "authorization signer temporarily unavailable")
	// TransientFailure maps core.ErrTransient: a momentary contention or
	// dependency hiccup (serialization conflict, deadlock victim, connection
	// reset, ...) an adapter wrapped rather than a business-rule outcome.
	TransientFailure = New(18105, "temporary failure, please retry")
)

// --- Display messages ---

// displayMessagesMu guards displayMessages: RegisterDisplayMessage can be
// called by a consumer at any time (e.g. lazy registration of custom codes),
// and a concurrent DisplayMessage read against a bare map write is a Go
// concurrent map access -> process panic. A RWMutex keeps reads cheap.
var displayMessagesMu sync.RWMutex

var displayMessages = map[int]string{
	10001: "Please check your input and try again",
	10101: "Authentication required",
	10150: "You don't have permission",
	10201: "The requested resource was not found",
	10301: "This resource already exists",
	10401: "Rate limit exceeded, please retry later",
	10901: "Operation conflicts with current state",
	18101: "Service is starting or temporarily unavailable",
	18102: "This feature is not enabled on this server",
	14001: "Insufficient balance for this operation",
	14002: "This operation has already been processed",
	14003: "Journal entries are not balanced",
	14004: "Invalid state transition",
	14005: "Reservation has expired",
	14006: "Amount has more decimal places than this currency allows",
	14007: "This account is frozen",
	14008: "This account is closed",
	14009: "This accounting period is closed",
	14010: "This transaction failed a security check and could not be completed. Please contact support",
	18103: "This request is temporarily unavailable, please try again shortly",
	18104: "This request could not be completed right now, please try again shortly",
	18105: "A temporary error occurred, please try again",
	19999: "An unexpected error occurred",
}

// DisplayMessage returns the user-facing message for a code.
func DisplayMessage(code int) string {
	displayMessagesMu.RLock()
	msg, ok := displayMessages[code]
	displayMessagesMu.RUnlock()
	if ok {
		return msg
	}
	return "An unexpected error occurred"
}

// Retryable reports whether a client may safely retry the request that
// produced this business code, using the SAME idempotency_key (retrying a
// mutation with a fresh key would create a duplicate side effect, not a
// no-op — see docs/api.md "Idempotency").
//
// The classification follows HTTP retry semantics, not just the numeric
// range:
//   - 10400-10499 (rate limited) and 18100-18199 (service unavailable /
//     starting) describe transient conditions — retrying after backoff is
//     expected to succeed. Retryable.
//   - 10000-10399 (input validation, auth, forbidden, not found,
//     already-exists) and 10900-10999 (state conflict) describe a defect in
//     the request or an outcome that depends on request content — retrying
//     the identical payload reproduces the identical result. Not retryable.
//   - 14000-14999 (ledger domain-invariant violation: insufficient balance,
//     unbalanced journal, invalid transition, ...) is a business-rule
//     outcome, not a transient failure. Not retryable.
//   - Anything else (internal error, or a code outside every known range)
//     defaults to retryable: unclassified failures most often indicate a
//     transient dependency hiccup (DB blip, network reset) rather than a
//     permanent defect in the request.
func Retryable(code int) bool {
	switch {
	case code >= 10400 && code <= 10499:
		return true // rate limited
	case code >= 18100 && code <= 18199:
		return true // service unavailable / starting
	case code >= 10000 && code <= 10399:
		return false // input validation / auth / forbidden / not found / already-exists
	case code >= 10900 && code <= 10999:
		return false // state conflict
	case code >= 14000 && code <= 14999:
		return false // ledger domain invariant violation
	default:
		return true // unclassified / internal error — assume transient
	}
}

// RegisterDisplayMessage registers a display message for a code.
func RegisterDisplayMessage(code int, msg string) {
	displayMessagesMu.Lock()
	displayMessages[code] = msg
	displayMessagesMu.Unlock()
}
