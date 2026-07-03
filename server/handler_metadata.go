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
	Code       string          `json:"code"`
	Name       string          `json:"name"`
	NormalSide string          `json:"normal_side"`
	IsSystem   bool            `json:"is_system"`
	Lifecycle  *core.Lifecycle `json:"lifecycle"`
}

type classificationResponse struct {
	UID        string          `json:"uid"`
	Code       string          `json:"code"`
	Name       string          `json:"name"`
	NormalSide string          `json:"normal_side"`
	IsSystem   bool            `json:"is_system"`
	IsActive   bool            `json:"is_active"`
	Lifecycle  *core.Lifecycle `json:"lifecycle,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

func toClassificationResponse(c *core.Classification) classificationResponse {
	return classificationResponse{
		UID:        c.UID,
		Code:       c.Code,
		Name:       c.Name,
		NormalSide: string(c.NormalSide),
		IsSystem:   c.IsSystem,
		IsActive:   c.IsActive,
		Lifecycle:  c.Lifecycle,
		CreatedAt:  c.CreatedAt,
	}
}

// --- Journal Type types ---

type createJournalTypeRequest struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type journalTypeResponse struct {
	UID       string    `json:"uid"`
	Code      string    `json:"code"`
	Name      string    `json:"name"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

func toJournalTypeResponse(jt *core.JournalType) journalTypeResponse {
	return journalTypeResponse{
		UID:       jt.UID,
		Code:      jt.Code,
		Name:      jt.Name,
		IsActive:  jt.IsActive,
		CreatedAt: jt.CreatedAt,
	}
}

// --- Template types ---

type createTemplateRequest struct {
	Code           string                   `json:"code"`
	Name           string                   `json:"name"`
	JournalTypeUID string                   `json:"journal_type_uid"`
	Lines          []createTemplateLineJSON `json:"lines"`
}

type createTemplateLineJSON struct {
	ClassificationUID string `json:"classification_uid"`
	EntryType         string `json:"entry_type"`
	HolderRole        string `json:"holder_role"`
	AmountKey         string `json:"amount_key"`
	SortOrder         int    `json:"sort_order"`
}

type templateResponse struct {
	UID            string                 `json:"uid"`
	Code           string                 `json:"code"`
	Name           string                 `json:"name"`
	JournalTypeUID string                 `json:"journal_type_uid"`
	IsActive       bool                   `json:"is_active"`
	Lines          []templateLineResponse `json:"lines"`
	CreatedAt      time.Time              `json:"created_at"`
}

type templateLineResponse struct {
	ClassificationUID string `json:"classification_uid"`
	EntryType         string `json:"entry_type"`
	HolderRole        string `json:"holder_role"`
	AmountKey         string `json:"amount_key"`
	SortOrder         int    `json:"sort_order"`
}

func toTemplateResponse(t *core.EntryTemplate) templateResponse {
	lines := make([]templateLineResponse, len(t.Lines))
	for i, l := range t.Lines {
		lines[i] = templateLineResponse{
			ClassificationUID: l.ClassificationUID,
			EntryType:         string(l.EntryType),
			HolderRole:        string(l.HolderRole),
			AmountKey:         l.AmountKey,
			SortOrder:         l.SortOrder,
		}
	}
	return templateResponse{
		UID:            t.UID,
		Code:           t.Code,
		Name:           t.Name,
		JournalTypeUID: t.JournalTypeUID,
		IsActive:       t.IsActive,
		Lines:          lines,
		CreatedAt:      t.CreatedAt,
	}
}

// --- Currency types ---

type createCurrencyRequest struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// Exponent is the maximum number of decimal places entries in this
	// currency may carry (JPY=0, USD=2, USDT=6, wei=18). Required. A pointer
	// so that an omitted field is distinguishable from an explicit 0 — 0 is a
	// legal exponent (JPY), so silently defaulting a missing field to it
	// would create a wrong-precision currency without any error.
	Exponent *int32 `json:"exponent"`
}

