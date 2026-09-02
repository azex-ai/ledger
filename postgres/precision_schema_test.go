package postgres_test

// F-M4 / F-P6 (2026-09-02 audit): docs/INVARIANTS.md I-6 claims "Postgres
// uses NUMERIC(30,18)" for every amount column, but until this file existed
// nothing read information_schema to check it -- both of I-6's prior pins
// stayed inside Go (decimal round-trip, HTTP string decode) and never
// touched the database. A migration that shipped `amount NUMERIC(20,8)`, or
// worse `DOUBLE PRECISION` (financial.md's first red line), would not have
// turned anything red.
//
// The check is mechanically derived, not a hand-maintained table allowlist
// (working-agreements §5): every `numeric`-typed column in a non-system
// schema must be exactly (30, 18), full stop. Manually auditing
// postgres/sql/migrations/*.up.sql (2026-09-02) found 26 NUMERIC declarations,
// all (30, 18), and zero float-typed columns -- this file turns that manual
// finding into something that stays true.
//
// M-5 (W3 adversarial review of the gates): that formulation only holds the
// columns that ARE numeric. The reviewer added an amount column as BIGINT
// (wei style), one as TEXT (string amounts), and one as NUMERIC(20,8)[] --
// data_type 'ARRAY', so the precision lives in information_schema's
// element_types, not in columns -- and all three passed both checks here.
// financial.md's red line is "money is not a float"; the narrower rule this
// file is named for is "money is NUMERIC(30,18)", and nothing checked the
// direction where a money column simply is not numeric at all.
//
// So there is a third check, derived from the COLUMN NAME: anything whose
// name contains an amount-shaped word must be numeric(30,18), or be
// classified as not-money with a reason. And every query here covers all
// non-system schemas rather than `public` alone -- see
// object_ownership_test.go's note on the same scoping bug (M-7).

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
)

