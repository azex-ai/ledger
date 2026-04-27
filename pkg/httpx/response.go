package httpx

import (
	"errors"
	"log/slog"
	"net/http"

	jsoniter "github.com/json-iterator/go"
	"github.com/json-iterator/go/extra"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/bizcode"
)

func init() {
	extra.SetNamingStrategy(snakeCase)
}

var json = jsoniter.ConfigCompatibleWithStandardLibrary

// snakeCase converts Go PascalCase field names to snake_case,
// correctly handling consecutive uppercase runs like "ID", "URL", "HTTP".
// Examples: ID→id, CurrencyID→currency_id, HTTPStatus→http_status, IsActive→is_active
func snakeCase(name string) string {
	runes := []rune(name)
	n := len(runes)
	var buf []rune
	for i := 0; i < n; i++ {
		r := runes[i]
		if r >= 'A' && r <= 'Z' {
			// Find the end of consecutive uppercase run
			j := i + 1
			for j < n && runes[j] >= 'A' && runes[j] <= 'Z' {
				j++
			}
			runLen := j - i
			if i > 0 {
				buf = append(buf, '_')
			}
			if runLen == 1 || j == n {
				// Single uppercase or uppercase run at end: "Is" → "is", "ID" → "id"
				for k := i; k < j; k++ {
					buf = append(buf, runes[k]-'A'+'a')
				}
			} else {
				// Uppercase run followed by lowercase: "HTTPStatus" → "http_status"
				for k := i; k < j-1; k++ {
					buf = append(buf, runes[k]-'A'+'a')
				}
				buf = append(buf, '_')
				buf = append(buf, runes[j-1]-'A'+'a')
			}
			i = j - 1
		} else {
			buf = append(buf, r)
		}
	}
	return string(buf)
}

// Result is the unified success response envelope.
type Result[T any] struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

// ErrorBody is the unified error response envelope.
type ErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// OK writes a 200 success response.
func OK[T any](w http.ResponseWriter, data T) {
	writeJSON(w, http.StatusOK, Result[T]{Code: 0, Message: "ok", Data: data})
}

// Created writes a 201 success response.
func Created[T any](w http.ResponseWriter, data T) {
	writeJSON(w, http.StatusCreated, Result[T]{Code: 0, Message: "created", Data: data})
}

// Error resolves an error to a bizcode and writes the error response.
func Error(w http.ResponseWriter, err error) {
	ae := resolveError(err)
	slog.Error("api error", "code", ae.Code, "message", ae.Message, "err", err)
	writeJSON(w, ae.HTTPStatus(), ErrorBody{
		Code:    ae.Code,
		Message: bizcode.DisplayMessage(ae.Code),
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

func ErrBadRequest(msg string) *bizcode.AppError  { return bizcode.New(10001, msg) }
func ErrForbidden(msg string) *bizcode.AppError    { return bizcode.New(10150, msg) }
func ErrNotFound(msg string) *bizcode.AppError     { return bizcode.New(10201, msg) }
func ErrConflict(msg string) *bizcode.AppError     { return bizcode.New(10901, msg) }
func ErrInternal(msg string) *bizcode.AppError     { return bizcode.New(19999, msg) }

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
	case errors.Is(err, core.ErrInvalidInput):
		return bizcode.Wrap(10001, "invalid input", err)
	case errors.Is(err, core.ErrConflict):
		return bizcode.Wrap(10901, "conflict", err)
	default:
		return bizcode.Wrap(19999, "internal error", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"code":19999,"message":"internal error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(data)
}
