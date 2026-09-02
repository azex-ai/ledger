package service_test

// Pin tests for the unauthorized_journals reconcile check
// (docs/plans/2026-08-21-integrity-hardening-contracts.md §W2-2,
// docs/INVARIANTS.md I-32). Reuses attest_verify_test.go/attestation_test.go's
// setupAttestFixture / journalInput / ed25519KeyPair helpers (same
// service_test package) since the fixture shape (currency + one
// classification + journal type) is identical to what P6's own tests need.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// TestFullReconciliation_WithoutAuthVerifier_StillReportsFullCoverage pins
// C-m4 (2026-09-02 audit, tamper-evident.md m-4). Previously this check ran
// unconditionally and returned a Complete=false placeholder when no
// AuthVerifier was configured, which made report.FullCoverage permanently
// false for every consumer that never called ledger.WithAttestor -- and
// ReconcileCheckResult("unauthorized_journals", passed && complete)
// permanently red. That is the same dead vote that got check #8 deleted: a
// signal that can never be true carries no information.
//
// Now the check is not run at all and its name goes into SkippedChecks:
// absent-and-named, which is neither "passed" nor a permanent alarm.
func TestFullReconciliation_WithoutAuthVerifier_StillReportsFullCoverage(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rollup := postgres.NewRollupAdapter(pool)
	reconcileAdapter := postgres.NewReconcileAdapter(pool)
	engine := core.NewEngine()
	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{}, engine)
	// Deliberately no SetAuthCheck call.

	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)

	for _, c := range report.Checks {
		require.NotEqual(t, "unauthorized_journals", c.Name,
			"a check that cannot run in this deployment must not cast a vote")
	}
	assert.Contains(t, report.SkippedChecks, "unauthorized_journals",
		"skipping must be reported, not silent -- otherwise it is indistinguishable from having run")
	assert.True(t, report.FullCoverage,
		"an unrunnable check must not poison FullCoverage for every deployment that never enabled signing; report: %+v", report)
}

func TestFullReconciliation_UnauthorizedJournals_PassesWhenAllSignedJournalsAreValid(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-key-uj-1")
	require.NoError(t, err)
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)

	_, err = ledgerStore.PostJournal(ctx, f.journalInput(9101, postgrestest.UniqueKey("uj-ok-1")))
	require.NoError(t, err)
	_, err = ledgerStore.PostJournal(ctx, f.journalInput(9102, postgrestest.UniqueKey("uj-ok-2")))
	require.NoError(t, err)

	rollup := postgres.NewRollupAdapter(pool)
	reconcileAdapter := postgres.NewReconcileAdapter(pool)
	queries := postgres.NewQueryStore(pool)
	engine := core.NewEngine()
	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{}, engine)
	full.SetAuthCheck(queries, verifier)

	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check := findCheck(t, report, "unauthorized_journals")
	assert.True(t, check.Passed, "findings: %+v", check.Findings)
	assert.True(t, check.Complete)

	// Every check in the suite is now wired and nothing is capped or
	// skipped, so the report must be able to say so. Before operability.md's
	// "full_coverage 永远为假" fix (the permanently-skipped check #8), this
	// assertion could never pass against ANY database state or wiring --
	// FullCoverage was structurally false on every run. This is the DB-backed
	// half of that pin (TestFullReconciliation_FullCoverageCanBeTrue in
	// reconcile_full_test.go is the unit-level half).
	assert.True(t, report.FullCoverage,
		"every check ran to completion with nothing capped or skipped -- FullCoverage must be able to be true")
}

