package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/server"
)

// newProtectedTemplateServer builds a server with an explicit
// ProtectedTemplateCodes list -- newTestServerWith hardcodes its config, so
// this constructs one directly with the same mock set (mirrors
// newDevCreditServer's pattern in handler_devcredit_test.go).
func newProtectedTemplateServer(protected []string, journals core.JournalWriter) *server.Server {
	return server.NewWithConfig(
		&server.Config{
			Env:                    "dev",
			CORSAllowOrigin:        "*",
			MaxBodyBytes:           256 * 1024,
			ProtectedTemplateCodes: protected,
		},
		journals,
		&mockBalanceReader{},
		&mockReserver{},
		&mockBooker{},
		&mockBookingReader{},
		&mockEventReader{},
		&mockClassificationStore{},
		&mockJournalTypeStore{},
		&mockTemplateStore{},
		&mockCurrencyStore{},
		nil,
		&mockReconciler{},
		&mockSnapshotter{},
		nil,
		&mockQueryProvider{},
		&mockAuditQuerier{},
		&mockPlatformBalanceReader{},
		&mockSolvencyChecker{},
		&mockBalanceTrendReader{},
		&mockFullReconciler{},
		&mockAccountPolicyStore{},
		&mockPeriodCloser{},
		&mockTrialBalanceReader{},
	)
}

func postTemplateBody(code string) map[string]any {
	return map[string]any{
		"template_code":   code,
		"holder_id":       42,
		"currency_uid":    "cur-1",
		"idempotency_key": "tmpl-test-1",
		"amounts":         map[string]string{"amount": "10"},
	}
}

// TestPostTemplate_ProtectedCodeIsRefused pins structure.md's Major: before
// Config.ProtectedTemplateCodes existed, POST /journals/template had no
// allowlist/denylist at all -- any write-scope key could post a journal
// under a template code like presets.DepositConfirmTemplateCode
// ("deposit_confirm"), indistinguishable from a real verified deposit.
func TestPostTemplate_ProtectedCodeIsRefused(t *testing.T) {
	srv := newProtectedTemplateServer([]string{"deposit_confirm", "deposit_confirm_pending"}, &mockJournalWriter{
		templateFn: func(context.Context, string, core.TemplateParams) (*core.Journal, error) {
			t.Fatal("ExecuteTemplate must not be called for a protected template code")
			return nil, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody("deposit_confirm"))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestPostTemplate_UnprotectedCodeStillWorks pins the off-by-default half:
// a code not on the list still executes normally.
func TestPostTemplate_UnprotectedCodeStillWorks(t *testing.T) {
	var gotCode string
	srv := newProtectedTemplateServer([]string{"deposit_confirm"}, &mockJournalWriter{
		templateFn: func(_ context.Context, code string, params core.TemplateParams) (*core.Journal, error) {
			gotCode = code
			return &core.Journal{UID: "j-1", IdempotencyKey: params.IdempotencyKey}, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody("some_other_template"))
	require.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "some_other_template", gotCode)
}

// TestPostTemplate_EmptyProtectedListChangesNothing pins the default: an
// empty (unset) Config.ProtectedTemplateCodes is the same behavior as
// before this fix -- every template code, including deposit_confirm,
// remains postable. This is a deliberate default (mechanism in the
// library, policy in the consumer, same split as
// core.ReserveInput.RequireVerifiedBalance) -- not an oversight, and this
// test exists so nobody "fixes" it into a surprise default-deny later
// without updating this pin.
func TestPostTemplate_EmptyProtectedListChangesNothing(t *testing.T) {
	var gotCode string
	srv := newProtectedTemplateServer(nil, &mockJournalWriter{
		templateFn: func(_ context.Context, code string, params core.TemplateParams) (*core.Journal, error) {
			gotCode = code
			return &core.Journal{UID: "j-1", IdempotencyKey: params.IdempotencyKey}, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody("deposit_confirm"))
	require.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "deposit_confirm", gotCode)
}
