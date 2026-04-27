// Package bizcode is the canonical business-error registry. Every
// error surfaced over HTTP carries a numeric Code; the HTTP status is
// derived from the code's range (10000-10099 -> 400, 14000-14999 -> 422,
// etc., see AppError.HTTPStatus).
//
// AppError satisfies the standard error interface and supports
// errors.Is / errors.As, so handlers can wrap a core sentinel and
// callers further up the stack can still match on the bizcode.
//
// DisplayMessage maps a code to a stable user-facing string. The wire
// "message" field always uses the registered display message, never the
// internal reason -- this keeps internals out of API responses.
package bizcode