func TestFullReconciliation_UnauthorizedJournals_FlagsForgedSignature(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-key-uj-2")
	require.NoError(t, err)
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)

	genuine, err := ledgerStore.PostJournal(ctx, f.journalInput(9103, postgrestest.UniqueKey("uj-genuine")))
	require.NoError(t, err)

	// Forge a journal that CLAIMS a signature (non-empty auth_key_id) but
	// whose digest/signature are garbage -- the shape this check exists to
	// flag, as opposed to a never-signed journal (which it must skip).
	var forgedID int64
	var forgedUID string
	var effectiveAt time.Time
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	require.NoError(t, tx.QueryRow(ctx, `
		INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, effective_at, uid, auth_digest, auth_signature, auth_key_id, auth_status)
		VALUES ($1, $2, 1::numeric, 1::numeric, '{}'::jsonb, 0, 'forged-signature-claim', now(), gen_random_uuid(), decode('deadbeef','hex'), decode('deadbeef','hex'), $3, 'signed')
		RETURNING id, uid, effective_at
	`, f.journalTypeID, postgrestest.UniqueKey("uj-forged"), "forged-key-id").Scan(&forgedID, &forgedUID, &effectiveAt))
	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at, created_at)
		VALUES ($1, $2, $3, $4, 'debit', 1::numeric, $5, now())
	`, forgedID, int64(9104), f.currencyID, f.classificationID, effectiveAt)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at, created_at)
		VALUES ($1, $2, $3, $4, 'credit', 1::numeric, $5, now())
	`, forgedID, core.SystemAccountHolder(9104), f.currencyID, f.classificationID, effectiveAt)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	rollup := postgres.NewRollupAdapter(pool)
	reconcileAdapter := postgres.NewReconcileAdapter(pool)
	queries := postgres.NewQueryStore(pool)
	engine := core.NewEngine()
	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{}, engine)
	full.SetAuthCheck(queries, verifier)

	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check := findCheck(t, report, "unauthorized_journals")
	assert.False(t, check.Passed, "a journal claiming an invalid signature must fail this check")
	assert.True(t, check.Complete)

	var foundForged, foundGenuineFlagged bool
	for _, finding := range check.Findings {
		if containsUID(finding.Description, forgedUID) {
			foundForged = true
		}
		if containsUID(finding.Description, genuine.UID) {
			foundGenuineFlagged = true
		}
	}
	assert.True(t, foundForged, "findings must name the forged journal: %+v", check.Findings)
	assert.False(t, foundGenuineFlagged, "the genuinely signed journal must not be flagged: %+v", check.Findings)
}

// TestFullReconciliation_UnauthorizedJournals_FlagsUnknownKeyAsDistinctFromForgery
// pins I-45: a journal genuinely signed under a key the configured
// AuthVerifier does not currently hold (the state every journal is left in
// after a legitimate key rotation retires its old key) must be flagged --
// never silently skipped like a never-signed journal, or a forged journal
// carrying a made-up auth_key_id could evade this check entirely -- but
// under a Finding a reader can tell apart from real tamper evidence, so
// on-call registers the retired key instead of chasing a forgery that was
// never there.
func TestFullReconciliation_UnauthorizedJournals_FlagsUnknownKeyAsDistinctFromForgery(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	// rotatedAttestor genuinely signs a journal under a key id the
	// reconcile check's verifier below never learns about -- simulating
	// the fleet state right after that key was rotated out.
	rotatedAttestor, _, err := ed25519KeyPair(t, "verify-key-uj-rotated-out")
	require.NoError(t, err)
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(rotatedAttestor)
	rotated, err := ledgerStore.PostJournal(ctx, f.journalInput(9107, postgrestest.UniqueKey("uj-rotated")))
	require.NoError(t, err)

	// currentVerifier is a genuinely different keypair (the "currently
	// registered" key set) that never had the rotated-out key added to it
	// -- exactly what SetAuthCheck would be wired with post-rotation.
	_, currentVerifier, err := ed25519KeyPair(t, "verify-key-uj-current")
	require.NoError(t, err)

	rollup := postgres.NewRollupAdapter(pool)
	reconcileAdapter := postgres.NewReconcileAdapter(pool)
	queries := postgres.NewQueryStore(pool)
	engine := core.NewEngine()
	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{}, engine)
	full.SetAuthCheck(queries, currentVerifier)

	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check := findCheck(t, report, "unauthorized_journals")
	assert.True(t, check.Complete)

	const tamperPhrase = "claims a signature but fails authorization verification"
	var foundUnknownKeyFinding bool
	for _, finding := range check.Findings {
		if !containsUID(finding.Description, rotated.UID) {
			continue
		}
		foundUnknownKeyFinding = true
		// Must not be silently absent (b) ...
		assert.Contains(t, finding.Description, "does not recognize",
			"an unknown-key journal must be flagged with its own distinct wording: %+v", finding)
		// ... and must not be worded as tamper evidence (not conflated with (a)).
		assert.NotContains(t, finding.Description, tamperPhrase,
			"an unknown-key journal must not be worded as a forged/tampered signature: %+v", finding)
	}
	require.True(t, foundUnknownKeyFinding,
		"a journal signed under a key this verifier does not hold must produce a finding, not be silently skipped: %+v", check.Findings)
}

func containsUID(s, uid string) bool {
	return len(uid) > 0 && len(s) >= len(uid) && (func() bool {
		for i := 0; i+len(uid) <= len(s); i++ {
			if s[i:i+len(uid)] == uid {
				return true
			}
		}
		return false
	})()
}

