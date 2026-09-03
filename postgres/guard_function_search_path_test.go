package postgres_test

// Gate and pins for migration 030 / I-68 -- the C1 and C2 findings of
// `docs/audits/2026-09-03-independent-review/install-roles.md`.
//
// C1: `check_journal_currency_balance()` was SECURITY INVOKER with an empty
// proconfig and an unqualified `FROM journal_entries`, and PostgreSQL searches
// pg_temp FIRST for relation names when pg_temp is not itself on the path. One
// `CREATE TEMP TABLE journal_entries (...)` on a leaked `ledger_app`
// connection made the deferred balance trigger aggregate over an empty shadow,
// and a one-sided 999,999 entry committed.
//
// C2: the same function's dedup set was a `pg_temp` table created with
// `CREATE TEMP TABLE IF NOT EXISTS`, so a caller could create it first with
// `ON COMMIT PRESERVE ROWS`, pre-fill predictable journal ids, and make the
// aggregate never run at all -- independent of C1, and unfixable by any
// search_path pin, because a temp table can only live in pg_temp.
//
// The gate below replaces `TestPartitionFunctions_SearchPathIncludesPgTemp`
// (migration 013), which enumerated two SECURITY DEFINER partition functions
// by name. Nine SECURITY INVOKER guard functions were unpinned the whole time
// and that test could never have said so. This one asks the catalogue.

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
)

// wantSearchPath is the one form migration 030 settled on for every function
// in this schema, SECURITY DEFINER and SECURITY INVOKER alike.
//
// pg_temp is present and LAST. Omitting it does not exclude it -- an
// unqualified search_path implicitly searches pg_temp FIRST for relation names
// -- and putting pg_catalog first would send unqualified CREATE statements
// there (`permission denied to create "pg_catalog.x"`), which is exactly what
// ledger_create_monthly_partition does. Both measured; see 030's header.
const wantSearchPath = "search_path=public, pg_temp"

// searchPathExempt is the complete list of functions allowed to have no
// proconfig, and the reason is performance, not oversight: a `SET` clause
// makes a LANGUAGE sql function **un-inlinable**, and these two sit inside the
// balance / rollup / holder / trend aggregations. Measured over 50,000 rows:
// 3.770 ms inlined vs 31.453 ms pinned.
//
// The exemption is only safe because these functions cannot reference a
// relation at all, so TestGuardFunctionSearchPath_ExemptionsCannotReadRelations
// re-derives that from the catalogue rather than trusting this comment.
var searchPathExempt = map[string]bool{
	"ledger_signed_amount(text,text,numeric)":   true,
	"ledger_signed_delta(text,numeric,numeric)": true,
}

