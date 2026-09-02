package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

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

// --- installed-config fakes (D-C1 / D-m9) ---------------------------------
//
// The derived gate below needs a template table with real classification
// legs, so these three fakes are just enough of the config stores for the
// package's own exported installers (presets.InstallExtendedPresets,
// InstallPendingBundle, InstallDevCreditBundle) to run against. The template
// rows the gate then enumerates come from those installers -- not from the
// guard's own notion of what is dangerous, which is exactly what made
// TestPostTemplate_DefaultProtectsDepositCodes structurally unable to notice
// dev_credit was missing from the protected set (D-m9).
//
// mockTemplateStore/mockClassificationStore in server_test.go stay untouched:
// they answer every code with one non-system leg, which is what the
// hand-configured tests above want.

type fakeClassificationStore struct {
	byCode map[string]*core.Classification
	order  []string
}

func newFakeClassificationStore() *fakeClassificationStore {
	return &fakeClassificationStore{byCode: map[string]*core.Classification{}}
}

func (f *fakeClassificationStore) CreateClassification(_ context.Context, input core.ClassificationInput) (*core.Classification, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	c := &core.Classification{
		UID:          fmt.Sprintf("cls-%s", input.Code),
		Code:         input.Code,
		Name:         input.Name,
		NormalSide:   input.NormalSide,
		IsSystem:     input.IsSystem,
		IsActive:     true,
		DisplayLabel: input.DisplayLabel,
		BalanceRole:  input.BalanceRole,
		Lifecycle:    input.Lifecycle,
		CreatedAt:    time.Now(),
	}
	f.byCode[input.Code] = c
	f.order = append(f.order, input.Code)
	return c, nil
}

func (f *fakeClassificationStore) GetByCode(_ context.Context, code string) (*core.Classification, error) {
	c, ok := f.byCode[code]
	if !ok {
		return nil, fmt.Errorf("classification %q: %w", code, core.ErrNotFound)
	}
	return c, nil
}

func (f *fakeClassificationStore) SetBalanceRole(_ context.Context, uid string, role core.BalanceRole) error {
	for _, c := range f.byCode {
		if c.UID == uid {
			c.BalanceRole = role
		}
	}
	return nil
}

func (f *fakeClassificationStore) SetDisplayLabelIfEmpty(_ context.Context, uid string, label string) error {
	for _, c := range f.byCode {
		if c.UID == uid && c.DisplayLabel == "" {
			c.DisplayLabel = label
		}
	}
	return nil
}

func (f *fakeClassificationStore) SetLifecycleIfEmpty(_ context.Context, uid string, lifecycle *core.Lifecycle) error {
	for _, c := range f.byCode {
		if c.UID == uid && c.Lifecycle == nil {
			c.Lifecycle = lifecycle
		}
	}
	return nil
}

func (f *fakeClassificationStore) DeactivateClassification(_ context.Context, uid string) error {
	for _, c := range f.byCode {
		if c.UID == uid {
			c.IsActive = false
		}
	}
	return nil
}

func (f *fakeClassificationStore) ListClassifications(_ context.Context, activeOnly bool) ([]core.Classification, error) {
	out := make([]core.Classification, 0, len(f.order))
	for _, code := range f.order {
		c := f.byCode[code]
		if activeOnly && !c.IsActive {
			continue
		}
		out = append(out, *c)
	}
	return out, nil
}

type fakeJournalTypeStore struct {
	byCode map[string]*core.JournalType
	order  []string
}

func newFakeJournalTypeStore() *fakeJournalTypeStore {
	return &fakeJournalTypeStore{byCode: map[string]*core.JournalType{}}
}

func (f *fakeJournalTypeStore) CreateJournalType(_ context.Context, input core.JournalTypeInput) (*core.JournalType, error) {
	jt := &core.JournalType{
		UID:          fmt.Sprintf("jt-%s", input.Code),
		Code:         input.Code,
		Name:         input.Name,
		IsActive:     true,
		DisplayLabel: input.DisplayLabel,
		HolderKind:   input.HolderKind,
		CreatedAt:    time.Now(),
	}
	f.byCode[input.Code] = jt
	f.order = append(f.order, input.Code)
	return jt, nil
}

