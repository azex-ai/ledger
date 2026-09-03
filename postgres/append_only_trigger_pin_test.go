package postgres_test

// F-2 (2026-09-03 independent review): `ledger_block_mutation()` was
// rewritten to `BEGIN RETURN NEW; END` -- the function name kept, so the
// catalogue self-checks in migrations 001 and 006 (which match on
// `tgfoid = 'public.ledger_block_mutation()'::regprocedure`) all still
// passed -- and the entire suite went red in three places. Twenty-two of
// the twenty-five append-only triggers had no pin at all, including
// `journal_entries_no_update`, `journal_entries_no_delete` and
// `journals_no_delete`: the DB-layer backing for I-2 and I-25 on the
// ledger proper.
//
// Eight tests in this repo do `ALTER TABLE journal_entries DISABLE TRIGGER
// journal_entries_no_update` in order to plant tampering, so everyone knew
// the trigger was there. Disabling it is not an assertion that it works.
//
// The one test whose NAME claimed to cover this,
// TestIdempotencyReceiptTablesAreAppendOnly, wrote as `ledger_app` -- which
// has no UPDATE grant on those tables, so the statement was refused by the
// ACL and never reached a trigger. It measured least-privilege (I-22) while
// being named for append-only (I-25).
//
// This file derives its cases from pg_trigger, so a trigger added later is
// covered on arrival and a trigger deleted makes the census shrink. Each
// case seeds one row, then really runs the UPDATE or DELETE the trigger
// exists to refuse, as the OWNER -- the credential that does hold the
// privilege, so what refuses the statement can only be the trigger.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
)

// blockedMutation is one (table, event) pair guarded by
// ledger_block_mutation(), read from the catalogue.
//
// tgparentid = 0 keeps this to declared triggers. journal_entries is
// partitioned, so Postgres clones each of its triggers onto every
// partition; counting the clones would make the census grow every time a
// month rolls over, and seeding has to go through the parent anyway for
// tuple routing to place the row.
type blockedMutation struct {
	trigger  string
	table    string
	event    string // "UPDATE" or "DELETE"
	enabled  string // pg_trigger.tgenabled
	isBefore bool
	isRow    bool
}

// appendOnlyGuardFloor is the number of (table, event) pairs
// ledger_block_mutation() guarded when this gate was written. It is a
// floor, not a snapshot: adding a guard is ordinary work and needs no edit
// here, but losing one has to be a deliberate edit to this number.
// 25 when the gate was written against main c854c6e; 23 since migration 029,
// which replaced the two blanket DELETE guards on ledger_attestations and
// entry_attestations with ledger_attestation_chain_block_delete() -- the same
// refusal plus one owner-only door (ledger_discard_attestations_from). Those
// two are pinned by TestPoisonedAttestationTailHasAWayBack instead, which is
// why lowering this number is the honest edit rather than a loss.
const appendOnlyGuardFloor = 25

// appendOnlyGuardFunctions are the trigger functions that make a table
// append-only. Derived from, rather than assumed to be, one name.
//
// R-2 (2026-09-04 recheck): migration 029 moved the two attestation tables
// off ledger_block_mutation onto ledger_attestation_chain_block_delete,
// which refuses a DELETE exactly as before unless the owner has opened the
// audited discard door. The census matched one function name, so the two
// moved triggers silently left it -- appendOnlyGuardFloor went 25 -> 23 --
// and entry_attestations_no_delete ended up with no pin at all. Deleting it
// from 029 left the whole suite green, which matters because that trigger
// is what stops the per-entry coverage rows behind I-27 ("every entry
// covered exactly once") and I-29 from being removed without trace.
//
// A guard replaced by an equivalent guard must stay counted. Naming the set
// is how: a third function joining it is one line here, and a table quietly
// losing its guard still shrinks the floor.
var appendOnlyGuardFunctions = []string{
	"ledger_block_mutation",
	"ledger_attestation_chain_block_delete",
}

