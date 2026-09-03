package postgres_test

// F-1 (2026-09-03 independent review): dropping `UNIQUE` from
// journals.idempotency_key left `go test ./...` entirely green. I-3 listed
// fifteen pins and not one of them noticed, because every one of them wrote
// through the Go path, where acquireIdempotencyLock serializes on
// hashtextextended(key) and a pre-read settles the duplicate long before the
// database is asked. The constraint is not there for that path.
//
// financial.md's line -- "every write operation must carry an
// idempotency_key (UNIQUE index)" -- is about the paths that do NOT hold the
// advisory lock: a second process or replica whose lock does not overlap,
// a leaked ledger_app credential writing straight to the table (it does hold
// INSERT), and replay after a point-in-time restore. All three arrive as raw
// SQL, which is how these pins arrive too.
//
// Two layers, because they fail differently:
//
//   - Behaviour, on the table closest to the money: two INSERTs with the
//     same key, second one must be 23505. This is the shape
//     TestJournalBalanceTrigger_RejectsDirectSQLImbalance uses for I-24.
//   - Catalogue, derived, so it covers tables that do not exist yet: every
//     column named idempotency_key must be covered by a unique index that
//     is valid, ready, total (not partial) and over that column alone. A
//     new table with an unguarded idempotency_key is red on arrival, and
//     nobody has to remember to add it here.
//
// The catalogue half also closes something the behaviour half cannot see:
// an index left INVALID by a failed CREATE INDEX CONCURRENTLY is present in
// pg_class and enforces nothing.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
)

// TestJournalIdempotencyKey_RejectsDirectSQLDuplicate pins I-3 at the layer
// the advisory lock cannot reach.
//
// The first insert is not decoration: without it, an INSERT that failed for
// an unrelated reason (a NOT NULL, a FK, a typo in the column list) would
// read exactly like the constraint working -- the vacuous-negative shape
// this repo's other DB-layer pins take pains to avoid.
func TestJournalIdempotencyKey_RejectsDirectSQLDuplicate(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	_, deps := setupInvariantsFixture(t, pool, ctx)

	journalTypeID := postgrestest.InternalID(t, pool, "journal_types", deps.JournalType)
	key := postgrestest.UniqueKey("dup-idem")

	// total_debit = total_credit satisfies chk_journal_balance, and > 0
	// satisfies chk_journal_nonzero, so the only constraint left to fail on
	// is the one under test. Zero entries means the deferred per-currency
	// balance trigger has nothing to object to either.
	insert := func() error {
		_, err := pool.Exec(ctx,
			`INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, source, uid)
			 VALUES ($1, $2, 1, 1, 'f1-direct-sql', gen_random_uuid())`,
			journalTypeID, key)
		return err
	}

	require.NoError(t, insert(),
		"the first direct-SQL journal must go in -- if it does not, the rejection below proves nothing about uniqueness")

	err := insert()
	require.Error(t, err,
		"a second journal carrying an idempotency_key the ledger has already used must be refused by the database.\n\n"+
			"Every pin I-3 had went through PostJournal, where an advisory lock and a pre-read settle the duplicate before "+
			"Postgres is consulted; removing the UNIQUE left all fifteen green (F-1). The credential that arrives without "+
			"that lock -- a second replica, a leaked ledger_app, a replayed WAL -- is the one this constraint is for")

	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "expected a Postgres error, got %v", err)
	assert.Equal(t, "23505", pgErr.Code,
		"the duplicate must be refused as a unique violation (23505), not as something else that happens to fail: %s", pgErr.Message)
}

// idempotencyIndex describes how one table's idempotency_key column is (or
// is not) covered by a unique index.
type idempotencyIndex struct {
	table     string
	indexName string
	columns   []string
	unique    bool
	valid     bool
	ready     bool
	partial   bool
}