func TestFullReconciliation_UnauthorizedJournals_SkipsNeverSignedJournal(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	// A journal that was simply never signed (empty auth_key_id) --
	// insertForgedJournal's exact shape, reused from attestation_test.go.
	// This must be treated as a coverage gap, not tamper evidence.
	insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("uj-unsigned"))

	_, verifier, err := ed25519KeyPair(t, "verify-key-uj-3")
	require.NoError(t, err)

	rollup := postgres.NewRollupAdapter(pool)
	reconcileAdapter := postgres.NewReconcileAdapter(pool)
	queries := postgres.NewQueryStore(pool)
	engine := core.NewEngine()
	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{}, engine)
	full.SetAuthCheck(queries, verifier)

	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check := findCheck(t, report, "unauthorized_journals")
	assert.True(t, check.Passed, "a never-signed journal must not be flagged as unauthorized: %+v", check.Findings)
	// C-M8 (tamper-evident.md M-8): the only journal in the ledger was
	// skipped, so this run verified NOTHING. It used to report
	// Complete=true here, which made the check's metric green while the
	// whole ledger was unverifiable.
	assert.False(t, check.Complete,
		"a run in which every journal was skipped verified nothing and must not claim full coverage: %+v", check)
	assert.False(t, report.FullCoverage, "the report must carry that incompleteness up")
}

// TestFullReconciliation_UnauthorizedJournals_ZeroSignedIsIncomplete is
// C-M8's dedicated pin: several journals, none of them signed, an
// AuthVerifier wired. Passed stays true (finding nothing IS finding no
// violation) but Complete must be false, so that
// ReconcileCheckResult(name, Passed && Complete) -- the alertable signal --
// does not go green on a ledger nothing could be verified in.
func TestFullReconciliation_UnauthorizedJournals_ZeroSignedIsIncomplete(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	// Three journals through the real write path with NO attestor wired:
	// auth_status='unsigned_no_attestor', auth_key_id=''. This is the shape
	// of a deployment whose verifier is configured but whose write path
	// never got an Attestor -- and of a fleet whose entire history predates
	// P5.
	ledgerStore := postgres.NewLedgerStore(pool)
	for i := 0; i < 3; i++ {
		_, err := ledgerStore.PostJournal(ctx, f.journalInput(int64(9200+i), postgrestest.UniqueKey("uj-zero-signed")))
		require.NoError(t, err)
	}

	_, verifier, err := ed25519KeyPair(t, "verify-key-uj-zero")
	require.NoError(t, err)

	rollup := postgres.NewRollupAdapter(pool)
	reconcileAdapter := postgres.NewReconcileAdapter(pool)
	queries := postgres.NewQueryStore(pool)
	engine := core.NewEngine()
	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{}, engine)
	full.SetAuthCheck(queries, verifier)

	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check := findCheck(t, report, "unauthorized_journals")
	assert.True(t, check.Passed, "no violation was found, and that part is honest")
	assert.False(t, check.Complete,
		"scanning 3 journals and verifying 0 of them must not report full coverage; check: %+v", check)
	assert.False(t, report.FullCoverage)

	var found bool
	for _, finding := range check.Findings {
		if strings.Contains(finding.Description, "verified nothing") {
			found = true
		}
	}
	assert.True(t, found, "the finding must say the check verified nothing; findings: %+v", check.Findings)
}

// TestFullReconciliation_UnauthorizedJournals_ScansTheNewestPage pins the
// sibling of C-M1 found in this check (contract §0's "fix the shape, not the
// instance"): with no resume cursor it only ever looks at one page, and that
// page used to be the OLDEST journals -- the one place a row forged today
// cannot be. A forged signature among the NEWEST journals must be found even
// when older journals fill the page limit.
func TestFullReconciliation_UnauthorizedJournals_ScansTheNewestPage(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-key-uj-newest")
	require.NoError(t, err)
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)

	// Five legitimately signed journals...
	for i := 0; i < 5; i++ {
		_, err = ledgerStore.PostJournal(ctx, f.journalInput(int64(9300+i), postgrestest.UniqueKey("uj-newest-legit")))
		require.NoError(t, err)
	}
	// ...then, as the NEWEST row, one that CLAIMS a signature but carries
	// garbage: same insert shape as
	// TestFullReconciliation_UnauthorizedJournals_FlagsForgedSignature (a
	// forged row does not go through PostJournal, so it is inserted
	// directly, entries included so the ledger stays balanced).
	var forgedID int64
	var forgedUID string
	var effectiveAt time.Time
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	require.NoError(t, tx.QueryRow(ctx, `
		INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, effective_at, uid, auth_digest, auth_signature, auth_key_id, auth_status)
		VALUES ($1, $2, 1::numeric, 1::numeric, '{}'::jsonb, 0, 'newest-page-forgery', now(), gen_random_uuid(), decode('deadbeef','hex'), decode('deadbeef','hex'), $3, 'signed')
		RETURNING id, uid, effective_at
	`, f.journalTypeID, postgrestest.UniqueKey("uj-newest-forged"), "forged-key-id").Scan(&forgedID, &forgedUID, &effectiveAt))
	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at, created_at)
		VALUES ($1, $2, $3, $4, 'debit', 1::numeric, $5, now())
	`, forgedID, int64(9399), f.currencyID, f.classificationID, effectiveAt)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at, created_at)
		VALUES ($1, $2, $3, $4, 'credit', 1::numeric, $5, now())
	`, forgedID, core.SystemAccountHolder(9399), f.currencyID, f.classificationID, effectiveAt)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	rollup := postgres.NewRollupAdapter(pool)
	reconcileAdapter := postgres.NewReconcileAdapter(pool)
	queries := postgres.NewQueryStore(pool)
	engine := core.NewEngine()
	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
	// Page limit of 2: an oldest-first scan would see only the two earliest
	// (valid) journals and report clean.
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{
		UnauthorizedJournalsPageLimit: 2,
	}, engine)
	full.SetAuthCheck(queries, verifier)

	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check := findCheck(t, report, "unauthorized_journals")
	assert.False(t, check.Passed,
		"the forged signature is among the NEWEST journals; a one-page scan has to look there. check: %+v", check)
	var namedTheForgery bool
	for _, finding := range check.Findings {
		if containsUID(finding.Description, forgedUID) {
			namedTheForgery = true
		}
	}
	assert.True(t, namedTheForgery, "the finding must name the forged journal; findings: %+v", check.Findings)
}

