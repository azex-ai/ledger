package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/pkg/httpx"
)

type balanceResponse struct {
	AccountHolder     int64  `json:"account_holder"`
	CurrencyUID       string `json:"currency_uid"`
	ClassificationUID string `json:"classification_uid"`
	Balance           string `json:"balance"`
}

type batchBalancesRequest struct {
	HolderIDs   []int64 `json:"holder_ids"`
	CurrencyUID string  `json:"currency_uid"`
}

func (s *Server) handleGetBalances(w http.ResponseWriter, r *http.Request) {
	holder, err := parseIDParam(chi.URLParam(r, "holder"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid holder ID"))
		return
	}

	currencyUID := r.URL.Query().Get("currency_uid")
	if currencyUID == "" {
		httpx.Error(w, httpx.ErrBadRequest("currency_uid query param is required"))
		return
	}

	balances, err := s.balances.GetBalances(r.Context(), holder, currencyUID)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	data := make([]balanceResponse, len(balances))
	for i, b := range balances {
		data[i] = balanceResponse{
			AccountHolder:     b.AccountHolder,
			CurrencyUID:       b.CurrencyUID,
			ClassificationUID: b.ClassificationUID,
			Balance:           b.Balance.String(),
		}
	}
	httpx.OK(w, data)
}

type balanceByCurrencyResponse struct {
	Total           string            `json:"total"`
	Classifications []balanceResponse `json:"classifications"`
}

func (s *Server) handleGetBalanceByCurrency(w http.ResponseWriter, r *http.Request) {
	holder, err := parseIDParam(chi.URLParam(r, "holder"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid holder ID"))
		return
	}
	currencyUID := chi.URLParam(r, "currency")
	if currencyUID == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid currency uid"))
		return
	}

	balances, err := s.balances.GetBalances(r.Context(), holder, currencyUID)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	// Compute total
	total := decimal.Zero
	data := make([]balanceResponse, len(balances))
	for i, b := range balances {
		total = total.Add(b.Balance)
		data[i] = balanceResponse{
			AccountHolder:     b.AccountHolder,
			CurrencyUID:       b.CurrencyUID,
			ClassificationUID: b.ClassificationUID,
			Balance:           b.Balance.String(),
		}
	}
	httpx.OK(w, balanceByCurrencyResponse{
		Total:           total.String(),
		Classifications: data,
	})
}

func (s *Server) handleBatchBalances(w http.ResponseWriter, r *http.Request) {
	req, err := httpx.Decode[batchBalancesRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if len(req.HolderIDs) == 0 || req.CurrencyUID == "" {
		httpx.Error(w, httpx.ErrBadRequest("holder_ids and currency_uid required"))
		return
	}
	if len(req.HolderIDs) > 100 {
		httpx.Error(w, httpx.ErrBadRequest("max 100 holder_ids per batch"))
		return
	}

	result, err := s.balances.BatchGetBalances(r.Context(), req.HolderIDs, req.CurrencyUID)
	if err != nil {
		httpx.Error(w, err)
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
				AccountHolder:     b.AccountHolder,
				CurrencyUID:       b.CurrencyUID,
				ClassificationUID: b.ClassificationUID,
				Balance:           b.Balance.String(),
			}
		}
		data = append(data, hb)
	}
	httpx.OK(w, data)
}
