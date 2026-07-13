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

// depositClassificationCode is the Booking.ClassificationCode every crypto
// deposit booking is created against (see examples/crypto-deposit,
// presets.DepositLifecycle). handleWebhookCallback uses it to confine the
// legacy booking_uid transition path to deposit bookings only (design doc
// §5-5) -- an onchain channel's leaked HMAC key must never let an attacker
// forge a transition on an unrelated booking, most importantly a `sweep`
// booking (no journal, so a forged "confirmed" leaves no accounting trace).
const depositClassificationCode = "deposit"

// DepositAddressProvider is the address-issuance half of the crypto-deposit
// orchestration (service.OnchainService --
// docs/plans/2026-07-11-crypto-deposit-sweep-design.md §2). Optional: nil
// until SetDepositAddressProvider is called; both routes below then answer
// bizcode.FeatureNotEnabled. Modeled after the existing WebhookNonceRecorder
// optional-dependency pattern (see handler_webhooks.go).
type DepositAddressProvider interface {
	// EnsureDepositAddress derives (CREATE2, on first call) and registers a
	// holder's custody address, then returns it. Idempotent -- repeated
	// calls for the same holder always return the same address.
	EnsureDepositAddress(ctx context.Context, holder int64) (*core.DepositAddress, error)
	// GetDepositAddress looks up an already-registered address without
	// creating one. Returns an error satisfying errors.Is(err,
	// core.ErrNotFound) when the holder has none yet.
	GetDepositAddress(ctx context.Context, holder int64) (*core.DepositAddress, error)
}

// SetDepositAddressProvider installs the crypto-deposit address issuance
// service. Pass nil (the default) to leave POST/GET
// /holders/{holder}/deposit-address answering bizcode.FeatureNotEnabled.
func (s *Server) SetDepositAddressProvider(p DepositAddressProvider) { s.depositAddresses = p }

// DepositIngester is the sighting-ingestion half of the crypto-deposit
// orchestration (design doc §3): both the chains/evm watcher (pull, out of
// process) and the onchain webhook bridge (push, see handleWebhookCallback)
// converge on this single entry point. Optional: nil until
// SetDepositIngester is called.
type DepositIngester interface {
	IngestDeposit(ctx context.Context, sighting core.DepositSighting) (*core.Booking, error)
}

// SetDepositIngester installs the crypto-deposit sighting-ingestion service.
// Pass nil (the default) to leave the onchain webhook channel answering
// bizcode.FeatureNotEnabled instead of ingesting sightings.
func (s *Server) SetDepositIngester(i DepositIngester) { s.depositIngester = i }

// sightingParser is implemented by channel adapters that can normalize their
// webhook payload directly into a core.DepositSighting (channel/onchain's
// EVMAdapter.ParseSighting), as opposed to the classic "transition a
// pre-existing booking_uid" shape (channel.Adapter.ParseCallback).
// handleWebhookCallback prefers this path when the resolved adapter offers
// it -- see design doc §3: watcher (pull) and webhook (push) converge on the
// same IngestDeposit orchestration.
type sightingParser interface {
	ParseSighting(header http.Header, body []byte) (*core.DepositSighting, error)
}

// depositAddressResponse is deliberately narrower than core.DepositAddress:
// Factory/InitHash are the CREATE2 derivation fingerprint, an internal audit
// detail -- not something a caller needs to spend or verify a deposit
// (~/.claude/rules/user-facing-surfaces.md: implementation detail stays off
// the wire). account_holder is not an internal id here -- it's the same
// caller-supplied identifier used throughout the rest of this API
// (/balances/{holder}, /bookings?holder=...).
type depositAddressResponse struct {
	UID           string `json:"uid"`
	AccountHolder int64  `json:"account_holder"`
	Address       string `json:"address"`
	CreatedAt     string `json:"created_at"`
}

func depositAddressToResponse(a *core.DepositAddress) depositAddressResponse {
	return depositAddressResponse{
		UID:           a.UID,
		AccountHolder: a.AccountHolder,
		Address:       a.Address,
		CreatedAt:     a.CreatedAt.Format(time.RFC3339),
	}
}

// handleEnsureDepositAddress issues (idempotently) the holder's CREATE2
// custody address, deriving + registering it on first call.
func (s *Server) handleEnsureDepositAddress(w http.ResponseWriter, r *http.Request) {
	holder, err := parseIDParam(chi.URLParam(r, "holder"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid holder ID"))
		return
	}
	if s.depositAddresses == nil {
		httpx.Error(w, bizcode.FeatureNotEnabled)
		return
	}

	addr, err := s.depositAddresses.EnsureDepositAddress(r.Context(), holder)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, depositAddressToResponse(addr))
}

// handleGetDepositAddress looks up a holder's already-registered custody
// address without creating one (404 if absent).
func (s *Server) handleGetDepositAddress(w http.ResponseWriter, r *http.Request) {
	holder, err := parseIDParam(chi.URLParam(r, "holder"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid holder ID"))
		return
	}
	if s.depositAddresses == nil {
		httpx.Error(w, bizcode.FeatureNotEnabled)
		return
	}

	addr, err := s.depositAddresses.GetDepositAddress(r.Context(), holder)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, depositAddressToResponse(addr))
}
