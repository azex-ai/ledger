package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// --- Mock DepositReviewer ---

type mockDepositReviewer struct {
	listFn    func(ctx context.Context, cursor string, limit int32) ([]core.Booking, string, error)
	approveFn func(ctx context.Context, bookingUID string) (*core.Booking, error)
	rejectFn  func(ctx context.Context, bookingUID, reason string) (*core.Booking, error)
}

func (m *mockDepositReviewer) ListReviews(ctx context.Context, cursor string, limit int32) ([]core.Booking, string, error) {
	return m.listFn(ctx, cursor, limit)
}

func (m *mockDepositReviewer) ApproveReview(ctx context.Context, bookingUID string) (*core.Booking, error) {
	return m.approveFn(ctx, bookingUID)
}

func (m *mockDepositReviewer) RejectReview(ctx context.Context, bookingUID, reason string) (*core.Booking, error) {
	return m.rejectFn(ctx, bookingUID, reason)
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
	srv.SetDepositReviewer(&mockDepositReviewer{
		approveFn: func(ctx context.Context, bookingUID string) (*core.Booking, error) {
			calls++
			assert.Equal(t, "booking-1", bookingUID)
			return approved, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/deposits/booking-1/review/approve", nil)
	require.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "confirmed", data["status"])
	assert.Equal(t, "journal-1", data["journal_uid"])
	assert.Equal(t, 1, calls)
}

func TestApproveDepositReview_Conflict(t *testing.T) {
	srv := newTestServer()
	srv.SetDepositReviewer(&mockDepositReviewer{
		approveFn: func(ctx context.Context, bookingUID string) (*core.Booking, error) {
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

	var gotReason string
	srv.SetDepositReviewer(&mockDepositReviewer{
		rejectFn: func(ctx context.Context, bookingUID, reason string) (*core.Booking, error) {
			assert.Equal(t, "booking-1", bookingUID)
			gotReason = reason
			return rejected, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/deposits/booking-1/review/reject", map[string]string{"reason": "suspected fraud"})
	require.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "failed", data["status"])
	assert.Empty(t, data["journal_uid"], "reject must never carry a journal_uid (I-21)")
	assert.Equal(t, "suspected fraud", gotReason)
}

func TestRejectDepositReview_MissingReason(t *testing.T) {
	srv := newTestServer()
	srv.SetDepositReviewer(&mockDepositReviewer{
		rejectFn: func(ctx context.Context, bookingUID, reason string) (*core.Booking, error) {
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
		rejectFn: func(ctx context.Context, bookingUID, reason string) (*core.Booking, error) {
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