func readBlockedMutations(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []blockedMutation {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT tg.tgname, c.relname,
		       CASE WHEN (tg.tgtype & 16) <> 0 THEN 'UPDATE'
		            WHEN (tg.tgtype & 8)  <> 0 THEN 'DELETE'
		            WHEN (tg.tgtype & 4)  <> 0 THEN 'INSERT'
		            ELSE 'OTHER' END,
		       tg.tgenabled,
		       (tg.tgtype & 2) <> 0 AS is_before,
		       (tg.tgtype & 1) <> 0 AS is_row
		FROM pg_trigger tg
		JOIN pg_class c   ON c.oid = tg.tgrelid
		JOIN pg_proc p    ON p.oid = tg.tgfoid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE NOT tg.tgisinternal
		  AND tg.tgparentid = 0
		  AND n.nspname = 'public'
		  AND p.proname = ANY ($1::text[])
		ORDER BY c.relname, tg.tgname`, appendOnlyGuardFunctions)
	require.NoError(t, err)
	defer rows.Close()

	var out []blockedMutation
	for rows.Next() {
		var g blockedMutation
		require.NoError(t, rows.Scan(&g.trigger, &g.table, &g.event, &g.enabled, &g.isBefore, &g.isRow))
		out = append(out, g)
	}
	require.NoError(t, rows.Err())
	return out
}

// TestAppendOnlyGuards_EveryTriggerRefusesItsMutation is the behavioural
// half: for every trigger the catalogue says runs ledger_block_mutation(),
// seed a row and run the statement it is supposed to refuse.
//
// Seeding is generic on purpose. A per-table row factory would be
// twenty-five hand-written fixtures that go stale the moment a column is
// added, which is how twenty-two of these ended up with no pin in the first
// place. Instead the row is generated from the catalogue: every NOT NULL
// column without a default gets a value of the right type, and where a
// CHECK pins a column to a vocabulary, the first allowed value. Foreign
// keys are satisfied by seeding under session_replication_role = replica,
// which suppresses the FK triggers for that one INSERT and nothing else --
// the guard triggers are back on for the UPDATE / DELETE that follows,
// which is what the assertion is about.
func TestAppendOnlyGuards_EveryTriggerRefusesItsMutation(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	guards := readBlockedMutations(t, ctx, pool)
	require.GreaterOrEqualf(t, len(guards), appendOnlyGuardFloor,
		"pg_trigger reports only %d trigger(s) running ledger_block_mutation(), against a floor of %d.\n\n"+
			"Either an append-only guard was dropped, or this query stopped matching -- and a census that finds "+
			"nothing reads as a pass. If a guard was retired deliberately, lower appendOnlyGuardFloor in the same "+
			"commit that retires it, so the loss is a line in the diff rather than a silence",
		len(guards), appendOnlyGuardFloor)

	for _, g := range guards {
		t.Run(g.trigger, func(t *testing.T) {
			assert.Equalf(t, "O", g.enabled,
				"trigger %s on %s is not enabled (tgenabled=%q). A guard left disabled by an ALTER TABLE ... DISABLE "+
					"TRIGGER that was never re-enabled is present in the catalogue and stops nothing",
				g.trigger, g.table, g.enabled)
			assert.Truef(t, g.isBefore && g.isRow,
				"trigger %s on %s must be BEFORE ... FOR EACH ROW (before=%v row=%v): an AFTER trigger cannot refuse "+
					"the statement, and a statement-level one never sees the rows",
				g.trigger, g.table, g.isBefore, g.isRow)
			require.Containsf(t, []string{"UPDATE", "DELETE"}, g.event,
				"trigger %s guards %s, which this pin does not know how to provoke", g.trigger, g.event)

			seedGuardedRow(t, ctx, pool, g.table)

			var stmt string
			if g.event == "UPDATE" {
				stmt = fmt.Sprintf("UPDATE %s SET %s = %s", g.table, guardTouchColumn(t, ctx, pool, g.table), guardTouchColumn(t, ctx, pool, g.table))
			} else {
				stmt = fmt.Sprintf("DELETE FROM %s", g.table)
			}

			_, err := pool.Exec(ctx, stmt)
			require.Errorf(t, err,
				"%s ran against %s and was accepted. This connection is the table OWNER, so no GRANT stands in the "+
					"way -- the only thing that can refuse it is %s, and it did not. That is the state the whole "+
					"suite was in when ledger_block_mutation() was rewritten to `RETURN NEW` (F-2)",
				g.event, g.table, g.trigger)

			var pgErr *pgconn.PgError
			require.ErrorAsf(t, err, &pgErr, "expected a Postgres error from %s on %s, got %v", g.event, g.table, err)
			assert.Equalf(t, "23514", pgErr.Code,
				"%s on %s was refused with SQLSTATE %s (%s), not the check_violation ledger_block_mutation() raises. "+
					"A statement rejected for some other reason is not evidence the guard is working",
				g.event, g.table, pgErr.Code, pgErr.Message)
			assert.Containsf(t, pgErr.Message, "is not allowed",
				"%s on %s was refused, but not by ledger_block_mutation()'s message: %q", g.event, g.table, pgErr.Message)
		})
	}
}

// guardTouchColumn returns a column name that can be self-assigned to make
// an UPDATE statement that changes nothing but still visits every row. The
// trigger fires on the visit, not on the change.
func guardTouchColumn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) string {
	t.Helper()
	var col string
	err := pool.QueryRow(ctx, `
		SELECT a.attname FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname='public' AND c.relname=$1 AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum LIMIT 1`, table).Scan(&col)
	require.NoErrorf(t, err, "find a column to touch on %s", table)
	return col
}

// seedGuardedRow inserts one row into table, generating it from the
// catalogue. It fails the test rather than skipping when it cannot: a
// guarded table this helper cannot populate is a table whose guard goes
// unproven, and an unproven guard that reports `ok` is F-2 all over again.
func seedGuardedRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) {
	t.Helper()

	allowed := checkVocabularies(t, ctx, pool, table)
	unique := uniqueColumns(t, ctx, pool, table)

	rows, err := pool.Query(ctx, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE n.nspname='public' AND c.relname=$1
		  AND a.attnum > 0 AND NOT a.attisdropped
		  AND a.attnotnull AND d.adbin IS NULL AND a.attidentity = ''
		ORDER BY a.attnum`, table)
	require.NoError(t, err)

	var cols, vals []string
	for rows.Next() {
		var name, typ string
		require.NoError(t, rows.Scan(&name, &typ))
		cols = append(cols, name)
		if v, ok := allowed[name]; ok {
			vals = append(vals, quoteLiteral(v))
			continue
		}
		vals = append(vals, guardSeedValue(typ, unique[name]))
	}
	rows.Close()
	require.NoError(t, rows.Err())

	stmt := fmt.Sprintf("INSERT INTO %s DEFAULT VALUES", table)
	if len(cols) > 0 {
		stmt = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(cols, ", "), strings.Join(vals, ", "))
	}

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	// replica suppresses FK and user triggers for this transaction only.
	// The row is a fixture, not a claim about INSERT: what is under test is
	// the UPDATE / DELETE that runs afterwards, on a connection where the
	// guards are on.
	_, err = tx.Exec(ctx, "SET LOCAL session_replication_role = replica")
	require.NoErrorf(t, err, "seeding %s needs session_replication_role, which requires a superuser connection", table)
	_, err = tx.Exec(ctx, stmt)
	require.NoErrorf(t, err, "could not seed a row into %s with %s -- extend guardSeedValue or checkVocabularies rather "+
		"than dropping this table from the census, which would leave its guard unproven (F-2)", table, stmt)
	require.NoError(t, tx.Commit(ctx))
}

