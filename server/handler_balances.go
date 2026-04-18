package server

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"
)

type balanceResponse struct {
	AccountHolder    int64  `json:"account_holder"`
	CurrencyID       int64  `json:"currency_id"`
	ClassificationID int64  `json:"classification_id"`
	Balance          string `json:"balance"`
}

type batchBalancesRequest struct {
	HolderIDs  []int64 `json:"holder_ids"`
	CurrencyID int64   `json:"currency_id"`
}

func (s *Server) handleGetBalances(w http.ResponseWriter, r *http.Request) {
	holder, err := parseIDParam(chi.URLParam(r, "holder"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "invalid holder ID")
		return
	}

	currencyID, _ := strconv.ParseInt(r.URL.Query().Get("currency_id"), 10, 64)
	if currencyID == 0 {
		writeError(w, http.StatusBadRequest, "invalid_params", "currency_id query param is required")
		return
	}

	balances, err := s.balances.GetBalances(r.Context(), holder, currencyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	data := make([]balanceResponse, len(balances))
	for i, b := range balances {
		data[i] = balanceResponse{
			AccountHolder:    b.AccountHolder,
			CurrencyID:       b.CurrencyID,
			ClassificationID: b.ClassificationID,
			Balance:          b.Balance.String(),
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (s *Server) handleGetBalanceByCurrency(w http.ResponseWriter, r *http.Request) {
	holder, err := parseIDParam(chi.URLParam(r, "holder"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "invalid holder ID")
		return
	}
	currencyID, err := parseIDParam(chi.URLParam(r, "currency"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "invalid currency ID")
		return
	}

	balances, err := s.balances.GetBalances(r.Context(), holder, currencyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	// Compute total
	total := decimal.Zero
	data := make([]balanceResponse, len(balances))
	for i, b := range balances {
		total = total.Add(b.Balance)
		data[i] = balanceResponse{
			AccountHolder:    b.AccountHolder,
			CurrencyID:       b.CurrencyID,
			ClassificationID: b.ClassificationID,
			Balance:          b.Balance.String(),
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":          total.String(),
		"classifications": data,
	})
}

func (s *Server) handleBatchBalances(w http.ResponseWriter, r *http.Request) {
	var req batchBalancesRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if len(req.HolderIDs) == 0 || req.CurrencyID == 0 {
		writeError(w, http.StatusBadRequest, "invalid_params", "holder_ids and currency_id required")
		return
	}
	if len(req.HolderIDs) > 100 {
		writeError(w, http.StatusBadRequest, "invalid_params", "max 100 holder_ids per batch")
		return
	}

	result, err := s.balances.BatchGetBalances(r.Context(), req.HolderIDs, req.CurrencyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	// Convert to string amounts
	type holderBalances struct {
		HolderID int64             `json:"holder_id"`
		Balances []balanceResponse `json:"balances"`
	}
	data := make([]holderBalances, 0, len(result))
	for holderID, bals := range result {
		hb := holderBalances{HolderID: holderID}
		hb.Balances = make([]balanceResponse, len(bals))
		for i, b := range bals {
			hb.Balances[i] = balanceResponse{
				AccountHolder:    b.AccountHolder,
				CurrencyID:       b.CurrencyID,
				ClassificationID: b.ClassificationID,
				Balance:          b.Balance.String(),
			}
		}
		data = append(data, hb)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}
