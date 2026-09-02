package httpx

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/wirejson"
	"github.com/azex-ai/ledger/pkg/bizcode"
)

// json is the repository-wide wire encoder (internal/wirejson): snake_case
// for un-tagged exported fields, and every time.Time as RFC3339 UTC. It used
// to be defined here, which is exactly why outbound webhooks did not get it
// (H-M4) -- the encoder now lives in one place and both exits consume it.
var json = wirejson.API()

// snakeCase is re-exported from internal/wirejson for this package's tests
// and for callers that need the same field-naming rule outside the encoder.
func snakeCase(name string) string { return wirejson.SnakeCase(name) }

// Result is the unified success response envelope.
type Result[T any] struct {
	Code    int           `json:"code"`
	Message *ErrorMessage `json:"message"`
	Data    T             `json:"data"`
}

// ErrorMessage is the user-facing error carried in the canonical response
// envelope. Technical details remain server-side in logs.
type ErrorMessage struct {
	Text   string            `json:"text"`
	Fields map[string]string `json:"fields,omitempty"`
}

// ErrorBody is the unified error response envelope. Message and Data are
// mutually exclusive: failures carry a message object and a null data field.
type ErrorBody struct {
	Code    int          `json:"code"`
	Message ErrorMessage `json:"message"`
	Data    any          `json:"data"`
}

// OK writes a 200 success response.
func OK[T any](w http.ResponseWriter, data T) {
	writeJSON(w, http.StatusOK, Result[T]{Code: 200, Data: data})
}

// Created writes a 201 success response.
func Created[T any](w http.ResponseWriter, data T) {
	writeJSON(w, http.StatusCreated, Result[T]{Code: 200, Data: data})
}

// Error resolves an error to a bizcode and writes the error response.
//
// The log level follows the resolved HTTP status (I-N15): a 4xx is the
// caller's problem and logs at Info, so a scanner throwing bad uids at the
// API cannot bury real 5xx behind a wall of Error lines; a 503 (starting up,
// feature off, transient dependency) logs at Warn; anything else 5xx is the
// server's own failure and logs at Error.
//
// message.fields is populated only from AppError.Fields -- text a handler
// wrote for a user (see bizcode.AppError.WithField). err's own text stays in
// the log line and never crosses the wire.
func Error(w http.ResponseWriter, err error) {
	ae := resolveError(err)
	status := ae.HTTPStatus()
	attrs := []any{"code", ae.Code, "message", ae.Message, "status", status, "err", err}
	switch {
	case status >= 500 && status != http.StatusServiceUnavailable:
		slog.Error("api error", attrs...)
	case status == http.StatusServiceUnavailable:
		slog.Warn("api unavailable", attrs...)
	default:
		slog.Info("api rejected request", attrs...)
	}
	writeJSON(w, status, ErrorBody{
		Code:    ae.Code,
		Message: ErrorMessage{Text: bizcode.DisplayMessage(ae.Code), Fields: ae.Fields},
	})
}

// Decode decodes a JSON request body into T.
func Decode[T any](r *http.Request) (T, error) {
	var v T
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		return v, bizcode.Wrap(10001, "invalid request body", err)
	}
	return v, nil
}

// --- Shortcut constructors ---

func ErrBadRequest(msg string) *bizcode.AppError { return bizcode.New(10001, msg) }

// ErrField is ErrBadRequest for a validation failure attributable to one
// named request field. It carries the message twice: once as the AppError's
// own text (the log line, and the developer-facing string), and once in
// message.fields[field] on the wire, which is what api-contract.md §1
// reserves for field-level validation errors and what a form can map onto
// the offending input (J-8: the envelope had the field, nothing ever wrote
// it).
//
// field must be the request body's own snake_case field name, and msg must
// be written for a user to read -- it crosses the wire verbatim. Never pass
// a driver/storage error string.
func ErrField(field, msg string) *bizcode.AppError {
	return bizcode.New(10001, field+": "+msg).WithField(field, msg)
}
func ErrForbidden(msg string) *bizcode.AppError { return bizcode.New(10150, msg) }
func ErrNotFound(msg string) *bizcode.AppError  { return bizcode.New(10201, msg) }
func ErrConflict(msg string) *bizcode.AppError  { return bizcode.New(10901, msg) }
func ErrInternal(msg string) *bizcode.AppError  { return bizcode.New(19999, msg) }

// resolveError maps an error to an AppError.
func resolveError(err error) *bizcode.AppError {
	// Already an AppError
	var ae *bizcode.AppError
	if errors.As(err, &ae) {
		return ae
	}

	// Domain sentinel → bizcode mapping
	switch {
	case errors.Is(err, core.ErrNotFound):
		return bizcode.Wrap(10201, "not found", err)
	case errors.Is(err, core.ErrInsufficientBalance):
		return bizcode.Wrap(14001, "insufficient balance", err)
	case errors.Is(err, core.ErrDuplicateJournal):
		return bizcode.Wrap(14002, "duplicate journal", err)
	case errors.Is(err, core.ErrUnbalancedJournal):
		return bizcode.Wrap(14003, "unbalanced journal", err)
	case errors.Is(err, core.ErrInvalidTransition):
		return bizcode.Wrap(14004, "invalid transition", err)
	case errors.Is(err, core.ErrPrecisionExceeded):
		return bizcode.Wrap(14006, "amount exceeds currency precision", err)
	case errors.Is(err, core.ErrAccountFrozen):
		return bizcode.Wrap(14007, "account frozen", err)
	case errors.Is(err, core.ErrAccountClosed):
		return bizcode.Wrap(14008, "account closed", err)
	case errors.Is(err, core.ErrPeriodClosed):
		return bizcode.Wrap(14009, "accounting period is closed", err)
	case errors.Is(err, core.ErrUnauthorizedJournal):
		return bizcode.Wrap(14010, "unauthorized journal", err)
	// E-M9: ErrUnknownAuthKey had no mapping at all, so it fell through to
	// 19999, whose band Retryable calls retryable -- while core.IsRetryable
	// says false. Library-mode and HTTP-mode consumers reached opposite
	// verdicts on the same error, and the pin that exists to stop exactly
	// that was a hand-copied list of sentinels that had never been updated
	// to include it (now derived from core/errors.go instead).
	//
	// Ordered after ErrUnauthorizedJournal deliberately: VerifyJournalAuth
	// propagates an unknown key wrapped INSIDE ErrUnauthorizedJournal (Go
	// multi-%w, so both are errors.Is-able), and for a journal that failed
	// verification the caller-visible answer stays 14010 -- "failed a
	// security check" -- rather than becoming a narrower code that hints at
	// this deployment's key inventory. 14011 is for an ErrUnknownAuthKey
	// that arrives on its own, e.g. straight from an AuthVerifier.
	case errors.Is(err, core.ErrUnknownAuthKey):
		return bizcode.Wrap(14011, "unknown authorization key", err)
	case errors.Is(err, core.ErrInvalidInput):
		return bizcode.Wrap(10001, "invalid input", err)
	case errors.Is(err, core.ErrConflict):
		return bizcode.Wrap(10901, "conflict", err)
	case errors.Is(err, core.ErrRollupPending):
		return bizcode.Wrap(18103, "rollup queue item pending for this dimension", err)
	case errors.Is(err, core.ErrAttestorUnavailable):
		return bizcode.Wrap(18104, "authorization signer temporarily unavailable", err)
	case errors.Is(err, core.ErrTransient):
		return bizcode.Wrap(18105, "temporary failure", err)
	default:
		return bizcode.Wrap(19999, "internal error", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":19999,"message":{"text":"Internal server error"},"data":null}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
