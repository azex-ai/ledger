package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// This file pins two fields structure.md's Q3 comparator (see
// openapi_contract_test.go) found undocumented-by-absence: docs/openapi.yaml
// documented them, but the HTTP handler never read them into the domain
// input, so a caller following the docs got them silently dropped -- the
// same defect class as the expires_in bug, just on TransitionInput.Source
// and JournalInput.EffectiveAt instead of ReserveInput.ExpiresIn.

// TestTransition_SourcePassesThrough pins TransitionInput.Source.
func TestTransition_SourcePassesThrough(t *testing.T) {
	var got core.TransitionInput
	srv := newTestServerWith(func(o *testServerOpts) {
		o.booker = &mockBooker{
			transitionFn: func(_ context.Context, input core.TransitionInput) (*core.Event, error) {
				got = input
				return &core.Event{UID: "evt-1", BookingUID: input.BookingUID, ToStatus: input.ToStatus, OccurredAt: time.Now()}, nil
			},
		}
	})

	body := map[string]any{
		"to_status":       "confirmed",
		"source":          "webhook",
		"idempotency_key": "trans-source-1",
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/bookings/bk-1/transition", body)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "webhook", got.Source)
}

// TestPostJournal_EffectiveAtPassesThrough pins JournalInput.EffectiveAt.
func TestPostJournal_EffectiveAtPassesThrough(t *testing.T) {
	var got core.JournalInput
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			postFn: func(_ context.Context, input core.JournalInput) (*core.Journal, error) {
				got = input
				return &core.Journal{UID: "j-1", IdempotencyKey: input.IdempotencyKey, EffectiveAt: input.EffectiveAt}, nil
			},
		}
	})

	body := map[string]any{
		"journal_type_uid": "jt-1",
		"idempotency_key":  "journal-eff-1",
		"effective_at":     "2026-01-15T10:00:00Z",
		"entries": []map[string]any{
			{"account_holder": 100, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "debit", "amount": "10"},
			{"account_holder": -100, "currency_uid": "cur-1", "classification_uid": "cls-2", "entry_type": "credit", "amount": "10"},
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/journals", body)
	require.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "2026-01-15T10:00:00Z", got.EffectiveAt.Format(time.RFC3339))
}

// TestPostJournal_RejectsMalformedEffectiveAt pins the validation path
// alongside the passthrough: a non-RFC3339 value is a 400, not a silently
// ignored / zeroed field.
func TestPostJournal_RejectsMalformedEffectiveAt(t *testing.T) {
	srv := newTestServer()
	body := map[string]any{
		"journal_type_uid": "jt-1",
		"idempotency_key":  "journal-eff-bad",
		"effective_at":     "not-a-date",
		"entries": []map[string]any{
			{"account_holder": 100, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "debit", "amount": "10"},
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/journals", body)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestCreateBooking_DepositMetadataPassesThrough is the HTTP half of
// money-out N-1's reachability question (the re-check's §5.1, which named
// this path as the one most worth measuring and did not measure it).
//
// POST /api/v1/bookings takes a WRITE-scope API key -- not a database
// credential -- and this pins what the handler does with such a request:
// classification_code, account_holder, amount, channel_name and the whole
// metadata map arrive in core.CreateBookingInput unchanged. So a write-scope
// key can file a booking on the `deposit` classification carrying any
// on-chain identity it likes, and the recheck job does not distinguish where
// a booking came from.
//
// That is not a defect in this handler -- a booking API that could not take
// metadata would be useless -- it is the reason the fences live where they
// do: the deposit-identity index (migration 032) and the pre-credit
// corroboration (I-69/I-71), both of which sit BELOW this endpoint and apply
// to every caller. service.TestDepositIdentity_ACallerSuppliedBookingCannotStealATransfer
// is the other half.
func TestCreateBooking_DepositMetadataPassesThrough(t *testing.T) {
	var got core.CreateBookingInput
	srv := newTestServerWith(func(o *testServerOpts) {
		o.booker = &mockBooker{
			createFn: func(_ context.Context, input core.CreateBookingInput) (*core.Booking, error) {
				got = input
				return &core.Booking{
					UID: "bk-1", ClassificationUID: "cls-1", AccountHolder: input.AccountHolder,
					CurrencyUID: input.CurrencyUID, Amount: input.Amount, Status: "pending",
					ChannelName: input.ChannelName, IdempotencyKey: input.IdempotencyKey,
					Metadata: input.Metadata, CreatedAt: time.Now(), UpdatedAt: time.Now(),
				}, nil
			},
		}
	})

	body := map[string]any{
		"classification_code": "deposit",
		"account_holder":      4242,
		"currency_uid":        "cur-1",
		"amount":              "999",
		"idempotency_key":     "caller-chosen-1",
		"channel_name":        "anything-i-like",
		"metadata": map[string]string{
			"chain_id": "1", "tx_hash": "0xcallerchosen", "txlog_seq": "0",
			"token": "0xtoken", "block_number": "100",
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/bookings", body)
	require.Equal(t, http.StatusCreated, w.Code)

	assert.Equal(t, "deposit", got.ClassificationCode, "the caller names the classification")
	assert.Equal(t, int64(4242), got.AccountHolder)
	assert.Equal(t, "999", got.Amount.String(), "and the amount")
	assert.Equal(t, "anything-i-like", got.ChannelName, "and the channel name")
	assert.Equal(t, "0xcallerchosen", got.Metadata["tx_hash"],
		"and every metadata key, including the ones the deposit path derives a booking's identity from")
	assert.Equal(t, "0", got.Metadata["txlog_seq"])
	assert.Equal(t, "1", got.Metadata["chain_id"])
}
