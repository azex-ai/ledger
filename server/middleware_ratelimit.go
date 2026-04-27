// Package server: middleware_ratelimit.go
// In-memory per-IP token-bucket rate limiter (single-instance only).
package server

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/azex-ai/ledger/pkg/bizcode"
	"github.com/azex-ai/ledger/pkg/httpx"
)

// rateLimiterConfig configures per-IP rate limits. mutationPerMin applies to
// POST/PUT/PATCH/DELETE; readPerMin applies to GET/HEAD/OPTIONS.
type rateLimiterConfig struct {
	mutationPerMin int
	readPerMin     int
	gcInterval     time.Duration
	idleTimeout    time.Duration
}

// defaultRateLimiterConfig returns the production defaults: 100/min mutations,
// 1000/min reads. Buckets idle > 10m are GC'd every 5m to bound memory.
func defaultRateLimiterConfig() rateLimiterConfig {
	return rateLimiterConfig{
		mutationPerMin: 100,
		readPerMin:     1000,
		gcInterval:     5 * time.Minute,
		idleTimeout:    10 * time.Minute,
	}
}

// ipBucket holds a token-bucket limiter plus last-seen timestamp for GC.
type ipBucket struct {
	mutate   *rate.Limiter
	read     *rate.Limiter
	lastSeen time.Time
}

// rateLimiter is a single-process in-memory IP-based limiter. Fine for
// single-instance deployments; replace with Redis-backed token bucket for HA.
type rateLimiter struct {
	cfg     rateLimiterConfig
	mu      sync.Mutex
	buckets map[string]*ipBucket
}

func newRateLimiter(cfg rateLimiterConfig) *rateLimiter {
	return &rateLimiter{
		cfg:     cfg,
		buckets: make(map[string]*ipBucket),
	}
}

// Run starts the periodic GC loop until ctx is done.
func (rl *rateLimiter) gcLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(rl.cfg.gcInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			rl.gc(now)
		}
	}
}

func (rl *rateLimiter) gc(now time.Time) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for ip, b := range rl.buckets {
		if now.Sub(b.lastSeen) > rl.cfg.idleTimeout {
			delete(rl.buckets, ip)
		}
	}
}

// limiterFor returns the appropriate limiter for the request method.
func (rl *rateLimiter) limiterFor(ip, method string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[ip]
	if !ok {
		// rate.Every(d) means 1 token per d. Burst = full per-minute allowance.
		mutate := rate.NewLimiter(rate.Every(time.Minute/time.Duration(rl.cfg.mutationPerMin)), rl.cfg.mutationPerMin)
		read := rate.NewLimiter(rate.Every(time.Minute/time.Duration(rl.cfg.readPerMin)), rl.cfg.readPerMin)
		b = &ipBucket{mutate: mutate, read: read}
		rl.buckets[ip] = b
	}
	b.lastSeen = time.Now()

	if isMutating(method) {
		return b.mutate
	}
	return b.read
}

// rateLimitMiddleware enforces per-IP token-bucket limits.
// 100/min mutations, 1000/min reads. Single-instance only.
func rateLimitMiddleware(rl *rateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			lim := rl.limiterFor(ip, r.Method)
			if !lim.Allow() {
				w.Header().Set("Retry-After", "60")
				httpx.Error(w, bizcode.New(10401, "rate limit exceeded"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP extracts the request peer IP. RemoteAddr already accounts for the
// chi RealIP middleware (X-Forwarded-For / X-Real-IP) when configured upstream.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
