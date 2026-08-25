package postgres_test

// Pins for I-22 (docs/INVARIANTS.md): DB role least-privilege.
// Design: docs/plans/2026-08-21-tamper-evident-ledger-design.md §3.
// Contract: docs/plans/2026-08-21-integrity-hardening-contracts.md §1/§5.
//
// These tests connect as ledger_app / ledger_owner over real network
// sockets (not just inspecting pg_catalog) because the risk being pinned is
// "can a role holding these credentials actually do X", which a catalog
// query alone cannot prove (e.g. a role created NOLOGIN would look fine in
// an ACL query and still be unusable, or vice versa).

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// withRole returns connStr with its user/password replaced, keeping host,
// port, database and query params intact.
func withRole(t testing.TB, connStr, user, password string) string {
	t.Helper()
	u, err := url.Parse(connStr)
	require.NoError(t, err)
	u.User = url.UserPassword(user, password)
	return u.String()
}

// assertPermissionDenied requires err to be a Postgres insufficient_privilege
// error (SQLSTATE 42501) -- the error class Postgres raises uniformly for
// both ACL failures (permission denied for table X) and ownership failures
// (must be owner of relation X), which covers both the GRANT-based and the
// DDL/ownership-based restrictions the ledger's role setup relies on.
func assertPermissionDenied(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "expected a *pgconn.PgError, got %T: %v", err, err)
	assert.Equal(t, "42501", pgErr.Code, "expected insufficient_privilege (42501), got %s: %s", pgErr.Code, pgErr.Message)
}

