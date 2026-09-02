// Package server: middleware_logger.go
// Structured request logger that strips query strings to avoid leaking PII.
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// authLogHolder is a mutable cell the logger plants in the request context
// before the auth middleware runs; auth fills in the matched key's name and
// scope so the access log line can attribute the request to a caller. The
// pointer indirection is required because the logger wraps auth (outer
// middleware cannot see context values added by inner ones).
type authLogHolder struct {
	name  string
	scope string
}

type authLogCtxKey struct{}

func authLogFrom(ctx context.Context) (*authLogHolder, bool) {
	h, ok := ctx.Value(authLogCtxKey{}).(*authLogHolder)
	return h, ok
}

// requestLoggerMiddleware logs each request through the injected
// Config.Logger at info level (I-N15: it used to call the package-level
// slog, so a consumer had no way to route or format the HTTP layer's logs
// even though every other layer of this library takes a core.Logger).
//
// Query strings are intentionally dropped — they may contain holder IDs,
// idempotency keys, or other sensitive identifiers we don't want in logs.
// Included: status, duration, request ID, method, path, remote IP, and the
// authenticated API key's name + scope (never the secret). The api_key
// fields are deliberate: attributing a write to a caller is the point of an
// audit log. A holder ID is NOT deliberate anywhere in this package's logs —
// that is the identifier the query-string rule above exists to keep out.
func (s *Server) requestLoggerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		holder := &authLogHolder{}
		r = r.WithContext(context.WithValue(r.Context(), authLogCtxKey{}, holder))
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", clientIP(r),
			"request_id", middleware.GetReqID(r.Context()),
		}
		if holder.name != "" {
			attrs = append(attrs, "api_key", holder.name, "api_key_scope", holder.scope)
		}
		s.logger.Info("http request", attrs...)
	})
}
