package server

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/bizcode"
	"github.com/azex-ai/ledger/pkg/httpx"
)

// DeadLetterService is the operator surface over the deposit-ingest
// dead-letter queue: the sightings the pull path refused to book and then
// scanned past (core.IngestDeadLetter). Optional -- nil until
// SetDeadLetterService is called, after which both /deposits/dead-letters
// routes answer normally instead of bizcode.FeatureNotEnabled. Implemented by
// *service.Onchain, same as DepositReviewer.
//
// Why the replay lives HERE and not in `ledger-cli` (C-2,
// docs/audits/2026-09-03-independent-review/onchain-ops.md): re-driving a
// sighting means calling IngestDeposit, which needs the chain set -- a
// token's currency code and its auto-credit ceilings are Go configuration in
// the consumer's composition root, not rows in the database. A tool holding
// only DATABASE_URL cannot reconstruct them, and asking an operator to
// re-supply the ceilings on a command line at 3am would put the mint bound
// on the wrong side of the keyboard. So the CLI lists and shows, and the
// replay is an endpoint served by the process that already holds the
// configuration.
type DeadLetterService interface {
	// ListDeadLetters returns a page of dead letters, newest first ("" cursor
	// = first page, "" next-cursor = exhausted).
	ListDeadLetters(ctx context.Context, cursor string, limit int32) (letters []core.IngestDeadLetter, nextCursor string, err error)
	// ReplayDeadLetter re-drives one dead letter's sighting through the real
	// ingestion path. Idempotent: a sighting already booked resolves to the
	// same booking and posts nothing new.
	ReplayDeadLetter(ctx context.Context, uid string) (*core.Booking, error)
}

// SetDeadLetterService installs the dead-letter operator surface. Pass nil
// (the default) to leave both /deposits/dead-letters routes answering
// bizcode.FeatureNotEnabled.
func (s *Server) SetDeadLetterService(d DeadLetterService) { s.deadLetters = d }

// deadLetterResponse is the wire shape of one dead letter. `booked` is not
// stored anywhere -- it is recomputed from bookings on every read (see
// core.IngestDeadLetter.Booked), which is what lets this queue clear itself
// when a deposit is credited in the end.
type deadLetterResponse struct {
	UID            string `json:"uid"`
	ChainID        int64  `json:"chain_id"`
	TxHash         string `json:"tx_hash"`
	TxLogSeq       int32  `json:"txlog_seq"`
	IdempotencyKey string `json:"idempotency_key"`
	Reason         string `json:"reason"`
	Booked         bool   `json:"booked"`
	Token          string `json:"token"`
	To             string `json:"to"`
	// Amount is the sighting's amount as recorded, a decimal string
	// (api-contract.md §4) -- what the ledger would have credited.
	Amount    string `json:"amount"`
	CreatedAt string `json:"created_at"`
}

func deadLetterToResponse(dl core.IngestDeadLetter) deadLetterResponse {
	return deadLetterResponse{
		UID:            dl.UID,
		ChainID:        dl.ChainID,
		TxHash:         dl.TxHash,
		TxLogSeq:       dl.TxLogSeq,
		IdempotencyKey: dl.IdempotencyKey,
		Reason:         dl.Reason,
		Booked:         dl.Booked,
		Token:          dl.Sighting.Token,
		To:             dl.Sighting.To,
		Amount:         dl.Sighting.Amount.String(),
		CreatedAt:      dl.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// handleListDeadLetters lists dead-lettered deposit sightings, newest first
// (docs/RUNBOOK.md §18) -- the queue an on-call engineer works when
// ledger_deposit_ingest_dead_lettered_total moves.
func (s *Server) handleListDeadLetters(w http.ResponseWriter, r *http.Request) {
	if s.deadLetters == nil {
		httpx.Error(w, bizcode.FeatureNotEnabled)
		return
	}

	letters, nextCursor, err := s.deadLetters.ListDeadLetters(r.Context(), r.URL.Query().Get("cursor"), parsePageLimit(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}

	resp := PagedResponse[deadLetterResponse]{
		List:       make([]deadLetterResponse, len(letters)),
		NextCursor: cursorPtr(nextCursor),
	}
	for i, dl := range letters {
		resp.List[i] = deadLetterToResponse(dl)
	}
	httpx.OK(w, resp)
}

// handleReplayDeadLetter re-drives one dead-lettered sighting through
// IngestDeposit and returns the resulting booking.
//
// Gated on CapabilityDepositReview, not ScopeWrite: a replay ends in the
// same deposit_confirm journal an approval does (it runs the identical
// ingestion path, review gate included), so the key that can forge a
// sighting must not also be able to push one back into the ledger -- the
// same reasoning routes.go gives for approve/reject.
func (s *Server) handleReplayDeadLetter(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid dead letter uid"))
		return
	}
	if s.deadLetters == nil {
		httpx.Error(w, bizcode.FeatureNotEnabled)
		return
	}

	booking, err := s.deadLetters.ReplayDeadLetter(r.Context(), uid)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, bookingToResponse(booking))
}
