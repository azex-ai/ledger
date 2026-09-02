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
// (working-agreements §5): every `numeric`-typed column in the `public`
// schema must be exactly (30, 18), full stop. Manually auditing
// postgres/sql/migrations/*.up.sql (2026-09-02) found 26 NUMERIC declarations,
// all (30, 18), and zero float-typed columns -- this file turns that manual
// finding into something that stays true.

import (
	"context"
	"fmt"
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
		SELECT table_name, column_name, numeric_precision, numeric_scale
		FROM information_schema.columns
		WHERE table_schema = 'public' AND data_type = 'numeric'
		ORDER BY table_name, column_name
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

	rows, err := pool.Query(ctx, `
		SELECT table_name, column_name, data_type
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND data_type IN ('double precision', 'real', 'float')
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
