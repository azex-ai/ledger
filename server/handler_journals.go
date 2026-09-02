package server

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
	"github.com/azex-ai/ledger/presets"
)

// --- JSON request/response types ---

type postJournalRequest struct {
	JournalTypeUID string            `json:"journal_type_uid"`
	IdempotencyKey string            `json:"idempotency_key"`
	EventUID       string            `json:"event_uid"`
	Entries        []entryInputJSON  `json:"entries"`
	Metadata       map[string]string `json:"metadata"`
	ActorID        int64             `json:"actor_id"`
	Source         string            `json:"source"`
	// EffectiveAt is RFC3339; empty means "now" (core.JournalInput.EffectiveAt).
	EffectiveAt string `json:"effective_at"`
}

type entryInputJSON struct {
	AccountHolder     int64  `json:"account_holder"`
	CurrencyUID       string `json:"currency_uid"`
	ClassificationUID string `json:"classification_uid"`
	EntryType         string `json:"entry_type"`
	Amount            string `json:"amount"`
}

type postTemplateRequest struct {
	TemplateCode   string            `json:"template_code"`
	HolderID       int64             `json:"holder_id"`
	CurrencyUID    string            `json:"currency_uid"`
	IdempotencyKey string            `json:"idempotency_key"`
	EventUID       string            `json:"event_uid"`
	Amounts        map[string]string `json:"amounts"`
	ActorID        int64             `json:"actor_id"`
	Source         string            `json:"source"`
	Metadata       map[string]string `json:"metadata"`
}

type reverseJournalRequest struct {
	Reason string `json:"reason"`
	// IdempotencyKey is read only to refuse it (H-M3): a full reversal's key
	// is derived server-side from the journal uid and the reason, so a
	// client-supplied key has no effect on the replay scope of the write.
	// The field used to be absent from this struct while docs/openapi.yaml
	// listed it as required, which meant a caller's key -- including one
	// lifted out of the Idempotency-Key header by
	// idempotencyHeaderAliasMiddleware -- was parsed away in silence. See
	// handleReverseJournal.
	IdempotencyKey string `json:"idempotency_key"`
}

type reverseJournalFractionRequest struct {
	Num            int64  `json:"num"`
	Den            int64  `json:"den"`
	Reason         string `json:"reason"`
	IdempotencyKey string `json:"idempotency_key"`
}

type postDepositToleranceRequest struct {
	HolderID       int64             `json:"holder_id"`
	CurrencyUID    string            `json:"currency_uid"`
	IdempotencyKey string            `json:"idempotency_key"`
	ExpectedAmount string            `json:"expected_amount"`
	ActualAmount   string            `json:"actual_amount"`
	Tolerance      string            `json:"tolerance"`
	ActorID        int64             `json:"actor_id"`
	Source         string            `json:"source"`
	Metadata       map[string]string `json:"metadata"`
}

type journalResponse struct {
	UID            string            `json:"uid"`
	JournalTypeUID string            `json:"journal_type_uid"`
	IdempotencyKey string            `json:"idempotency_key"`
	TotalDebit     string            `json:"total_debit"`
	TotalCredit    string            `json:"total_credit"`
	Metadata       map[string]string `json:"metadata"`
	ActorID        int64             `json:"actor_id"`
	Source         string            `json:"source"`
	ReversalOfUID  string            `json:"reversal_of_uid,omitempty"`
	EventUID       string            `json:"event_uid,omitempty"`
	EffectiveAt    time.Time         `json:"effective_at"`
	CreatedAt      time.Time         `json:"created_at"`
	Entries        []entryResponse   `json:"entries,omitempty"`
}

type entryResponse struct {
	JournalUID        string    `json:"journal_uid"`
	AccountHolder     int64     `json:"account_holder"`
	CurrencyUID       string    `json:"currency_uid"`
	ClassificationUID string    `json:"classification_uid"`
	EntryType         string    `json:"entry_type"`
	Amount            string    `json:"amount"`
	EffectiveAt       time.Time `json:"effective_at"`
	CreatedAt         time.Time `json:"created_at"`
}

type depositToleranceResponse struct {
	Outcome              string            `json:"outcome"`
	ExpectedAmount       string            `json:"expected_amount"`
	ActualAmount         string            `json:"actual_amount"`
	Tolerance            string            `json:"tolerance"`
	Delta                string            `json:"delta"`
	RequiresManualReview bool              `json:"requires_manual_review"`
	Journals             []journalResponse `json:"journals"`
}