// TestLedgerAppIsLeastPrivilege pins I-22: ledger_app can do its job
// (SELECT/INSERT/UPDATE ordinary tables, SELECT/INSERT on journal_entries)
// but cannot DROP/TRUNCATE/ALTER/DELETE anything, anywhere, and has no
// access at all to schema_migrations.
func TestLedgerAppIsLeastPrivilege(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	const appPassword = "roles-test-app-password-not-a-real-secret" //nolint:gosec // test-only credential
	_, err := pool.Exec(ctx, fmt.Sprintf("ALTER ROLE ledger_app WITH PASSWORD '%s'", appPassword))
	require.NoError(t, err)

	appPool, err := pgxpool.New(ctx, withRole(t, roleURLFromPool(pool), "ledger_app", appPassword))
	require.NoError(t, err)
	t.Cleanup(appPool.Close)
	require.NoError(t, appPool.Ping(ctx))

	t.Run("cannot TRUNCATE journal_entries", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "TRUNCATE journal_entries")
		assertPermissionDenied(t, err)
	})

	t.Run("cannot DROP TRIGGER on journal_entries", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "DROP TRIGGER journal_entries_no_update ON journal_entries")
		assertPermissionDenied(t, err)
	})

	t.Run("cannot ALTER TABLE journal_entries", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "ALTER TABLE journal_entries ADD COLUMN pwned INT")
		assertPermissionDenied(t, err)
	})

	t.Run("cannot CREATE TABLE (no DDL of any kind)", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "CREATE TABLE evil (id INT)")
		assertPermissionDenied(t, err)
	})

	t.Run("cannot UPDATE journal_entries", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "UPDATE journal_entries SET amount = amount WHERE id < 0")
		assertPermissionDenied(t, err)
	})

	t.Run("cannot DELETE from journal_entries", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "DELETE FROM journal_entries WHERE id < 0")
		assertPermissionDenied(t, err)
	})

	t.Run("cannot read or write schema_migrations", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "SELECT * FROM schema_migrations")
		assertPermissionDenied(t, err)
	})

	// --- ledger_app can still do its actual job --------------------------
	//
	// webhook_subscribers rather than currencies: migration 003 gave
	// currencies a column guard, so it is no longer an ordinary table and
	// updating its name is now refused on purpose. This subtest exists to
	// prove the refusals above are not vacuous -- a role granted nothing at
	// all would also fail every forbidden operation -- so it has to use a
	// table where a plain UPDATE really is allowed.
	t.Run("can SELECT/INSERT/UPDATE an ordinary table", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "INSERT INTO webhook_subscribers (url, name) VALUES ('https://example.test/hook', 'RoleTest1')")
		require.NoError(t, err)
		_, err = appPool.Exec(ctx, "UPDATE webhook_subscribers SET name = 'RoleTest1x' WHERE name = 'RoleTest1'")
		require.NoError(t, err)
		var name string
		require.NoError(t, appPool.QueryRow(ctx, "SELECT name FROM webhook_subscribers WHERE url = 'https://example.test/hook'").Scan(&name))
		assert.Equal(t, "RoleTest1x", name)
	})

	// The guard migration 003 put on the configuration tables, checked from
	// the credential it exists to constrain. Repointing a deposit address is
	// the attack that motivated it: the resulting journal would have been
	// correctly signed, chain-attested and reported VERIFIED, because the
	// signature covers what the application read and this changes what it
	// reads.
	t.Run("cannot repoint a deposit address at another holder", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			"INSERT INTO deposit_addresses (uid, account_holder, address, factory, init_hash) VALUES (gen_random_uuid(), 7701, '0xRoleTestVictim', '0xfactory', '0xinit')")
		require.NoError(t, err)

		_, err = appPool.Exec(ctx, "UPDATE deposit_addresses SET account_holder = 7702 WHERE address = '0xRoleTestVictim'")
		require.Error(t, err, "an application credential must not be able to change who a deposit address credits")

		// The no-op upsert address registration relies on must still pass:
		// its conflict branch assigns account_holder its own value so
		// RETURNING yields the existing row, and nothing actually changes.
		var holder int64
		require.NoError(t, appPool.QueryRow(ctx,
			`INSERT INTO deposit_addresses (uid, account_holder, address, factory, init_hash)
			 VALUES (gen_random_uuid(), 7701, '0xRoleTestVictim', '0xfactory', '0xinit')
			 ON CONFLICT (account_holder) DO UPDATE SET account_holder = EXCLUDED.account_holder
			 RETURNING account_holder`).Scan(&holder))
		assert.Equal(t, int64(7701), holder, "registration must stay idempotent")
	})

	t.Run("cannot change a currency's precision or a classification's sign", func(t *testing.T) {
		// Seeded through the superuser pool so the subtest does not depend on
		// what any other subtest left behind -- and so a zero-row UPDATE can
		// never be mistaken for a refusal.
		_, err := pool.Exec(ctx, "INSERT INTO currencies (uid, code, name, exponent) VALUES (gen_random_uuid(), 'RTG', 'RoleTestGuarded', 6)")
		require.NoError(t, err)

		_, err = appPool.Exec(ctx, "UPDATE currencies SET exponent = 0 WHERE code = 'RTG'")
		require.Error(t, err, "exponent is what every amount is validated against")
		_, err = appPool.Exec(ctx, "UPDATE classifications SET normal_side = 'credit' WHERE id = 1")
		require.Error(t, err, "normal_side fixes the sign of every entry already recorded against this classification")
		_, err = appPool.Exec(ctx, "UPDATE classifications SET code = 'hijacked' WHERE id = 1")
		require.Error(t, err, "code is what presets resolve a classification by")
	})

	t.Run("can SELECT/INSERT journal_entries", func(t *testing.T) {
		currencyUID := postgrestest.SeedCurrency(t, pool, "RT2", "RoleTest2")
		classUID := postgrestest.SeedClassification(t, pool, "rt_wallet", "RT Wallet", "debit", false)
		jtUID := postgrestest.SeedJournalType(t, pool, "rt_deposit", "RT Deposit")

		var journalID int64
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO journals (uid, journal_type_id, idempotency_key, total_debit, total_credit)
			SELECT gen_random_uuid(), jt.id, 'roles-test-j1', 10, 10
			FROM journal_types jt WHERE jt.uid = $1::uuid
			RETURNING id
		`, jtUID).Scan(&journalID))

		currencyID := postgrestest.InternalID(t, pool, "currencies", currencyUID)
		classID := postgrestest.InternalID(t, pool, "classifications", classUID)

		// Both legs, one transaction. The per-journal balance trigger
		// evaluates balance at commit, so a lone debit on its own autocommit
		// connection is correctly refused as unbalanced -- which would look
		// like a permission failure here and silently invert what this
		// subtest claims to prove.
		tx, err := appPool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = tx.Exec(ctx, `
			INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
			VALUES ($1, 999001, $2, $3, 'debit', 10)
		`, journalID, currencyID, classID)
		require.NoError(t, err, "ledger_app must be able to INSERT into journal_entries")
		_, err = tx.Exec(ctx, `
			INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
			VALUES ($1, -999001, $2, $3, 'credit', 10)
		`, journalID, currencyID, classID)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))

		var amount string
		require.NoError(t, appPool.QueryRow(ctx, `
			SELECT amount::text FROM journal_entries WHERE journal_id = $1 AND entry_type = 'debit'
		`, journalID).Scan(&amount))
		assert.Equal(t, "10.000000000000000000", amount)
	})
}

// TestLedgerAppInsertsIntoPartitionCreatedAfterGrant is the
// partition-inheritance pin: ledger_app's grant on journal_entries only
// covers the parent table's ACL, not any specific partition. This proves a
// partition created LATER by PartitionService (running as ledger_owner,
// journal_entries' owner) is still writable by ledger_app through the
// parent table name, without any additional GRANT ever being issued on that
// partition.
func TestLedgerAppInsertsIntoPartitionCreatedAfterGrant(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	const ownerPassword = "roles-test-owner-password-not-a-real-secret" //nolint:gosec // test-only credential
	const appPassword = "roles-test-app-password-2-not-a-real-secret"   //nolint:gosec // test-only credential
	_, err := pool.Exec(ctx, fmt.Sprintf("ALTER ROLE ledger_owner WITH PASSWORD '%s'", ownerPassword))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, fmt.Sprintf("ALTER ROLE ledger_app WITH PASSWORD '%s'", appPassword))
	require.NoError(t, err)

	baseURL := roleURLFromPool(pool)

	ownerPool, err := pgxpool.New(ctx, withRole(t, baseURL, "ledger_owner", ownerPassword))
	require.NoError(t, err)
	t.Cleanup(ownerPool.Close)
	require.NoError(t, ownerPool.Ping(ctx))

	appPool, err := pgxpool.New(ctx, withRole(t, baseURL, "ledger_app", appPassword))
	require.NoError(t, err)
	t.Cleanup(appPool.Close)
	require.NoError(t, appPool.Ping(ctx))

	// A date far enough out that no other test in this package could
	// already have created the partition.
	future := time.Now().AddDate(3, 0, 0)
	store := postgres.NewPartitionStore(ownerPool)
	created, err := store.EnsureMonthlyPartitions(ctx, future, 0)
	require.NoError(t, err, "ledger_owner must be able to run the partition-creation DDL PartitionService issues")
	require.NotEmpty(t, created, "expected a brand-new partition for a date 3 years out")

	currencyUID := postgrestest.SeedCurrency(t, pool, "PT1", "PartitionTest")
	classUID := postgrestest.SeedClassification(t, pool, "pt_wallet", "PT Wallet", "debit", false)
	jtUID := postgrestest.SeedJournalType(t, pool, "pt_deposit", "PT Deposit")

	var journalID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO journals (uid, journal_type_id, idempotency_key, total_debit, total_credit)
		SELECT gen_random_uuid(), jt.id, 'roles-test-partition-j1', 5, 5
		FROM journal_types jt WHERE jt.uid = $1::uuid
		RETURNING id
	`, jtUID).Scan(&journalID))
	currencyID := postgrestest.InternalID(t, pool, "currencies", currencyUID)
	classID := postgrestest.InternalID(t, pool, "classifications", classUID)

	// Both legs in one transaction -- see the note in
	// TestLedgerAppIsLeastPrivilege: the deferred balance trigger would
	// reject a lone debit at commit, which here would read as "ledger_app
	// cannot write the new partition" and invert the finding.
	tx, err := appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at)
		VALUES ($1, 999002, $2, $3, 'debit', 5, $4)
	`, journalID, currencyID, classID, future)
	require.NoError(t, err, "ledger_app must be able to insert into a partition ledger_owner created after the schema's GRANT ran")
	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at)
		VALUES ($1, -999002, $2, $3, 'credit', 5, $4)
	`, journalID, currencyID, classID, future)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	var count int
	require.NoError(t, pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s", created[0])).Scan(&count))
	assert.Equal(t, 2, count, "both rows must have physically routed into the new partition, not the default")
}