// TestGuardFunctionSearchPath_EveryFunctionIsPinned is the catalogue-derived
// gate: every function in `public` carries exactly `wantSearchPath`, except
// the named exemptions.
//
// Closed by default, opened by review -- the same direction as
// grant_coverage_test.go and function_acl_test.go. A migration that adds a
// function and forgets the pin turns this red on the first CI run, which is
// the property migration 013's hand-written two-name list did not have.
func TestGuardFunctionSearchPath_EveryFunctionIsPinned(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT p.oid::regprocedure::text,
		       COALESCE(p.proconfig, ARRAY[]::text[]),
		       p.prosecdef,
		       EXISTS (SELECT 1 FROM pg_trigger t WHERE t.tgfoid = p.oid AND NOT t.tgisinternal)
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public' AND p.prokind IN ('f', 'p')
		ORDER BY 1
	`)
	require.NoError(t, err)
	defer rows.Close()

	type fn struct {
		sig     string
		config  []string
		secdef  bool
		trigger bool
	}
	var fns []fn
	for rows.Next() {
		var f fn
		require.NoError(t, rows.Scan(&f.sig, &f.config, &f.secdef, &f.trigger))
		fns = append(fns, f)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, fns, "sanity: the schema installs functions")

	seenExempt := map[string]bool{}
	for _, f := range fns {
		if searchPathExempt[f.sig] {
			seenExempt[f.sig] = true
			assert.Empty(t, f.config,
				"%s is on the search_path exemption list because a SET clause would stop it being inlined; it must therefore have no proconfig at all", f.sig)
			assert.False(t, f.secdef, "%s: a SECURITY DEFINER function may never be exempt from pinning search_path", f.sig)
			assert.False(t, f.trigger, "%s: a trigger function may never be exempt from pinning search_path", f.sig)
			continue
		}

		var searchPath string
		for _, cfg := range f.config {
			if strings.HasPrefix(cfg, "search_path=") {
				searchPath = cfg
			}
		}
		assert.Equal(t, wantSearchPath, searchPath,
			"%s must pin search_path to exactly %q -- pg_temp present and last. Omitting pg_temp promotes it to first for relation names (the C1 vector); adding a function without the pin is how C1 survived three audit rounds", f.sig, wantSearchPath)
	}

	for sig := range searchPathExempt {
		assert.True(t, seenExempt[sig],
			"%s is on the exemption list but no longer exists -- a stale exemption is a hole waiting for a name collision", sig)
	}
}

// TestGuardFunctionSearchPath_ExemptionsCannotReadRelations bounds the
// exemption structurally, so it cannot later be widened to cover a function
// that has something to shadow.
//
// An exempt function must be LANGUAGE sql, IMMUTABLE, not SECURITY DEFINER,
// referenced by no trigger, and its source must not name a relation.
func TestGuardFunctionSearchPath_ExemptionsCannotReadRelations(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	readsRelations := regexp.MustCompile(`(?i)\b(from|join|insert|update|delete|merge|table)\b`)

	for sig := range searchPathExempt {
		sig := sig
		t.Run(sig, func(t *testing.T) {
			var lang, volatile, src string
			var secdef, trigger bool
			require.NoError(t, pool.QueryRow(ctx, `
				SELECT l.lanname, p.provolatile, p.prosrc, p.prosecdef,
				       EXISTS (SELECT 1 FROM pg_trigger t WHERE t.tgfoid = p.oid AND NOT t.tgisinternal)
				FROM pg_proc p
				JOIN pg_namespace n ON n.oid = p.pronamespace
				JOIN pg_language l ON l.oid = p.prolang
				WHERE n.nspname = 'public' AND p.oid::regprocedure::text = $1
			`, sig).Scan(&lang, &volatile, &src, &secdef, &trigger))

			assert.Equal(t, "sql", lang, "the exemption exists to preserve SQL-function inlining; nothing else can claim it")
			assert.Equal(t, "i", volatile, "%s must be IMMUTABLE -- a volatile function is doing something worth pinning", sig)
			assert.False(t, secdef, "%s must not be SECURITY DEFINER", sig)
			assert.False(t, trigger, "%s must not back a trigger", sig)
			assert.NotRegexp(t, readsRelations, src,
				"%s names a relation, so it has something for pg_temp to shadow and cannot stay exempt:\n%s", sig, src)
		})
	}
}

// TestLedgerApp_CannotCreateTemporaryRelations pins migration 030 section 3:
// TEMPORARY is withdrawn from PUBLIC, which is the layer under the pin and the
// rewrite.
//
// It has to be revoked from PUBLIC, not from ledger_app: a privilege reaching
// a role through PUBLIC can only be revoked from PUBLIC, so
// `REVOKE TEMPORARY ... FROM ledger_app` would have been a no-op that reads
// like a fix.
func TestLedgerApp_CannotCreateTemporaryRelations(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "searchpath-temp-revoked-not-a-real-secret") //nolint:gosec

	_, err := appPool.Exec(ctx, `CREATE TEMP TABLE w5_probe (x int)`)
	require.Error(t, err, "ledger_app must not be able to create a temporary relation at all -- that is what C1 and C2 both needed")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "42501", pgErr.Code, "expected insufficient_privilege, got %s: %s", pgErr.Code, pgErr.Message)

	var publicHasTemp bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT (d.datacl IS NULL)
		    OR EXISTS (SELECT 1 FROM aclexplode(d.datacl) a WHERE a.grantee = 0 AND a.privilege_type = 'TEMPORARY')
		FROM pg_database d WHERE d.datname = current_database()
	`).Scan(&publicHasTemp))
	assert.False(t, publicHasTemp, "PUBLIC must not hold TEMPORARY on the ledger database")
}

