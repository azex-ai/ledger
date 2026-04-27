package bizcode

import "fmt"

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
)

// --- Display messages ---

var displayMessages = map[int]string{
	10001: "Please check your input and try again",
	10101: "Authentication required",
	10150: "You don't have permission",
	10201: "The requested resource was not found",
	10301: "This resource already exists",
	10401: "Rate limit exceeded, please retry later",
	10901: "Operation conflicts with current state",
	18101: "Service is starting or temporarily unavailable",
	14001: "Insufficient balance for this operation",
	14002: "This operation has already been processed",
	14003: "Journal entries are not balanced",
	14004: "Invalid state transition",
	14005: "Reservation has expired",
	19999: "An unexpected error occurred",
}

// DisplayMessage returns the user-facing message for a code.
func DisplayMessage(code int) string {
	if msg, ok := displayMessages[code]; ok {
		return msg
	}
	return "An unexpected error occurred"
}

// RegisterDisplayMessage registers a display message for a code.
func RegisterDisplayMessage(code int, msg string) {
	displayMessages[code] = msg
}