// roleURLFromPool derives a connection string from an already-connected
// pool's resolved config, so callers can build role-specific URLs against
// the same (dynamically-assigned, testcontainers) host/port/database
// without re-parsing a DSN string.
func roleURLFromPool(pool *pgxpool.Pool) string {
	cfg := pool.Config().ConnConfig
	return fmt.Sprintf("postgres://placeholder:placeholder@%s:%d/%s?sslmode=disable", cfg.Host, cfg.Port, cfg.Database)
}

// TestRoleAttributes pins the static catalog shape (role attributes +
// grants) that the behavioral tests above build on.
func TestRoleAttributes(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT rolname, rolcanlogin, rolsuper, rolcreatedb, rolcreaterole
		FROM pg_roles WHERE rolname IN ('ledger_owner', 'ledger_app', 'ledger_ro')
	`)
	require.NoError(t, err)
	defer rows.Close()

	type attrs struct{ canLogin, super, createDB, createRole bool }
	got := map[string]attrs{}
	for rows.Next() {
		var name string
		var a attrs
		require.NoError(t, rows.Scan(&name, &a.canLogin, &a.super, &a.createDB, &a.createRole))
		got[name] = a
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 3, "expected ledger_owner, ledger_app, ledger_ro to all exist")

	for _, name := range []string{"ledger_owner", "ledger_app", "ledger_ro"} {
		a, ok := got[name]
		require.True(t, ok, "%s must exist", name)
		assert.True(t, a.canLogin, "%s must be able to log in", name)
		assert.False(t, a.super, "%s must not be superuser", name)
		assert.False(t, a.createDB, "%s must not be able to create databases", name)
		assert.False(t, a.createRole, "%s must not be able to create roles", name)
	}

	// ledger_app: SELECT/INSERT/UPDATE on an ordinary table, SELECT/INSERT
	// only (no UPDATE) on journal_entries, nothing on schema_migrations.
	assertGrants(t, pool, "ledger_app", "currencies", []string{"SELECT", "INSERT", "UPDATE"})
	assertGrants(t, pool, "ledger_app", "journal_entries", []string{"SELECT", "INSERT"})
	assertGrants(t, pool, "ledger_app", "schema_migrations", nil)

	// ledger_ro: SELECT everywhere, including journal_entries.
	assertGrants(t, pool, "ledger_ro", "currencies", []string{"SELECT"})
	assertGrants(t, pool, "ledger_ro", "journal_entries", []string{"SELECT"})
}

// assertGrants queries information_schema.role_table_grants for exactly the
// privilege types (grantee, table) holds on table -- nil/empty means "no
// grants at all".
func assertGrants(t *testing.T, pool *pgxpool.Pool, grantee, table string, want []string) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
		SELECT privilege_type FROM information_schema.role_table_grants
		WHERE grantee = $1 AND table_schema = 'public' AND table_name = $2
	`, grantee, table)
	require.NoError(t, err)
	defer rows.Close()

	var got []string
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		got = append(got, p)
	}
	require.NoError(t, rows.Err())
	assert.ElementsMatch(t, want, got, "%s privileges on %s", grantee, table)
}
