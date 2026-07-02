package server

import (
	"net/http"
	"time"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
)

// --- JSON response types ---

type trialBalanceRowResponse struct {
	ClassificationID   int64  `json:"classification_id"`
	ClassificationCode string `json:"classification_code"`
	ClassificationName string `json:"classification_name"`
	NormalSide         string `json:"normal_side"`
	TotalDebit         string `json:"total_debit"`
	TotalCredit        string `json:"total_credit"`
	Net                string `json:"net"`
}

type trialBalanceResponse struct {
	CurrencyID  int64                     `json:"currency_id"`
	AsOf        time.Time                 `json:"as_of"`
	Rows        []trialBalanceRowResponse `json:"rows"`
	TotalDebit  string                    `json:"total_debit"`
	TotalCredit string                    `json:"total_credit"`
	Balanced    bool                      `json:"balanced"`
}

func toTrialBalanceResponse(r *core.TrialBalanceReport) trialBalanceResponse {
	rows := make([]trialBalanceRowResponse, len(r.Rows))
	for i, row := range r.Rows {
		rows[i] = trialBalanceRowResponse{
			ClassificationID:   row.ClassificationID,
			ClassificationCode: row.ClassificationCode,
			ClassificationName: row.ClassificationName,
			NormalSide:         string(row.NormalSide),
			TotalDebit:         row.TotalDebit.String(),
			TotalCredit:        row.TotalCredit.String(),
			Net:                row.Net.String(),
		}
	}
	return trialBalanceResponse{
		CurrencyID:  r.CurrencyID,
		AsOf:        r.AsOf,
		Rows:        rows,
		TotalDebit:  r.TotalDebit.String(),
		TotalCredit: r.TotalCredit.String(),
		Balanced:    r.Balanced,
	}
}

// --- Handlers ---

func (s *Server) handleGetTrialBalance(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	currencyID, err := parseIDParam(q.Get("currency_id"))
	if err != nil || currencyID == 0 {
		httpx.Error(w, httpx.ErrBadRequest("currency_id is required"))
		return
	}

	asOf := time.Now()
	if v := q.Get("as_of"); v != "" {
		asOf, err = time.Parse(time.RFC3339, v)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("as_of must be RFC3339"))
			return
		}
	}

	report, err := s.trialBalance.TrialBalance(r.Context(), currencyID, asOf)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, toTrialBalanceResponse(report))
}
