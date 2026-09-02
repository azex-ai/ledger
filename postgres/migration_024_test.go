package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// ledgerAppPool opens a pool authenticated as ledger_app -- the credential
// this repository's threat model assumes is leaked. Every assertion in this
// file is about what that credential can do, so running them as the container
// superuser (which every other test does) would prove nothing.
func ledgerAppPool(t *testing.T, ctx context.Context, pool *pgxpool.Pool, password string) *pgxpool.Pool {
	t.Helper()
	_, err := pool.Exec(ctx, fmt.Sprintf("ALTER ROLE ledger_app WITH PASSWORD '%s'", password))
	require.NoError(t, err)
	appPool, err := pgxpool.New(ctx, withRole(t, roleURLFromPool(pool), "ledger_app", password))
	require.NoError(t, err)
	t.Cleanup(appPool.Close)
	require.NoError(t, appPool.Ping(ctx))
	return appPool
}

// TestBalanceRolePromotion_RefusedForLedgerApp re-runs w3-review/money-path.md
// m-3's scenario under the credential the threat model is written about, and
// finds it already refused.
//
// m-3 cites 003_config_table_guards.up.sql:111-135, where the guard permits
// ” -> <role> unconditionally. 004_refuse_balance_role_promotion_with_history
// REPLACED that function four migrations later, and its replacement refuses
// ” -> 'available' whenever the classification has any journal_entries row --
// which is exactly the shape m-3 describes ("a role-less classification with a
// real, correctly signed positive balance"). The permission m-3 measured
// (`UPDATE classifications balance_role ”->available -> <nil>`) is the
// EMPTY-classification case, where there is no balance to promote.
//
// TestBalanceRolePromotion_RefusedOnceEntriesExist (roles_test.go) already
// pins the rule, but through the container superuser. This adds the missing
// dimension -- the leaked application credential -- so the next reviewer
// reading 003 does not have to re-derive it, and so a future migration that
// re-replaces the guard without the history clause goes red here.
//
// No migration accompanies this test: verified, not reproducible, not fixed
// (contract §3 rule 5).
func TestBalanceRolePromotion_RefusedForLedgerApp(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	app := ledgerAppPool(t, ctx, pool, "mig024-promo-password-not-a-real-secret") //nolint:gosec // test-only credential

	curUID := postgrestest.SeedCurrencyWithExponent(t, pool, "M024PROMO", "Promo Unit", 2)
	jtUID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	rolelessUID := postgrestest.SeedClassification(t, pool, "m024_accrued", "Accrued", "debit", false)
	counterpartUID := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	// Real, signed money in a role-less user-side classification: m-3's exact
	// precondition.
	attestor, _ := newTestAttestor(t, "m024-promo-key")
	_, err := postgres.NewLedgerStore(pool).WithAuth(attestor).PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtUID,
		IdempotencyKey: postgrestest.UniqueKey("m024-promo"),
		Entries: []core.EntryInput{
			{AccountHolder: 8901, CurrencyUID: curUID, ClassificationUID: rolelessUID, EntryType: core.EntryTypeDebit, Amount: decimal.RequireFromString("1200")},
			{AccountHolder: -8901, CurrencyUID: curUID, ClassificationUID: counterpartUID, EntryType: core.EntryTypeCredit, Amount: decimal.RequireFromString("1200")},
		},
	})
	require.NoError(t, err)

	_, err = app.Exec(ctx, `UPDATE classifications SET balance_role = 'available' WHERE uid = $1::uuid`, rolelessUID)
	require.Error(t, err, "1200 of signed balance must not become spendable through one config UPDATE by the app credential")
	assert.Contains(t, err.Error(), "already has journal entries")

	// Still nothing in the gate's base.
	var role string
	require.NoError(t, pool.QueryRow(ctx, `SELECT balance_role FROM classifications WHERE uid = $1::uuid`, rolelessUID).Scan(&role))
	assert.Equal(t, "", role)
}

// TestMigration024_AnchorObservationsAreOwnerWritten is m-4's pin. A single
// row -- INSERT INTO anchor_observations (observed_seq) VALUES (999999) --
// used to be enough for ledger_app to nail VerifyLedger to TAMPERED forever:
// the table is append-only by trigger, MAX(observed_seq) is the only read, and
// the comparison `anchorSeq < lastObserved` is then true on every future run,
// with no runbook path back.
//
// Direct INSERT is now revoked; the legitimate writer goes through a
// SECURITY DEFINER function that refuses to record an observation ahead of the
// DB's own attestation chain. The forgery is therefore bounded by the real
// chain height, which the anchor catches up to on its own -- a false red that
// heals instead of one that has to be surgically removed.
func TestMigration024_AnchorObservationsAreOwnerWritten(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	app := ledgerAppPool(t, ctx, pool, "mig024-anchor-password-not-a-real-secret") //nolint:gosec // test-only credential

	_, err := app.Exec(ctx, `
		INSERT INTO anchor_observations (uid, observed_seq, observed_head)
		VALUES (gen_random_uuid(), 999999, ''::bytea)
	`)
	assertPermissionDenied(t, err)

	// The function is the only door, and it does not open onto a seq the DB's
	// own chain has never reached.
	_, err = app.Exec(ctx, `SELECT ledger_record_anchor_observation($1::uuid, 999999, ''::bytea)`, uuid.NewString())
	require.Error(t, err, "an observation ahead of the local attestation chain is not a memory, it is a claim")
	assert.Contains(t, err.Error(), "attestation chain")

	// A real observation (seq 0 on an empty chain: "the anchor was reachable
	// and reported empty") still records.
	_, err = app.Exec(ctx, `SELECT ledger_record_anchor_observation($1::uuid, 0, ''::bytea)`, uuid.NewString())
	require.NoError(t, err)

	var highest int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COALESCE(MAX(observed_seq), 0) FROM anchor_observations`).Scan(&highest))
	assert.EqualValues(t, 0, highest, "the forged 999999 must not be on record")

	// And the shipped store still records through the same door, connected as
	// ledger_app -- the half a grant change can silently break (the
	// I-55 writer is AttestationService.catchUpAnchor calling this method).
	store := postgres.NewAttestationStore(app)
	require.NoError(t, store.RecordAnchorObservation(ctx, 0, []byte{}),
		"postgres.AttestationStore.RecordAnchorObservation must keep working for the role the application connects as")
	seen, err := store.HighestObservedAnchorSeq(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 0, seen)
}