// withTemporaryGrantedBack hands ledger_app the TEMPORARY privilege for the
// duration of fn, so the two layers underneath it can be tested on their own.
//
// Testing C1/C2 only through "ledger_app cannot create a temp table" would
// make the pin depend entirely on section 3 of migration 030, and a future
// deployment that grants TEMPORARY back for some unrelated reason would
// silently re-open both. The guard has to hold without that layer, and these
// tests are what say so.
func withTemporaryGrantedBack(t *testing.T, pool *pgxpool.Pool, fn func()) {
	t.Helper()
	ctx := context.Background()

	var db string
	require.NoError(t, pool.QueryRow(ctx, `SELECT current_database()`).Scan(&db))
	_, err := pool.Exec(ctx, `GRANT TEMPORARY ON DATABASE `+pgx.Identifier{db}.Sanitize()+` TO ledger_app`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(ctx, `REVOKE TEMPORARY ON DATABASE `+pgx.Identifier{db}.Sanitize()+` FROM ledger_app`)
		require.NoError(t, err, "the grant must come back off -- TestLedgerApp_CannotCreateTemporaryRelations depends on it")
	})

	fn()
}

// TestBalanceGuard_SurvivesPgTempRelationShadowing is C1's red pin, run as
// ledger_app with TEMPORARY handed back so the attack can actually be
// attempted.
//
// The measured pre-030 behaviour was: with `CREATE TEMP TABLE journal_entries`
// in the session, an unbalanced INSERT committed and the holder gained 999,999
// out of nothing. The identical statements must now be refused.
func TestBalanceGuard_SurvivesPgTempRelationShadowing(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	store, deps := setupInvariantsFixture(t, pool, ctx)
	appPool := newAppPool(t, pool, "searchpath-c1-shadow-not-a-real-secret") //nolint:gosec

	journalID := postBalancedJournal(t, store, pool, ctx, deps, 5101, decimal.NewFromInt(100), "c1-shadow")
	currencyID := postgrestest.InternalID(t, pool, "currencies", deps.Currency)
	classID := postgrestest.InternalID(t, pool, "classifications", deps.MainWallet)

	withTemporaryGrantedBack(t, pool, func() {
		conn, err := appPool.Acquire(ctx)
		require.NoError(t, err)
		defer conn.Release()

		_, err = conn.Exec(ctx, `
			CREATE TEMP TABLE journal_entries (
				journal_id bigint, currency_id bigint, entry_type text, amount numeric
			)`)
		require.NoError(t, err, "the attack's first statement must still be reachable, or this pin proves nothing about the guard")

		_, err = conn.Exec(ctx, `
			INSERT INTO public.journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
			VALUES ($1, $2, $3, $4, 'debit', 999999)`,
			journalID, 5101, currencyID, classID)
		require.Error(t, err, "a one-sided entry must be refused even with public.journal_entries shadowed in pg_temp")
		assert.Contains(t, err.Error(), "unbalanced entries",
			"the refusal must come from the balance guard, not from some unrelated error that happens to look like success")
	})

	// The imbalance must not have landed.
	var drift decimal.Decimal
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN entry_type='debit' THEN amount ELSE -amount END), 0)
		FROM journal_entries WHERE journal_id = $1`, journalID).Scan(&drift))
	assert.True(t, drift.IsZero(), "the journal must still balance; drift = %s", drift)
}

// TestBalanceGuard_DedupSetCannotBePreSeeded is C2's red pin.
//
// Pre-030 the guard skipped its aggregate whenever `pg_temp
// .ledger_balance_checked` already contained the journal id, and the caller
// could create that table (`ON COMMIT PRESERVE ROWS`) and fill it with
// `generate_series` before writing anything. Migration 030 keeps no
// caller-writable memo at all, so the pre-seeded table is inert.
//
// The transaction here is the strongest shape of the attack available to
// ledger_app: it writes the journals row and the single one-sided entry
// itself, so both the journals-level trigger and the per-entry fallback are in
// scope.
func TestBalanceGuard_DedupSetCannotBePreSeeded(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	_, deps := setupInvariantsFixture(t, pool, ctx)
	appPool := newAppPool(t, pool, "searchpath-c2-preseed-not-a-real-secret") //nolint:gosec

	currencyID := postgrestest.InternalID(t, pool, "currencies", deps.Currency)
	classID := postgrestest.InternalID(t, pool, "classifications", deps.MainWallet)
	journalTypeID := postgrestest.InternalID(t, pool, "journal_types", deps.JournalType)

	withTemporaryGrantedBack(t, pool, func() {
		conn, err := appPool.Acquire(ctx)
		require.NoError(t, err)
		defer conn.Release()

		_, err = conn.Exec(ctx, `
			CREATE TEMP TABLE ledger_balance_checked (journal_id BIGINT PRIMARY KEY)`)
		require.NoError(t, err, "pre-seeding must still be reachable, or this pin proves nothing")
		_, err = conn.Exec(ctx, `INSERT INTO ledger_balance_checked SELECT generate_series(1, 10000)`)
		require.NoError(t, err)

		tx, err := conn.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()

		var journalID int64
		require.NoError(t, tx.QueryRow(ctx, `
			INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, uid)
			VALUES ($1, $2, 999999, 999999, gen_random_uuid()) RETURNING id`,
			journalTypeID, postgrestest.UniqueKey("c2-preseed")).Scan(&journalID),
			"ledger_app can write journals; that is not what this test is about")

		_, err = tx.Exec(ctx, `
			INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
			VALUES ($1, $2, $3, $4, 'debit', 999999)`,
			journalID, 5202, currencyID, classID)
		if err == nil {
			err = tx.Commit(ctx)
		}
		require.Error(t, err, "a pre-seeded dedup set must not buy the caller a skipped balance check")
		assert.Contains(t, err.Error(), "unbalanced entries")
	})
}

