package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/presets"
	"github.com/azex-ai/ledger/server"
)

// newProtectedTemplateServer builds a server with an explicit
// ProtectedTemplateCodes / AllowGenericTemplatePost pair -- newTestServerWith
// hardcodes its config, so this constructs one directly with the same mock
// set (mirrors newDevCreditServer's pattern in handler_devcredit_test.go).
func newProtectedTemplateServer(protected, allowed []string, journals core.JournalWriter) *server.Server {
	return server.NewWithConfig(
		&server.Config{
			Env:                      "dev",
			CORSAllowOrigin:          "*",
			MaxBodyBytes:             256 * 1024,
			ProtectedTemplateCodes:   protected,
			AllowGenericTemplatePost: allowed,
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
// ("deposit_confirm"), indistinguishable from a real verified deposit. This
// exercises the additive half: a deployment-specific code named in
// Config.ProtectedTemplateCodes, on top of the library default.
func TestPostTemplate_ProtectedCodeIsRefused(t *testing.T) {
	srv := newProtectedTemplateServer([]string{"acme_custom_confirm"}, nil, &mockJournalWriter{
		templateFn: func(context.Context, string, core.TemplateParams) (*core.Journal, error) {
			t.Fatal("ExecuteTemplate must not be called for a protected template code")
			return nil, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody("acme_custom_confirm"))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestPostTemplate_UnprotectedCodeStillWorks: a code that is neither in the
// library default set nor in Config.ProtectedTemplateCodes still executes
// normally.
func TestPostTemplate_UnprotectedCodeStillWorks(t *testing.T) {
	var gotCode string
	srv := newProtectedTemplateServer([]string{"acme_custom_confirm"}, nil, &mockJournalWriter{
		templateFn: func(_ context.Context, code string, params core.TemplateParams) (*core.Journal, error) {
			gotCode = code
			return &core.Journal{UID: "j-1", IdempotencyKey: params.IdempotencyKey}, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody("some_other_template"))
	require.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "some_other_template", gotCode)
}

// TestPostTemplate_DefaultProtectsDepositCodes pins M-2 of the 2026-08-26
// independent review: an empty (unset) Config.ProtectedTemplateCodes used to
// mean "protect nothing" -- the finding structure.md raised stayed open in
// every deployment that installed a deposit preset and didn't separately
// remember to opt in. It now means "protect the library's own
// presets.ProtectedTemplateCodes() set" -- every one of them, not just the
// single code the earlier version of this test happened to cover. Before
// this fix this loop is red on all four (each posts 201, not 403).
func TestPostTemplate_DefaultProtectsDepositCodes(t *testing.T) {
	for _, code := range presets.ProtectedTemplateCodes() {
		t.Run(code, func(t *testing.T) {
			srv := newProtectedTemplateServer(nil, nil, &mockJournalWriter{
				templateFn: func(context.Context, string, core.TemplateParams) (*core.Journal, error) {
					t.Fatal("ExecuteTemplate must not be called for a default-protected template code")
					return nil, nil
				},
			})

			w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody(code))
			assert.Equal(t, http.StatusForbidden, w.Code)
		})
	}
}

// TestPostTemplate_DefaultDoesNotProtectUnrelatedCodes proves the default
// isn't a blanket deny: a code that isn't one of the library's own
// deposit-confirmation codes, and isn't listed in
// Config.ProtectedTemplateCodes, still posts normally under the new
// default -- so a deployment with entirely unrelated template codes isn't
// affected by this default flipping on.
func TestPostTemplate_DefaultDoesNotProtectUnrelatedCodes(t *testing.T) {
	var gotCode string
	srv := newProtectedTemplateServer(nil, nil, &mockJournalWriter{
		templateFn: func(_ context.Context, code string, params core.TemplateParams) (*core.Journal, error) {
			gotCode = code
			return &core.Journal{UID: "j-1", IdempotencyKey: params.IdempotencyKey}, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody("withdraw_fee"))
	require.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "withdraw_fee", gotCode)
}

// TestPostTemplate_AllowGenericTemplatePostOptsCodeBackIn answers M-2's (b)
// direction: does defaulting the deposit codes to protected brick an
// existing deployment that has a reviewed reason to post one of them
// through this generic endpoint? No -- AllowGenericTemplatePost is a
// deliberate, per-code, machine-checkable escape hatch, applied after both
// the library default and Config.ProtectedTemplateCodes.
func TestPostTemplate_AllowGenericTemplatePostOptsCodeBackIn(t *testing.T) {
	var gotCode string
	srv := newProtectedTemplateServer(nil, []string{presets.DepositConfirmTemplateCode}, &mockJournalWriter{
		templateFn: func(_ context.Context, code string, params core.TemplateParams) (*core.Journal, error) {
			gotCode = code
			return &core.Journal{UID: "j-1", IdempotencyKey: params.IdempotencyKey}, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody(presets.DepositConfirmTemplateCode))
	require.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, presets.DepositConfirmTemplateCode, gotCode)
}

// TestPostTemplate_AllowGenericTemplatePostIsScopedToOneCode: opting out
// deposit_confirm does not opt out the other three default-protected codes
// -- AllowGenericTemplatePost removes exactly the codes it names.
func TestPostTemplate_AllowGenericTemplatePostIsScopedToOneCode(t *testing.T) {
	srv := newProtectedTemplateServer(nil, []string{presets.DepositConfirmTemplateCode}, &mockJournalWriter{
		templateFn: func(context.Context, string, core.TemplateParams) (*core.Journal, error) {
			t.Fatal("ExecuteTemplate must not be called for a still-protected template code")
			return nil, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody(presets.DepositConfirmPendingTemplateCode))
	assert.Equal(t, http.StatusForbidden, w.Code)
}
