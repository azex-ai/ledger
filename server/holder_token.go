// Package server: holder_token.go
// Holder-scoped read tokens for the wallet surface
// (docs/plans/2026-07-08-holder-scoped-wallet-surface.md §3.1).
//
// A holder token is a stateless HMAC-signed credential bound to ONE account
// holder (it only ever authenticates the /holder/* endpoints, and only that
// holder's own data -- the holder is never a request parameter). It shares
// the Authorization: Bearer header with API keys and is disambiguated by the
// "lht_" prefix. Blast radius of a leak: one holder's balances/
// transactions/holds, plus the ability to (re-)issue that same holder's own
// CREATE2 deposit address -- never funds, never another holder -- until exp.
//
// Format: lht_<base64url(payload)>.<base64url(HMAC-SHA256(payload))>
// payload = {"holder":N,"iat":unix,"exp":unix}. Deliberately not JWT — no
// algorithm negotiation surface, one fixed construction.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/azex-ai/ledger/pkg/bizcode"
	"github.com/azex-ai/ledger/pkg/httpx"
)

// HolderTokenPrefix distinguishes holder tokens from API keys in the shared
// Authorization: Bearer header.
const HolderTokenPrefix = "lht_"

const (
	defaultHolderTokenTTL = 15 * time.Minute
	defaultHolderTokenMax = time.Hour
	minHolderSecretLen    = 32
)

type holderTokenPayload struct {
	Holder int64 `json:"holder"`
	Iat    int64 `json:"iat"`
	Exp    int64 `json:"exp"`
}

// MintHolderToken signs a read-only token bound to holder, valid for ttl
// (clamped to sane bounds by the callers that expose it over HTTP). Library
// hosts embedding HolderHandler call this in-process after their own session
// auth — no HTTP mint hop needed.
func MintHolderToken(secret []byte, holder int64, ttl time.Duration, now time.Time) (string, error) {
	if len(secret) < minHolderSecretLen {
		return "", fmt.Errorf("server: holder token secret must be at least %d bytes", minHolderSecretLen)
	}
	if holder == 0 {
		return "", fmt.Errorf("server: holder token requires a non-zero holder")
	}
	if ttl <= 0 {
		ttl = defaultHolderTokenTTL
	}
	payload, err := json.Marshal(holderTokenPayload{
		Holder: holder,
		Iat:    now.Unix(),
		Exp:    now.Add(ttl).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("server: marshal holder token payload: %w", err)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return HolderTokenPrefix +
		base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// verifyHolderToken checks signature and expiry, returning the bound holder.
func verifyHolderToken(secret []byte, token string, now time.Time) (int64, error) {
	raw, ok := strings.CutPrefix(token, HolderTokenPrefix)
	if !ok {
		return 0, fmt.Errorf("server: not a holder token")
	}
	payloadB64, sigB64, ok := strings.Cut(raw, ".")
	if !ok {
		return 0, fmt.Errorf("server: malformed holder token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return 0, fmt.Errorf("server: malformed holder token payload")
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return 0, fmt.Errorf("server: malformed holder token signature")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return 0, fmt.Errorf("server: holder token signature mismatch")
	}
	var p holderTokenPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return 0, fmt.Errorf("server: malformed holder token payload")
	}
	if p.Holder == 0 {
		return 0, fmt.Errorf("server: holder token bound to no holder")
	}
	if now.Unix() >= p.Exp {
		return 0, fmt.Errorf("server: holder token expired")
	}
	return p.Holder, nil
}

// holderCtxKey carries the authenticated holder id.
type holderCtxKey struct{}

// holderFrom extracts the token-authenticated holder, if any.
func holderFrom(ctx context.Context) (int64, bool) {
	h, ok := ctx.Value(holderCtxKey{}).(int64)
	return h, ok
}

// holderAuthMiddleware authenticates requests with a holder token and stores
// the bound holder in the context. It rejects API keys and everything else:
// the holder surface is reachable ONLY with a holder-scoped credential.
func holderAuthMiddleware(secret []byte, now func() time.Time) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided, ok := extractBearer(r.Header.Get("Authorization"))
			if !ok {
				httpx.Error(w, bizcode.New(10101, "missing or malformed Authorization header"))
				return
			}
			holder, err := verifyHolderToken(secret, string(provided), now())
			if err != nil {
				// Uniform message: no oracle for expired-vs-forged.
				httpx.Error(w, bizcode.New(10101, "invalid holder token"))
				return
			}
			if h, hok := authLogFrom(r.Context()); hok {
				h.name, h.scope = fmt.Sprintf("holder:%d", holder), "holder"
			}
			ctx := context.WithValue(r.Context(), holderCtxKey{}, holder)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
