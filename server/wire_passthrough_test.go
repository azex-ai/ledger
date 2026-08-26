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
