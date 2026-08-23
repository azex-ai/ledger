package service_test

// Pin tests for the unauthorized_journals reconcile check
// (docs/plans/2026-08-21-integrity-hardening-contracts.md §W2-2,
// docs/INVARIANTS.md I-32). Reuses attest_verify_test.go/attestation_test.go's
// setupAttestFixture / journalInput / ed25519KeyPair helpers (same
// service_test package) since the fixture shape (currency + one
// classification + journal type) is identical to what P6's own tests need.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

func TestFullReconciliation_UnauthorizedJournals_SkippedWithoutSetAuthCheck(t *testing.T) {
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
	check := findCheck(t, report, "unauthorized_journals")
	assert.True(t, check.Passed, "a skipped check must not report a violation")
	assert.False(t, check.Complete, "a skipped check must never look like full coverage")
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
	assert.True(t, check.Complete)
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
