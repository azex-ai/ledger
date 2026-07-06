package server

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// mustCIDRs parses trusted-proxy CIDRs for tests, failing fast on a bad spec.
func mustCIDRs(t *testing.T, spec string) []netip.Prefix {
	t.Helper()
	p, err := parseTrustedProxyCIDRs(spec)
	if err != nil {
		t.Fatalf("parseTrustedProxyCIDRs(%q): %v", spec, err)
	}
	return p
}

func TestParseTrustedProxyCIDRs(t *testing.T) {
	t.Run("empty yields nil", func(t *testing.T) {
		p, err := parseTrustedProxyCIDRs("   ")
		if err != nil || p != nil {
			t.Fatalf("want nil,nil; got %v,%v", p, err)
		}
	})

	t.Run("mixed CIDR and bare IP", func(t *testing.T) {
		p, err := parseTrustedProxyCIDRs("10.0.0.0/8, 172.16.0.0/12 ,192.168.1.1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(p) != 3 {
			t.Fatalf("want 3 prefixes, got %d: %v", len(p), p)
		}
		// bare IP becomes a host route (/32).
		if p[2].Bits() != 32 {
			t.Errorf("bare IP prefix bits = %d, want 32", p[2].Bits())
		}
	})

	t.Run("IPv6 CIDR", func(t *testing.T) {
		p, err := parseTrustedProxyCIDRs("2001:db8::/32")
		if err != nil || len(p) != 1 || p[0].Bits() != 32 {
			t.Fatalf("IPv6 CIDR parse = %v, %v", p, err)
		}
	})

	t.Run("invalid CIDR is a hard error", func(t *testing.T) {
		if _, err := parseTrustedProxyCIDRs("10.0.0.0/8,not-a-cidr"); err == nil {
			t.Fatal("want error for invalid CIDR token")
		}
	})

	t.Run("host bits are normalized away", func(t *testing.T) {
		// 10.1.2.3/8 has host bits set; Masked() must fold them so Contains works.
		p := mustCIDRs(t, "10.1.2.3/8")
		if !p[0].Contains(netip.MustParseAddr("10.255.255.1")) {
			t.Error("expected 10.255.255.1 to be inside a normalized 10.0.0.0/8")
		}
	})
}

