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

type initDepositRequest struct {
	AccountHolder  int64             `json:"account_holder"`
	CurrencyID     int64             `json:"currency_id"`
	ExpectedAmount string            `json:"expected_amount"`
	ChannelName    string            `json:"channel_name"`
	IdempotencyKey string            `json:"idempotency_key"`
	Metadata       map[string]string `json:"metadata"`
	ExpiresAt      *string           `json:"expires_at,omitempty"`
}

type confirmingDepositRequest struct {
	ChannelRef string `json:"channel_ref"`
}

type confirmDepositRequest struct {
	ActualAmount string `json:"actual_amount"`
	ChannelRef   string `json:"channel_ref"`
}

type failDepositRequest struct {
	Reason string `json:"reason"`
}

type depositResponse struct {
	ID             int64             `json:"id"`
	AccountHolder  int64             `json:"account_holder"`
	CurrencyID     int64             `json:"currency_id"`
	ExpectedAmount string            `json:"expected_amount"`
	ActualAmount   *string           `json:"actual_amount,omitempty"`
	Status         string            `json:"status"`
	ChannelName    string            `json:"channel_name"`
	ChannelRef     *string           `json:"channel_ref,omitempty"`
	JournalID      *int64            `json:"journal_id,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	ExpiresAt      *time.Time        `json:"expires_at,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

func toDepositResponse(d *core.Deposit) depositResponse {
	resp := depositResponse{
		ID:             d.ID,
		AccountHolder:  d.AccountHolder,
		CurrencyID:     d.CurrencyID,
		ExpectedAmount: d.ExpectedAmount.String(),
		Status:         string(d.Status),
		ChannelName:    d.ChannelName,
		ChannelRef:     d.ChannelRef,
		JournalID:      d.JournalID,
		IdempotencyKey: d.IdempotencyKey,
		Metadata:       d.Metadata,
		ExpiresAt:      d.ExpiresAt,
		CreatedAt:      d.CreatedAt,
		UpdatedAt:      d.UpdatedAt,
	}
	if d.ActualAmount != nil {
		s := d.ActualAmount.String()
		resp.ActualAmount = &s
	}
	return resp
}

func (s *Server) handleInitDeposit(w http.ResponseWriter, r *http.Request) {
	var req initDepositRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	amount, err := decimal.NewFromString(req.ExpectedAmount)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_amount", "expected_amount is not a valid decimal")
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

	input := core.DepositInput{
		AccountHolder:  req.AccountHolder,
		CurrencyID:     req.CurrencyID,
		ExpectedAmount: amount,
		ChannelName:    req.ChannelName,
		IdempotencyKey: req.IdempotencyKey,
		Metadata:       req.Metadata,
		ExpiresAt:      expiresAt,
	}

	deposit, err := s.depositor.InitDeposit(r.Context(), input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toDepositResponse(deposit))
}

func (s *Server) handleConfirmingDeposit(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "invalid deposit ID")
		return
	}

	var req confirmingDepositRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.ChannelRef == "" {
		writeError(w, http.StatusBadRequest, "invalid_params", "channel_ref is required")
		return
	}

	if err := s.depositor.ConfirmingDeposit(r.Context(), id, req.ChannelRef); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		if strings.Contains(err.Error(), "invalid transition") {
			writeError(w, http.StatusConflict, "invalid_transition", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "confirming"})
}

func (s *Server) handleConfirmDeposit(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "invalid deposit ID")
		return
	}

	var req confirmDepositRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	actualAmount, err := decimal.NewFromString(req.ActualAmount)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_amount", "actual_amount is not a valid decimal")
		return
	}

	input := core.ConfirmDepositInput{
		DepositID:    id,
		ActualAmount: actualAmount,
		ChannelRef:   req.ChannelRef,
	}

	if err := s.depositor.ConfirmDeposit(r.Context(), input); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		if strings.Contains(err.Error(), "invalid transition") {
			writeError(w, http.StatusConflict, "invalid_transition", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "confirmed"})
}

func (s *Server) handleFailDeposit(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "invalid deposit ID")
		return
	}

	var req failDepositRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "invalid_params", "reason is required")
		return
	}

	if err := s.depositor.FailDeposit(r.Context(), id, req.Reason); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		if strings.Contains(err.Error(), "invalid transition") {
			writeError(w, http.StatusConflict, "invalid_transition", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "failed"})
}

func (s *Server) handleListDeposits(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	holder, err := strconv.ParseInt(q.Get("holder"), 10, 64)
	if err != nil || holder == 0 {
		writeError(w, http.StatusBadRequest, "invalid_params", "holder is required")
		return
	}
	limit := parsePageLimit(r)

	deposits, err := s.queries.ListDeposits(r.Context(), holder, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	data := make([]depositResponse, len(deposits))
	for i, d := range deposits {
		data[i] = toDepositResponse(&d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}
