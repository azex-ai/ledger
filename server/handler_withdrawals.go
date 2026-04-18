package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
)

type initWithdrawRequest struct {
	AccountHolder  int64             `json:"account_holder"`
	CurrencyID     int64             `json:"currency_id"`
	Amount         string            `json:"amount"`
	ChannelName    string            `json:"channel_name"`
	IdempotencyKey string            `json:"idempotency_key"`
	ReviewRequired bool              `json:"review_required"`
	Metadata       map[string]string `json:"metadata"`
	ExpiresAt      *string           `json:"expires_at,omitempty"`
}

type reviewWithdrawRequest struct {
	Approved bool `json:"approved"`
}

type processWithdrawRequest struct {
	ChannelRef string `json:"channel_ref"`
}

type failWithdrawRequest struct {
	Reason string `json:"reason"`
}

type withdrawalResponse struct {
	ID             int64             `json:"id"`
	AccountHolder  int64             `json:"account_holder"`
	CurrencyID     int64             `json:"currency_id"`
	Amount         string            `json:"amount"`
	Status         string            `json:"status"`
	ChannelName    string            `json:"channel_name"`
	ChannelRef     *string           `json:"channel_ref,omitempty"`
	ReservationID  *int64            `json:"reservation_id,omitempty"`
	JournalID      *int64            `json:"journal_id,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	ReviewRequired bool              `json:"review_required"`
	ExpiresAt      *time.Time        `json:"expires_at,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

func toWithdrawalResponse(w *core.Withdrawal) withdrawalResponse {
	return withdrawalResponse{
		ID:             w.ID,
		AccountHolder:  w.AccountHolder,
		CurrencyID:     w.CurrencyID,
		Amount:         w.Amount.String(),
		Status:         string(w.Status),
		ChannelName:    w.ChannelName,
		ChannelRef:     w.ChannelRef,
		ReservationID:  w.ReservationID,
		JournalID:      w.JournalID,
		IdempotencyKey: w.IdempotencyKey,
		Metadata:       w.Metadata,
		ReviewRequired: w.ReviewRequired,
		ExpiresAt:      w.ExpiresAt,
		CreatedAt:      w.CreatedAt,
		UpdatedAt:      w.UpdatedAt,
	}
}

func (s *Server) handleInitWithdraw(w http.ResponseWriter, r *http.Request) {
	req, err := httpx.Decode[initWithdrawRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	amount, err := decimal.NewFromString(req.Amount)
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("amount is not a valid decimal"))
		return
	}

	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("expires_at must be RFC3339 format"))
			return
		}
		expiresAt = &t
	}

	input := core.WithdrawInput{
		AccountHolder:  req.AccountHolder,
		CurrencyID:     req.CurrencyID,
		Amount:         amount,
		ChannelName:    req.ChannelName,
		IdempotencyKey: req.IdempotencyKey,
		ReviewRequired: req.ReviewRequired,
		Metadata:       req.Metadata,
		ExpiresAt:      expiresAt,
	}

	withdrawal, err := s.withdrawer.InitWithdraw(r.Context(), input)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, toWithdrawalResponse(withdrawal))
}

func (s *Server) handleReserveWithdraw(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid withdrawal ID"))
		return
	}

	if err := s.withdrawer.ReserveWithdraw(r.Context(), id); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, map[string]string{"status": "reserved"})
}

func (s *Server) handleReviewWithdraw(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid withdrawal ID"))
		return
	}

	req, err := httpx.Decode[reviewWithdrawRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	if err := s.withdrawer.ReviewWithdraw(r.Context(), id, req.Approved); err != nil {
		httpx.Error(w, err)
		return
	}
	status := "processing"
	if !req.Approved {
		status = "failed"
	}
	httpx.OK(w, map[string]string{"status": status})
}

func (s *Server) handleProcessWithdraw(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid withdrawal ID"))
		return
	}

	req, err := httpx.Decode[processWithdrawRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if req.ChannelRef == "" {
		httpx.Error(w, httpx.ErrBadRequest("channel_ref is required"))
		return
	}

	if err := s.withdrawer.ProcessWithdraw(r.Context(), id, req.ChannelRef); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, map[string]string{"status": "processing"})
}

func (s *Server) handleConfirmWithdraw(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid withdrawal ID"))
		return
	}

	if err := s.withdrawer.ConfirmWithdraw(r.Context(), id); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, map[string]string{"status": "confirmed"})
}

func (s *Server) handleFailWithdraw(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid withdrawal ID"))
		return
	}

	req, err := httpx.Decode[failWithdrawRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if req.Reason == "" {
		httpx.Error(w, httpx.ErrBadRequest("reason is required"))
		return
	}

	if err := s.withdrawer.FailWithdraw(r.Context(), id, req.Reason); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, map[string]string{"status": "failed"})
}

func (s *Server) handleRetryWithdraw(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid withdrawal ID"))
		return
	}

	if err := s.withdrawer.RetryWithdraw(r.Context(), id); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, map[string]string{"status": "reserved"})
}

func (s *Server) handleListWithdrawals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var holder int64
	if h := q.Get("holder"); h != "" {
		var err error
		holder, err = strconv.ParseInt(h, 10, 64)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("holder must be a number"))
			return
		}
	}
	status := q.Get("status")
	limit := parsePageLimit(r)

	withdrawals, err := s.queries.ListWithdrawals(r.Context(), holder, status, limit)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	data := make([]withdrawalResponse, len(withdrawals))
	for i, wd := range withdrawals {
		data[i] = toWithdrawalResponse(&wd)
	}
	httpx.OK(w, data)
}

