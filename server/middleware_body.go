// Package server: middleware_body.go
// Caps inbound request bodies via http.MaxBytesReader to prevent memory exhaustion.
package server

import "net/http"

// bodyLimitMiddleware wraps r.Body in http.MaxBytesReader so any subsequent
// io.ReadAll / json.Decode bails out cleanly once the limit is exceeded.
// maxBytes <= 0 disables the limiter (test convenience only).
func bodyLimitMiddleware(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if maxBytes > 0 && r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}
