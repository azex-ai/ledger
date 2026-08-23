package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/server"
)

// --- Mock DepositReviewer ---

type mockDepositReviewer struct {
	listFn    func(ctx context.Context, cursor string, limit int32) ([]core.Booking, string, error)
	approveFn func(ctx context.Context, bookingUID, actor string) (*core.Booking, error)
	rejectFn  func(ctx context.Context, bookingUID, actor, reason string) (*core.Booking, error)
}

func (m *mockDepositReviewer) ListReviews(ctx context.Context, cursor string, limit int32) ([]core.Booking, string, error) {
	return m.listFn(ctx, cursor, limit)
}

func (m *mockDepositReviewer) ApproveReview(ctx context.Context, bookingUID, actor string) (*core.Booking, error) {
	return m.approveFn(ctx, bookingUID, actor)
}

func (m *mockDepositReviewer) RejectReview(ctx context.Context, bookingUID, actor, reason string) (*core.Booking, error) {
	return m.rejectFn(ctx, bookingUID, actor, reason)
}

func reviewBooking(uid string) *core.Booking {
	return &core.Booking{
		UID:               uid,
		ClassificationUID: "class-deposit",
		AccountHolder:     1001,
		CurrencyUID:       "cur-usdt",
		Amount:            decimal.RequireFromString("150"),
		SettledAmount:     decimal.Zero,
		Status:            "review",
		ChannelName:       "onchain",
		ChannelRef:        "0xdeadbeef#0",
		IdempotencyKey:    "deposit-1-0xdeadbeef-0",
		Metadata:          map[string]string{"review_reason": "over_ceiling"},
		CreatedAt:         time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC),
		UpdatedAt:         time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC),
	}
}

func TestListDepositReviews(t *testing.T) {
	srv := newTestServer()
	srv.SetDepositReviewer(&mockDepositReviewer{
		listFn: func(ctx context.Context, cursor string, limit int32) ([]core.Booking, string, error) {
			assert.Equal(t, int32(50), limit)
			return []core.Booking{*reviewBooking("booking-1")}, "", nil
		},
	})

	w := doRequest(srv, http.MethodGet, "/api/v1/deposits/reviews", nil)
	require.Equal(t, http.StatusOK, w.Code)

	list := parseEnvelopeList(t, w.Body.Bytes())
	require.Len(t, list, 1)
	item := list[0].(map[string]any)
	assert.Equal(t, "booking-1", item["uid"])
	assert.Equal(t, "review", item["status"])
	assert.Equal(t, "150", item["amount"])
	assert.Equal(t, "over_ceiling", item["metadata"].(map[string]any)["review_reason"])

	// Internal identifiers/derivation fingerprints never leak onto this
	// surface (user-facing-surfaces.md).
	assert.NotContains(t, item, "factory")
	assert.NotContains(t, item, "init_hash")
}

