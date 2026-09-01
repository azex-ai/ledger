package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/presets"
	"github.com/azex-ai/ledger/server"
)

// newDevCreditServer builds a server with an explicit DevCreditEnabled
// setting. newTestServerWith hardcodes its config, so this constructs one
// directly with the same mock set.
func newDevCreditServer(enabled bool, journals core.JournalWriter) *server.Server {
	return server.NewWithConfig(
		&server.Config{
			Env:              "dev",
			CORSAllowOrigin:  "*",
			MaxBodyBytes:     256 * 1024,
			DevCreditEnabled: enabled,
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

func devCreditBody() map[string]any {
	return map[string]any{
		"holder_id":       42,
		"currency_uid":    "cur-1",
		"amount":          "100.5",
		"idempotency_key": "devcredit-test-1",
	}
}

// TestDevCredit_DisabledByDefault pins the default-off contract: a server
// that never opted in must not mint balance, and says so with the same
// feature-availability code the other optional add-ons use.
func TestDevCredit_DisabledByDefault(t *testing.T) {
	srv := newDevCreditServer(false, &mockJournalWriter{
		templateFn: func(context.Context, string, core.TemplateParams) (*core.Journal, error) {
			t.Fatal("template must not be executed while dev credit is disabled")
			return nil, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/dev/credits", devCreditBody())
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var env map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, float64(18102), env["code"])
}

// TestDevCredit_EnabledPostsDevCreditTemplate pins the template the endpoint
// is hardwired to. Aiming it at deposit_confirm would credit custodial and
// make unbacked balance look custodied.
func TestDevCredit_EnabledPostsDevCreditTemplate(t *testing.T) {
	var gotCode string
	var gotParams core.TemplateParams
	srv := newDevCreditServer(true, &mockJournalWriter{
		templateFn: func(_ context.Context, code string, params core.TemplateParams) (*core.Journal, error) {
			gotCode = code
			gotParams = params
			return &core.Journal{UID: "j-1", IdempotencyKey: params.IdempotencyKey}, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/dev/credits", devCreditBody())
	require.Equal(t, http.StatusCreated, w.Code)

	assert.Equal(t, presets.DevCreditTemplateCode, gotCode)
	assert.Equal(t, int64(42), gotParams.HolderID)
	assert.Equal(t, "cur-1", gotParams.CurrencyUID)
	assert.Equal(t, "devcredit-test-1", gotParams.IdempotencyKey)
	require.Contains(t, gotParams.Amounts, "amount")
	assert.Equal(t, "100.5", gotParams.Amounts["amount"].String())
}

func TestDevCredit_RejectsNonPositiveAmount(t *testing.T) {
	for _, amount := range []string{"0", "-1", "-0.00001"} {
		t.Run(amount, func(t *testing.T) {
			srv := newDevCreditServer(true, &mockJournalWriter{
				templateFn: func(context.Context, string, core.TemplateParams) (*core.Journal, error) {
					t.Fatal("template must not be executed for a non-positive amount")
					return nil, nil
				},
			})

			body := devCreditBody()
			body["amount"] = amount
			w := doRequest(srv, http.MethodPost, "/api/v1/dev/credits", body)
			assert.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestDevCredit_RejectsMalformedAmount(t *testing.T) {
	srv := newDevCreditServer(true, &mockJournalWriter{})

	body := devCreditBody()
	body["amount"] = "1e10" // scientific notation is not a wire amount
	w := doRequest(srv, http.MethodPost, "/api/v1/dev/credits", body)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestDevCreditConfig_RefusedOutsideDev pins the boot-time fence: the switch
// cannot be turned on in any environment but dev, whichever way the config
// is assembled.
func TestDevCreditConfig_RefusedOutsideDev(t *testing.T) {
	for _, env := range []string{"production", "staging"} {
		t.Run(env, func(t *testing.T) {
			cfg := &server.Config{
				Env:              env,
				CORSAllowOrigin:  "https://ledger.example",
				APIKeys:          []server.APIKey{{Name: "admin", Scope: server.ScopeAdmin, Secret: []byte("secret-key-value")}},
				MaxBodyBytes:     256 * 1024,
				DevCreditEnabled: true,
			}
			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "ENV=dev")
		})
	}
}

func TestDevCreditConfig_AllowedInDev(t *testing.T) {
	cfg := &server.Config{
		Env:              "dev",
		CORSAllowOrigin:  "*",
		MaxBodyBytes:     256 * 1024,
		DevCreditEnabled: true,
	}
	require.NoError(t, cfg.Validate())
}

func TestLoadConfig_DevCreditRefusedInProduction(t *testing.T) {
	t.Setenv("ENV", "production")
	t.Setenv("CORS_ALLOWED_ORIGIN", "https://ledger.example")
	t.Setenv("API_KEYS", "admin:admin:secret-key-value")
	t.Setenv("DEV_CREDIT_ENABLED", "true")

	_, err := server.LoadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ENV=dev")
}

func TestLoadConfig_DevCreditDefaultsOff(t *testing.T) {
	t.Setenv("ENV", "dev")
	t.Setenv("CORS_ALLOWED_ORIGIN", "")
	t.Setenv("API_KEYS", "")
	t.Setenv("DEV_CREDIT_ENABLED", "")

	cfg, err := server.LoadConfig()
	require.NoError(t, err)
	assert.False(t, cfg.DevCreditEnabled)
}