// checkVocabularies reads each `col = ANY (ARRAY['a','b'])` CHECK on the
// table and returns the first allowed value per column, so a generated row
// satisfies a vocabulary constraint instead of failing it.
func checkVocabularies(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) map[string]string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT pg_get_constraintdef(con.oid)
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname='public' AND c.relname=$1 AND con.contype='c'`, table)
	require.NoError(t, err)
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var def string
		require.NoError(t, rows.Scan(&def))
		col, value, ok := firstAllowedValue(def)
		if ok {
			if _, seen := out[col]; !seen {
				out[col] = value
			}
		}
	}
	require.NoError(t, rows.Err())
	return out
}

// firstAllowedValue parses `CHECK (((col)::text = ANY ((ARRAY['a'::character
// varying, ...])::text[])))` -- Postgres's normalised rendering of `col IN
// (...)` -- and returns col and the first literal.
func firstAllowedValue(def string) (string, string, bool) {
	i := strings.Index(def, "= ANY")
	if i < 0 {
		return "", "", false
	}
	head := def[:i]
	open := strings.LastIndex(head, "(")
	if open < 0 {
		return "", "", false
	}
	col := strings.Trim(head[open+1:], "() \t")
	if j := strings.Index(col, ")"); j >= 0 {
		col = col[:j]
	}
	col = strings.Trim(col, `" `)
	if col == "" {
		return "", "", false
	}

	rest := def[i:]
	open = strings.Index(rest, "'")
	if open < 0 {
		return "", "", false
	}
	close := strings.Index(rest[open+1:], "'")
	if close < 0 {
		return "", "", false
	}
	return col, rest[open+1 : open+1+close], true
}

func quoteLiteral(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// uniqueColumns returns the columns of table that participate in a unique
// index.
func uniqueColumns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) map[string]bool {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT a.attname
		FROM pg_index x
		JOIN pg_class c ON c.oid = x.indrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = ANY (x.indkey::int2[])
		WHERE n.nspname='public' AND c.relname=$1 AND x.indisunique`, table)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		out[name] = true
	}
	require.NoError(t, rows.Err())
	return out
}

// guardSeedValue picks a value of the right shape for a column.
//
// unique decides between a fresh value and a constant, and it has to be
// both ways round. Unique columns need a fresh value, because each guard on
// a table gets its own row and a constant collides on the second one
// (ledger_attestations.seq, and every idempotency_key). Non-unique columns
// need the constant, because CHECKs relate columns to each other --
// journals carries `CHECK (total_debit = total_credit)`, which two
// independent random numbers fail every time.
func guardSeedValue(typ string, unique bool) string {
	switch {
	case strings.HasSuffix(typ, "[]"):
		return "'{}'"
	case strings.Contains(typ, "uuid"):
		return "gen_random_uuid()"
	case strings.Contains(typ, "jsonb"):
		return "'{}'::jsonb"
	case strings.Contains(typ, "json"):
		return "'{}'::json"
	case strings.Contains(typ, "timestamp"):
		return "now()"
	case strings.Contains(typ, "date"):
		return "now()::date"
	case strings.Contains(typ, "bytea"):
		return "''::bytea"
	case strings.Contains(typ, "bool"):
		return "false"
	case strings.Contains(typ, "numeric"), strings.Contains(typ, "int"),
		strings.Contains(typ, "double"), strings.Contains(typ, "real"):
		if unique {
			return "(floor(random() * 1000000000)::bigint + 1)"
		}
		return "1"
	default:
		if unique {
			return "gen_random_uuid()::text"
		}
		return "''"
	}
}
