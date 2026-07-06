// Package server: middleware_realip.go
// Opt-in replacement for chi's deprecated middleware.RealIP. RealIP trusted
// X-Forwarded-For / X-Real-IP unconditionally, which lets any direct caller
// spoof its IP (GHSA-3fxj-6jh8-hvhx) — defeating the per-IP rate limiter and
// polluting access logs.
//
// Policy here:
//   - Default: never trust proxy headers. r.RemoteAddr is the socket peer.
//   - TRUST_PROXY_HEADERS=true (deployments where ledgerd is ONLY reachable
//     through an edge proxy that sets/overwrites these headers): use
//     X-Real-IP if present, else the LAST X-Forwarded-For hop — the one
//     appended by your own proxy. The leftmost hop is client-controlled and
//     must not be used for anything security-relevant.
package server

import (
	"net/http"
	"strings"
)

// trustedProxyRealIP rewrites r.RemoteAddr from proxy-set headers. Mount it
// only when the deployment guarantees every request traverses the trusted
// proxy (see Config.TrustProxyHeaders).
func trustedProxyRealIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := realIPFromHeaders(r); ip != "" {
			r.RemoteAddr = ip
		}
		next.ServeHTTP(w, r)
	})
}

func realIPFromHeaders(r *http.Request) string {
	if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
		return xrip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		hops := strings.Split(xff, ",")
		return strings.TrimSpace(hops[len(hops)-1])
	}
	return ""
}
