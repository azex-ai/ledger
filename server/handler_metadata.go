package server

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
)

// --- Classification types ---

type createClassificationRequest struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	NormalSide string `json:"normal_side"`
	IsSystem   bool   `json:"is_system"`
}

type classificationResponse struct {
	ID         int64     `json:"id"`
	Code       string    `json:"code"`
	Name       string    `json:"name"`
	NormalSide string    `json:"normal_side"`
	IsSystem   bool      `json:"is_system"`
	IsActive   bool      `json:"is_active"`
	CreatedAt  time.Time `json:"created_at"`
}

func toClassificationResponse(c *core.Classification) classificationResponse {
	return classificationResponse{
		ID:         c.ID,
		Code:       c.Code,
		Name:       c.Name,
		NormalSide: string(c.NormalSide),
		IsSystem:   c.IsSystem,
		IsActive:   c.IsActive,
		CreatedAt:  c.CreatedAt,
	}
}

// --- Journal Type types ---

type createJournalTypeRequest struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type journalTypeResponse struct {
	ID        int64     `json:"id"`
	Code      string    `json:"code"`
	Name      string    `json:"name"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

func toJournalTypeResponse(jt *core.JournalType) journalTypeResponse {
	return journalTypeResponse{
		ID:        jt.ID,
		Code:      jt.Code,
		Name:      jt.Name,
		IsActive:  jt.IsActive,
		CreatedAt: jt.CreatedAt,
	}
}

// --- Template types ---

type createTemplateRequest struct {
	Code          string                   `json:"code"`
	Name          string                   `json:"name"`
	JournalTypeID int64                    `json:"journal_type_id"`
	Lines         []createTemplateLineJSON `json:"lines"`
}

type createTemplateLineJSON struct {
	ClassificationID int64  `json:"classification_id"`
	EntryType        string `json:"entry_type"`
	HolderRole       string `json:"holder_role"`
	AmountKey        string `json:"amount_key"`
	SortOrder        int    `json:"sort_order"`
}

type templateResponse struct {
	ID            int64                  `json:"id"`
	Code          string                 `json:"code"`
	Name          string                 `json:"name"`
	JournalTypeID int64                  `json:"journal_type_id"`
	IsActive      bool                   `json:"is_active"`
	Lines         []templateLineResponse `json:"lines"`
	CreatedAt     time.Time              `json:"created_at"`
}

type templateLineResponse struct {
	ID               int64  `json:"id"`
	ClassificationID int64  `json:"classification_id"`
	EntryType        string `json:"entry_type"`
	HolderRole       string `json:"holder_role"`
	AmountKey        string `json:"amount_key"`
	SortOrder        int    `json:"sort_order"`
}

func toTemplateResponse(t *core.EntryTemplate) templateResponse {
	lines := make([]templateLineResponse, len(t.Lines))
	for i, l := range t.Lines {
		lines[i] = templateLineResponse{
			ID:               l.ID,
			ClassificationID: l.ClassificationID,
			EntryType:        string(l.EntryType),
			HolderRole:       string(l.HolderRole),
			AmountKey:        l.AmountKey,
			SortOrder:        l.SortOrder,
		}
	}
	return templateResponse{
		ID:            t.ID,
		Code:          t.Code,
		Name:          t.Name,
		JournalTypeID: t.JournalTypeID,
		IsActive:      t.IsActive,
		Lines:         lines,
		CreatedAt:     t.CreatedAt,
	}
}

// --- Currency types ---

type createCurrencyRequest struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type currencyResponse struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
}

// --- Template preview types ---

type previewTemplateRequest struct {
	HolderID   int64             `json:"holder_id"`
	CurrencyID int64             `json:"currency_id"`
	Amounts    map[string]string `json:"amounts"`
}

type previewEntryResponse struct {
	AccountHolder    int64  `json:"account_holder"`
	CurrencyID       int64  `json:"currency_id"`
	ClassificationID int64  `json:"classification_id"`
	EntryType        string `json:"entry_type"`
	Amount           string `json:"amount"`
}

type previewTemplateResponse struct {
	Entries []previewEntryResponse `json:"entries"`
}

// --- Classification handlers ---

func (s *Server) handleCreateClassification(w http.ResponseWriter, r *http.Request) {
	req, err := httpx.Decode[createClassificationRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if req.Code == "" || req.Name == "" {
		httpx.Error(w, httpx.ErrBadRequest("code and name required"))
		return
	}
	ns := core.NormalSide(req.NormalSide)
	if !ns.IsValid() {
		httpx.Error(w, httpx.ErrBadRequest("normal_side must be debit or credit"))
		return
	}

	cls, err := s.classifications.CreateClassification(r.Context(), core.ClassificationInput{
		Code:       req.Code,
		Name:       req.Name,
		NormalSide: ns,
		IsSystem:   req.IsSystem,
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, toClassificationResponse(cls))
}

func (s *Server) handleDeactivateClassification(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid classification ID"))
		return
	}
	if err := s.classifications.DeactivateClassification(r.Context(), id); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, map[string]string{"status": "deactivated"})
}

func (s *Server) handleListClassifications(w http.ResponseWriter, r *http.Request) {
	activeOnly := r.URL.Query().Get("active_only") == "true"
	list, err := s.classifications.ListClassifications(r.Context(), activeOnly)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	data := make([]classificationResponse, len(list))
	for i, c := range list {
		data[i] = toClassificationResponse(&c)
	}
	httpx.OK(w, data)
}

// --- Journal Type handlers ---

func (s *Server) handleCreateJournalType(w http.ResponseWriter, r *http.Request) {
	req, err := httpx.Decode[createJournalTypeRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if req.Code == "" || req.Name == "" {
		httpx.Error(w, httpx.ErrBadRequest("code and name required"))
		return
	}

	jt, err := s.journalTypes.CreateJournalType(r.Context(), core.JournalTypeInput{
		Code: req.Code,
		Name: req.Name,
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, toJournalTypeResponse(jt))
}

func (s *Server) handleDeactivateJournalType(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid journal type ID"))
		return
	}
	if err := s.journalTypes.DeactivateJournalType(r.Context(), id); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, map[string]string{"status": "deactivated"})
}

func (s *Server) handleListJournalTypes(w http.ResponseWriter, r *http.Request) {
	activeOnly := r.URL.Query().Get("active_only") == "true"
	list, err := s.journalTypes.ListJournalTypes(r.Context(), activeOnly)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	data := make([]journalTypeResponse, len(list))
	for i, jt := range list {
		data[i] = toJournalTypeResponse(&jt)
	}
	httpx.OK(w, data)
}

// --- Template handlers ---

func (s *Server) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	req, err := httpx.Decode[createTemplateRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if req.Code == "" || req.Name == "" || req.JournalTypeID == 0 {
		httpx.Error(w, httpx.ErrBadRequest("code, name, and journal_type_id required"))
		return
	}

	lines := make([]core.TemplateLineInput, len(req.Lines))
	for i, l := range req.Lines {
		lines[i] = core.TemplateLineInput{
			ClassificationID: l.ClassificationID,
			EntryType:        core.EntryType(l.EntryType),
			HolderRole:       core.HolderRole(l.HolderRole),
			AmountKey:        l.AmountKey,
			SortOrder:        l.SortOrder,
		}
	}

	tmpl, err := s.templates.CreateTemplate(r.Context(), core.TemplateInput{
		Code:          req.Code,
		Name:          req.Name,
		JournalTypeID: req.JournalTypeID,
		Lines:         lines,
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, toTemplateResponse(tmpl))
}

func (s *Server) handleDeactivateTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid template ID"))
		return
	}
	if err := s.templates.DeactivateTemplate(r.Context(), id); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, map[string]string{"status": "deactivated"})
}

func (s *Server) handlePreviewTemplate(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")

	req, err := httpx.Decode[previewTemplateRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	tmpl, err := s.templates.GetTemplate(r.Context(), code)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	amounts := make(map[string]decimal.Decimal, len(req.Amounts))
	for k, v := range req.Amounts {
		d, err := decimal.NewFromString(v)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("amount "+v+" is not a valid decimal"))
			return
		}
		amounts[k] = d
	}

	params := core.TemplateParams{
		HolderID:       req.HolderID,
		CurrencyID:     req.CurrencyID,
		IdempotencyKey: "preview",
		Amounts:        amounts,
	}

	input, err := tmpl.Render(params)
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest(err.Error()))
		return
	}

	entries := make([]previewEntryResponse, len(input.Entries))
	for i, e := range input.Entries {
		entries[i] = previewEntryResponse{
			AccountHolder:    e.AccountHolder,
			CurrencyID:       e.CurrencyID,
			ClassificationID: e.ClassificationID,
			EntryType:        string(e.EntryType),
			Amount:           e.Amount.String(),
		}
	}
	httpx.OK(w, previewTemplateResponse{Entries: entries})
}

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	activeOnly := r.URL.Query().Get("active_only") == "true"
	list, err := s.templates.ListTemplates(r.Context(), activeOnly)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	data := make([]templateResponse, len(list))
	for i, t := range list {
		data[i] = toTemplateResponse(&t)
	}
	httpx.OK(w, data)
}

// --- Currency handlers ---

func (s *Server) handleCreateCurrency(w http.ResponseWriter, r *http.Request) {
	req, err := httpx.Decode[createCurrencyRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if req.Code == "" || req.Name == "" {
		httpx.Error(w, httpx.ErrBadRequest("code and name required"))
		return
	}

	c, err := s.currencies.CreateCurrency(r.Context(), core.CurrencyInput{
		Code: req.Code,
		Name: req.Name,
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, currencyResponse{ID: c.ID, Code: c.Code, Name: c.Name})
}

func (s *Server) handleListCurrencies(w http.ResponseWriter, r *http.Request) {
	list, err := s.currencies.ListCurrencies(r.Context())
	if err != nil {
		httpx.Error(w, err)
		return
	}
	data := make([]currencyResponse, len(list))
	for i, c := range list {
		data[i] = currencyResponse{ID: c.ID, Code: c.Code, Name: c.Name}
	}
	httpx.OK(w, data)
}
