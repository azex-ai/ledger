package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// TestIdempotencyAlias_IgnoresParamsKey is H-m10's pin.
//
// idempotencyHeaderAliasMiddleware used to redirect the injected key into a
// nested "params" object whenever the body had one -- documented as "template
// execution nests the key under params", a shape that no longer exists
// (postTemplateRequest carries idempotency_key flat, and core's
// TemplateExecutionRequest with a params field has no HTTP route). The branch
// was unconditional, so any POST body that merely CONTAINED a top-level
// object named "params" had its Idempotency-Key buried where the handler
// does not look, and the request failed with "idempotency_key is required".
//
// Before the fix this test fails with 400; after it, the key lands at the
// top level and the write goes through with the caller's key.
func TestIdempotencyAlias_IgnoresParamsKey(t *testing.T) {
	captured := ""
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{postFn: func(_ context.Context, input core.JournalInput) (*core.Journal, error) {
			captured = input.IdempotencyKey
			return &core.Journal{UID: "uid-1", IdempotencyKey: input.IdempotencyKey}, nil
		}}
	})

	body := map[string]any{
		"journal_type_uid": "jt-1",
		// An unrelated object that happens to be named "params" -- e.g. a
		// caller echoing its own request context back through metadata-ish
		// fields. It must not change where the header alias is injected.
		"params": map[string]any{"anything": "at all"},
		"entries": []map[string]any{
			{"account_holder": 1, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "debit", "amount": "5"},
			{"account_holder": -1, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "credit", "amount": "5"},
		},
	}

	w := doRequestWithHeader(srv, http.MethodPost, "/api/v1/journals", body, "Idempotency-Key", "hdr-key-params")
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, "hdr-key-params", captured,
		"the Idempotency-Key header must be injected at the top level regardless of an unrelated params object in the body")
}
