// Package server: handler_platform.go
// Read-only platform-wide balance + solvency endpoints backed by
// core.PlatformBalanceReader and core.SolvencyChecker. These expose facade
// capability (ledger.Service.PlatformBalanceReader() / SolvencyChecker())
// that was previously only reachable via cmd/ledger-cli.
package server

import (
	"net/http"
	"strconv"

	"github.com/azex-ai/ledger/pkg/httpx"
)

type platformBalanceResponse struct {
	CurrencyID int64             `json:"currency_id"`
	UserSide   map[string]string `json:"user_side"`
	SystemSide map[string]string `json:"system_side"`
}

type solvencyResponse struct {
	CurrencyID int64  `json:"currency_id"`
	Liability  string `json:"liability"`
	Custodial  string `json:"custodial"`
	Solvent    bool   `json:"solvent"`
	Margin     string `json:"margin"`
}

// handleGetPlatformBalances returns the per-classification user-side vs
// system-side balance breakdown for a currency, computed in real time
// (checkpoint + delta, no rollup-worker lag).
func (s *Server) handleGetPlatformBalances(w http.ResponseWriter, r *http.Request) {
	currencyID, err := strconv.ParseInt(r.URL.Query().Get("currency_id"), 10, 64)
	if err != nil || currencyID == 0 {
		httpx.Error(w, httpx.ErrBadRequest("currency_id query param is required"))
		return
	}

	pb, err := s.platformBalances.GetPlatformBalances(r.Context(), currencyID)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	resp := platformBalanceResponse{
		CurrencyID: pb.CurrencyID,
		UserSide:   make(map[string]string, len(pb.UserSide)),
		SystemSide: make(map[string]string, len(pb.SystemSide)),
	}
	for k, v := range pb.UserSide {
		resp.UserSide[k] = v.String()
	}
	for k, v := range pb.SystemSide {
		resp.SystemSide[k] = v.String()
	}
	httpx.OK(w, resp)
}

// handleGetSolvency compares total user-side liability against the
// custodial system balance for a currency.
func (s *Server) handleGetSolvency(w http.ResponseWriter, r *http.Request) {
	currencyID, err := strconv.ParseInt(r.URL.Query().Get("currency_id"), 10, 64)
	if err != nil || currencyID == 0 {
		httpx.Error(w, httpx.ErrBadRequest("currency_id query param is required"))
		return
	}

	report, err := s.solvency.SolvencyCheck(r.Context(), currencyID)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	httpx.OK(w, solvencyResponse{
		CurrencyID: report.CurrencyID,
		Liability:  report.Liability.String(),
		Custodial:  report.Custodial.String(),
		Solvent:    report.Solvent,
		Margin:     report.Margin.String(),
	})
}