func toJournalResponse(j *core.Journal) journalResponse {
	return journalResponse{
		UID:            j.UID,
		JournalTypeUID: j.JournalTypeUID,
		IdempotencyKey: j.IdempotencyKey,
		TotalDebit:     j.TotalDebit.String(),
		TotalCredit:    j.TotalCredit.String(),
		Metadata:       j.Metadata,
		ActorID:        j.ActorID,
		Source:         j.Source,
		ReversalOfUID:  j.ReversalOfUID,
		EventUID:       j.EventUID,
		EffectiveAt:    j.EffectiveAt,
		CreatedAt:      j.CreatedAt,
	}
}

func toEntryResponse(e *core.Entry) entryResponse {
	return entryResponse{
		JournalUID:        e.JournalUID,
		AccountHolder:     e.AccountHolder,
		CurrencyUID:       e.CurrencyUID,
		ClassificationUID: e.ClassificationUID,
		EntryType:         string(e.EntryType),
		Amount:            e.Amount.String(),
		EffectiveAt:       e.EffectiveAt,
		CreatedAt:         e.CreatedAt,
	}
}

// --- Handlers ---

func (s *Server) handlePostJournal(w http.ResponseWriter, r *http.Request) {
	req, err := httpx.Decode[postJournalRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	entries := make([]core.EntryInput, len(req.Entries))
	for i, e := range req.Entries {
		amount, err := parseWireAmount(e.Amount, "entries[].amount")
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("entry "+e.Amount+" is not a valid decimal"))
			return
		}
		entries[i] = core.EntryInput{
			AccountHolder:     e.AccountHolder,
			CurrencyUID:       e.CurrencyUID,
			ClassificationUID: e.ClassificationUID,
			EntryType:         core.EntryType(e.EntryType),
			Amount:            amount,
		}
	}

	// Deposit-forgery guard (I-38, M-2): a handwritten journal must not touch
	// an is_system classification unless the deployment has opted out. This is
	// the handwritten-path counterpart to handlePostTemplate's protected-code
	// check — without it a write-scope key could post
	// `DR main_wallet(user) / CR custodial(system)`, indistinguishable from a
	// verified deposit, straight past the template blacklist. The library's own
	// system-side flows go through templates/orchestration, not this endpoint.
	if !s.allowSystemClassificationPost {
		if err := s.rejectSystemClassificationEntries(r.Context(), entries); err != nil {
			httpx.Error(w, err)
			return
		}
	}

	var effectiveAt time.Time
	if req.EffectiveAt != "" {
		effectiveAt, err = time.Parse(time.RFC3339, req.EffectiveAt)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("effective_at must be RFC3339 format"))
			return
		}
	}

	input := core.JournalInput{
		JournalTypeUID: req.JournalTypeUID,
		IdempotencyKey: req.IdempotencyKey,
		EventUID:       req.EventUID,
		Entries:        entries,
		Metadata:       req.Metadata,
		ActorID:        req.ActorID,
		Source:         req.Source,
		EffectiveAt:    effectiveAt,
	}

	journal, err := s.journals.PostJournal(r.Context(), input)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, toJournalResponse(journal))
}

// systemClassificationUIDs resolves the set of classifications flagged
// is_system from the classification store (which the postgres adapter caches),
// keyed by uid, so both guards below reflect the live is_system flag rather
// than a hand-maintained code list.
func (s *Server) systemClassificationUIDs(ctx context.Context) (map[string]bool, error) {
	all, err := s.classifications.ListClassifications(ctx, false)
	if err != nil {
		return nil, err
	}
	systemUIDs := make(map[string]bool, len(all))
	for _, c := range all {
		if c.IsSystem {
			systemUIDs[c.UID] = true
		}
	}
	return systemUIDs, nil
}

// rejectSystemClassificationEntries returns a 403 error if any entry targets a
// classification flagged is_system.
func (s *Server) rejectSystemClassificationEntries(ctx context.Context, entries []core.EntryInput) error {
	systemUIDs, err := s.systemClassificationUIDs(ctx)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if systemUIDs[e.ClassificationUID] {
			return httpx.ErrForbidden("classification " + e.ClassificationUID + " is system-managed and cannot be posted through the generic journal endpoint; use its dedicated template/orchestration, or set AllowSystemClassificationPost after review")
		}
	}
	return nil
}