func (f *fakeJournalTypeStore) GetJournalTypeByCode(_ context.Context, code string) (*core.JournalType, error) {
	jt, ok := f.byCode[code]
	if !ok {
		return nil, fmt.Errorf("journal type %q: %w", code, core.ErrNotFound)
	}
	return jt, nil
}

func (f *fakeJournalTypeStore) SetDisplayLabelIfEmpty(_ context.Context, uid string, label string) error {
	for _, jt := range f.byCode {
		if jt.UID == uid && jt.DisplayLabel == "" {
			jt.DisplayLabel = label
		}
	}
	return nil
}

func (f *fakeJournalTypeStore) SetHolderKind(_ context.Context, uid string, kind core.HolderTxKind) error {
	for _, jt := range f.byCode {
		if jt.UID == uid {
			jt.HolderKind = kind
		}
	}
	return nil
}

func (f *fakeJournalTypeStore) DeactivateJournalType(_ context.Context, uid string) error {
	for _, jt := range f.byCode {
		if jt.UID == uid {
			jt.IsActive = false
		}
	}
	return nil
}

func (f *fakeJournalTypeStore) ListJournalTypes(_ context.Context, activeOnly bool) ([]core.JournalType, error) {
	out := make([]core.JournalType, 0, len(f.order))
	for _, code := range f.order {
		jt := f.byCode[code]
		if activeOnly && !jt.IsActive {
			continue
		}
		out = append(out, *jt)
	}
	return out, nil
}

type fakeTemplateStore struct {
	byCode map[string]*core.EntryTemplate
	order  []string
}

func newFakeTemplateStore() *fakeTemplateStore {
	return &fakeTemplateStore{byCode: map[string]*core.EntryTemplate{}}
}

func (f *fakeTemplateStore) CreateTemplate(_ context.Context, input core.TemplateInput) (*core.EntryTemplate, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	lines := make([]core.EntryTemplateLine, len(input.Lines))
	for i, l := range input.Lines {
		lines[i] = core.EntryTemplateLine(l)
	}
	tmpl := &core.EntryTemplate{
		UID:            fmt.Sprintf("tmpl-%s", input.Code),
		Code:           input.Code,
		Name:           input.Name,
		JournalTypeUID: input.JournalTypeUID,
		IsActive:       true,
		Lines:          lines,
		CreatedAt:      time.Now(),
	}
	f.byCode[input.Code] = tmpl
	f.order = append(f.order, input.Code)
	return tmpl, nil
}

func (f *fakeTemplateStore) DeactivateTemplate(_ context.Context, uid string) error {
	for _, tmpl := range f.byCode {
		if tmpl.UID == uid {
			tmpl.IsActive = false
		}
	}
	return nil
}

func (f *fakeTemplateStore) GetTemplate(_ context.Context, code string) (*core.EntryTemplate, error) {
	tmpl, ok := f.byCode[code]
	if !ok {
		return nil, fmt.Errorf("template %q: %w", code, core.ErrNotFound)
	}
	return tmpl, nil
}

func (f *fakeTemplateStore) ListTemplates(_ context.Context, activeOnly bool) ([]core.EntryTemplate, error) {
	out := make([]core.EntryTemplate, 0, len(f.order))
	for _, code := range f.order {
		tmpl := f.byCode[code]
		if activeOnly && !tmpl.IsActive {
			continue
		}
		out = append(out, *tmpl)
	}
	return out, nil
}

// emptyTemplateStore answers ErrNotFound for every code -- the state a
// deployment is in before it installs any preset bundle.
type emptyTemplateStore struct{ fakeTemplateStore }

// installedPresetConfig runs every preset installer this library ships
// against in-memory config stores and returns them, so a test can ask "what
// does the template table actually contain, and which of its rows touch an
// is_system classification".
type installedPresetConfig struct {
	classifications *fakeClassificationStore
	journalTypes    *fakeJournalTypeStore
	templates       *fakeTemplateStore
}