func TestFullReconciliation_UnauthorizedJournals_ReportsIncompleteWhenPageLimitHit(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-key-uj-4")
	require.NoError(t, err)
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)
	_, err = ledgerStore.PostJournal(ctx, f.journalInput(9105, postgrestest.UniqueKey("uj-limit-1")))
	require.NoError(t, err)
	_, err = ledgerStore.PostJournal(ctx, f.journalInput(9106, postgrestest.UniqueKey("uj-limit-2")))
	require.NoError(t, err)

	rollup := postgres.NewRollupAdapter(pool)
	reconcileAdapter := postgres.NewReconcileAdapter(pool)
	queries := postgres.NewQueryStore(pool)
	engine := core.NewEngine()
	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{
		UnauthorizedJournalsPageLimit: 1,
	}, engine)
	full.SetAuthCheck(queries, verifier)

	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check := findCheck(t, report, "unauthorized_journals")
	assert.False(t, check.Complete, "hitting the page limit must not claim full coverage")
}

// TestFullReconciliation_UnauthorizedJournals_PartialCoverageIsIncomplete is
// W3-M7's pin (2026-09-02 adversarial re-review, w3-review/money-path.md M-7).
// C-M8 above made "ZERO journals carried a signature" report Complete=false,
// but left the skip itself untouched: one genuinely signed journal in the page
// was enough to set checked=1, and every forged, never-signed journal beside
// it was silently dropped -- no Finding, no count, Complete=true, the check's
// alertable signal GREEN. On any fleet with signing history that condition is
// always met, so 1-of-200 coverage and 200-of-200 coverage were the same
// machine-readable output.
//
// The rule is now checked == len(journalList): anything this check did not
// verify is reported as skipped_unsigned=N and costs Complete, never Passed
// (skipping is a coverage gap, not a violation).
func TestFullReconciliation_UnauthorizedJournals_PartialCoverageIsIncomplete(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-key-uj-partial")
	require.NoError(t, err)

	// One real, correctly signed journal -- the single row that used to buy
	// the whole page a green light.
	signedStore := postgres.NewLedgerStore(pool).WithAuth(attestor)
	_, err = signedStore.PostJournal(ctx, f.journalInput(9301, postgrestest.UniqueKey("uj-partial-signed")))
	require.NoError(t, err)

	// Three journals inserted the way a leaked ledger_app credential inserts
	// them: no signature at all (auth_key_id = '').
	for i := 0; i < 3; i++ {
		insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("uj-partial-forged"))
	}

	rollup := postgres.NewRollupAdapter(pool)
	reconcileAdapter := postgres.NewReconcileAdapter(pool)
	queries := postgres.NewQueryStore(pool)
	engine := core.NewEngine()
	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{}, engine)
	full.SetAuthCheck(queries, verifier)

	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check := findCheck(t, report, "unauthorized_journals")

	assert.True(t, check.Passed, "an unsigned journal is a coverage gap, not a violation: %+v", check.Findings)
	assert.False(t, check.Complete,
		"3 of the 4 journals on this page were never verified; claiming full coverage is the M-7 hole: %+v", check)
	assert.False(t, report.FullCoverage, "the report must carry that incompleteness up")

	var found bool
	for _, finding := range check.Findings {
		if strings.Contains(finding.Description, "skipped_unsigned=3") {
			found = true
		}
	}
	assert.True(t, found,
		"the number of journals this check could not verify must be in the output, not only in Complete; findings: %+v", check.Findings)
}