// TestEveryIdempotencyKeyColumnHasATotalUniqueIndex derives I-3's DB half
// from the live catalogue rather than from a list somebody maintains.
//
// The register in core/invariants_pins_test.go used to stand in for this,
// and it looked for the literal string "idempotency_key TEXT UNIQUE NOT
// NULL" anywhere in the baseline migration. That is a whole-file substring
// search over five declarations: deleting four of them still matched
// (measured -- F-1). Asking Postgres what it actually built cannot be
// satisfied by a string that survives elsewhere in the file.
func TestEveryIdempotencyKeyColumnHasATotalUniqueIndex(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	rows, err := pool.Query(ctx, `
		SELECT c.relname                                   AS table_name,
		       COALESCE(i.relname, '')                     AS index_name,
		       COALESCE(array_agg(a.attname ORDER BY k.ord)
		                FILTER (WHERE a.attname IS NOT NULL), '{}') AS columns,
		       COALESCE(bool_or(x.indisunique), false)      AS is_unique,
		       COALESCE(bool_or(x.indisvalid), false)       AS is_valid,
		       COALESCE(bool_or(x.indisready), false)       AS is_ready,
		       COALESCE(bool_or(x.indpred IS NOT NULL), false) AS is_partial
		FROM pg_class c
		JOIN pg_namespace n      ON n.oid = c.relnamespace
		JOIN pg_attribute target ON target.attrelid = c.oid
		                        AND target.attname = 'idempotency_key'
		                        AND target.attnum > 0
		                        AND NOT target.attisdropped
		LEFT JOIN pg_index x     ON x.indrelid = c.oid
		                        AND x.indisunique
		                        AND target.attnum = ANY (x.indkey::int2[])
		LEFT JOIN pg_class i     ON i.oid = x.indexrelid
		LEFT JOIN LATERAL unnest(x.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord) ON true
		LEFT JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
		WHERE n.nspname = 'public' AND c.relkind = 'r'
		GROUP BY c.relname, i.relname
		ORDER BY c.relname, i.relname
	`)
	require.NoError(t, err)
	defer rows.Close()

	byTable := map[string][]idempotencyIndex{}
	for rows.Next() {
		var idx idempotencyIndex
		require.NoError(t, rows.Scan(&idx.table, &idx.indexName, &idx.columns,
			&idx.unique, &idx.valid, &idx.ready, &idx.partial))
		byTable[idx.table] = append(byTable[idx.table], idx)
	}
	require.NoError(t, rows.Err())

	// Fail-closed: if the catalogue query above ever stops matching (a
	// column rename, a schema move), this check inspects nothing and reads
	// as a pass -- working-agreements.md §3.
	require.GreaterOrEqual(t, len(byTable), 5,
		"only %d table(s) with an idempotency_key column were found. This ledger has at least five (journals, bookings, "+
			"reservations, the two terminal-op receipt tables, ingest_dead_letters); a query that finds almost none is a "+
			"broken query, not a clean schema", len(byTable))

	for table, indexes := range byTable {
		var covered bool
		for _, idx := range indexes {
			if idx.indexName == "" || !idx.unique {
				continue
			}
			// A composite or partial unique index does not make the key
			// unique: (idempotency_key, tenant_id) admits the same key
			// twice, and a partial one admits it outside the predicate.
			// Both look like coverage in a migration diff.
			if len(idx.columns) != 1 || idx.columns[0] != "idempotency_key" || idx.partial {
				continue
			}
			assert.Truef(t, idx.valid && idx.ready,
				"%s.%s is not a live index (indisvalid=%v indisready=%v) -- a unique index left behind by a failed "+
					"CREATE INDEX CONCURRENTLY is present in the catalogue and enforces nothing",
				table, idx.indexName, idx.valid, idx.ready)
			if idx.valid && idx.ready {
				covered = true
			}
		}
		assert.Truef(t, covered,
			"table %q has an idempotency_key column with no total, single-column, valid unique index on it.\n\n"+
				"financial.md: every write operation must carry an idempotency_key (UNIQUE index). The Go write paths "+
				"take an advisory lock and pre-read, so they will look correct without this -- that is exactly how "+
				"dropping the UNIQUE from journals left the whole suite green (F-1). Indexes seen on this table: %+v",
			table, indexes)
	}
}

// --- The other CHECK constraints INVARIANTS.md claims, pinned directly ---
//
// F-1's general form: a section whose **Enforced by** names a Postgres
// constraint needs at least one pin that arrives the way an unlocked writer
// arrives. These three did not have one -- their pins all went through the
// Go API, where the same rule is checked earlier and the constraint never
// gets consulted.

