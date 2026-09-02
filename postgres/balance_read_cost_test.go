package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// balance_read_cost_test.go is H-M6's evidence and its gate.
//
// The audit could only establish the defect statically: journal_entries is
// partitioned by RANGE (created_at), no balance read carries a created_at
// predicate, and the `populated` CTE ran a DISTINCT over the holder's whole
// history -- "which cancels out checkpoint+delta". Nothing measured it. These
// tests measure it, and then hold the fix in place by the only property that
// matters: the cost of reading a balance must not grow with how long the
// holder has been a customer.
//
// They read the query out of sql/queries/checkpoints.sql (the file sqlc
// itself consumes) rather than restating it, and assert that what they
// EXPLAIN is the text the generated code runs -- a cost gate measuring a
// private copy of a query is a gate measuring nothing.

// generatedQuery returns the SQL sqlc generated for a named query along with
// the Params fields it binds, in positional order, both read out of the
// generated file -- i.e. the statement the store actually executes.
//
// Reading the generated artifact rather than sql/queries/*.sql is deliberate:
// sqlc's parameter numbering is its own (in this repo's history it has
// assigned $1 to the second sqlc.arg to appear), so a test that renumbered
// the source query itself could silently EXPLAIN the arguments the wrong way
// round and measure a plan nobody runs. `make sqlc-diff` already gates the
// generated file against the source; this consumes the gated artifact.
func generatedQuery(t *testing.T, file, constName string) (sql string, params []string) {
	t.Helper()

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	require.NoError(t, err, "a cost gate that cannot read its own subject must not pass")

	ast.Inspect(parsed, func(n ast.Node) bool {
		if vs, ok := n.(*ast.ValueSpec); ok && len(vs.Names) == 1 && vs.Names[0].Name == constName && len(vs.Values) == 1 {
			if lit, ok := vs.Values[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				unquoted, uerr := strconv.Unquote(lit.Value)
				require.NoError(t, uerr)
				sql = strings.TrimSpace(unquoted)
			}
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Query" && sel.Sel.Name != "QueryRow" && sel.Sel.Name != "Exec") {
			return true
		}
		if id, ok := call.Args[1].(*ast.Ident); !ok || id.Name != constName {
			return true
		}
		params = nil
		for _, arg := range call.Args[2:] {
			argSel, ok := arg.(*ast.SelectorExpr)
			require.True(t, ok, "%s: argument %s is not arg.Field; teach this gate how to read it", constName, arg)
			params = append(params, argSel.Sel.Name)
		}
		return true
	})

	require.NotEmpty(t, sql, "const %s not found in %s", constName, file)
	return sql, params
}

// entryScanCost is what an EXPLAIN ANALYZE says the statement actually did to
// journal_entries: how many of its partitions it visited, and how many rows it
// read out of them.
//
// "Read" means rows the scan touched, which is `Actual Rows` PLUS
// `Rows Removed by Filter` -- a scan that reads 2,000 rows and keeps 20 did
// 2,000 rows of work, and Actual Rows alone reports the 20. Counting only the
// survivors made an unindexed plan look identical to an indexed one in an
// earlier draft of this gate, which is precisely the reading a cost gate must
// not produce. Multiplied by Actual Loops so a nested loop is counted once
// per iteration.
type entryScanCost struct {
	partitions int
	rowsRead   float64
	execMS     float64
}

func explainEntryScanCost(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) entryScanCost {
	t.Helper()

	var doc string
	require.NoError(t, pool.QueryRow(context.Background(),
		"EXPLAIN (ANALYZE, FORMAT JSON) "+sql, args...).Scan(&doc))

	var parsed []struct {
		Plan        map[string]any `json:"Plan"`
		ExecutionMS float64        `json:"Execution Time"`
	}
	require.NoError(t, json.Unmarshal([]byte(doc), &parsed))
	require.Len(t, parsed, 1)

	touched := map[string]bool{}
	cost := entryScanCost{execMS: parsed[0].ExecutionMS}
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if rel, ok := node["Relation Name"].(string); ok && strings.HasPrefix(rel, "journal_entries") {
			touched[rel] = true
			rows, _ := node["Actual Rows"].(float64)
			filtered, _ := node["Rows Removed by Filter"].(float64)
			loops, _ := node["Actual Loops"].(float64)
			if loops == 0 {
				loops = 1
			}
			cost.rowsRead += (rows + filtered) * loops
		}
		for _, key := range []string{"Plans", "Subplans"} {
			if kids, ok := node[key].([]any); ok {
				for _, k := range kids {
					if m, ok := k.(map[string]any); ok {
						walk(m)
					}
				}
			}
		}
	}
	walk(parsed[0].Plan)
	cost.partitions = len(touched)
	require.Positive(t, cost.partitions,
		"the plan touched no journal_entries partition at all; the cost extractor is not reading this plan")
	return cost
}