func installedPresets(t *testing.T) *installedPresetConfig {
	t.Helper()
	cfg := &installedPresetConfig{
		classifications: newFakeClassificationStore(),
		journalTypes:    newFakeJournalTypeStore(),
		templates:       newFakeTemplateStore(),
	}
	ctx := context.Background()
	require.NoError(t, presets.InstallExtendedPresets(ctx, cfg.classifications, cfg.journalTypes, cfg.templates))
	require.NoError(t, presets.InstallPendingBundle(ctx, cfg.classifications, cfg.journalTypes, cfg.templates))
	// The one bundle InstallExtendedPresets deliberately excludes: a
	// deployment opts into minting balance out of nothing explicitly. Once it
	// has, the template row is in the table for good, ENV-independent -- which
	// is the whole of D-C1.
	require.NoError(t, presets.InstallDevCreditBundle(ctx, cfg.classifications, cfg.journalTypes, cfg.templates))
	return cfg
}

// templateCodesBySystemLeg partitions the installed template table by the
// structural property the guard is supposed to derive its verdict from: does
// any leg of this template post to a classification flagged is_system.
func (c *installedPresetConfig) templateCodesBySystemLeg(t *testing.T) (withSystemLeg, withoutSystemLeg []string) {
	t.Helper()
	ctx := context.Background()

	all, err := c.classifications.ListClassifications(ctx, false)
	require.NoError(t, err)
	systemUIDs := map[string]bool{}
	for _, cl := range all {
		if cl.IsSystem {
			systemUIDs[cl.UID] = true
		}
	}

	templates, err := c.templates.ListTemplates(ctx, false)
	require.NoError(t, err)
	for _, tmpl := range templates {
		system := false
		for _, line := range tmpl.Lines {
			if systemUIDs[line.ClassificationUID] {
				system = true
				break
			}
		}
		if system {
			withSystemLeg = append(withSystemLeg, tmpl.Code)
		} else {
			withoutSystemLeg = append(withoutSystemLeg, tmpl.Code)
		}
	}
	sort.Strings(withSystemLeg)
	sort.Strings(withoutSystemLeg)
	return withSystemLeg, withoutSystemLeg
}

// newInstalledTemplateServer wires a server against config stores holding the
// real installed preset rows, so handlePostTemplate's structural check reads
// the same template legs a deployment's database would hold.
func newInstalledTemplateServer(
	cfg *installedPresetConfig,
	templates core.TemplateStore,
	allowed []string,
	journals core.JournalWriter,
) *server.Server {
	srv, err := server.NewFromDeps(
		&server.Config{
			Env:                      "dev",
			CORSAllowOrigin:          "*",
			MaxBodyBytes:             256 * 1024,
			AllowGenericTemplatePost: allowed,
		},
		server.Deps{
			Journals:         journals,
			Balances:         &mockBalanceReader{},
			Reserver:         &mockReserver{},
			Booker:           &mockBooker{},
			BookingReader:    &mockBookingReader{},
			EventReader:      &mockEventReader{},
			Classifications:  cfg.classifications,
			JournalTypes:     cfg.journalTypes,
			Templates:        templates,
			Currencies:       &mockCurrencyStore{},
			Reconciler:       &mockReconciler{},
			Queries:          &mockQueryProvider{},
			Audit:            &mockAuditQuerier{},
			PlatformBalances: &mockPlatformBalanceReader{},
			Solvency:         &mockSolvencyChecker{},
			BalanceTrends:    &mockBalanceTrendReader{},
			FullReconciler:   &mockFullReconciler{},
			AccountPolicies:  &mockAccountPolicyStore{},
			PeriodCloser:     &mockPeriodCloser{},
			TrialBalance:     &mockTrialBalanceReader{},
		},
	)
	if err != nil {
		panic(err)
	}
	return srv
}

