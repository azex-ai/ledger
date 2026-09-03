package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
)

// --- JSON request/response types ---

type createBookingRequest struct {
	ClassificationCode string            `json:"classification_code"`
	AccountHolder      int64             `json:"account_holder"`
	CurrencyUID        string            `json:"currency_uid"`
	Amount             string            `json:"amount"`
	IdempotencyKey     string            `json:"idempotency_key"`
	ChannelName        string            `json:"channel_name"`
	Metadata           map[string]string `json:"metadata"`
	ExpiresAt          string            `json:"expires_at"`
}

type transitionRequest struct {
	ToStatus   string            `json:"to_status"`
	ChannelRef string            `json:"channel_ref"`
	Amount     string            `json:"amount"`
	Metadata   map[string]string `json:"metadata"`
	ActorID    int64             `json:"actor_id"`
	Source     string            `json:"source"`
	// IdempotencyKey is required (I-3, core.TransitionInput.Validate).
	// Callers may instead pass it via the Idempotency-Key header
	// (idempotencyHeaderAliasMiddleware injects it here before decoding).
	IdempotencyKey string `json:"idempotency_key"`
}

type bookingResponse struct {
	UID               string            `json:"uid"`
	ClassificationUID string            `json:"classification_uid"`
	AccountHolder     int64             `json:"account_holder"`
	CurrencyUID       string            `json:"currency_uid"`
	Amount            string            `json:"amount"`
	SettledAmount     string            `json:"settled_amount"`
	Status            string            `json:"status"`
	ChannelName       string            `json:"channel_name"`
	ChannelRef        string            `json:"channel_ref"`
	ReservationUID    string            `json:"reservation_uid,omitempty"`
	JournalUID        string            `json:"journal_uid,omitempty"`
	IdempotencyKey    string            `json:"idempotency_key"`
	Metadata          map[string]string `json:"metadata"`
	// ExpiresAt is a pointer, without omitempty, so a booking with no
	// expiry serializes as a literal `"expires_at": null` (H-M2). It used to
	// be a plain string left at "" in that case, which is a third state --
	// "", null, or a timestamp -- that no date parser accepts and no
	// generated client can model, on the ordinary path of every booking
	// created without an expiry. The spec's matching declaration is
	// `oneOf: [null, Timestamp]`, still inside `required`: the key is always
	// present, its value is null or an RFC3339 UTC instant.
	ExpiresAt *string `json:"expires_at"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

type eventResponse struct {
	UID                string            `json:"uid"`
	ClassificationCode string            `json:"classification_code"`
	BookingUID         string            `json:"booking_uid"`
	AccountHolder      int64             `json:"account_holder"`
	CurrencyUID        string            `json:"currency_uid"`
	FromStatus         string            `json:"from_status"`
	ToStatus           string            `json:"to_status"`
	Amount             string            `json:"amount"`
	SettledAmount      string            `json:"settled_amount"`
	JournalUID         string            `json:"journal_uid,omitempty"`
	Metadata           map[string]string `json:"metadata"`
	OccurredAt         string            `json:"occurred_at"`
	ActorID            int64             `json:"actor_id"`
	Source             string            `json:"source"`
}

// --- Conversion helpers ---

func bookingToResponse(op *core.Booking) bookingResponse {
	resp := bookingResponse{
		UID:               op.UID,
		ClassificationUID: op.ClassificationUID,
		AccountHolder:     op.AccountHolder,
		CurrencyUID:       op.CurrencyUID,
		Amount:            op.Amount.String(),
		SettledAmount:     op.SettledAmount.String(),
		Status:            string(op.Status),
		ChannelName:       op.ChannelName,
		ChannelRef:        op.ChannelRef,
		ReservationUID:    op.ReservationUID,
		JournalUID:        op.JournalUID,
		IdempotencyKey:    op.IdempotencyKey,
		Metadata:          op.Metadata,
	}
	if !op.ExpiresAt.IsZero() {
		expiresAt := op.ExpiresAt.UTC().Format(time.RFC3339)
		resp.ExpiresAt = &expiresAt
	}
	resp.CreatedAt = op.CreatedAt.UTC().Format(time.RFC3339)
	resp.UpdatedAt = op.UpdatedAt.UTC().Format(time.RFC3339)
	return resp
}

func eventToResponse(evt *core.Event) eventResponse {
	return eventResponse{
		UID:                evt.UID,
		ClassificationCode: evt.ClassificationCode,
		BookingUID:         evt.BookingUID,
		AccountHolder:      evt.AccountHolder,
		CurrencyUID:        evt.CurrencyUID,
		FromStatus:         string(evt.FromStatus),
		ToStatus:           string(evt.ToStatus),
		Amount:             evt.Amount.String(),
		SettledAmount:      evt.SettledAmount.String(),
		JournalUID:         evt.JournalUID,
		Metadata:           evt.Metadata,
		OccurredAt:         evt.OccurredAt.UTC().Format(time.RFC3339),
		ActorID:            evt.ActorID,
		Source:             evt.Source,
	}
}

// --- Handlers ---

func (s *Server) handleCreateBooking(w http.ResponseWriter, r *http.Request) {
	req, err := httpx.Decode[createBookingRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	amount, err := parseWireAmount(req.Amount, "amount")
	if err != nil {
		httpx.Error(w, err)
		return
	}

	var expiresAt time.Time
	if req.ExpiresAt != "" {
		expiresAt, err = time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			httpx.Error(w, httpx.ErrField("expires_at", "must be an RFC3339 timestamp, e.g. 2026-09-02T12:00:00Z"))
			return
		}
	}

	input := core.CreateBookingInput{
		ClassificationCode: req.ClassificationCode,
		AccountHolder:      req.AccountHolder,
		CurrencyUID:        req.CurrencyUID,
		Amount:             amount,
		IdempotencyKey:     req.IdempotencyKey,
		ChannelName:        req.ChannelName,
		Metadata:           req.Metadata,
		ExpiresAt:          expiresAt,
	}

	op, err := s.booker.CreateBooking(r.Context(), input)
	if err != nil {
		if isMissingLifecycleErr(err) {
			// F-M1 (2026-09-03 consumer audit): this is a caller-fixable
			// configuration problem -- the out-of-the-box `deposit`
			// classification ships label-only (README "Tier 2 -- With
			// Built-in Presets": InstallExtendedPresets installs accounting
			// templates, not lifecycles), and CreateBooking has no way to
			// create a booking against a classification with no lifecycle
			// attached -- not "internal error". postgres.BookingStore
			// returns this as a plain, unwrapped error (no core.ErrXxx
			// sentinel to switch on), so it fell through resolveError's
			// default case to 500/19999 with a text that gave the caller
			// nothing to act on; api.md's error table marks 500 as
			// Retryable, so a conformant client retried a request that
			// could never succeed. Detected by message match rather than
			// errors.Is because postgres/booking_store.go (where the
			// sentinel would need to live) is outside this task's file
			// scope -- see the same string in
			// postgres/booking_store.go's "has no lifecycle" error.
			//
			// httpx.ErrField, not ErrBadRequest: a plain AppError's Message
			// is server-log-only (httpx.Error renders message.text on the
			// wire from bizcode.DisplayMessage(code), a static per-code
			// string) -- ErrField is what api-contract.md §1's
			// message.fields exists for, and the only shortcut constructor
			// that puts caller-specific text where the caller can read it.
			httpx.Error(w, httpx.ErrField("classification_code",
				"classification \""+req.ClassificationCode+"\" has no lifecycle attached, so no booking "+
					"can be created against it -- call ClassificationStore.SetLifecycleIfEmpty(ctx, uid, "+
					"lifecycle) first (README \"Add a custom lifecycle\", or presets.DepositLifecycle / "+
					"presets.WithdrawalLifecycle for the out-of-the-box deposit/withdraw classifications)"))
			return
		}
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, bookingToResponse(op))
}

// isMissingLifecycleErr reports whether err is postgres.BookingStore's
// "classification has no lifecycle" error -- see the comment where this is
// called.
func isMissingLifecycleErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "has no lifecycle")
}

func (s *Server) handleTransition(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid booking uid"))
		return
	}

	req, err := httpx.Decode[transitionRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	var amount decimal.Decimal
	if req.Amount != "" {
		amount, err = parseWireAmount(req.Amount, "amount")
		if err != nil {
			httpx.Error(w, err)
			return
		}
	}

	input := core.TransitionInput{
		BookingUID:     uid,
		ToStatus:       core.Status(req.ToStatus),
		ChannelRef:     req.ChannelRef,
		Amount:         amount,
		Metadata:       req.Metadata,
		ActorID:        req.ActorID,
		Source:         req.Source,
		IdempotencyKey: req.IdempotencyKey,
	}

	evt, err := s.booker.Transition(r.Context(), input)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, eventToResponse(evt))
}

func (s *Server) handleGetBooking(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid booking uid"))
		return
	}

	op, err := s.bookingReader.GetBooking(r.Context(), uid)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, bookingToResponse(op))
}

func (s *Server) handleListBookings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var holder int64
	if h := q.Get("holder"); h != "" {
		var err error
		holder, err = strconv.ParseInt(h, 10, 64)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("holder must be a number"))
			return
		}
	}

	status := q.Get("status")
	limit := parsePageLimit(r)

	filter := core.BookingFilter{
		AccountHolder:     holder,
		ClassificationUID: q.Get("classification_uid"),
		Status:            status,
		Cursor:            q.Get("cursor"),
		Limit:             int(limit),
	}

	bookings, nextCursor, err := s.bookingReader.ListBookings(r.Context(), filter)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	resp := PagedResponse[bookingResponse]{
		List:       make([]bookingResponse, len(bookings)),
		NextCursor: cursorPtr(nextCursor),
	}
	for i, op := range bookings {
		resp.List[i] = bookingToResponse(&op)
	}
	httpx.OK(w, resp)
}
