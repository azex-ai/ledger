package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
)

// --- JSON request/response types ---

type createBookingRequest struct {
	ClassificationCode string         `json:"classification_code"`
	AccountHolder      int64          `json:"account_holder"`
	CurrencyID         int64          `json:"currency_id"`
	Amount             string         `json:"amount"`
	IdempotencyKey     string         `json:"idempotency_key"`
	ChannelName        string         `json:"channel_name"`
	Metadata           map[string]any `json:"metadata"`
	ExpiresAt          string         `json:"expires_at"`
}

type transitionRequest struct {
	ToStatus   string         `json:"to_status"`
	ChannelRef string         `json:"channel_ref"`
	Amount     string         `json:"amount"`
	Metadata   map[string]any `json:"metadata"`
	ActorID    int64          `json:"actor_id"`
}

type bookingResponse struct {
	ID               int64          `json:"id"`
	ClassificationID int64          `json:"classification_id"`
	AccountHolder    int64          `json:"account_holder"`
	CurrencyID       int64          `json:"currency_id"`
	Amount           string         `json:"amount"`
	SettledAmount    string         `json:"settled_amount"`
	Status           string         `json:"status"`
	ChannelName      string         `json:"channel_name"`
	ChannelRef       string         `json:"channel_ref"`
	ReservationID    *int64         `json:"reservation_id,omitempty"`
	JournalID        *int64         `json:"journal_id,omitempty"`
	IdempotencyKey   string         `json:"idempotency_key"`
	Metadata         map[string]any `json:"metadata"`
	ExpiresAt        string         `json:"expires_at"`
	CreatedAt        string         `json:"created_at"`
	UpdatedAt        string         `json:"updated_at"`
}

type eventResponse struct {
	ID                 int64          `json:"id"`
	ClassificationCode string         `json:"classification_code"`
	BookingID          int64          `json:"booking_id"`
	AccountHolder      int64          `json:"account_holder"`
	CurrencyID         int64          `json:"currency_id"`
	FromStatus         string         `json:"from_status"`
	ToStatus           string         `json:"to_status"`
	Amount             string         `json:"amount"`
	SettledAmount      string         `json:"settled_amount"`
	JournalID          *int64         `json:"journal_id,omitempty"`
	Metadata           map[string]any `json:"metadata"`
	OccurredAt         string         `json:"occurred_at"`
}

// --- Conversion helpers ---

func bookingToResponse(op *core.Booking) bookingResponse {
	resp := bookingResponse{
		ID:               op.ID,
		ClassificationID: op.ClassificationID,
		AccountHolder:    op.AccountHolder,
		CurrencyID:       op.CurrencyID,
		Amount:           op.Amount.String(),
		SettledAmount:    op.SettledAmount.String(),
		Status:           string(op.Status),
		ChannelName:      op.ChannelName,
		ChannelRef:       op.ChannelRef,
		ReservationID:    op.ReservationID,
		JournalID:        op.JournalID,
		IdempotencyKey:   op.IdempotencyKey,
		Metadata:         op.Metadata,
	}
	if !op.ExpiresAt.IsZero() {
		resp.ExpiresAt = op.ExpiresAt.Format(time.RFC3339)
	}
	resp.CreatedAt = op.CreatedAt.Format(time.RFC3339)
	resp.UpdatedAt = op.UpdatedAt.Format(time.RFC3339)
	return resp
}

func eventToResponse(evt *core.Event) eventResponse {
	return eventResponse{
		ID:                 evt.ID,
		ClassificationCode: evt.ClassificationCode,
		BookingID:          evt.BookingID,
		AccountHolder:      evt.AccountHolder,
		CurrencyID:         evt.CurrencyID,
		FromStatus:         string(evt.FromStatus),
		ToStatus:           string(evt.ToStatus),
		Amount:             evt.Amount.String(),
		SettledAmount:      evt.SettledAmount.String(),
		JournalID:          evt.JournalID,
		Metadata:           evt.Metadata,
		OccurredAt:         evt.OccurredAt.Format(time.RFC3339),
	}
}

// --- Handlers ---

func (s *Server) handleCreateBooking(w http.ResponseWriter, r *http.Request) {
	req, err := httpx.Decode[createBookingRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	amount, err := decimal.NewFromString(req.Amount)
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("amount is not a valid decimal"))
		return
	}

	var expiresAt time.Time
	if req.ExpiresAt != "" {
		expiresAt, err = time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("expires_at must be RFC3339 format"))
			return
		}
	}

	input := core.CreateBookingInput{
		ClassificationCode: req.ClassificationCode,
		AccountHolder:      req.AccountHolder,
		CurrencyID:         req.CurrencyID,
		Amount:             amount,
		IdempotencyKey:     req.IdempotencyKey,
		ChannelName:        req.ChannelName,
		Metadata:           req.Metadata,
		ExpiresAt:          expiresAt,
	}

	op, err := s.booker.CreateBooking(r.Context(), input)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, bookingToResponse(op))
}

func (s *Server) handleTransition(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid booking ID"))
		return
	}

	req, err := httpx.Decode[transitionRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	var amount decimal.Decimal
	if req.Amount != "" {
		amount, err = decimal.NewFromString(req.Amount)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("amount is not a valid decimal"))
			return
		}
	}

	input := core.TransitionInput{
		BookingID:  id,
		ToStatus:   core.Status(req.ToStatus),
		ChannelRef: req.ChannelRef,
		Amount:     amount,
		Metadata:   req.Metadata,
		ActorID:    req.ActorID,
	}

	evt, err := s.booker.Transition(r.Context(), input)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, eventToResponse(evt))
}

func (s *Server) handleGetBooking(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid booking ID"))
		return
	}

	op, err := s.bookingReader.GetBooking(r.Context(), id)
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

	var classificationID int64
	if c := q.Get("classification_id"); c != "" {
		var err error
		classificationID, err = strconv.ParseInt(c, 10, 64)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("classification_id must be a number"))
			return
		}
	}

	status := q.Get("status")

	cursor, err := decodeCursor(q.Get("cursor"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid cursor value"))
		return
	}

	limit := parsePageLimit(r)

	filter := core.BookingFilter{
		AccountHolder:    holder,
		ClassificationID: classificationID,
		Status:           status,
		Cursor:           cursor,
		Limit:            int(limit),
	}

	bookings, err := s.bookingReader.ListBookings(r.Context(), filter)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	resp := PagedResponse[bookingResponse]{
		Data: make([]bookingResponse, len(bookings)),
	}
	for i, op := range bookings {
		resp.Data[i] = bookingToResponse(&op)
	}
	if len(bookings) == int(limit) {
		resp.NextCursor = encodeCursor(bookings[len(bookings)-1].ID)
	}
	httpx.OK(w, resp)
}