// TestBalanceGuard_NoOpUpdateCannotAdoptAnOldJournal used to live here. It
// pinned the xmin-adoption bypass against migration 030's journals-level
// trigger, and it was one `SET CONSTRAINTS ALL IMMEDIATE` away from red: the
// recheck showed that statement moves the journals-level check to a moment
// when the journal has no entries, after which the per-entry skip lets
// everything through (N1). Migration 031 removed the skip and the
// journals-level trigger with it, so there is no longer an INSERT-only variant
// to contrast against.
//
// The attack itself did not go away, it got a better home: it is one of the
// three shapes in postgres/constraint_timing_test.go, replayed under every
// constraint-timing mode the catalogue reports, with the reverse confirmation
// (restore 030's skip, watch it succeed) alongside it.

// TestBalanceGuard_LegitimateJournalsStillPost is the control: none of the
// above is achieved by refusing everything. A many-legged, multi-statement
// journal still posts through the real write path, and the journals-level
// trigger accepts a journal whose entries arrive after it.
func TestBalanceGuard_LegitimateJournalsStillPost(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	store, deps := setupInvariantsFixture(t, pool, ctx)

	entries := make([]core.EntryInput, 0, 40)
	for i := 0; i < 20; i++ {
		entries = append(entries,
			core.EntryInput{AccountHolder: 5401, CurrencyUID: deps.Currency, ClassificationUID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(7)},
			core.EntryInput{AccountHolder: -5401, CurrencyUID: deps.Currency, ClassificationUID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(7)},
		)
	}
	j, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: deps.JournalType,
		IdempotencyKey: postgrestest.UniqueKey("w5-control-multileg"),
		Source:         "searchpath-control",
		Entries:        entries,
	})
	require.NoError(t, err, "a 40-leg balanced journal must still post")

	journalID := postgrestest.InternalID(t, pool, "journals", j.UID)
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE journal_id = $1`, journalID).Scan(&count))
	assert.Equal(t, 40, count)
}