// historyFixture is a holder with a long, partitioned history in a small
// number of classifications, checkpointed to within `tail` entries of the
// present -- i.e. the steady state, where checkpoint+delta is supposed to make
// the history irrelevant.
type historyFixture struct {
	holder      int64
	currencyUID string
	currencyID  int64
	classUIDs   []string
	entryCount  int
	partitions  int
}

func seedHistory(t *testing.T, pool *pgxpool.Pool, holder int64, months, perMonth, dims, tail int) historyFixture {
	t.Helper()
	ctx := context.Background()

	// Push the partition horizon back so the history has somewhere to live:
	// 001 only bootstraps the current month plus four.
	partitions := postgres.NewPartitionStore(pool)
	base := time.Now().UTC().AddDate(0, -months, 0)
	for i := 0; i <= months+1; i++ {
		_, err := partitions.EnsureMonthlyPartitions(ctx, base.AddDate(0, i, 0), 1)
		require.NoError(t, err)
	}

	tag := fmt.Sprintf("%d", holder)
	currencyUID := postgrestest.SeedCurrency(t, pool, "HIST-"+tag, "History "+tag)
	currencyID := postgrestest.InternalID(t, pool, "currencies", currencyUID)
	jtUID := postgrestest.SeedJournalType(t, pool, "jt_hist_"+tag, "History JT "+tag)
	jtID := postgrestest.InternalID(t, pool, "journal_types", jtUID)

	classUIDs := make([]string, 0, dims)
	classIDs := make([]int64, 0, dims)
	for d := 0; d < dims; d++ {
		uid := postgrestest.SeedClassificationWithRole(t, pool,
			fmt.Sprintf("hist_%s_%d", tag, d), "History class", "debit", false, "available")
		classUIDs = append(classUIDs, uid)
		classIDs = append(classIDs, postgrestest.InternalID(t, pool, "classifications", uid))
	}
	sysUID := postgrestest.SeedClassificationWithRole(t, pool,
		"hist_sys_"+tag, "History system", "credit", true, "")
	sysID := postgrestest.InternalID(t, pool, "classifications", sysUID)

	// Bulk SQL, spread across the months so every partition is populated.
	// Direct inserts are legitimate here because this is a COST fixture: the
	// measured statement is still the store's own, and
	// TestBalanceRead_AgreesWithEntriesOnlyRecompute checks the arithmetic
	// against the rows independently of any of this.
	_, err := pool.Exec(ctx, `
		WITH spread AS (
		    SELECT ($1::timestamptz + (m || ' months')::interval + (i || ' hours')::interval) AS created_at
		    FROM generate_series(0, $2::int - 1) AS m, generate_series(1, $3::int) AS i
		),
		j AS (
		    INSERT INTO journals (journal_type_id, uid, idempotency_key, total_debit, total_credit, effective_at, created_at)
		    SELECT $4::bigint, gen_random_uuid(),
		           'hist-' || $5::bigint::text || '-' || row_number() OVER (),
		           1, 1, created_at, created_at
		    FROM spread
		    RETURNING id, created_at
		)
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at)
		SELECT j.id, $5::bigint, $6::bigint, ($7::bigint[])[1 + (j.id % $8::int)], 'debit', 1, j.created_at, j.created_at FROM j
		UNION ALL
		SELECT j.id, -($5::bigint), $6::bigint, $9::bigint, 'credit', 1, j.created_at, j.created_at FROM j`,
		base, months, perMonth, jtID, holder, currencyID, classIDs, dims, sysID)
	require.NoError(t, err)

	// Checkpoint every dimension to within `tail` entries of the head, so the
	// delta is small and constant while the history is not.
	_, err = pool.Exec(ctx, `
		INSERT INTO balance_checkpoints (account_holder, currency_id, classification_id, balance, last_entry_id, last_entry_at)
		SELECT je.account_holder, je.currency_id, je.classification_id,
		       SUM(ledger_signed_amount(c.normal_side, je.entry_type, je.amount)),
		       MAX(je.id), MAX(je.created_at)
		FROM journal_entries je
		JOIN classifications c ON c.id = je.classification_id
		WHERE je.account_holder = $1 AND je.currency_id = $2
		  AND je.id <= (SELECT MAX(id) - $3::bigint FROM journal_entries WHERE account_holder = $1 AND currency_id = $2)
		GROUP BY je.account_holder, je.currency_id, je.classification_id`,
		holder, currencyID, tail)
	require.NoError(t, err)

	require.NoError(t, analyze(pool, "journal_entries", "balance_checkpoints", "classifications"))

	f := historyFixture{
		holder: holder, currencyUID: currencyUID, currencyID: currencyID,
		classUIDs: classUIDs,
	}
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM journal_entries WHERE account_holder = $1 AND currency_id = $2`,
		holder, currencyID).Scan(&f.entryCount))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_inherits WHERE inhparent = 'journal_entries'::regclass`).Scan(&f.partitions))
	require.GreaterOrEqual(t, f.partitions, months,
		"the fixture must have a realistic partition count for the measurement to mean anything")
	return f
}