func refuseExecuteTemplate(t *testing.T) *mockJournalWriter {
	t.Helper()
	return &mockJournalWriter{
		templateFn: func(_ context.Context, code string, _ core.TemplateParams) (*core.Journal, error) {
			t.Fatalf("ExecuteTemplate must not be called for template_code %q", code)
			return nil, nil
		},
	}
}

// TestPostTemplate_ProtectsEveryInstalledTemplateWithASystemLeg is the derived
// gate D-C1 / D-m9 ask for: enumerate the template table this library's own
// installers produce, and require a 403 on every template with a leg on an
// is_system classification. The verdict comes from the installed rows, so a
// new preset that touches a system account is covered the moment it exists --
// nobody has to remember to add its code to a list.
//
// Verified red at 2026-09-02 on the pre-fix handler: dev_credit,
// capital_injection, capital_withdraw, fee_charge, checkout_settlement_*,
// fx_* and transfer_* all answered 201 with ExecuteTemplate called, because
// handlePostTemplate only consulted the four-code deposit list.
func TestPostTemplate_ProtectsEveryInstalledTemplateWithASystemLeg(t *testing.T) {
	cfg := installedPresets(t)
	withSystemLeg, _ := cfg.templateCodesBySystemLeg(t)
	require.NotEmpty(t, withSystemLeg, "no installed template touches an is_system classification -- the enumeration is broken, not the guard")
	require.Contains(t, withSystemLeg, presets.DevCreditTemplateCode,
		"dev_credit must appear in the enumeration: it is the one template whose doc comment says it mints balance out of nothing")

	for _, code := range withSystemLeg {
		t.Run(code, func(t *testing.T) {
			srv := newInstalledTemplateServer(cfg, cfg.templates, nil, refuseExecuteTemplate(t))
			w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody(code))
			assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
		})
	}
}

// TestPostTemplate_AllowsInstalledTemplatesWithoutASystemLeg is the control
// for the gate above: the structural rule is not a blanket deny of every
// installed template. Templates whose legs are all holder-side still execute.
func TestPostTemplate_AllowsInstalledTemplatesWithoutASystemLeg(t *testing.T) {
	cfg := installedPresets(t)
	_, withoutSystemLeg := cfg.templateCodesBySystemLeg(t)
	require.NotEmpty(t, withoutSystemLeg, "every installed template touches a system classification -- this control proves nothing")

	for _, code := range withoutSystemLeg {
		t.Run(code, func(t *testing.T) {
			var gotCode string
			srv := newInstalledTemplateServer(cfg, cfg.templates, nil, &mockJournalWriter{
				templateFn: func(_ context.Context, code string, params core.TemplateParams) (*core.Journal, error) {
					gotCode = code
					return &core.Journal{UID: "j-1", IdempotencyKey: params.IdempotencyKey}, nil
				},
			})
			w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody(code))
			require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
			assert.Equal(t, code, gotCode)
		})
	}
}

// TestPostTemplate_RefusesTheAuditedMintingCodes names the three codes the
// 2026-09-02 audit's own httptest reproduction posted successfully (201, with
// ExecuteTemplate reached) under default configuration. Written as literals,
// deliberately: the derived gate above can only fail loudly if the
// enumeration itself is intact, and these three are the concrete claim.
func TestPostTemplate_RefusesTheAuditedMintingCodes(t *testing.T) {
	cfg := installedPresets(t)
	withSystemLeg, _ := cfg.templateCodesBySystemLeg(t)

	for _, code := range []string{"dev_credit", "capital_injection", "fee_charge"} {
		t.Run(code, func(t *testing.T) {
			require.Contains(t, withSystemLeg, code,
				"template %q is no longer installed with a system leg -- if a preset renamed it, update this pin with the new code", code)

			srv := newInstalledTemplateServer(cfg, cfg.templates, nil, refuseExecuteTemplate(t))
			w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody(code))
			assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
		})
	}
}