func TestListDepositReviews_NotEnabled(t *testing.T) {
	srv := newTestServer() // SetDepositReviewer never called
	w := doRequest(srv, http.MethodGet, "/api/v1/deposits/reviews", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestApproveDepositReview(t *testing.T) {
	srv := newTestServer()
	approved := reviewBooking("booking-1")
	approved.Status = "confirmed"
	approved.JournalUID = "journal-1"

	calls := 0
	var gotActor string
	srv.SetDepositReviewer(&mockDepositReviewer{
		approveFn: func(ctx context.Context, bookingUID, actor string) (*core.Booking, error) {
			calls++
			assert.Equal(t, "booking-1", bookingUID)
			gotActor = actor
			return approved, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/deposits/booking-1/review/approve", nil)
	require.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "confirmed", data["status"])
	assert.Equal(t, "journal-1", data["journal_uid"])
	assert.Equal(t, 1, calls)
	// MJ2: no API key configured in this test server -- the defensive
	// fallback records "unknown" rather than an empty/missing actor.
	assert.Equal(t, "unknown", gotActor)
}

// TestApproveDepositReview_ForwardsAuthenticatedActor pins MJ2: when an API
// key is configured and the request authenticates, the key's Name is
// forwarded as the approving actor (audit attribution for the deposit
// path's highest-privilege action). The key holds CapabilityDepositReview
// alongside ScopeWrite (W3-A, mi2): approve/reject requires the capability
// regardless of scope, and this test's subject is actor forwarding, not
// duty separation -- TestApproveDepositReview_RequiresCapability below pins
// that ScopeWrite alone is no longer sufficient.
func TestApproveDepositReview_ForwardsAuthenticatedActor(t *testing.T) {
	srv := newScopedTestServer(server.APIKey{Name: "ops-carol", Scope: server.ScopeWrite, Capabilities: server.CapabilityDepositReview, Secret: []byte("secret-1")})
	approved := reviewBooking("booking-1")
	approved.Status = "confirmed"
	approved.JournalUID = "journal-1"

	var gotActor string
	srv.SetDepositReviewer(&mockDepositReviewer{
		approveFn: func(ctx context.Context, bookingUID, actor string) (*core.Booking, error) {
			gotActor = actor
			return approved, nil
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/deposits/booking-1/review/approve", nil)
	req.Header.Set("Authorization", "Bearer secret-1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ops-carol", gotActor)
}

func TestApproveDepositReview_Conflict(t *testing.T) {
	srv := newTestServer()
	srv.SetDepositReviewer(&mockDepositReviewer{
		approveFn: func(ctx context.Context, bookingUID, actor string) (*core.Booking, error) {
			return nil, core.ErrConflict
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/deposits/booking-1/review/approve", nil)
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestApproveDepositReview_NotEnabled(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodPost, "/api/v1/deposits/booking-1/review/approve", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestRejectDepositReview(t *testing.T) {
	srv := newTestServer()
	rejected := reviewBooking("booking-1")
	rejected.Status = "failed"
	rejected.Metadata = map[string]string{"reject_reason": "suspected fraud"}

	var gotReason, gotActor string
	srv.SetDepositReviewer(&mockDepositReviewer{
		rejectFn: func(ctx context.Context, bookingUID, actor, reason string) (*core.Booking, error) {
			assert.Equal(t, "booking-1", bookingUID)
			gotReason = reason
			gotActor = actor
			return rejected, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/deposits/booking-1/review/reject", map[string]string{"reason": "suspected fraud"})
	require.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "failed", data["status"])
	assert.Empty(t, data["journal_uid"], "reject must never carry a journal_uid (I-21)")
	assert.Equal(t, "suspected fraud", gotReason)
	// MJ2: no API key configured in this test server -- the defensive
	// fallback records "unknown" rather than an empty/missing actor.
	assert.Equal(t, "unknown", gotActor)
}

// TestRejectDepositReview_ForwardsAuthenticatedActor pins MJ2's reject half:
// the authenticated API key's Name is forwarded as the rejecting actor. The
// key holds CapabilityDepositReview alongside ScopeWrite (W3-A, mi2) --
// same rationale as TestApproveDepositReview_ForwardsAuthenticatedActor.
func TestRejectDepositReview_ForwardsAuthenticatedActor(t *testing.T) {
	srv := newScopedTestServer(server.APIKey{Name: "ops-dave", Scope: server.ScopeWrite, Capabilities: server.CapabilityDepositReview, Secret: []byte("secret-2")})
	rejected := reviewBooking("booking-1")
	rejected.Status = "failed"

	var gotActor string
	srv.SetDepositReviewer(&mockDepositReviewer{
		rejectFn: func(ctx context.Context, bookingUID, actor, reason string) (*core.Booking, error) {
			gotActor = actor
			return rejected, nil
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/deposits/booking-1/review/reject", jsonBody(t, map[string]string{"reason": "n/a"}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-2")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ops-dave", gotActor)
}

func TestRejectDepositReview_MissingReason(t *testing.T) {
	srv := newTestServer()
	srv.SetDepositReviewer(&mockDepositReviewer{
		rejectFn: func(ctx context.Context, bookingUID, actor, reason string) (*core.Booking, error) {
			t.Fatal("must not reach the reviewer without a reason")
			return nil, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/deposits/booking-1/review/reject", map[string]string{"reason": ""})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRejectDepositReview_Conflict(t *testing.T) {
	srv := newTestServer()
	srv.SetDepositReviewer(&mockDepositReviewer{
		rejectFn: func(ctx context.Context, bookingUID, actor, reason string) (*core.Booking, error) {
			return nil, core.ErrConflict
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/deposits/booking-1/review/reject", map[string]string{"reason": "n/a"})
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestRejectDepositReview_NotEnabled(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodPost, "/api/v1/deposits/booking-1/review/reject", map[string]string{"reason": "n/a"})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestDepositReview_SelfMintSelfApprove_MI2 pins the W3-A fix for mi2
// (docs/bugs/2026-07-11-m3-security-review.md): before this fix, a single
// ScopeWrite key could forge a deposit booking (POST /bookings), drive it
// through the lifecycle (POST /bookings/{uid}/transition), and approve its
// own review (POST /deposits/{uid}/review/approve) -- all three endpoints
// sat in the same ScopeWrite route group, so nothing separated "the key
// that ingests deposits" from "the key that approves them", defeating the
// entire point of the M3 review gate (design doc §9.2/§9.4).
//
// This test exercises the full chain end-to-end with ONE key holding only
// ScopeWrite: the create+transition half ("造") must still succeed --
// ScopeWrite legitimately grants those two endpoints, and this fix does not
// touch them -- but the approve half ("批") must now be rejected with 403,
// because CapabilityDepositReview is independent of, and not implied by,
// ScopeWrite (or even ScopeAdmin -- see the second sub-test).
func TestDepositReview_SelfMintSelfApprove_MI2(t *testing.T) {
	t.Run("scope write alone can self-mint but not self-approve", func(t *testing.T) {
		srv := newScopedTestServer(server.APIKey{Name: "attacker", Scope: server.ScopeWrite, Secret: []byte("attacker-secret")})
		srv.SetDepositReviewer(&mockDepositReviewer{
			approveFn: func(ctx context.Context, bookingUID, actor string) (*core.Booking, error) {
				t.Fatal("must never reach the reviewer: CapabilityDepositReview was not granted")
				return nil, nil
			},
		})

		// "造": create an arbitrary, self-declared over-ceiling deposit
		// booking with the attacker's ScopeWrite key -- unaffected by this
		// fix, ScopeWrite legitimately grants this.
		w := doAuthedRequest(t, srv, http.MethodPost, "/api/v1/bookings", "attacker-secret", map[string]any{
			"classification_code": "deposit",
			"account_holder":      100,
			"currency_uid":        "cur-1",
			"amount":              "1000000.00",
			"channel_name":        "onchain",
			"idempotency_key":     "mi2-self-mint",
		})
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		created := parseEnvelope(t, w.Body.Bytes())
		bookingUID := created["uid"].(string)

		// drive it to "review" with the same key -- also unaffected.
		w = doAuthedRequest(t, srv, http.MethodPost, "/api/v1/bookings/"+bookingUID+"/transition", "attacker-secret", map[string]any{
			"to_status":   "review",
			"channel_ref": "0xselfmint#0",
		})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		// "批": the SAME key attempts to approve its own review -- this is
		// the fix. Must be rejected, and the reviewer must never be called.
		w = doAuthedRequest(t, srv, http.MethodPost, "/api/v1/deposits/"+bookingUID+"/review/approve", "attacker-secret", nil)
		assert.Equal(t, http.StatusForbidden, w.Code, "a ScopeWrite-only key must not be able to approve its own forged review (mi2)")
	})

	t.Run("scope admin alone also cannot approve without the capability", func(t *testing.T) {
		srv := newScopedTestServer(server.APIKey{Name: "ops", Scope: server.ScopeAdmin, Secret: []byte("admin-secret")})
		srv.SetDepositReviewer(&mockDepositReviewer{
			approveFn: func(ctx context.Context, bookingUID, actor string) (*core.Booking, error) {
				t.Fatal("must never reach the reviewer: CapabilityDepositReview is not implied by any Scope, including admin")
				return nil, nil
			},
		})

		w := doAuthedRequest(t, srv, http.MethodPost, "/api/v1/deposits/booking-1/review/approve", "admin-secret", nil)
		assert.Equal(t, http.StatusForbidden, w.Code, "ScopeAdmin must not imply CapabilityDepositReview")
	})

	t.Run("the capability, once explicitly granted, allows approval", func(t *testing.T) {
		srv := newScopedTestServer(server.APIKey{Name: "reviewer", Scope: server.ScopeRead, Capabilities: server.CapabilityDepositReview, Secret: []byte("reviewer-secret")})
		approved := reviewBooking("booking-1")
		approved.Status = "confirmed"
		called := false
		srv.SetDepositReviewer(&mockDepositReviewer{
			approveFn: func(ctx context.Context, bookingUID, actor string) (*core.Booking, error) {
				called = true
				return approved, nil
			},
		})

		w := doAuthedRequest(t, srv, http.MethodPost, "/api/v1/deposits/booking-1/review/approve", "reviewer-secret", nil)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.True(t, called, "a key holding CapabilityDepositReview -- even with only ScopeRead, not ScopeWrite -- must be able to approve")
	})
}