// rejectSystemClassificationTemplate is handlePostTemplate's structural
// counterpart to handlePostJournal's rejectSystemClassificationEntries, and
// the primary guard on that endpoint (D-C1): it resolves the named template's
// own legs and refuses the request if any of them posts to an is_system
// classification, exactly as the handwritten path refuses the same entries
// spelled out by hand.
//
// It derives the verdict rather than consulting a list, because a list only
// covers what somebody remembered to name: dev_credit -- the one template in
// this library whose doc comment says it mints balance with nothing behind it
// -- was reachable here for as long as the four hardcoded deposit codes were
// the only check, in any ENV, for any write-scope key. Deployment-defined
// system-side templates were never covered at all.
//
// A template that cannot be resolved fails closed: the error propagates
// (404 for an unknown code) and ExecuteTemplate is never reached, so "the
// check could not run" never reads as "the check passed"
// (working-agreements.md §3).
func (s *Server) rejectSystemClassificationTemplate(ctx context.Context, code string) error {
	tmpl, err := s.templates.GetTemplate(ctx, code)
	if err != nil {
		return err
	}
	systemUIDs, err := s.systemClassificationUIDs(ctx)
	if err != nil {
		return err
	}
	for _, line := range tmpl.Lines {
		if systemUIDs[line.ClassificationUID] {
			return httpx.ErrForbidden("template_code " + code + " posts to system-managed classification " + line.ClassificationUID + " and cannot be executed through this generic endpoint; post it through its dedicated orchestration, or name it in AllowGenericTemplatePost after review")
		}
	}
	return nil
}

func (s *Server) handlePostTemplate(w http.ResponseWriter, r *http.Request) {
	req, err := httpx.Decode[postTemplateRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	// Two layers, both defaulting closed, with Config.AllowGenericTemplatePost
	// as the single opt-out for either (contract §7.5):
	//
	//  1. s.protectedTemplateCodes -- presets.ProtectedTemplateCodes() plus
	//     whatever Config.ProtectedTemplateCodes adds. A name list, which
	//     therefore still answers when the template table cannot be read at
	//     all, and covers a code before its rows exist.
	//  2. rejectSystemClassificationTemplate -- the structural rule: any
	//     template with a leg on an is_system classification is refused,
	//     whether this library shipped it or the deployment defined it.
	//
	// This endpoint refuses those templates no matter how the caller got a
	// write-scope key, same as POST /dev/credits being hardcoded to a single
	// narrow template rather than accepting an arbitrary code
	// (handler_devcredit.go).
	if s.protectedTemplateCodes[req.TemplateCode] {
		httpx.Error(w, httpx.ErrForbidden("template_code "+req.TemplateCode+" is protected: post it through its dedicated orchestration, not this generic endpoint"))
		return
	}
	if !s.allowGenericTemplatePost[req.TemplateCode] {
		if err := s.rejectSystemClassificationTemplate(r.Context(), req.TemplateCode); err != nil {
			httpx.Error(w, err)
			return
		}
	}

	amounts := make(map[string]decimal.Decimal, len(req.Amounts))
	for k, v := range req.Amounts {
		d, err := parseWireAmount(v, "amounts")
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("amount "+v+" is not a valid decimal"))
			return
		}
		amounts[k] = d
	}

	params := core.TemplateParams{
		HolderID:       req.HolderID,
		CurrencyUID:    req.CurrencyUID,
		IdempotencyKey: req.IdempotencyKey,
		EventUID:       req.EventUID,
		Amounts:        amounts,
		ActorID:        req.ActorID,
		Source:         req.Source,
		Metadata:       req.Metadata,
	}

	journal, err := s.journals.ExecuteTemplate(r.Context(), req.TemplateCode, params)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, toJournalResponse(journal))
}

