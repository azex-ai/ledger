// Package server: middleware_realip.go
// Opt-in replacement for chi's deprecated middleware.RealIP. RealIP trusted
// X-Forwarded-For / X-Real-IP unconditionally, which lets any direct caller
// spoof its IP (GHSA-3fxj-6jh8-hvhx) — defeating the per-IP rate limiter and
// polluting access logs.
//
// Policy here:
//   - Default (no trusted CIDRs configured): never trust proxy headers.
//     r.RemoteAddr stays the socket peer.
//   - TRUSTED_PROXY_CIDRS set: rewrite r.RemoteAddr from proxy headers ONLY
//     when the socket peer is itself inside a trusted-proxy CIDR. The rewrite
//     walks X-Forwarded-For right-to-left, skipping hops that are themselves
//     trusted proxies, and takes the first remaining (untrusted) hop — that is
//     the real client. Every candidate is validated with netip.ParseAddr, so a
//     non-IP or client-forged garbage value can never reach the rate-limiter
//     key or the access log. When the peer is NOT a trusted proxy, headers are
//     ignored entirely — a direct caller cannot spoof its way past the limiter.
package server

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// trustedProxyRealIP rewrites r.RemoteAddr from proxy-set headers, but only for
// requests whose socket peer is inside one of the trusted CIDRs. Mount it only
// when len(trusted) > 0 (see Config.TrustedProxyCIDRs).
func trustedProxyRealIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ip := realIPFromHeaders(r, trusted); ip != "" {
				r.RemoteAddr = ip
			}
			next.ServeHTTP(w, r)
		})
	}
}

// realIPFromHeaders returns the real client IP derived from proxy headers, or
// "" to keep the socket peer. It returns "" unless the socket peer is a trusted
// proxy, so untrusted direct callers can never influence the result.
func realIPFromHeaders(r *http.Request, trusted []netip.Prefix) string {
	peer, err := netip.ParseAddr(hostOnly(r.RemoteAddr))
	if err != nil || !inTrusted(peer, trusted) {
		return ""
	}

	// X-Forwarded-For may arrive as multiple physical header lines and/or a
	// comma-joined list; Header.Values captures every line (Header.Get would
	// drop all but the first). The chain reads client, proxy1, proxy2, ... so
	// the rightmost hops are the ones our own proxies appended. Walk from the
	// right, skip hops that are themselves trusted proxies, and take the first
	// untrusted hop — that is the real client behind a chain of trusted proxies.
	var hops []string
	for _, line := range r.Header.Values("X-Forwarded-For") {
		for hop := range strings.SplitSeq(line, ",") {
			hops = append(hops, hop)
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			continue
		}
		if !inTrusted(addr, trusted) {
			return addr.String()
		}
	}

	// No untrusted X-Forwarded-For hop (missing header, or every hop was a
	// trusted proxy). Fall back to the single-value headers a proxy may set to
	// the real client, validated the same way.
	for _, h := range []string{"X-Real-IP", "True-Client-IP"} {
		if addr, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get(h))); err == nil {
			return addr.String()
		}
	}
	return ""
}

// hostOnly strips the port from a host:port address, returning the host as-is
// when there is no port (e.g. an already-bare IP).
func hostOnly(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// inTrusted reports whether addr falls inside any trusted-proxy CIDR. Both
// sides are unmapped so a 4-in-6 (::ffff:1.2.3.4) peer matches a plain IPv4
// prefix and vice versa.
func inTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	addr = addr.Unmap()
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// parseTrustedProxyCIDRs parses a comma-separated list of CIDR prefixes (e.g.
// "10.0.0.0/8, 172.16.0.0/12"). Empty input yields nil (headers never trusted).
// A bare IP is accepted as a /32 or /128. Masks are normalized to the prefix
// boundary so Contains works regardless of host bits in the input.
func parseTrustedProxyCIDRs(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for tok := range strings.SplitSeq(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if strings.Contains(tok, "/") {
			p, err := netip.ParsePrefix(tok)
			if err != nil {
				return nil, err
			}
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(tok)
		if err != nil {
			return nil, err
		}
		out = append(out, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
	}
	return out, nil
}