// TestSchema_NumericColumnsAreExactly30_18 pins I-6's schema half: every
// `numeric`-typed column in the public schema must declare precision 30,
// scale 18. A new migration that narrows an amount column (or any other
// numeric column -- this repo has none that aren't amounts, see the file
// doc comment) to a different precision/scale must fail here, not silently
// truncate money at the 1e-18 place.
func TestSchema_NumericColumnsAreExactly30_18(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT table_schema || '.' || table_name, column_name, numeric_precision, numeric_scale
		FROM information_schema.columns
		WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
		  AND data_type = 'numeric'
		ORDER BY table_schema, table_name, column_name
	`)
	require.NoError(t, err)
	defer rows.Close()

	type col struct {
		table, name      string
		precision, scale int
	}
	var got []col
	for rows.Next() {
		var c col
		require.NoError(t, rows.Scan(&c.table, &c.name, &c.precision, &c.scale))
		got = append(got, c)
	}
	require.NoError(t, rows.Err())

	require.NotEmpty(t, got, "expected at least one numeric column (fail-closed: if this schema ever has zero, that's the check failing to run, not a pass)")

	for _, c := range got {
		assert.Equalf(t, 30, c.precision, "%s.%s: numeric precision must be 30 (docs/INVARIANTS.md I-6)", c.table, c.name)
		assert.Equalf(t, 18, c.scale, "%s.%s: numeric scale must be 18 (docs/INVARIANTS.md I-6)", c.table, c.name)
	}
}

// TestSchema_NoFloatTypedColumns pins financial.md's red line at the schema
// layer: no column anywhere in the public schema may be `double precision`,
// `real`, or (Postgres has no bare `float` type, but guard the name anyway)
// `float`. shopspring/decimal round-trips through NUMERIC only.
func TestSchema_NoFloatTypedColumns(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	// Arrays are joined to element_types because a `double precision[]`
	// column reports data_type 'ARRAY' in information_schema.columns -- the
	// float hides one level down (M-5).
	rows, err := pool.Query(ctx, `
		SELECT c.table_schema || '.' || c.table_name, c.column_name,
		       CASE WHEN c.data_type = 'ARRAY' THEN e.data_type || '[]' ELSE c.data_type END
		FROM information_schema.columns c
		LEFT JOIN information_schema.element_types e
		       ON e.object_schema = c.table_schema
		      AND e.object_name = c.table_name
		      AND e.object_type = 'TABLE'
		      AND e.collection_type_identifier = c.dtd_identifier
		WHERE c.table_schema NOT IN ('pg_catalog', 'information_schema')
		  AND (c.data_type IN ('double precision', 'real', 'float')
		       OR e.data_type IN ('double precision', 'real', 'float'))
	`)
	require.NoError(t, err)
	defer rows.Close()

	var offenders []string
	for rows.Next() {
		var table, column, dataType string
		require.NoError(t, rows.Scan(&table, &column, &dataType))
		offenders = append(offenders, fmt.Sprintf("%s.%s (%s)", table, column, dataType))
	}
	require.NoError(t, rows.Err())

	assert.Empty(t, offenders, "financial.md: money columns must never be a float type -- found: %v", offenders)
}

// moneyNameToken matches a column-name word that means "this holds money".
// Split on underscores, so `min_balance`, `total_debit`, `actual_amount` and
// `settled_amount` all match while `balance_role` matches too and has to be
// classified below -- which is the point: the classification is the reviewable
// act, and a name nobody classified is red.
var moneyNameTokens = map[string]bool{
	"amount": true, "amounts": true,
	"balance": true, "balances": true,
	"total": true, "totals": true,
	"fee": true, "fees": true,
	"price": true, "prices": true,
	"cost": true, "costs": true,
	"bps": true, "rate": true, "rates": true,
	"qty": true, "quantity": true,
	"delta": true, "deltas": true,
}

// notMoneyColumns maps "<schema>.<table>.<column>" to why a column whose name
// contains an amount-shaped word is not itself money. Every entry is a
// deliberate exemption; a new name-matching column that is not
// numeric(30,18) fails until it is either fixed or listed here.
var notMoneyColumns = map[string]string{
	"public.account_policies.enforce_min_balance": "boolean flag: whether the min_balance beside it is enforced, not a figure",
	"public.classifications.balance_role":         "text tag (available/pending/locked/memo/'') naming which balance bucket a classification feeds",
	"public.entry_template_lines.amount_key":      "text: the NAME of the template parameter an entry line reads its amount from, not an amount",
}

func hasMoneyNameToken(column string) bool {
	for _, token := range strings.Split(column, "_") {
		if moneyNameTokens[token] {
			return true
		}
	}
	return false
}

// TestSchema_MoneyNamedColumnsAreNumeric30_18 closes M-5: the other two checks
// in this file constrain columns that are already numeric, or catch floats.
// A money column declared BIGINT, TEXT, or NUMERIC(20,8)[] satisfies both and
// is exactly as wrong -- 1e-18 truncation, string arithmetic, or an
// array whose element precision no check ever reads.
//
// Fail-closed by name: a column whose name says money must be
// numeric(30,18), or be classified in notMoneyColumns with a reason.
func TestSchema_MoneyNamedColumnsAreNumeric30_18(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT c.table_schema, c.table_name, c.column_name, c.data_type,
		       COALESCE(e.data_type, ''),
		       COALESCE(c.numeric_precision, e.numeric_precision, -1),
		       COALESCE(c.numeric_scale, e.numeric_scale, -1)
		FROM information_schema.columns c
		LEFT JOIN information_schema.element_types e
		       ON e.object_schema = c.table_schema
		      AND e.object_name = c.table_name
		      AND e.object_type = 'TABLE'
		      AND e.collection_type_identifier = c.dtd_identifier
		WHERE c.table_schema NOT IN ('pg_catalog', 'information_schema')
		ORDER BY c.table_schema, c.table_name, c.column_name
	`)
	require.NoError(t, err)
	defer rows.Close()

	checked, seen := 0, map[string]bool{}
	for rows.Next() {
		var schema, table, column, dataType, elemType string
		var precision, scale int
		require.NoError(t, rows.Scan(&schema, &table, &column, &dataType, &elemType, &precision, &scale))
		if !hasMoneyNameToken(column) {
			continue
		}
		key := schema + "." + table + "." + column
		if reason, ok := notMoneyColumns[key]; ok {
			seen[key] = true
			assert.NotEqualf(t, "numeric", dataType,
				"%s is classified as not-money (%s) but IS numeric -- delete the exemption rather than carry a wrong reason", key, reason)
			continue
		}
		checked++
		declared := dataType
		if dataType == "ARRAY" {
			declared = elemType + "[]"
		}
		if !assert.Equalf(t, "numeric", strings.TrimSuffix(declared, "[]"),
			"%s has a money-shaped name but is %s. financial.md: money is NUMERIC(30,18) -- BIGINT (wei), TEXT (string amounts) and "+
				"an array of narrower numerics are each a way to hold money that the precision and float checks in this file cannot see (M-5). "+
				"If this column is not money, add %q to notMoneyColumns with the reason", key, declared, key) {
			continue
		}
		assert.Equalf(t, 30, precision, "%s: numeric precision must be 30 (docs/INVARIANTS.md I-6)", key)
		assert.Equalf(t, 18, scale, "%s: numeric scale must be 18 (docs/INVARIANTS.md I-6)", key)
	}
	require.NoError(t, rows.Err())

	require.Positivef(t, checked, "no money-named column was found -- the scan regressed (fail-closed: zero columns checked is the check not running, not a pass)")
	for key, reason := range notMoneyColumns {
		assert.Truef(t, seen[key], "stale exemption %q (%s): no such column exists any more -- delete the entry", key, reason)
	}
}
