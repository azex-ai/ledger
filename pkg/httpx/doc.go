// Package httpx is the shared HTTP plumbing used by every handler in
// server/. It standardises:
//
//   - Result[T] / ErrorBody envelopes ({code, message, data} on success,
//     {code, message} on error).
//   - OK / Created / Error helpers that write the envelope with the
//     correct status code.
//   - Decode[T] for JSON request bodies, including snake_case <-> Go
//     field-name mapping via a custom jsoniter naming strategy.
//   - resolveError, which maps core.* sentinel errors to bizcode.AppError
//     so HTTP status derivation is consistent across handlers.
//
// Handlers are expected to return early via httpx.Error(w, err); they
// never set status codes or marshal bodies themselves.
package httpx