func analyze(pool *pgxpool.Pool, tables ...string) error {
	for _, table := range tables {
		if _, err := pool.Exec(context.Background(), "ANALYZE "+table); err != nil {
			return err
		}
	}
	return nil
}

// TestBalanceRead_CostDoesNotGrowWithHistory is H-M6's pin.
//
// Two holders, identical in every way except that one has ten times the
// history, both checkpointed to within five entries of the head. A balance
// read is `checkpoint + the entries after it`, so the two calls have the same
// work to do and reading ten times as much to do it is the defect.
//
// Verified red against the pre-fix query, whose DISTINCT-over-all-history CTE
// plus hash-joined delta read 2,880 and 28,800 entry rows for these two
// fixtures (a 10.0x ratio, i.e. purely a function of history) against a
// five-entry delta.
func TestBalanceRead_CostDoesNotGrowWithHistory(t *testing.T) {
	pool := postgrestest.SetupDB(t)

	sql, params := generatedQuery(t, "sqlcgen/checkpoints.sql.go", "listComputedBalancesForHolders")
	require.ElementsMatch(t, []string{"CurrencyID", "HolderIds"}, params,
		"the query's parameters changed; teach this gate how to bind them")

	small := seedHistory(t, pool, 7101, 24, 40, 3, 5)
	large := seedHistory(t, pool, 7102, 24, 400, 3, 5)
	require.Equal(t, 10*small.entryCount, large.entryCount, "the two fixtures must differ only in history length")

	costOf := func(f historyFixture) entryScanCost {
		// Bound by name, not by position: the two are easy to reorder in the
		// SQL and a silently swapped pair would measure the wrong thing.
		args := make([]any, len(params))
		for i, name := range params {
			switch name {
			case "CurrencyID":
				args[i] = f.currencyID
			case "HolderIds":
				args[i] = []int64{f.holder}
			}
		}
		// Best of three: this asserts on rows read, but the timing in the log
		// is only useful warm.
		best := entryScanCost{rowsRead: 1e18}
		for i := 0; i < 3; i++ {
			c := explainEntryScanCost(t, pool, sql, args...)
			if c.rowsRead <= best.rowsRead {
				best = c
			}
		}
		t.Logf("holder %d: history=%d partitions=%d -> entry rows read=%.0f, exec=%.2fms",
			f.holder, f.entryCount, f.partitions, best.rowsRead, best.execMS)
		return best
	}

	smallCost := costOf(small)
	largeCost := costOf(large)

	// The delta is 5 entries; the dimension walk costs one index tuple per
	// (dimension, partition). Both are independent of history. A generous
	// ceiling still fails by three orders of magnitude on the old query.
	ceiling := float64(4 * (small.partitions + 1) * (len(small.classUIDs) + 1))
	assert.LessOrEqual(t, largeCost.rowsRead, ceiling,
		"a balance read for a holder with %d entries read %.0f of them. The read is checkpoint + the %d entries "+
			"after it, so its cost must be a function of the DELTA and the partition count, not of the history "+
			"(H-M6; sql/queries/checkpoints.sql explains the shape that makes this hold).",
		large.entryCount, largeCost.rowsRead, 5)

	assert.LessOrEqual(t, largeCost.rowsRead, 2*smallCost.rowsRead,
		"ten times the history cost %.0f rows against %.0f: the balance read still scales with how long the holder "+
			"has been a customer, which is what storing a checkpoint exists to prevent.",
		largeCost.rowsRead, smallCost.rowsRead)

	// The other half of H-M6, unfixed on purpose and therefore asserted as it
	// is rather than left to be rediscovered: with no predicate on the
	// partition key, every partition is still visited. If this ever stops
	// being true, pruning has started working -- good news that must be
	// reflected in docs/INVARIANTS.md I-5 and docs/CAPACITY.md, and this
	// assertion should become the real (lower) bound.
	assert.Equal(t, large.partitions, largeCost.partitions,
		"the balance read visited %d of %d partitions. I-5 documents that it visits all of them, because the "+
			"predicates are (account_holder, currency_id, classification_id, id) and the partition key is "+
			"created_at. A change here is a documentation change too.",
		largeCost.partitions, large.partitions)
}