func (s *Server) handlePostDepositTolerance(w http.ResponseWriter, r *http.Request) {
	req, err := httpx.Decode[postDepositToleranceRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	expectedAmount, err := parseWireAmount(req.ExpectedAmount, "expected_amount")
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("expected_amount is not a valid decimal"))
		return
	}
	actualAmount, err := parseWireAmount(req.ActualAmount, "actual_amount")
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("actual_amount is not a valid decimal"))
		return
	}
	toleranceAmount, err := parseWireAmount(req.Tolerance, "tolerance")
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("tolerance is not a valid decimal"))
		return
	}

	plan, err := presets.BuildDepositTolerancePlan(expectedAmount, actualAmount, presets.DepositToleranceConfig{
		Amount: toleranceAmount,
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}

	journals, err := presets.ExecuteDepositTolerancePlan(r.Context(), s.journals, core.TemplateParams{
		HolderID:       req.HolderID,
		CurrencyUID:    req.CurrencyUID,
		IdempotencyKey: req.IdempotencyKey,
		ActorID:        req.ActorID,
		Source:         req.Source,
		Metadata:       req.Metadata,
	}, plan)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	resp := depositToleranceResponse{
		Outcome:              string(plan.Outcome),
		ExpectedAmount:       plan.ExpectedAmount.String(),
		ActualAmount:         plan.ActualAmount.String(),
		Tolerance:            plan.ToleranceAmount.String(),
		Delta:                plan.Delta.String(),
		RequiresManualReview: plan.RequiresManualReview,
		Journals:             make([]journalResponse, len(journals)),
	}
	for i, journal := range journals {
		resp.Journals[i] = toJournalResponse(journal)
	}
	httpx.Created(w, resp)
}

func (s *Server) handleReverseJournal(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid journal uid"))
		return
	}

	req, err := httpx.Decode[reverseJournalRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if req.Reason == "" {
		httpx.Error(w, httpx.ErrBadRequest("reason is required"))
		return
	}
	// Fail loudly rather than accept-and-ignore (working-agreements.md §3):
	// this endpoint's idempotency key is derived server-side, so honoring a
	// caller-supplied one is not possible here and pretending to would leave
	// the caller believing it scoped a money-correcting write it did not.
	// Callers that need to choose the key use POST
	// /journals/{uid}/reverse-partial with num == den ("1/1"), which takes
	// idempotency_key as an explicit parameter.
	if req.IdempotencyKey != "" {
		httpx.Error(w, httpx.ErrBadRequest("idempotency_key must not be supplied for a full reversal: the key is derived from the journal uid and reason; use reverse-partial with num=den=1 to choose your own key"))
		return
	}

	journal, err := s.journals.ReverseJournal(r.Context(), uid, req.Reason)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, toJournalResponse(journal))
}

func (s *Server) handleReverseJournalFraction(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid journal uid"))
		return
	}

	req, err := httpx.Decode[reverseJournalFractionRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if req.Reason == "" {
		httpx.Error(w, httpx.ErrBadRequest("reason is required"))
		return
	}
	if req.IdempotencyKey == "" {
		httpx.Error(w, httpx.ErrBadRequest("idempotency_key is required"))
		return
	}

	journal, err := s.journals.ReverseJournalFraction(r.Context(), uid, req.Num, req.Den, req.Reason, req.IdempotencyKey)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, toJournalResponse(journal))
}

func (s *Server) handleGetJournal(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid journal uid"))
		return
	}

	journal, entries, err := s.queries.GetJournal(r.Context(), uid)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	resp := toJournalResponse(journal)
	resp.Entries = make([]entryResponse, len(entries))
	for i, e := range entries {
		resp.Entries[i] = toEntryResponse(&e)
	}
	httpx.OK(w, resp)
}

func (s *Server) handleListJournals(w http.ResponseWriter, r *http.Request) {
	limit := parsePageLimit(r)

	journals, nextCursor, err := s.queries.ListJournals(r.Context(), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	resp := PagedResponse[journalResponse]{
		List:       make([]journalResponse, len(journals)),
		NextCursor: cursorPtr(nextCursor),
	}
	for i, j := range journals {
		resp.List[i] = toJournalResponse(&j)
	}
	httpx.OK(w, resp)
}

func (s *Server) handleListEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	holder, err := parseIDParam(q.Get("holder"))
	if err != nil || holder == 0 {
		httpx.Error(w, httpx.ErrBadRequest("holder is required"))
		return
	}
	currencyUID := q.Get("currency_uid")
	if currencyUID == "" {
		httpx.Error(w, httpx.ErrBadRequest("currency_uid is required"))
		return
	}

	limit := parsePageLimit(r)

	entries, nextCursor, err := s.queries.ListEntriesByAccount(r.Context(), holder, currencyUID, q.Get("cursor"), limit)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	resp := PagedResponse[entryResponse]{
		List:       make([]entryResponse, len(entries)),
		NextCursor: cursorPtr(nextCursor),
	}
	for i, e := range entries {
		resp.List[i] = toEntryResponse(&e)
	}
	httpx.OK(w, resp)
}