// callRealIP runs realIPFromHeaders with the given socket peer and headers.
func callRealIP(peer string, trusted []netip.Prefix, headers map[string][]string) string {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = peer
	r.Header = http.Header{}
	for k, vs := range headers {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	return realIPFromHeaders(r, trusted)
}

func TestRealIPFromHeaders(t *testing.T) {
	trusted := mustCIDRs(t, "10.0.0.0/8")

	t.Run("untrusted peer: headers ignored entirely", func(t *testing.T) {
		// finding #8 / max#1 core: a direct caller (peer outside trusted CIDRs)
		// cannot spoof its IP via any header — result is "" (keep socket peer).
		got := callRealIP("203.0.113.9:5555", trusted, map[string][]string{
			"X-Forwarded-For": {"1.2.3.4"},
			"X-Real-IP":       {"5.6.7.8"},
			"True-Client-IP":  {"9.9.9.9"},
		})
		if got != "" {
			t.Fatalf("untrusted peer should ignore headers, got %q", got)
		}
	})

	t.Run("trusted peer: XFF client resolved", func(t *testing.T) {
		got := callRealIP("10.0.0.1:5555", trusted, map[string][]string{
			"X-Forwarded-For": {"203.0.113.7"},
		})
		if got != "203.0.113.7" {
			t.Fatalf("want 203.0.113.7, got %q", got)
		}
	})

	t.Run("finding #1: forged X-Real-IP does not beat proxy XFF", func(t *testing.T) {
		// Behind an append-only proxy (ALB/nginx) the client pre-sends X-Real-IP;
		// XFF (walked) must win, not the client-controlled X-Real-IP.
		got := callRealIP("10.0.0.1:5555", trusted, map[string][]string{
			"X-Forwarded-For": {"203.0.113.7"},
			"X-Real-IP":       {"1.2.3.4"}, // attacker-forged
		})
		if got != "203.0.113.7" {
			t.Fatalf("XFF must win over forged X-Real-IP, got %q", got)
		}
	})

	t.Run("finding #3: multi-hop chain skips trusted proxies", func(t *testing.T) {
		// CDN(untrusted client) -> proxy A(10.0.0.2) -> proxy B(10.0.0.1). Both
		// proxies are trusted; the first untrusted hop from the right is the client.
		got := callRealIP("10.0.0.1:5555", trusted, map[string][]string{
			"X-Forwarded-For": {"198.51.100.5, 10.0.0.2, 10.0.0.1"},
		})
		if got != "198.51.100.5" {
			t.Fatalf("want real client 198.51.100.5, got %q", got)
		}
	})

	t.Run("finding #5: trailing comma / empty hop skipped", func(t *testing.T) {
		got := callRealIP("10.0.0.1:5555", trusted, map[string][]string{
			"X-Forwarded-For": {"203.0.113.7,"},
		})
		if got != "203.0.113.7" {
			t.Fatalf("empty last hop must fall back to prev valid hop, got %q", got)
		}
	})

	t.Run("max#3: multiple XFF header lines all considered", func(t *testing.T) {
		// A proxy that Adds its hop as a separate header line rather than
		// comma-joining. Header.Values (not Get) must see both lines.
		got := callRealIP("10.0.0.1:5555", trusted, map[string][]string{
			"X-Forwarded-For": {"203.0.113.7", "10.0.0.2"},
		})
		if got != "203.0.113.7" {
			t.Fatalf("want client from first line, got %q", got)
		}
	})

	t.Run("finding #2/max#1: garbage header value rejected", func(t *testing.T) {
		// Non-IP junk must never reach r.RemoteAddr — keep socket peer ("").
		got := callRealIP("10.0.0.1:5555", trusted, map[string][]string{
			"X-Forwarded-For": {"not-an-ip-just-attacker-garbage"},
		})
		if got != "" {
			t.Fatalf("garbage XFF must be rejected, got %q", got)
		}
	})

	t.Run("finding #7: True-Client-IP fallback when no XFF", func(t *testing.T) {
		got := callRealIP("10.0.0.1:5555", trusted, map[string][]string{
			"True-Client-IP": {"198.51.100.9"},
		})
		if got != "198.51.100.9" {
			t.Fatalf("want True-Client-IP fallback, got %q", got)
		}
	})

	t.Run("X-Real-IP fallback when no XFF", func(t *testing.T) {
		got := callRealIP("10.0.0.1:5555", trusted, map[string][]string{
			"X-Real-IP": {"198.51.100.10"},
		})
		if got != "198.51.100.10" {
			t.Fatalf("want X-Real-IP fallback, got %q", got)
		}
	})

	t.Run("all hops trusted, no fallback: keep socket peer", func(t *testing.T) {
		got := callRealIP("10.0.0.1:5555", trusted, map[string][]string{
			"X-Forwarded-For": {"10.0.0.2, 10.0.0.1"},
		})
		if got != "" {
			t.Fatalf("all-trusted chain with no client hop should keep peer, got %q", got)
		}
	})

	t.Run("IPv6 peer and client", func(t *testing.T) {
		v6trusted := mustCIDRs(t, "2001:db8::/32")
		got := callRealIP("[2001:db8::1]:5555", v6trusted, map[string][]string{
			"X-Forwarded-For": {"2606:4700::1234"},
		})
		if got != "2606:4700::1234" {
			t.Fatalf("want IPv6 client, got %q", got)
		}
	})

	t.Run("no trusted CIDRs: never trusts headers", func(t *testing.T) {
		got := callRealIP("10.0.0.1:5555", nil, map[string][]string{
			"X-Forwarded-For": {"203.0.113.7"},
		})
		if got != "" {
			t.Fatalf("nil trusted set must ignore headers, got %q", got)
		}
	})
}

// TestTrustedProxyRealIPMiddleware exercises the middleware end-to-end,
// confirming r.RemoteAddr is (or is not) rewritten before downstream handlers.
func TestTrustedProxyRealIPMiddleware(t *testing.T) {
	trusted := mustCIDRs(t, "10.0.0.0/8")
	mw := trustedProxyRealIP(trusted)

	run := func(peer string, headers map[string]string) string {
		var seen string
		h := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = r.RemoteAddr
		}))
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.RemoteAddr = peer
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		return seen
	}

	t.Run("trusted peer rewrites to client IP", func(t *testing.T) {
		if got := run("10.0.0.1:9999", map[string]string{"X-Forwarded-For": "203.0.113.7"}); got != "203.0.113.7" {
			t.Fatalf("want rewrite to 203.0.113.7, got %q", got)
		}
	})

	t.Run("untrusted peer keeps original RemoteAddr", func(t *testing.T) {
		if got := run("203.0.113.9:9999", map[string]string{"X-Forwarded-For": "1.2.3.4"}); got != "203.0.113.9:9999" {
			t.Fatalf("untrusted peer must keep socket peer, got %q", got)
		}
	})
}
