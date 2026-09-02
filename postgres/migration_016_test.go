package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/presets"
)

// TestMigration016_CorrectsAnAlreadyInstalledDeployment is the pin for the
// delivery half of the 2026-09-02 sign corrections (audit A-C1 / A-M2 / A-M4).
//
// Fixing presets/*.go only fixes deployments that have not installed yet.
// InstallTemplatePresets validates existing templates and errors rather than
// updating them, entry_template_lines carries a BEFORE UPDATE guard that
// permits no change at all, and ledger_app has had UPDATE revoked on it
// (migration 003). A source-only fix would therefore have left every existing
// database posting the wrong direction forever while the tests went green --
// the exact "the mechanism exists but is not wired to the real path" shape
// both audits kept finding.
//
// So this test does not check the migration against a fresh database (where
// the Go presets would have produced the right rows anyway). It reproduces a
// PRE-016 deployment -- old template lines, credit-normal equity, guards
// armed -- and then applies the migration to it.
func TestMigration016_CorrectsAnAlreadyInstalledDeployment(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	require.NoError(t, presets.InstallExtendedPresets(ctx, classStore, classStore, tmplStore))

	// --- Rewind to the pre-016 state by running the down migration. ---
	require.NoError(t, execMigrationFile(ctx, pool, "sql/migrations/016_preset_sign_correction.down.sql"))

	assert.Equal(t, "credit", classificationNormalSide(ctx, t, pool, "equity"),
		"the rewind must actually restore the old polarity, otherwise this test proves nothing")
	assert.Equal(t,
		[]string{"custodial/debit/gross_amount", "main_wallet/credit/gross_amount"},
		templateLineShape(ctx, t, pool, "checkout_settlement_gross"))
	assert.Equal(t,
		[]string{"main_wallet/credit/amount", "fees/debit/amount"},
		templateLineShape(ctx, t, pool, "fee_charge"))

	// The guards must be armed again after the down migration -- if they were
	// left disabled, the up migration below would pass for the wrong reason.
	_, err := pool.Exec(ctx, `UPDATE entry_template_lines SET amount_key = amount_key || 'x'`)
	require.Error(t, err, "entry_template_lines guard must be re-armed by the down migration")

	// --- Apply 016. ---
	require.NoError(t, execMigrationFile(ctx, pool, "sql/migrations/016_preset_sign_correction.up.sql"))

	assert.Equal(t, "debit", classificationNormalSide(ctx, t, pool, "equity"),
		"equity must be debit-normal: it increases together with credit-normal custodial on a capital injection")

	assert.Equal(t,
		[]string{"custodial/credit/amount", "equity/debit/amount"},
		templateLineShape(ctx, t, pool, "capital_injection"))
	assert.Equal(t,
		[]string{"custodial/debit/amount", "equity/credit/amount"},
		templateLineShape(ctx, t, pool, "capital_withdraw"))
	assert.Equal(t,
		[]string{"main_wallet/debit/gross_amount", "custodial/credit/gross_amount"},
		templateLineShape(ctx, t, pool, "checkout_settlement_gross"))
	assert.Equal(t,
		[]string{
			"main_wallet/debit/net_amount",
			"fee_expense/debit/fee_amount",
			"custodial/credit/net_amount",
			"fees/credit/fee_amount",
		},
		templateLineShape(ctx, t, pool, "checkout_settlement_net"))
	assert.Equal(t,
		[]string{
			"fee_expense/debit/amount",
			"custodial/debit/amount",
			"main_wallet/credit/amount",
			"fees/credit/amount",
		},
		templateLineShape(ctx, t, pool, "fee_charge"))

	// The guards are back on afterwards.
	_, err = pool.Exec(ctx, `UPDATE entry_template_lines SET amount_key = amount_key || 'x'`)
	require.Error(t, err, "entry_template_lines guard must be re-armed by the up migration")

	// The one sanctioned edit in this table's history is visible to whoever
	// reads the config audit trail, per template.
	var audited int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM config_table_changes
		WHERE table_name = 'entry_template_lines'
		  AND new_row->>'migration' = '016_preset_sign_correction'`).Scan(&audited))
	assert.EqualValues(t, 5, audited, "one audit row per corrected template")

	var equityAudited int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM config_table_changes
		WHERE table_name = 'classifications'
		  AND old_row->>'code' = 'equity'
		  AND old_row->>'normal_side' = 'credit'
		  AND new_row->>'normal_side' = 'debit'`).Scan(&equityAudited))
	assert.EqualValues(t, 1, equityAudited, "the classifications_audit trigger records the polarity flip")

	// And the migrated rows are byte-identical to what the Go presets would
	// have created, so a redeploy's InstallExtendedPresets validates instead
	// of erroring -- which is the whole reason the migration has to produce
	// exactly this shape and not merely a correct one.
	require.NoError(t, presets.InstallExtendedPresets(ctx, classStore, classStore, tmplStore))
}

func execMigrationFile(ctx context.Context, pool *pgxpool.Pool, path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, string(body))
	return err
}

func classificationNormalSide(ctx context.Context, t *testing.T, pool *pgxpool.Pool, code string) string {
	t.Helper()
	var side string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT normal_side FROM classifications WHERE code = $1`, code).Scan(&side))
	return side
}

// templateLineShape renders a template's lines as
// "<classification code>/<entry_type>/<amount_key>" in sort order -- the three
// facts that decide which account a leg hits, in which direction, and for how
// much.
func templateLineShape(ctx context.Context, t *testing.T, pool *pgxpool.Pool, templateCode string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT c.code || '/' || l.entry_type || '/' || l.amount_key
		FROM entry_template_lines l
		JOIN entry_templates t  ON t.id = l.template_id
		JOIN classifications c  ON c.id = l.classification_id
		WHERE t.code = $1
		ORDER BY l.sort_order`, templateCode)
	require.NoError(t, err)
	defer rows.Close()

	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}
