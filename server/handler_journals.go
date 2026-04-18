package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
)

// --- JSON request/response types ---

type postJournalRequest struct {
	JournalTypeID  int64             `json:"journal_type_id"`
	IdempotencyKey string            `json:"idempotency_key"`
	Entries        []entryInputJSON  `json:"entries"`
	Metadata       map[string]string `json:"metadata"`
	ActorID        *int64            `json:"actor_id"`
	Source         string            `json:"source"`
}

type entryInputJSON struct {
	AccountHolder    int64  `json:"account_holder"`
	CurrencyID       int64  `json:"currency_id"`
	ClassificationID int64  `json:"classification_id"`
	EntryType        string `json:"entry_type"`
	Amount           string `json:"amount"`
}

type postTemplateRequest struct {
	TemplateCode   string            `json:"template_code"`
	HolderID       int64             `json:"holder_id"`
	CurrencyID     int64             `json:"currency_id"`
	IdempotencyKey string            `json:"idempotency_key"`
	Amounts        map[string]string `json:"amounts"`
	ActorID        *int64            `json:"actor_id"`
	Source         string            `json:"source"`
	Metadata       map[string]string `json:"metadata"`
}

type reverseJournalRequest struct {
	Reason string `json:"reason"`
}

type journalResponse struct {
	ID             int64             `json:"id"`
	JournalTypeID  int64             `json:"journal_type_id"`
	IdempotencyKey string            `json:"idempotency_key"`
	TotalDebit     string            `json:"total_debit"`
	TotalCredit    string            `json:"total_credit"`
	Metadata       map[string]string `json:"metadata"`
	ActorID        *int64            `json:"actor_id"`
	Source         string            `json:"source"`
	ReversalOf     *int64            `json:"reversal_of,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	Entries        []entryResponse   `json:"entries,omitempty"`
}

type entryResponse struct {
	ID               int64     `json:"id"`
	JournalID        int64     `json:"journal_id"`
	AccountHolder    int64     `json:"account_holder"`
	CurrencyID       int64     `json:"currency_id"`
	ClassificationID int64     `json:"classification_id"`
	EntryType        string    `json:"entry_type"`
	Amount           string    `json:"amount"`
	CreatedAt        time.Time `json:"created_at"`
}

func toJournalResponse(j *core.Journal) journalResponse {
	return journalResponse{
		ID:             j.ID,
		JournalTypeID:  j.JournalTypeID,
		IdempotencyKey: j.IdempotencyKey,
		TotalDebit:     j.TotalDebit.String(),
		TotalCredit:    j.TotalCredit.String(),
		Metadata:       j.Metadata,
		ActorID:        j.ActorID,
		Source:         j.Source,
		ReversalOf:     j.ReversalOf,
		CreatedAt:      j.CreatedAt,
	}
}

func toEntryResponse(e *core.Entry) entryResponse {
	return entryResponse{
		ID:               e.ID,
		JournalID:        e.JournalID,
		AccountHolder:    e.AccountHolder,
		CurrencyID:       e.CurrencyID,
		ClassificationID: e.ClassificationID,
		EntryType:        string(e.EntryType),
		Amount:           e.Amount.String(),
		CreatedAt:        e.CreatedAt,
	}
}

// --- Handlers ---

func (s *Server) handlePostJournal(w http.ResponseWriter, r *http.Request) {
	var req postJournalRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	entries := make([]core.EntryInput, len(req.Entries))
	for i, e := range req.Entries {
		amount, err := decimal.NewFromString(e.Amount)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_amount", "entry "+e.Amount+" is not a valid decimal")
			return
		}
		entries[i] = core.EntryInput{
			AccountHolder:    e.AccountHolder,
			CurrencyID:       e.CurrencyID,
			ClassificationID: e.ClassificationID,
			EntryType:        core.EntryType(e.EntryType),
			Amount:           amount,
		}
	}

	input := core.JournalInput{
		JournalTypeID:  req.JournalTypeID,
		IdempotencyKey: req.IdempotencyKey,
		Entries:        entries,
		Metadata:       req.Metadata,
		ActorID:        req.ActorID,
		Source:         req.Source,
	}

	journal, err := s.journals.PostJournal(r.Context(), input)
	if err != nil {
		if strings.Contains(err.Error(), "unbalanced") || strings.Contains(err.Error(), "must be positive") || strings.Contains(err.Error(), "required") {
			writeError(w, http.StatusBadRequest, "validation_error", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toJournalResponse(journal))
}

func (s *Server) handlePostTemplate(w http.ResponseWriter, r *http.Request) {
	var req postTemplateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	amounts := make(map[string]decimal.Decimal, len(req.Amounts))
	for k, v := range req.Amounts {
		d, err := decimal.NewFromString(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_amount", "amount "+v+" is not a valid decimal")
			return
		}
		amounts[k] = d
	}

	params := core.TemplateParams{
		HolderID:       req.HolderID,
		CurrencyID:     req.CurrencyID,
		IdempotencyKey: req.IdempotencyKey,
		Amounts:        amounts,
		ActorID:        req.ActorID,
		Source:         req.Source,
		Metadata:       req.Metadata,
	}

	journal, err := s.journals.ExecuteTemplate(r.Context(), req.TemplateCode, params)
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "inactive") || strings.Contains(err.Error(), "missing amount") {
			writeError(w, http.StatusBadRequest, "template_error", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toJournalResponse(journal))
}

func (s *Server) handleReverseJournal(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "invalid journal ID")
		return
	}

	var req reverseJournalRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "invalid_params", "reason is required")
		return
	}

	journal, err := s.journals.ReverseJournal(r.Context(), id, req.Reason)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toJournalResponse(journal))
}

func (s *Server) handleGetJournal(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "invalid journal ID")
		return
	}

	journal, entries, err := s.queries.GetJournal(r.Context(), id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	resp := toJournalResponse(journal)
	resp.Entries = make([]entryResponse, len(entries))
	for i, e := range entries {
		resp.Entries[i] = toEntryResponse(&e)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListJournals(w http.ResponseWriter, r *http.Request) {
	cursor, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cursor", "invalid cursor value")
		return
	}
	limit := parsePageLimit(r)

	journals, err := s.queries.ListJournals(r.Context(), cursor, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	resp := PagedResponse[journalResponse]{
		Data: make([]journalResponse, len(journals)),
	}
	for i, j := range journals {
		resp.Data[i] = toJournalResponse(&j)
	}
	if len(journals) == int(limit) {
		resp.NextCursor = encodeCursor(journals[len(journals)-1].ID)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	holder, err := parseIDParam(q.Get("holder"))
	if err != nil || holder == 0 {
		writeError(w, http.StatusBadRequest, "invalid_params", "holder is required")
		return
	}
	currencyID, err := parseIDParam(q.Get("currency_id"))
	if err != nil || currencyID == 0 {
		writeError(w, http.StatusBadRequest, "invalid_params", "currency_id is required")
		return
	}

	cursor, err := decodeCursor(q.Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cursor", "invalid cursor value")
		return
	}
	limit := parsePageLimit(r)

	entries, err := s.queries.ListEntriesByAccount(r.Context(), holder, currencyID, cursor, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	resp := PagedResponse[entryResponse]{
		Data: make([]entryResponse, len(entries)),
	}
	for i, e := range entries {
		resp.Data[i] = toEntryResponse(&e)
	}
	if len(entries) == int(limit) {
		resp.NextCursor = encodeCursor(entries[len(entries)-1].ID)
	}
	writeJSON(w, http.StatusOK, resp)
}