// TestPostTemplate_AllowGenericTemplatePostIsTheOnlyWayPastTheSystemLegRule:
// the structural rule has exactly one opt-out (contract §7.5), and it is
// per-code and explicit.
func TestPostTemplate_AllowGenericTemplatePostIsTheOnlyWayPastTheSystemLegRule(t *testing.T) {
	cfg := installedPresets(t)

	var gotCode string
	srv := newInstalledTemplateServer(cfg, cfg.templates, []string{presets.DevCreditTemplateCode}, &mockJournalWriter{
		templateFn: func(_ context.Context, code string, params core.TemplateParams) (*core.Journal, error) {
			gotCode = code
			return &core.Journal{UID: "j-1", IdempotencyKey: params.IdempotencyKey}, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody(presets.DevCreditTemplateCode))
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, presets.DevCreditTemplateCode, gotCode)
}

// TestPostTemplate_HardcodedListStandsWithoutTheTemplateTable proves the two
// layers are independent: presets.ProtectedTemplateCodes() still refuses its
// codes when the template table cannot answer at all (nothing installed), so
// the hardcoded list is not merely a slower path to the same lookup. The
// order also matters — the code check runs before the table read, so a
// protected code gets 403 rather than the 404 the missing row would produce.
func TestPostTemplate_HardcodedListStandsWithoutTheTemplateTable(t *testing.T) {
	cfg := installedPresets(t)
	srv := newInstalledTemplateServer(cfg, &emptyTemplateStore{}, nil, refuseExecuteTemplate(t))

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody(presets.DepositConfirmTemplateCode))
	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
}

// TestPostTemplate_UnknownTemplateCodeNeverReachesExecuteTemplate: the
// structural check resolves the template before executing anything, so a code
// with no row fails closed at the guard (working-agreements.md §3: a check
// that cannot run must not be read as a pass).
func TestPostTemplate_UnknownTemplateCodeNeverReachesExecuteTemplate(t *testing.T) {
	cfg := installedPresets(t)
	srv := newInstalledTemplateServer(cfg, &emptyTemplateStore{}, nil, refuseExecuteTemplate(t))

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody("acme_unknown_template"))
	assert.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
}

// TestPostTemplate_ProtectedCodeIsRefused pins structure.md's Major: before
// Config.ProtectedTemplateCodes existed, POST /journals/template had no
// allowlist/denylist at all -- any write-scope key could post a journal
// under a template code like presets.DepositConfirmTemplateCode
// ("deposit_confirm"), indistinguishable from a real verified deposit. This
// exercises the additive half: a deployment-specific code named in
// Config.ProtectedTemplateCodes, on top of the library default.
func TestPostTemplate_ProtectedCodeIsRefused(t *testing.T) {
	srv := newProtectedTemplateServer([]string{"acme_custom_confirm"}, nil, refuseExecuteTemplate(t))

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody("acme_custom_confirm"))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestPostTemplate_UnprotectedCodeStillWorks: a code that is neither in the
// library default set nor in Config.ProtectedTemplateCodes, and whose legs
// are all holder-side, still executes normally.
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
// remember to opt in. It now means "protect the library's own hardcoded
// set" -- every one of them.
//
// The codes are written out as literals rather than ranged over from
// presets.ProtectedTemplateCodes() (D-m9): a table driven by the
// implementation's own return value can never fail because a dangerous code
// is missing from that return value, which is precisely how dev_credit went
// unnoticed. The structural counterpart lives in
// TestPostTemplate_ProtectsEveryInstalledTemplateWithASystemLeg above.
func TestPostTemplate_DefaultProtectsDepositCodes(t *testing.T) {
	for _, code := range []string{
		"deposit_confirm",
		"deposit_confirm_pending",
		"deposit_release_pending",
		"deposit_record_overage",
		"dev_credit",
	} {
		t.Run(code, func(t *testing.T) {
			require.Contains(t, presets.ProtectedTemplateCodes(), code,
				"presets.ProtectedTemplateCodes() dropped %q from the hardcoded set", code)

			srv := newProtectedTemplateServer(nil, nil, refuseExecuteTemplate(t))

			w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody(code))
			assert.Equal(t, http.StatusForbidden, w.Code)
		})
	}
}

