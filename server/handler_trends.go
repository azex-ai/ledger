// Package server: handler_trends.go
// Historical balance trend endpoint backed by core.BalanceTrendReader. This
// exposes facade capability (ledger.Service.BalanceTrends()) that was
// previously only reachable via cmd/ledger-cli.
package server

import (
	"net/http"
	"strconv"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
)

type balanceTrendPointResponse struct {
	Date    string `json:"date"`
	Balance string `json:"balance"`
	Inflow  string `json:"inflow"`
	Outflow string `json:"outflow"`
}

// handleGetBalanceTrends returns one balance point per calendar day in
// [from, to] for a single account dimension. classification_id=0 (or
// omitted) sums across all classifications.
func (s *Server) handleGetBalanceTrends(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	holder, err := strconv.ParseInt(q.Get("holder"), 10, 64)
	if err != nil || holder == 0 {
		httpx.Error(w, httpx.ErrBadRequest("holder query param is required"))
		return
	}
	currencyID, err := strconv.ParseInt(q.Get("currency_id"), 10, 64)
	if err != nil || currencyID == 0 {
		httpx.Error(w, httpx.ErrBadRequest("currency_id query param is required"))
		return
	}

	var classificationID int64
	if c := q.Get("classification_id"); c != "" {
		if classificationID, err = strconv.ParseInt(c, 10, 64); err != nil {
			httpx.Error(w, httpx.ErrBadRequest("classification_id must be a number"))
			return
		}
	}

	from, err := parseRFC3339Param("from", q.Get("from"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if from.IsZero() {
		httpx.Error(w, httpx.ErrBadRequest("from query param is required"))
		return
	}
	until, err := parseRFC3339Param("to", q.Get("to"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if until.IsZero() {
		httpx.Error(w, httpx.ErrBadRequest("to query param is required"))
		return
	}

	filter := core.BalanceTrendFilter{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
		From:             from,
		Until:            until,
	}

	points, err := s.balanceTrends.GetBalanceTrends(r.Context(), filter)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	data := make([]balanceTrendPointResponse, len(points))
	for i, p := range points {
		data[i] = balanceTrendPointResponse{
			Date:    p.Date.Format("2006-01-02"),
			Balance: p.Balance.String(),
			Inflow:  p.Inflow.String(),
			Outflow: p.Outflow.String(),
		}
	}
	httpx.OK(w, data)
}