// TestJournalTotalsCheck_RejectsDirectSQLImbalance pins the second half of
// I-1's DB layer: `chk_journal_balance` on journals.total_debit =
// total_credit. The deferred per-currency trigger (I-24's pin) covers the
// entry legs; this covers the header row, which is what every balance
// summary and reconcile scan reads first.
func TestJournalTotalsCheck_RejectsDirectSQLImbalance(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	_, deps := setupInvariantsFixture(t, pool, ctx)
	journalTypeID := postgrestest.InternalID(t, pool, "journal_types", deps.JournalType)

	post := func(debit, credit string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, source, uid)
			 VALUES ($1, $2, $3::numeric, $4::numeric, 'f1-totals-check', gen_random_uuid())`,
			journalTypeID, postgrestest.UniqueKey("totals"), debit, credit)
		return err
	}

	require.NoError(t, post("100", "100"),
		"a balanced header must go in -- otherwise the rejection below could be any other constraint failing")

	err := post("100", "99")
	require.Error(t, err, "a journal header whose totals disagree must be refused by the database, not only by Go")
	assertCheckViolation(t, err, "chk_journal_balance")
}

// TestBalanceRoleCheck_RejectsDirectSQLUnknownRole pins the DB half of
// I-11. balance_role is the basis Reserve sums over: a classification
// carrying a role the reader does not know about is money that is either
// invisible or double-counted, depending on which side of the sum the
// unknown value lands on. The Go path validates the role, so the CHECK is
// for everything that does not use the Go path.
func TestBalanceRoleCheck_RejectsDirectSQLUnknownRole(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	insert := func(role string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO classifications (code, name, normal_side, balance_role, uid)
			 VALUES ($1, 'F-1 balance role pin', 'debit', $2, gen_random_uuid())`,
			postgrestest.UniqueKey("f1-role"), role)
		return err
	}

	require.NoError(t, insert("available"),
		"a known role must be accepted -- without this the rejection below proves nothing")

	err := insert("spendable")
	require.Error(t, err,
		"a balance_role outside the known vocabulary must be refused by the database. Every reader of this column "+
			"partitions on it; a value none of them match is silently excluded from the available basis (I-11)")
	assertCheckViolation(t, err, "balance_role")
}

// TestCurrencyExponentCheck_RejectsDirectSQLOutOfRange pins the DB half of
// I-16. The exponent is the bound every amount-precision check is measured
// against (core.Round, validateEntriesPrecision, checkAmountPrecision); an
// exponent outside 0..18 makes the bound itself meaningless, and
// NUMERIC(30,18) cannot store what a larger one would admit.
func TestCurrencyExponentCheck_RejectsDirectSQLOutOfRange(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	insert := func(exponent int) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO currencies (code, name, exponent, uid)
			 VALUES ($1, 'F-1 exponent pin', $2, gen_random_uuid())`,
			postgrestest.UniqueKey("F1EXP"), exponent)
		return err
	}

	require.NoError(t, insert(8),
		"an in-range exponent must be accepted -- without this the rejections below prove nothing")

	for _, exponent := range []int{-1, 19} {
		err := insert(exponent)
		require.Errorf(t, err,
			"exponent %d is outside 0..18 and must be refused by the database. NUMERIC(30,18) cannot store more than "+
				"eighteen fractional digits, so an exponent above it turns the precision check into a promise the "+
				"storage does not keep (I-16)", exponent)
		assertCheckViolation(t, err, "exponent")
	}
}

// assertCheckViolation requires err to be Postgres 23514 (check_violation)
// and to name the constraint the caller expected -- so a row rejected by
// some other rule cannot be read as the rule under test working.
func assertCheckViolation(t *testing.T, err error, constraintSubstring string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "expected a Postgres error, got %v", err)
	assert.Equal(t, "23514", pgErr.Code,
		"expected a check violation (23514), got %s: %s", pgErr.Code, pgErr.Message)
	assert.Containsf(t, pgErr.ConstraintName, constraintSubstring,
		"the row was rejected by %q, not by the constraint this pin is about (%q). A test that accepts any rejection "+
			"passes for the wrong reason the moment the intended constraint is dropped",
		pgErr.ConstraintName, constraintSubstring)
}