// TestPostTemplate_DefaultDoesNotProtectUnrelatedCodes proves the hardcoded
// default isn't a blanket deny: a code that isn't one of the library's own
// protected codes, and isn't listed in Config.ProtectedTemplateCodes, is not
// refused by the name-list layer -- so a deployment with entirely unrelated
// template codes isn't affected by that default flipping on. It says nothing
// about the structural layer, which is what actually decides a real
// installed template's fate
// (TestPostTemplate_ProtectsEveryInstalledTemplateWithASystemLeg /
// TestPostTemplate_AllowsInstalledTemplatesWithoutASystemLeg); the mock
// template store this server is wired with answers every code with one
// holder-side leg.
func TestPostTemplate_DefaultDoesNotProtectUnrelatedCodes(t *testing.T) {
	var gotCode string
	srv := newProtectedTemplateServer(nil, nil, &mockJournalWriter{
		templateFn: func(_ context.Context, code string, params core.TemplateParams) (*core.Journal, error) {
			gotCode = code
			return &core.Journal{UID: "j-1", IdempotencyKey: params.IdempotencyKey}, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody("acme_unrelated_code"))
	require.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "acme_unrelated_code", gotCode)
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
	srv := newProtectedTemplateServer(nil, []string{presets.DepositConfirmTemplateCode}, refuseExecuteTemplate(t))

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", postTemplateBody(presets.DepositConfirmPendingTemplateCode))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- POST /journals/{uid}/reverse idempotency key (H-M3) ------------------
//
// These live in this file rather than a new one because handler_journals.go is
// this task's exclusive surface for the wave; they are the Go half of H-M3
// (docs/openapi.yaml is D-contract's).

func doRequestWithHeader(srv http.Handler, method, path string, body any, header, value string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(header, value)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

// TestReverseJournal_RejectsClientSuppliedIdempotencyKey pins H-M3's Go half:
// docs/openapi.yaml listed idempotency_key as required on this endpoint while
// reverseJournalRequest did not even have the field, so any key a client sent
// was parsed away and silently dropped -- the caller believed it controlled
// the replay scope of a money-correcting write while the server derived its
// own key. The field now exists and a supplied value is refused outright
// (400) rather than accepted and ignored.
func TestReverseJournal_RejectsClientSuppliedIdempotencyKey(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			reverseFn: func(context.Context, string, string) (*core.Journal, error) {
				t.Fatal("ReverseJournal must not be called when the request carries an idempotency_key")
				return nil, nil
			},
		}
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/j-1/reverse", map[string]any{
		"reason":          "operator error",
		"idempotency_key": "client-chosen-key",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
}

// TestReverseJournal_RejectsIdempotencyKeyHeader: the same refusal reached
// through the Idempotency-Key header alias, which the middleware lifts into
// the body field. Before the field existed this path was the silent one --
// a client that sets the header on every POST got a 201 and never learned
// its key had no effect.
func TestReverseJournal_RejectsIdempotencyKeyHeader(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			reverseFn: func(context.Context, string, string) (*core.Journal, error) {
				t.Fatal("ReverseJournal must not be called when the request carries an Idempotency-Key header")
				return nil, nil
			},
		}
	})

	w := doRequestWithHeader(srv, http.MethodPost, "/api/v1/journals/j-1/reverse",
		map[string]any{"reason": "operator error"}, "Idempotency-Key", "client-chosen-key")
	assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
}

// TestReverseJournal_WithoutIdempotencyKeyPostsTheReversal: the documented
// shape stays usable -- reason alone, key derived server-side.
func TestReverseJournal_WithoutIdempotencyKeyPostsTheReversal(t *testing.T) {
	var gotUID, gotReason string
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			reverseFn: func(_ context.Context, uid, reason string) (*core.Journal, error) {
				gotUID, gotReason = uid, reason
				return &core.Journal{UID: "rev-1", ReversalOfUID: uid, IdempotencyKey: "reversal:" + uid + ":" + reason}, nil
			},
		}
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/j-1/reverse", map[string]any{"reason": "operator error"})
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "j-1", gotUID)
	assert.Equal(t, "operator error", gotReason)
}
