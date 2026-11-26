package core

import (
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Booking is the unified record replacing Deposit/Withdrawal.
// Its lifecycle is governed by the classification's state machine.
type Booking struct {
	UID               string          `json:"uid"`
	ClassificationUID string          `json:"classification_uid"`
	AccountHolder     int64           `json:"account_holder"`
	CurrencyUID       string          `json:"currency_uid"`
	Amount            decimal.Decimal `json:"amount"`
	SettledAmount     decimal.Decimal `json:"settled_amount"`
	Status            Status          `json:"status"`
	ChannelName       string          `json:"channel_name"`
	ChannelRef        string          `json:"channel_ref"`
	// ReservationUID and JournalUID are empty when "no reservation /
	// journal linked yet". Sentinel 0 cannot be used because Postgres needs a
	// real NULL to skip FK enforcement.
	ReservationUID string            `json:"reservation_uid,omitempty"`
	JournalUID     string            `json:"journal_uid,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
	Metadata       map[string]string `json:"metadata"`
	ExpiresAt      time.Time         `json:"expires_at"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

// Idempotency-key namespaces the ledger derives its own keys in. Idempotency
// keys are table-wide UNIQUE (bookings, transition receipts, journals,
// reservations, ...), and the crypto-deposit add-on, the webhook handler,
// the expiration worker, the pending-timeout sweeper and ReverseJournal mint
// keys from public or enumerable values (chain id, tx hash, token, the hot
// signer's pending nonce, booking / reservation / journal uid, the sequential
// journal id). A caller that wrote anything under such a key first would make
// the system's own write hit ErrConflict for good -- a sweep saga stalled, a
// deposit never confirmed, an expiry never applied, an expired pending never
// cancelled, a reorg auto-reversal silently skipped -- so the HTTP layer
// refuses these prefixes on every write route (IsSystemIdempotencyKey).
//
// The derivation sites that build their keys from these constants:
// service/onchain.go (deposit ingest / transition / confirm, the sweep saga),
// server/handler_webhooks.go, service/expiration.go,
// postgres/pending_store.go (ExpirePending) and postgres/ledger_store.go
// (ReverseJournal). A new system-derived key must get a constant here, not a
// literal, or it is claimable over HTTP. Keys derived as a suffix of a
// caller's own key (Exchange, Capture, the tolerance preset) need none: the
// caller chose the prefix.
const (
	IdempotencyPrefixDeposit       = "deposit-"
	IdempotencyPrefixSweep         = "sweep-"
	IdempotencyPrefixWebhook       = "webhook-"
	IdempotencyPrefixExpire        = "expire-"
	IdempotencyPrefixPendingExpire = "pending:expire:"
	IdempotencyPrefixReversal      = "reversal:"
)

// IsSystemIdempotencyKey reports whether key falls in a namespace the ledger
// derives its own idempotency keys in (see IdempotencyPrefixDeposit).
func IsSystemIdempotencyKey(key string) bool {
	return strings.HasPrefix(key, IdempotencyPrefixDeposit) ||
		strings.HasPrefix(key, IdempotencyPrefixSweep) ||
		strings.HasPrefix(key, IdempotencyPrefixWebhook) ||
		strings.HasPrefix(key, IdempotencyPrefixExpire) ||
		strings.HasPrefix(key, IdempotencyPrefixPendingExpire) ||
		strings.HasPrefix(key, IdempotencyPrefixReversal)
}

// CreateBookingInput is the input to create a new booking.
type CreateBookingInput struct {
	ClassificationCode string            `json:"classification_code"`
	AccountHolder      int64             `json:"account_holder"`
	CurrencyUID        string            `json:"currency_uid"`
	Amount             decimal.Decimal   `json:"amount"`
	IdempotencyKey     string            `json:"idempotency_key"`
	ChannelName        string            `json:"channel_name"`
	Metadata           map[string]string `json:"metadata"`
	ExpiresAt          time.Time         `json:"expires_at"`
}

func (i CreateBookingInput) Validate() error {
	if err := ValidateAmountMagnitude("create booking", "amount", i.Amount); err != nil {
		return err
	}
	if i.ClassificationCode == "" {
		return fmt.Errorf("core: booking: classification_code required: %w", ErrInvalidInput)
	}
	if i.AccountHolder == 0 {
		return fmt.Errorf("core: booking: account_holder required: %w", ErrInvalidInput)
	}
	if i.CurrencyUID == "" {
		return fmt.Errorf("core: booking: currency_uid required: %w", ErrInvalidInput)
	}
	if !i.Amount.IsPositive() {
		return fmt.Errorf("core: booking: amount must be positive: %w", ErrInvalidInput)
	}
	if err := validateFreeformFields("booking", "", i.Metadata); err != nil {
		return err
	}
	if _, err := NormalizeDepositIdentityMetadata(i.Metadata); err != nil {
		return err
	}
	if i.IdempotencyKey == "" {
		return fmt.Errorf("core: booking: idempotency key required: %w", ErrInvalidInput)
	}
	return nil
}

// NormalizeDepositIdentityMetadata gives a booking's on-chain deposit
// identity (I-71) one spelling. When metadata carries all three of chain_id,
// tx_hash and txlog_seq -- the rows uq_bookings_deposit_identity constrains --
// it returns metadata with those values rewritten to the canonical form:
// tx_hash lowercased with a "0x" prefix, chain_id and txlog_seq as
// strconv.FormatInt prints them. The unique index and the sibling lookup
// compare this text verbatim, while the chain-side checks (ParseInt,
// HexToHash, EqualFold) read "0xAB.."/"0Xab.." and "01"/"+1" as the same
// log; storing one spelling is what makes a second spelling of a real
// transfer collide with the index instead of being credited again. Hashes
// are case-insensitive identifiers, so a different case is the same value
// and is normalized, not refused. A value that is not one -- a tx_hash
// without a 0x/0X prefix, a chain_id or txlog_seq
// that is not a base-10 integer -- is ErrInvalidInput; nothing is guessed.
//
// md is never mutated: when a rewrite is needed the result is a copy,
// otherwise md itself is returned. BookingStore applies this on create
// (before the idempotency comparison and the INSERT) and to the merged
// metadata of every Transition, the two places booking metadata is written;
// CreateBookingInput.Validate calls it only for the refusal.
func NormalizeDepositIdentityMetadata(md map[string]string) (map[string]string, error) {
	chainID, hasChain := md["chain_id"]
	txHash, hasTx := md["tx_hash"]
	txLogSeq, hasSeq := md["txlog_seq"]
	if !hasChain || !hasTx || !hasSeq {
		return md, nil
	}
	canonHash, ok := canonicalTxHash(txHash)
	if !ok {
		return nil, fmt.Errorf("core: booking: metadata tx_hash %q must have a 0x prefix: %w", txHash, ErrInvalidInput)
	}
	canon := map[string]string{"tx_hash": canonHash}
	for _, f := range [...]struct{ name, value string }{{"chain_id", chainID}, {"txlog_seq", txLogSeq}} {
		n, err := strconv.ParseInt(f.value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("core: booking: metadata %s %q must be a base-10 integer: %w", f.name, f.value, ErrInvalidInput)
		}
		canon[f.name] = strconv.FormatInt(n, 10)
	}
	if canon["tx_hash"] == txHash && canon["chain_id"] == chainID && canon["txlog_seq"] == txLogSeq {
		return md, nil
	}
	out := maps.Clone(md)
	maps.Copy(out, canon)
	return out, nil
}

// canonicalTxHash lowercases a "0x"/"0X"-prefixed string; ok is false
// without the prefix. The digits are not checked for hex and there is no
// length requirement: the uniqueness argument needs neither (every spelling
// the chain side reads as one hash lowercases to one string), the refusal
// set before normalization never included them, and IngestDeposit's
// fixtures use short non-hex placeholders such as "0xreorgtx".
func canonicalTxHash(h string) (string, bool) {
	if len(h) < 2 || h[0] != '0' || (h[1] != 'x' && h[1] != 'X') {
		return "", false
	}
	return "0x" + strings.ToLower(h[2:]), true
}

// TransitionInput is the input to advance a booking's state.
type TransitionInput struct {
	BookingUID string `json:"booking_uid"`
	ToStatus   Status `json:"to_status"`
	ChannelRef string `json:"channel_ref"`
	// Amount is intentionally allowed to be zero: not every lifecycle
	// transition moves money (e.g. a pure status change like
	// "reviewing" -> "processing"). Amount is only required to be positive
	// where a transition actually triggers accounting — that is enforced by
	// JournalInput.Validate at the point a journal is composed for the
	// transition, not here.
	Amount   decimal.Decimal   `json:"amount"`
	Metadata map[string]string `json:"metadata"`
	ActorID  int64             `json:"actor_id"`
	// Source identifies the calling service or scope (e.g. "api", "worker", "webhook").
	Source string `json:"source"`
	// IdempotencyKey is REQUIRED, like every other write in this package
	// (I-3: every state-changing operation requires a key). It is checked
	// under the booking's row lock, before the lifecycle transition gate,
	// against a durable booking_transition_receipts row (booking, to_status,
	// channel_ref, amount, and the event_id the original call produced): a
	// replay with the same key and the same payload always returns the
	// original event, even after the booking has since moved on to a later
	// status; the same key with a different payload is ErrConflict.
	//
	// Deriving the key is the caller's responsibility, and it is not simply
	// "booking uid + to_status": a booking's lifecycle can legitimately
	// revisit the same status more than once (e.g. the withdrawal preset's
	// failed -> reserved retry edge, or the sweep preset's failed -> pending
	// revival edge). A key that ignores which *occurrence* of that status a
	// call is describing collides across two genuinely different, legitimate
	// transitions -- the second one would silently short-circuit to the
	// first one's receipt instead of applying. The key must be deterministic
	// across retries of the exact same source event (system-driven callers:
	// derive from that event's own identity -- an upstream tx hash, event
	// id, or attempt count) or a client-generated random UUID carried on the
	// caller's Idempotency-Key header (api-contract.md §9). See
	// service/onchain.go's Transition call sites for worked examples.
	//
	// Separately from this key, Transition also has a narrower,
	// state-comparison-based idempotency path for calls that race to apply
	// the *same* occurrence concurrently under two different (but both
	// freshly-derived) keys: when the booking is already AT ToStatus with no
	// matching receipt for this call's key, it compares the latest event's
	// payload against this call and either returns that event (matching
	// payload) or ErrConflict (diverging payload) -- see
	// idempotentTransitionEvent in postgres/booking_store.go. That path only
	// fires when nothing has moved the booking away from ToStatus since; a
	// later, distinct occurrence targeting the same status necessarily
	// passes through an intervening transition first, so it never mis-fires
	// on the retry-to-the-same-status case above.
	IdempotencyKey string `json:"idempotency_key"`
}

func (i TransitionInput) Validate() error {
	if err := ValidateAmountMagnitude("transition", "amount", i.Amount); err != nil {
		return err
	}
	if i.BookingUID == "" {
		return fmt.Errorf("core: booking: booking_uid required: %w", ErrInvalidInput)
	}
	if err := validateFreeformFields("transition", i.Source, i.Metadata); err != nil {
		return err
	}
	if i.ToStatus == "" {
		return fmt.Errorf("core: booking: to_status required: %w", ErrInvalidInput)
	}
	// Zero is deliberately allowed here (see TransitionInput.Amount doc) —
	// only negative amounts are a shape error at this layer.
	if i.Amount.IsNegative() {
		return fmt.Errorf("core: booking: amount must not be negative: %w", ErrInvalidInput)
	}
	if i.IdempotencyKey == "" {
		return fmt.Errorf("core: booking: idempotency key required: %w", ErrInvalidInput)
	}
	return nil
}

// BookingFilter is the filter for listing bookings.
type BookingFilter struct {
	AccountHolder     int64  `json:"account_holder"`
	ClassificationUID string `json:"classification_uid"`
	Status            string `json:"status"`
	// Cursor is the opaque keyset cursor from the previous page ("" = start).
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}
