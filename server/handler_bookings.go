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
	ExpiresAt         string            `json:"expires_at"`
	CreatedAt         string            `json:"created_at"`
	UpdatedAt         string            `json:"updated_at"`
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
		resp.ExpiresAt = op.ExpiresAt.Format(time.RFC3339)
	}
	resp.CreatedAt = op.CreatedAt.Format(time.RFC3339)
	resp.UpdatedAt = op.UpdatedAt.Format(time.RFC3339)
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
		CurrencyUID:        req.CurrencyUID,
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
		amount, err = decimal.NewFromString(req.Amount)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("amount is not a valid decimal"))
			return
		}
	}

	input := core.TransitionInput{
		BookingUID: uid,
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
		NextCursor: nextCursor,
	}
	for i, op := range bookings {
		resp.List[i] = bookingToResponse(&op)
	}
	httpx.OK(w, resp)
}
