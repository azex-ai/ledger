package server

import (
	"net/http"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/bizcode"
	"github.com/azex-ai/ledger/pkg/httpx"
	"github.com/azex-ai/ledger/presets"
)

// devCreditRequest credits a holder without a matching inbound asset — the
// developer-mode stand-in for a real deposit. Deliberately narrower than
// postTemplateRequest: the template code is fixed (a caller must not be able
// to aim this endpoint at deposit_confirm and forge custodial balance) and
// there is a single amount rather than a map.
type devCreditRequest struct {
	HolderID       int64             `json:"holder_id"`
	CurrencyUID    string            `json:"currency_uid"`
	Amount         string            `json:"amount"`
	IdempotencyKey string            `json:"idempotency_key"`
	ActorID        int64             `json:"actor_id"`
	Source         string            `json:"source"`
	Metadata       map[string]string `json:"metadata"`
}

// handleIssueDevCredit posts a presets.DevCreditTemplateCode journal:
//
//	DR main_wallet (holder)  CR dev_credit (system counterpart)
//
// The result is an ordinary journal — append-only, reversible only through
// POST /journals/{uid}/reverse. What separates it from a real deposit is the
// counterparty account, not the strength of the record: because the credit
// leg is dev_credit rather than custodial, /platform/solvency counts the new
// liability without any offsetting asset and reports the shortfall.
//
// Answers bizcode.FeatureNotEnabled unless Config.DevCreditEnabled is set,
// which Config.Validate only permits when ENV=dev.
func (s *Server) handleIssueDevCredit(w http.ResponseWriter, r *http.Request) {
	if !s.devCreditEnabled {
		httpx.Error(w, bizcode.FeatureNotEnabled)
		return
	}

	req, err := httpx.Decode[devCreditRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	amount, err := parseWireAmount(req.Amount, "amount")
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if !amount.IsPositive() {
		httpx.Error(w, httpx.ErrBadRequest("amount must be positive"))
		return
	}

	journal, err := s.journals.ExecuteTemplate(r.Context(), presets.DevCreditTemplateCode, core.TemplateParams{
		HolderID:       req.HolderID,
		CurrencyUID:    req.CurrencyUID,
		IdempotencyKey: req.IdempotencyKey,
		Amounts:        map[string]decimal.Decimal{"amount": amount},
		ActorID:        req.ActorID,
		Source:         req.Source,
		Metadata:       req.Metadata,
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, toJournalResponse(journal))
}
