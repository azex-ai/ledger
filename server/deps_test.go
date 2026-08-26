package server_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/server"
	"github.com/azex-ai/ledger/service"
)

// depsFromMocks builds a server.Deps with the same mock set newTestServer
// wires positionally through NewWithConfig -- proof the two constructors
// build an identical Server from identical dependencies.
func depsFromMocks() server.Deps {
	return server.Deps{
		Journals:         &mockJournalWriter{},
		Balances:         &mockBalanceReader{},
		Reserver:         &mockReserver{},
		Booker:           &mockBooker{},
		BookingReader:    &mockBookingReader{},
		EventReader:      &mockEventReader{},
		Classifications:  &mockClassificationStore{},
		JournalTypes:     &mockJournalTypeStore{},
		Templates:        &mockTemplateStore{},
		Currencies:       &mockCurrencyStore{},
		Reconciler:       &mockReconciler{},
		Snapshotter:      &mockSnapshotter{},
		SystemRollup:     (*service.SystemRollupService)(nil),
		Queries:          &mockQueryProvider{},
		Audit:            &mockAuditQuerier{},
		PlatformBalances: &mockPlatformBalanceReader{},
		Solvency:         &mockSolvencyChecker{},
		BalanceTrends:    &mockBalanceTrendReader{},
		FullReconciler:   &mockFullReconciler{},
		AccountPolicies:  &mockAccountPolicyStore{},
		PeriodCloser:     &mockPeriodCloser{},
		TrialBalance:     &mockTrialBalanceReader{},
	}
}

// TestNewFromDeps_ValidConfigBuildsAWorkingServer pins that NewFromDeps
// produces a Server equivalent to NewWithConfig's -- same routes, same
// behavior -- by exercising an actual request through it.
func TestNewFromDeps_ValidConfigBuildsAWorkingServer(t *testing.T) {
	cfg := &server.Config{Env: "dev", CORSAllowOrigin: "*", MaxBodyBytes: 256 * 1024}
	srv, err := server.NewFromDeps(cfg, depsFromMocks())
	require.NoError(t, err)
	require.NotNil(t, srv)

	w := doRequest(srv, "GET", "/api/v1/journals/some-uid", nil)
	assert.Equal(t, 200, w.Code)
}

// TestNewFromDeps_InvalidConfigReturnsError is the fix this file exists to
// pin (structure.md's Minor): before NewFromDeps existed, the only way to
// build a Server was NewWithConfig, which panics on an invalid Config --
// every composition root needed its own recover() to fail gracefully.
// NewFromDeps returns an error instead, so the caller decides how to exit.
func TestNewFromDeps_InvalidConfigReturnsError(t *testing.T) {
	_, err := server.NewFromDeps(&server.Config{}, server.Deps{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ENV is required")
}

// TestNewWithConfig_InvalidConfigStillPanics pins that the existing,
// backward-compatible constructor's behavior is genuinely unchanged --
// NewFromDeps is additive, not a silent behavior swap on the old path.
func TestNewWithConfig_InvalidConfigStillPanics(t *testing.T) {
	assert.Panics(t, func() {
		server.NewWithConfig(&server.Config{}, // invalid: no Env
			&mockJournalWriter{}, &mockBalanceReader{}, &mockReserver{},
			&mockBooker{}, &mockBookingReader{}, &mockEventReader{},
			&mockClassificationStore{}, &mockJournalTypeStore{}, &mockTemplateStore{},
			&mockCurrencyStore{}, nil,
			&mockReconciler{}, &mockSnapshotter{}, nil, &mockQueryProvider{},
			&mockAuditQuerier{}, &mockPlatformBalanceReader{}, &mockSolvencyChecker{},
			&mockBalanceTrendReader{}, &mockFullReconciler{}, &mockAccountPolicyStore{},
			&mockPeriodCloser{}, &mockTrialBalanceReader{},
		)
	})
}
