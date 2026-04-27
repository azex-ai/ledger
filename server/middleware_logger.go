// Package server: middleware_logger.go
// Structured request logger that strips query strings to avoid leaking PII.
package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// requestLoggerMiddleware logs each request via slog at info level.
// Query strings are intentionally dropped — they may contain holder IDs,
// idempotency keys, or other sensitive identifiers we don't want in logs.
// Status, duration, request ID, method, path, and remote IP are included.
func requestLoggerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		slog.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", clientIP(r),
			"request_id", middleware.GetReqID(r.Context()),
		)
	})
}
