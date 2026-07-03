package server

import (
	"net/http"
	"time"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
)

// --- JSON response types ---

type trialBalanceRowResponse struct {
	ClassificationUID  string `json:"classification_uid"`
	ClassificationCode string `json:"classification_code"`
	ClassificationName string `json:"classification_name"`
	NormalSide         string `json:"normal_side"`
	TotalDebit         string `json:"total_debit"`
	TotalCredit        string `json:"total_credit"`
	Net                string `json:"net"`
}

type trialBalanceResponse struct {
	CurrencyUID string                    `json:"currency_uid"`
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
			ClassificationUID:  row.ClassificationUID,
			ClassificationCode: row.ClassificationCode,
			ClassificationName: row.ClassificationName,
			NormalSide:         string(row.NormalSide),
			TotalDebit:         row.TotalDebit.String(),
			TotalCredit:        row.TotalCredit.String(),
			Net:                row.Net.String(),
		}
	}
	return trialBalanceResponse{
		CurrencyUID: r.CurrencyUID,
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

	currencyUID := q.Get("currency_uid")
	if currencyUID == "" {
		httpx.Error(w, httpx.ErrBadRequest("currency_uid is required"))
		return
	}

	asOf := time.Now()
	if v := q.Get("as_of"); v != "" {
		var err error
		asOf, err = time.Parse(time.RFC3339, v)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("as_of must be RFC3339"))
			return
		}
	}

	report, err := s.trialBalance.TrialBalance(r.Context(), currencyUID, asOf)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, toTrialBalanceResponse(report))
}
