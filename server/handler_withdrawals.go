package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
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
	var req initWithdrawRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	amount, err := decimal.NewFromString(req.Amount)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_amount", "amount is not a valid decimal")
		return
	}

	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_params", "expires_at must be RFC3339 format")
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
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toWithdrawalResponse(withdrawal))
}

func (s *Server) handleReserveWithdraw(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "invalid withdrawal ID")
		return
	}

	if err := s.withdrawer.ReserveWithdraw(r.Context(), id); err != nil {
		s.writeWithdrawError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reserved"})
}

func (s *Server) handleReviewWithdraw(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "invalid withdrawal ID")
		return
	}

	var req reviewWithdrawRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	if err := s.withdrawer.ReviewWithdraw(r.Context(), id, req.Approved); err != nil {
		s.writeWithdrawError(w, err)
		return
	}
	status := "processing"
	if !req.Approved {
		status = "failed"
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (s *Server) handleProcessWithdraw(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "invalid withdrawal ID")
		return
	}

	var req processWithdrawRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.ChannelRef == "" {
		writeError(w, http.StatusBadRequest, "invalid_params", "channel_ref is required")
		return
	}

	if err := s.withdrawer.ProcessWithdraw(r.Context(), id, req.ChannelRef); err != nil {
		s.writeWithdrawError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "processing"})
}

func (s *Server) handleConfirmWithdraw(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "invalid withdrawal ID")
		return
	}

	if err := s.withdrawer.ConfirmWithdraw(r.Context(), id); err != nil {
		s.writeWithdrawError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "confirmed"})
}

func (s *Server) handleFailWithdraw(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "invalid withdrawal ID")
		return
	}

	var req failWithdrawRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "invalid_params", "reason is required")
		return
	}

	if err := s.withdrawer.FailWithdraw(r.Context(), id, req.Reason); err != nil {
		s.writeWithdrawError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "failed"})
}

func (s *Server) handleRetryWithdraw(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "invalid withdrawal ID")
		return
	}

	if err := s.withdrawer.RetryWithdraw(r.Context(), id); err != nil {
		s.writeWithdrawError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reserved"})
}

func (s *Server) handleListWithdrawals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	holder, err := strconv.ParseInt(q.Get("holder"), 10, 64)
	if err != nil || holder == 0 {
		writeError(w, http.StatusBadRequest, "invalid_params", "holder is required")
		return
	}
	limit := parsePageLimit(r)

	withdrawals, err := s.queries.ListWithdrawals(r.Context(), holder, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	data := make([]withdrawalResponse, len(withdrawals))
	for i, wd := range withdrawals {
		data[i] = toWithdrawalResponse(&wd)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// writeWithdrawError handles common withdrawal error patterns.
func (s *Server) writeWithdrawError(w http.ResponseWriter, err error) {
	msg := err.Error()
	if strings.Contains(msg, "not found") {
		writeError(w, http.StatusNotFound, "not_found", msg)
		return
	}
	if strings.Contains(msg, "invalid transition") {
		writeError(w, http.StatusConflict, "invalid_transition", msg)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", msg)
}
