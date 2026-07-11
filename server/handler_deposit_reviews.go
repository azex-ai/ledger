package server

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/bizcode"
	"github.com/azex-ai/ledger/pkg/httpx"
)

// DepositReviewer is the human-review half of the crypto-deposit
// orchestration (service.Onchain -- design doc §9.4): listing deposit
// bookings parked in `review` status by the threshold gate / reconciliation
// compensating controls (§9.1/§9.3), and resolving them to confirmed
// (approve, posting the deposit's journal) or failed (reject, no journal
// ever posted). Optional: nil until SetDepositReviewer is called; all three
// /deposits/reviews* routes then answer bizcode.FeatureNotEnabled. Modeled
// after DepositAddressProvider's optional-dependency pattern (see
// handler_onchain.go).
type DepositReviewer interface {
	// ListReviews lists deposit bookings currently parked in review status,
	// oldest first, cursor-paginated ("" cursor = first page, "" next-cursor
	// = exhausted).
	ListReviews(ctx context.Context, cursor string, limit int32) (bookings []core.Booking, nextCursor string, err error)
	// ApproveReview approves a review-parked deposit booking, posting its
	// deposit_confirm journal. Idempotent: a no-op returning the current
	// booking if already confirmed; core.ErrConflict for any other non-review
	// status.
	ApproveReview(ctx context.Context, bookingUID string) (*core.Booking, error)
	// RejectReview rejects a review-parked deposit booking to failed -- no
	// journal is ever posted. Idempotent: a no-op returning the current
	// booking if already failed; core.ErrConflict for any other non-review
	// status.
	RejectReview(ctx context.Context, bookingUID, reason string) (*core.Booking, error)
}

// SetDepositReviewer installs the crypto-deposit human-review service. Pass
// nil (the default) to leave GET/POST /deposits/reviews* answering
// bizcode.FeatureNotEnabled.
func (s *Server) SetDepositReviewer(r DepositReviewer) { s.depositReviewer = r }

type rejectDepositReviewRequest struct {
	Reason string `json:"reason"`
}

// handleListDepositReviews lists deposit bookings currently parked in
// review, oldest first (design doc §9.4) -- the review queue an on-call
// operator works through (see docs/RUNBOOK.md).
func (s *Server) handleListDepositReviews(w http.ResponseWriter, r *http.Request) {
	if s.depositReviewer == nil {
		httpx.Error(w, bizcode.FeatureNotEnabled)
		return
	}

	cursor := r.URL.Query().Get("cursor")
	limit := parsePageLimit(r)

	bookings, nextCursor, err := s.depositReviewer.ListReviews(r.Context(), cursor, limit)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	resp := PagedResponse[bookingResponse]{
		List:       make([]bookingResponse, len(bookings)),
		NextCursor: nextCursor,
	}
	for i := range bookings {
		resp.List[i] = bookingToResponse(&bookings[i])
	}
	httpx.OK(w, resp)
}

// handleApproveDepositReview approves a review-parked deposit, posting its
// deposit_confirm journal (design doc §9.4).
func (s *Server) handleApproveDepositReview(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid deposit booking uid"))
		return
	}
	if s.depositReviewer == nil {
		httpx.Error(w, bizcode.FeatureNotEnabled)
		return
	}

	booking, err := s.depositReviewer.ApproveReview(r.Context(), uid)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, bookingToResponse(booking))
}

// handleRejectDepositReview rejects a review-parked deposit to failed --
// never posting a journal (design doc §9.4). reason is caller-supplied
// (operator-authored) free text recorded on the booking's audit trail and
// the emitted deposit.review_rejected signal; it is not derived from an
// internal error, so no further sanitization happens here.
func (s *Server) handleRejectDepositReview(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid deposit booking uid"))
		return
	}
	if s.depositReviewer == nil {
		httpx.Error(w, bizcode.FeatureNotEnabled)
		return
	}

	req, err := httpx.Decode[rejectDepositReviewRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if req.Reason == "" {
		httpx.Error(w, httpx.ErrBadRequest("reason is required"))
		return
	}

	booking, err := s.depositReviewer.RejectReview(r.Context(), uid, req.Reason)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, bookingToResponse(booking))
}