type currencyResponse struct {
	UID      string `json:"uid"`
	Code     string `json:"code"`
	Name     string `json:"name"`
	IsActive bool   `json:"is_active"`
	Exponent int32  `json:"exponent"`
}

// --- Template preview types ---

type previewTemplateRequest struct {
	HolderID    int64             `json:"holder_id"`
	CurrencyUID string            `json:"currency_uid"`
	Amounts     map[string]string `json:"amounts"`
}

type previewEntryResponse struct {
	AccountHolder     int64  `json:"account_holder"`
	CurrencyUID       string `json:"currency_uid"`
	ClassificationUID string `json:"classification_uid"`
	EntryType         string `json:"entry_type"`
	Amount            string `json:"amount"`
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
		Lifecycle:  req.Lifecycle,
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, toClassificationResponse(cls))
}

func (s *Server) handleDeactivateClassification(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid classification uid"))
		return
	}
	if err := s.classifications.DeactivateClassification(r.Context(), uid); err != nil {
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
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid journal type uid"))
		return
	}
	if err := s.journalTypes.DeactivateJournalType(r.Context(), uid); err != nil {
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
	if req.Code == "" || req.Name == "" || req.JournalTypeUID == "" {
		httpx.Error(w, httpx.ErrBadRequest("code, name, and journal_type_uid required"))
		return
	}

	lines := make([]core.TemplateLineInput, len(req.Lines))
	for i, l := range req.Lines {
		lines[i] = core.TemplateLineInput{
			ClassificationUID: l.ClassificationUID,
			EntryType:         core.EntryType(l.EntryType),
			HolderRole:        core.HolderRole(l.HolderRole),
			AmountKey:         l.AmountKey,
			SortOrder:         l.SortOrder,
		}
	}

	input := core.TemplateInput{
		Code:           req.Code,
		Name:           req.Name,
		JournalTypeUID: req.JournalTypeUID,
		Lines:          lines,
	}

	tmpl, err := s.templates.CreateTemplate(r.Context(), input)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, toTemplateResponse(tmpl))
}

func (s *Server) handleDeactivateTemplate(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid template uid"))
		return
	}
	if err := s.templates.DeactivateTemplate(r.Context(), uid); err != nil {
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
		CurrencyUID:    req.CurrencyUID,
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
			AccountHolder:     e.AccountHolder,
			CurrencyUID:       e.CurrencyUID,
			ClassificationUID: e.ClassificationUID,
			EntryType:         string(e.EntryType),
			Amount:            e.Amount.String(),
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
	if req.Exponent == nil {
		httpx.Error(w, httpx.ErrBadRequest("exponent required (0-18; e.g. JPY=0, USD=2, wei=18)"))
		return
	}
	if *req.Exponent < 0 || *req.Exponent > 18 {
		httpx.Error(w, httpx.ErrBadRequest("exponent must be between 0 and 18"))
		return
	}

	c, err := s.currencies.CreateCurrency(r.Context(), core.CurrencyInput{
		Code:     req.Code,
		Name:     req.Name,
		Exponent: *req.Exponent,
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, currencyResponse{UID: c.UID, Code: c.Code, Name: c.Name, IsActive: c.IsActive, Exponent: c.Exponent})
}

func (s *Server) handleDeactivateCurrency(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid currency uid"))
		return
	}
	if err := s.currencies.DeactivateCurrency(r.Context(), uid); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, map[string]string{"status": "deactivated"})
}

func (s *Server) handleListCurrencies(w http.ResponseWriter, r *http.Request) {
	activeOnly := r.URL.Query().Get("active_only") == "true"
	list, err := s.currencies.ListCurrencies(r.Context(), activeOnly)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	data := make([]currencyResponse, len(list))
	for i, c := range list {
		data[i] = currencyResponse{UID: c.UID, Code: c.Code, Name: c.Name, IsActive: c.IsActive, Exponent: c.Exponent}
	}
	httpx.OK(w, data)
}