// TestBalanceRead_AgreesWithEntriesOnlyRecompute is the correctness half, and
// deliberately does not compare against the previous query: it recomputes
// every balance from the append-only entries alone, ignoring checkpoints
// entirely, and requires the store to agree. That is the definition in
// docs/INVARIANTS.md I-5 (`checkpoint.balance + SUM(entries after it)` must
// equal `SUM(all entries)`), so it holds any rewrite of the read path to the
// ledger's own arithmetic rather than to its predecessor's output.
func TestBalanceRead_AgreesWithEntriesOnlyRecompute(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	store := postgres.NewLedgerStore(pool)

	f := seedHistory(t, pool, 7201, 6, 20, 4, 7)

	// Deliberately uneven watermarks: one dimension fully checkpointed, one
	// with no checkpoint row at all (the case a checkpoint-only dimension
	// walk would silently drop), the rest partial.
	_, err := pool.Exec(ctx, `
		DELETE FROM balance_checkpoints
		WHERE account_holder = $1 AND currency_id = $2
		  AND classification_id = (SELECT id FROM classifications WHERE uid = $3::uuid)`,
		f.holder, f.currencyID, f.classUIDs[0])
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		UPDATE balance_checkpoints SET balance = 0, last_entry_id = 0, last_entry_at = 'epoch'
		WHERE account_holder = $1 AND currency_id = $2
		  AND classification_id = (SELECT id FROM classifications WHERE uid = $3::uuid)`,
		f.holder, f.currencyID, f.classUIDs[1])
	require.NoError(t, err)

	balances, err := store.GetBalances(ctx, f.holder, f.currencyUID)
	require.NoError(t, err)

	got := map[string]decimal.Decimal{}
	for _, b := range balances {
		got[b.ClassificationUID] = b.Balance
	}

	rows, err := pool.Query(ctx, `
		SELECT c.uid::text,
		       SUM(ledger_signed_amount(c.normal_side, je.entry_type, je.amount))::text
		FROM journal_entries je
		JOIN classifications c ON c.id = je.classification_id
		WHERE je.account_holder = $1 AND je.currency_id = $2
		GROUP BY c.uid`, f.holder, f.currencyID)
	require.NoError(t, err)
	defer rows.Close()

	want := map[string]decimal.Decimal{}
	for rows.Next() {
		var uid, sum string
		require.NoError(t, rows.Scan(&uid, &sum))
		want[uid] = decimal.RequireFromString(sum)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, want, "the recompute found no entries; the fixture is not exercising anything")

	require.Len(t, got, len(want),
		"the read returned %d dimensions, the entries contain %d: a dimension the walk cannot see is a balance "+
			"silently reported as absent", len(got), len(want))
	for uid, expected := range want {
		actual, ok := got[uid]
		require.True(t, ok, "classification %s has entries but no balance row", uid)
		assert.True(t, expected.Equal(actual),
			"classification %s: read says %s, the entries say %s", uid, actual, expected)
	}

	// And the single-dimension read must agree with the batch one.
	for uid, expected := range want {
		one, err := store.GetBalance(ctx, f.holder, f.currencyUID, uid)
		require.NoError(t, err)
		assert.True(t, expected.Equal(one), "GetBalance(%s) = %s, entries say %s", uid, one, expected)
	}
	_ = core.Balance{}
}

// TestListHolderTransactions_PageCostDoesNotGrowWithTheTable is H-m9's pin for
// the index half of migration 023.
//
// The cost that matters here is not the holder's own history -- it is how much
// of OTHER holders' history the statement has to wade through to find this
// holder's newest twenty journals. page_journals is
// `SELECT DISTINCT j.id FROM journal_entries je JOIN journals j ... WHERE
// je.account_holder = $1 AND j.id < cursor ORDER BY j.id DESC LIMIT n`, and
// without an index carrying (account_holder, journal_id) together, nothing can
// serve the equality, the duplicate elimination and the j.id DESC ordering at
// once. The planner's remaining option is to walk journals newest-first and
// test each one -- which is cheap when the holder owns most of the table and
// linear in the table when they own a slice of it. A production ledger is the
// second case, so the fixture is too: the measured holder's own history is
// held constant and the rest of the table grows around it.
//
// Verified red by dropping idx_entries_account_journal: page one then reads
// roughly one row per journal in the table, and the ratio between the two
// fixtures tracks the table's growth instead of staying flat.
func TestListHolderTransactions_PageCostDoesNotGrowWithTheTable(t *testing.T) {
	pool := postgrestest.SetupDB(t)

	sql, params := generatedQuery(t, "sqlcgen/holder.sql.go", "listHolderTransactionRows")
	require.ElementsMatch(t, []string{"AccountHolder", "CursorID", "PageLimit"}, params,
		"the query's parameters changed; teach this gate how to bind them")

	costOf := func(f historyFixture) entryScanCost {
		args := make([]any, len(params))
		for i, name := range params {
			switch name {
			case "AccountHolder":
				args[i] = f.holder
			case "CursorID":
				args[i] = int64(0) // first page
			case "PageLimit":
				args[i] = int64(20)
			default:
				t.Fatalf("ListHolderTransactionRows gained parameter %q; teach this gate how to bind it", name)
			}
		}
		best := entryScanCost{rowsRead: 1e18}
		for i := 0; i < 3; i++ {
			c := explainEntryScanCost(t, pool, sql, args...)
			if c.rowsRead <= best.rowsRead {
				best = c
			}
		}
		return best
	}

	// The holder under measurement: a modest, fixed history.
	subject := seedHistory(t, pool, 7301, 12, 20, 3, 5)
	beforeNeighbours := costOf(subject)
	t.Logf("holder %d alone: entry rows read for page one=%.0f", subject.holder, beforeNeighbours.rowsRead)

	// Now the ledger acquires other customers, ten times over.
	var tableRows int
	for i, neighbour := range []int64{7311, 7312, 7313, 7314, 7315} {
		seedHistory(t, pool, neighbour, 12, 200, 3, 5)
		_ = i
	}
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM journals`).Scan(&tableRows))

	afterNeighbours := costOf(subject)
	t.Logf("holder %d among %d journals: entry rows read for page one=%.0f", subject.holder, tableRows, afterNeighbours.rowsRead)

	assert.LessOrEqual(t, afterNeighbours.rowsRead, 3*beforeNeighbours.rowsRead,
		"page one for the same holder, with the same %d entries, went from %.0f rows to %.0f once the ledger had "+
			"%d journals in total. The first page's cost is tracking the size of the TABLE rather than the size of "+
			"the page (H-m9, migration 023's idx_entries_account_journal).",
		subject.entryCount, beforeNeighbours.rowsRead, afterNeighbours.rowsRead, tableRows)
	assert.Less(t, afterNeighbours.rowsRead, float64(tableRows)/2,
		"page one read %.0f rows out of a %d-journal table to return 20 journals",
		afterNeighbours.rowsRead, tableRows)
}
