package server

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
)

type setAccountPolicyRequest struct {
	CurrencyID        int64  `json:"currency_id"`
	ClassificationID  int64  `json:"classification_id"`
	Status            string `json:"status"`
	MinBalance        string `json:"min_balance"`
	EnforceMinBalance bool   `json:"enforce_min_balance"`
	Note              string `json:"note"`
	ActorID           int64  `json:"actor_id"`
}

type accountPolicyResponse struct {
	ID                int64     `json:"id"`
	AccountHolder     int64     `json:"account_holder"`
	CurrencyID        int64     `json:"currency_id"`
	ClassificationID  int64     `json:"classification_id"`
	Status            string    `json:"status"`
	MinBalance        string    `json:"min_balance"`
	EnforceMinBalance bool      `json:"enforce_min_balance"`
	Note              string    `json:"note"`
	UpdatedAt         time.Time `json:"updated_at"`
	CreatedAt         time.Time `json:"created_at"`
}

func toAccountPolicyResponse(p *core.AccountPolicy) accountPolicyResponse {
	return accountPolicyResponse{
		ID:                p.ID,
		AccountHolder:     p.AccountHolder,
		CurrencyID:        p.CurrencyID,
		ClassificationID:  p.ClassificationID,
		Status:            string(p.Status),
		MinBalance:        p.MinBalance.String(),
		EnforceMinBalance: p.EnforceMinBalance,
		Note:              p.Note,
		UpdatedAt:         p.UpdatedAt,
		CreatedAt:         p.CreatedAt,
	}
}

// handleSetAccountPolicy handles PUT /api/v1/accounts/{holder}/policy.
// currency_id / classification_id default to 0 (wildcard tiers) when omitted.
func (s *Server) handleSetAccountPolicy(w http.ResponseWriter, r *http.Request) {
	holder, err := parseIDParam(chi.URLParam(r, "holder"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid account holder"))
		return
	}

	req, err := httpx.Decode[setAccountPolicyRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	minBalance := decimal.Zero
	if req.MinBalance != "" {
		minBalance, err = decimal.NewFromString(req.MinBalance)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("min_balance is not a valid decimal"))
			return
		}
	}

	input := core.AccountPolicyInput{
		AccountHolder:     holder,
		CurrencyID:        req.CurrencyID,
		ClassificationID:  req.ClassificationID,
		Status:            core.AccountPolicyStatus(req.Status),
		MinBalance:        minBalance,
		EnforceMinBalance: req.EnforceMinBalance,
		Note:              req.Note,
		ActorID:           req.ActorID,
	}

	policy, err := s.accountPolicies.SetPolicy(r.Context(), input)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, toAccountPolicyResponse(policy))
}

// handleListAccountPolicies handles GET /api/v1/accounts/{holder}/policies.
func (s *Server) handleListAccountPolicies(w http.ResponseWriter, r *http.Request) {
	holder, err := parseIDParam(chi.URLParam(r, "holder"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid account holder"))
		return
	}

	policies, err := s.accountPolicies.ListPolicies(r.Context(), holder)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	data := make([]accountPolicyResponse, len(policies))
	for i := range policies {
		data[i] = toAccountPolicyResponse(&policies[i])
	}
	httpx.OK(w, data)
}
