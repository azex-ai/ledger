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
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
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
	// webhook_subscribers is column-scoped for ledger_app's write as of
	// migration 014 (security review M-3): it may UPDATE only the three
	// delivery-status columns RecordDeliveryStatus writes, and may NOT INSERT
	// a subscriber or touch url/secret. This subtest proves the refusals above
	// are not vacuous -- the allowed status UPDATE really does succeed -- while
	// the next one pins the M-3 boundary.
	t.Run("can UPDATE its own delivery-status columns", func(t *testing.T) {
		var id int64
		require.NoError(t, pool.QueryRow(ctx,
			"INSERT INTO webhook_subscribers (url, name, secret) VALUES ('https://example.test/hook', 'RoleTest1', 's3cr3t') RETURNING id").Scan(&id))
		_, err := appPool.Exec(ctx, "UPDATE webhook_subscribers SET last_status_code = 200, last_error = '' WHERE id = $1", id)
		require.NoError(t, err)
		var code int
		require.NoError(t, appPool.QueryRow(ctx, "SELECT last_status_code FROM webhook_subscribers WHERE id = $1", id).Scan(&code))
		assert.Equal(t, 200, code)
	})

	// M-3: a leaked ledger_app credential must not be able to redirect the
	// event stream to its own URL, blank a subscriber's signing secret, or
	// create a subscriber at all. Subscriber lifecycle belongs to ledger_owner.
	t.Run("cannot forge or tamper with webhook subscribers", func(t *testing.T) {
		var id int64
		require.NoError(t, pool.QueryRow(ctx,
			"INSERT INTO webhook_subscribers (url, name, secret) VALUES ('https://legit.test/hook', 'RoleTestM3', 'realsecret') RETURNING id").Scan(&id))

		_, err := appPool.Exec(ctx, "INSERT INTO webhook_subscribers (url, name) VALUES ('https://attacker.tld/e', 'evil')")
		assertPermissionDenied(t, err)

		_, err = appPool.Exec(ctx, "UPDATE webhook_subscribers SET secret = '' WHERE id = $1", id)
		assertPermissionDenied(t, err)

		_, err = appPool.Exec(ctx, "UPDATE webhook_subscribers SET url = 'https://attacker.tld/e' WHERE id = $1", id)
		assertPermissionDenied(t, err)
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
// partition-inheritance pin, updated for migration 007: PartitionStore no
// longer needs an owner-backed pool at all. It calls
// ledger_create_monthly_partition/ledger_rebalance_default_partition, two
// SECURITY DEFINER functions that run with ledger_owner's privileges
// regardless of caller, so ledger_app can create the partition itself.
//
// This replaces the shape threat-model.md flagged: the previous version of
// this test built an ownerPool that PartitionStore in production has no way
// to construct (Worker() only ever has the single app pool,
// ledger.go:730-ish), so a green result here proved nothing about what
// ledgerd could actually do. There is now exactly one pool in this test, and
// it is the same one every other test in this file calls "ledger_app".
func TestLedgerAppInsertsIntoPartitionCreatedAfterGrant(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	const appPassword = "roles-test-app-password-2-not-a-real-secret" //nolint:gosec // test-only credential
	_, err := pool.Exec(ctx, fmt.Sprintf("ALTER ROLE ledger_app WITH PASSWORD '%s'", appPassword))
	require.NoError(t, err)

	baseURL := roleURLFromPool(pool)

	appPool, err := pgxpool.New(ctx, withRole(t, baseURL, "ledger_app", appPassword))
	require.NoError(t, err)
	t.Cleanup(appPool.Close)
	require.NoError(t, appPool.Ping(ctx))

	// A date far enough out that no other test in this package could
	// already have created the partition.
	future := time.Now().AddDate(3, 0, 0)
	store := postgres.NewPartitionStore(appPool)
	created, err := store.EnsureMonthlyPartitions(ctx, future, 0)
	require.NoError(t, err, "ledger_app must be able to create its own monthly partitions through the SECURITY DEFINER function -- no owner pool involved")
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
	require.NoError(t, err, "ledger_app must be able to insert into a partition it created itself, with no per-partition GRANT ever issued")
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

	// ledger_app: SELECT/INSERT/UPDATE on an ordinary table, SELECT
	// table-level plus a column-scoped INSERT (id excluded, migration 008)
	// on journal_entries, nothing on schema_migrations.
	assertGrants(t, pool, "ledger_app", "currencies", []string{"SELECT", "INSERT", "UPDATE"})
	assertGrants(t, pool, "ledger_app", "journal_entries", []string{"SELECT"})
	assertColumnPrivilegeExists(t, pool, "ledger_app", "journal_entries", "journal_id", "INSERT")
	assertColumnPrivilegeAbsent(t, pool, "ledger_app", "journal_entries", "id", "INSERT")
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

// TestBalanceRolePromotion_RefusedOnceEntriesExist pins migration 004.
//
// balance_role buckets a classification into the holder-facing breakdown, and
// 'available' is the only bucket Reserve spends from. The ” -> <role>
// upgrade was allowed unconditionally as a one-time install-time move, but
// nothing tied it to install time: one UPDATE promotes a classification that
// already holds balances, and every holder's history in it becomes spendable.
//
// fee_expense is the shipped example -- user-side, debited on every withdrawal
// fee -- so promoting it turns each holder's cumulative fees into withdrawable
// funds through an ordinary, correctly signed withdrawal. Nothing above the
// bucket notices: the entries are real, the signatures verify, debits equal
// credits, and the accounting identity is untouched.
func TestBalanceRolePromotion_RefusedOnceEntriesExist(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	curID := postgrestest.SeedCurrencyWithExponent(t, pool, "BRP", "Role Promo Unit", 2)
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	// Two classifications with no role yet: one will be given history, one
	// stays empty, so the test distinguishes "has entries" from "is named
	// fee_expense".
	withHistory := postgrestest.SeedClassification(t, pool, "accrued_fees", "Accrued Fees", "debit", false)
	untouched := postgrestest.SeedClassification(t, pool, "future_bucket", "Future Bucket", "debit", false)
	counterpart := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	store := postgres.NewLedgerStore(pool)
	_, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("role-promo"),
		Entries: []core.EntryInput{
			{AccountHolder: 8801, CurrencyUID: curID, ClassificationUID: withHistory, EntryType: core.EntryTypeDebit, Amount: decimal.RequireFromString("1200")},
			{AccountHolder: -8801, CurrencyUID: curID, ClassificationUID: counterpart, EntryType: core.EntryTypeCredit, Amount: decimal.RequireFromString("1200")},
		},
	})
	require.NoError(t, err)

	t.Run("promoting a classification that already holds balances is refused", func(t *testing.T) {
		_, err := pool.Exec(ctx, "UPDATE classifications SET balance_role = 'available' WHERE uid = $1::uuid", withHistory)
		require.Error(t, err, "1200 of recorded balance must not become spendable through a config UPDATE")
	})

	t.Run("the same promotion is allowed while the classification is empty", func(t *testing.T) {
		_, err := pool.Exec(ctx, "UPDATE classifications SET balance_role = 'available' WHERE uid = $1::uuid", untouched)
		require.NoError(t, err, "the install-time upgrade the rule was written for must still work")
	})

	t.Run("non-spendable buckets stay free even with history", func(t *testing.T) {
		// pending and locked are shown to the holder but never spent from, so
		// promoting into them cannot make anyone richer and stays unrestricted.
		_, err := pool.Exec(ctx, "UPDATE classifications SET balance_role = 'pending' WHERE uid = $1::uuid", withHistory)
		require.NoError(t, err, "pending is not spendable, so this upgrade carries no money risk")
	})
}

// newAppPool sets a password on ledger_app in the cluster backing pool and
// returns a fresh pool connected as ledger_app to the same (isolated)
// database. Tests in this file never run t.Parallel(), so setting the
// cluster-level role password sequentially per test is safe.
func newAppPool(t *testing.T, pool *pgxpool.Pool, password string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, fmt.Sprintf("ALTER ROLE ledger_app WITH PASSWORD '%s'", password))
	require.NoError(t, err)
	appPool, err := pgxpool.New(ctx, withRole(t, roleURLFromPool(pool), "ledger_app", password))
	require.NoError(t, err)
	t.Cleanup(appPool.Close)
	require.NoError(t, appPool.Ping(ctx))
	return appPool
}

// newRoPool is newAppPool's ledger_ro counterpart.
func newRoPool(t *testing.T, pool *pgxpool.Pool, password string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, fmt.Sprintf("ALTER ROLE ledger_ro WITH PASSWORD '%s'", password))
	require.NoError(t, err)
	roPool, err := pgxpool.New(ctx, withRole(t, roleURLFromPool(pool), "ledger_ro", password))
	require.NoError(t, err)
	t.Cleanup(roPool.Close)
	require.NoError(t, roPool.Ping(ctx))
	return roPool
}

// TestAccountPoliciesGuard pins migration 006: account_policies was one of
// the seven tables with no trigger at all, so 001_baseline's ACL-derivation
// loop (which only recognizes ledger_block_mutation()) classified it as an
// ordinary table and granted ledger_app full UPDATE. account_policies is the
// only DB-enforced freeze/overdraft floor (postgres/account_policy_enforce.go),
// so that UPDATE let a leaked ledger_app credential unfreeze an account or
// blow through its min_balance in one statement -- run against a real
// database and confirmed successful before migration 006 existed.
func TestAccountPoliciesGuard(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "roles-test-app-policies-not-a-real-secret") //nolint:gosec

	var policyUID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO account_policies (account_holder, currency_id, classification_id, status, min_balance, enforce_min_balance, note, uid)
		VALUES (900101, 0, 0, 'frozen', 0, true, 'seed', gen_random_uuid())
		RETURNING uid
	`).Scan(&policyUID))

	t.Run("cannot unfreeze by changing the policy's identifying dimension", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "UPDATE account_policies SET status = 'active', currency_id = 1 WHERE uid = $1::uuid", policyUID)
		require.Error(t, err, "the attack: an attacker who cannot forge a new policy row instead widens which dimension an existing one covers")
	})

	t.Run("legitimate SetPolicy-shaped update still works", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `
			UPDATE account_policies SET status = 'active', min_balance = 5, enforce_min_balance = false, note = 'reviewed', updated_at = now()
			WHERE uid = $1::uuid
		`, policyUID)
		require.NoError(t, err, "status/min_balance/enforce_min_balance/note/updated_at is UpsertAccountPolicy's actual mutable set")
	})
}

// TestBookingsAndEventsGuards pins migration 006's coverage of bookings and
// events, both unguarded before it despite CLAUDE.md documenting
// bookings.journal_id as set-once. Every UPDATE in this test's "attack"
// subtests was run against a real database as ledger_app and succeeded
// before migration 006 existed.
func TestBookingsAndEventsGuards(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "roles-test-app-bookevt-not-a-real-secret") //nolint:gosec

	currencyUID := postgrestest.SeedCurrency(t, pool, "BEG", "BookEventGuard")
	classUID := postgrestest.SeedClassification(t, pool, "beg_wallet", "BEG Wallet", "debit", false)
	jtUID := postgrestest.SeedJournalType(t, pool, "beg_deposit", "BEG Deposit")
	currencyID := postgrestest.InternalID(t, pool, "currencies", currencyUID)
	classID := postgrestest.InternalID(t, pool, "classifications", classUID)

	var journal1, journal2 int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO journals (uid, journal_type_id, idempotency_key, total_debit, total_credit)
		SELECT gen_random_uuid(), jt.id, 'beg-j1', 10, 10 FROM journal_types jt WHERE jt.uid = $1::uuid RETURNING id
	`, jtUID).Scan(&journal1))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO journals (uid, journal_type_id, idempotency_key, total_debit, total_credit)
		SELECT gen_random_uuid(), jt.id, 'beg-j2', 20, 20 FROM journal_types jt WHERE jt.uid = $1::uuid RETURNING id
	`, jtUID).Scan(&journal2))

	var bookingID int64
	var bookingUID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO bookings (classification_id, account_holder, currency_id, amount, status, idempotency_key, journal_id, uid)
		VALUES ($1, 900201, $2, 100, 'confirmed', 'beg-book-1', $3, gen_random_uuid())
		RETURNING id, uid
	`, classID, currencyID, journal1).Scan(&bookingID, &bookingUID))

	var eventUID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO events (classification_code, booking_id, account_holder, currency_id, to_status, amount, journal_id, uid)
		VALUES ('beg_wallet', $1, 900201, $2, 'confirmed', 100, $3, gen_random_uuid())
		RETURNING uid
	`, bookingID, currencyID, journal1).Scan(&eventUID))

	t.Run("bookings: cannot re-point journal_id or tamper with amount", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "UPDATE bookings SET journal_id = $1, amount = 999999 WHERE uid = $2::uuid", journal2, bookingUID)
		require.Error(t, err, "journal_id is documented set-once; a leaked credential must not be able to redirect which journal a booking is linked to")
	})

	t.Run("bookings: settled_amount must not decrease", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "UPDATE bookings SET settled_amount = -1 WHERE uid = $1::uuid", bookingUID)
		require.Error(t, err)
	})

	t.Run("bookings: UpdateBookingTransition's actual column set still works", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `
			UPDATE bookings SET status = 'settled', channel_ref = 'ref-1', settled_amount = 100, metadata = '{"x":1}'::jsonb, updated_at = now()
			WHERE uid = $1::uuid
		`, bookingUID)
		require.NoError(t, err)
	})

	t.Run("events: cannot tamper with amount/to_status/journal_id", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "UPDATE events SET amount = 999999, to_status = 'rejected', journal_id = $1 WHERE uid = $2::uuid", journal2, eventUID)
		require.Error(t, err, "amount/to_status/journal_id describe what happened and must not move after the fact")
	})

	t.Run("events: delivery-tracking columns still work", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "UPDATE events SET delivery_status = 'delivered', delivered_at = now(), attempts = attempts + 1 WHERE uid = $1::uuid", eventUID)
		require.NoError(t, err)
	})
}

// TestIdempotencyReceiptTablesAreAppendOnly pins migration 006's fix for
// board #14 item #25: account_policy_changes and the three idempotency
// receipt tables (reservation_settlement_legs, reservation_operation_receipts,
// booking_transition_receipts) had no legitimate UPDATE anywhere in
// postgres/sql/queries/, yet ledger_app held UPDATE on all four because none
// carried a trigger the ACL-derivation loop recognized. Forging a receipt
// lets an attacker short-circuit the next legitimate Settle/Release/
// FinalizeSettlement/Transition call for that idempotency key -- the
// operation reports success without re-applying, while the reservation it
// was supposed to close stays active and its funds stay locked.
func TestIdempotencyReceiptTablesAreAppendOnly(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "roles-test-app-receipts-not-a-real-secret") //nolint:gosec

	currencyUID := postgrestest.SeedCurrency(t, pool, "RCT", "ReceiptGuard")
	currencyID := postgrestest.InternalID(t, pool, "currencies", currencyUID)

	var policyID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO account_policies (account_holder, currency_id, classification_id, status, min_balance, enforce_min_balance, note, uid)
		VALUES (900301, 0, 0, 'active', 0, false, '', gen_random_uuid())
		RETURNING id
	`).Scan(&policyID))
	var changeID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO account_policy_changes (policy_id, old_state, new_state, actor_id)
		VALUES ($1, '{}'::jsonb, '{"status":"active"}'::jsonb, 1)
		RETURNING id
	`, policyID).Scan(&changeID))

	var reservationID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO reservations (account_holder, currency_id, reserved_amount, idempotency_key, uid)
		VALUES (900301, $1, 50, 'rct-res-1', gen_random_uuid())
		RETURNING id
	`, currencyID).Scan(&reservationID))

	_, err := pool.Exec(ctx, `INSERT INTO reservation_settlement_legs (reservation_id, idempotency_key, amount) VALUES ($1, 'rct-leg-1', 10)`, reservationID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO reservation_operation_receipts (reservation_id, operation, idempotency_key, amount) VALUES ($1, 'settle', 'rct-op-1', 10)`, reservationID)
	require.NoError(t, err)

	classUID := postgrestest.SeedClassification(t, pool, "rct_wallet", "RCT Wallet", "debit", false)
	classID := postgrestest.InternalID(t, pool, "classifications", classUID)
	jtUID := postgrestest.SeedJournalType(t, pool, "rct_deposit", "RCT Deposit")
	var journalID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO journals (uid, journal_type_id, idempotency_key, total_debit, total_credit)
		SELECT gen_random_uuid(), jt.id, 'rct-j1', 10, 10 FROM journal_types jt WHERE jt.uid = $1::uuid RETURNING id
	`, jtUID).Scan(&journalID))
	var bookingID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO bookings (classification_id, account_holder, currency_id, amount, status, idempotency_key, journal_id, uid)
		VALUES ($1, 900301, $2, 10, 'confirmed', 'rct-book-1', $3, gen_random_uuid())
		RETURNING id
	`, classID, currencyID, journalID).Scan(&bookingID))
	var eventID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO events (classification_code, booking_id, account_holder, currency_id, to_status, amount, journal_id, uid)
		VALUES ('rct_wallet', $1, 900301, $2, 'confirmed', 10, $3, gen_random_uuid())
		RETURNING id
	`, bookingID, currencyID, journalID).Scan(&eventID))
	_, err = pool.Exec(ctx, `
		INSERT INTO booking_transition_receipts (booking_id, idempotency_key, to_status, amount, event_id)
		VALUES ($1, 'rct-trans-1', 'confirmed', 10, $2)
	`, bookingID, eventID)
	require.NoError(t, err)

	cases := []struct {
		name string
		sql  string
	}{
		{"account_policy_changes", "UPDATE account_policy_changes SET new_state = '{\"forged\":true}'::jsonb WHERE id = " + fmt.Sprint(changeID)},
		{"reservation_settlement_legs", "UPDATE reservation_settlement_legs SET amount = 999999 WHERE idempotency_key = 'rct-leg-1'"},
		{"reservation_operation_receipts", "UPDATE reservation_operation_receipts SET amount = 999999 WHERE idempotency_key = 'rct-op-1'"},
		{"booking_transition_receipts", "UPDATE booking_transition_receipts SET amount = 999999 WHERE idempotency_key = 'rct-trans-1'"},
	}
	for _, tc := range cases {
		t.Run(tc.name+" refuses UPDATE", func(t *testing.T) {
			_, err := appPool.Exec(ctx, tc.sql)
			assertPermissionDenied(t, err)
		})
	}

	for _, table := range []string{"account_policy_changes", "reservation_settlement_legs", "reservation_operation_receipts", "booking_transition_receipts"} {
		t.Run(table+" refuses DELETE", func(t *testing.T) {
			_, err := appPool.Exec(ctx, "DELETE FROM "+table)
			assertPermissionDenied(t, err)
		})
	}
}

// TestReconcileScanCursorChangesAudited pins migration 006's answer to the
// §4-3 seam this task shares with D-ops: reconcile_scan_cursors cannot take
// a mutation guard (SetScanCursor legitimately overwrites it to any value,
// including a lap reset, service/reconcile.go:716), so a leaked ledger_app
// credential can still set after_holder to the max int64 to make the next
// scan see zero rows -- that half is not fixable at the DB layer. What this
// migration adds is an AFTER trigger that logs every write regardless of who
// issued it or why, so the tampering this test performs is no longer
// invisible even though it still succeeds.
func TestReconcileScanCursorChangesAudited(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "roles-test-app-cursor-not-a-real-secret") //nolint:gosec

	_, err := pool.Exec(ctx, "INSERT INTO reconcile_scan_cursors (check_name, lap_dirty) VALUES ('rt_checkpoint_balance', true)")
	require.NoError(t, err)

	// The attack: fake a fully-scanned lap by parking the cursor at the
	// maximum holder/currency and clearing lap_dirty, so the next scan sees
	// zero rows and the reconcile check reports Complete=true, Passed=true.
	_, err = appPool.Exec(ctx, `
		UPDATE reconcile_scan_cursors
		SET after_holder = 9223372036854775807, after_currency = 9223372036854775807, lap_dirty = false
		WHERE check_name = 'rt_checkpoint_balance'
	`)
	require.NoError(t, err, "the DB layer cannot refuse this write -- SetScanCursor legitimately writes arbitrary values")

	var oldHolder, newHolder int64
	var oldDirty, newDirty bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT old_after_holder, new_after_holder, old_lap_dirty, new_lap_dirty
		FROM reconcile_scan_cursor_changes WHERE check_name = 'rt_checkpoint_balance'
	`).Scan(&oldHolder, &newHolder, &oldDirty, &newDirty))
	assert.Equal(t, int64(-9223372036854775808), oldHolder)
	assert.Equal(t, int64(9223372036854775807), newHolder)
	assert.True(t, oldDirty)
	assert.False(t, newDirty, "the tamper this test performed must show up verbatim in the audit row")

	t.Run("the audit trail itself cannot be tampered with", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "UPDATE reconcile_scan_cursor_changes SET changed_by = 'nobody'")
		assertPermissionDenied(t, err)
		_, err = appPool.Exec(ctx, "DELETE FROM reconcile_scan_cursor_changes")
		assertPermissionDenied(t, err)
	})
}

// TestConfigTableChangesAudited pins migration 006's §9 fix: 003 stops
// currencies/classifications/journal_types/entry_templates from being
// tampered with, but a legitimate narrow mutation those same guards allow
// (an is_active toggle, say) previously left no record of who changed what
// or when -- "看不出改过" per docs/audits/.../TODO.md §9. current_user is
// always ledger_app in every deployment (that is the credential this whole
// guard system defends against), so this does not attribute to a business
// actor; it answers "this row changed from A to B at time T", which is what
// was missing.
func TestConfigTableChangesAudited(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "roles-test-app-cfgaudit-not-a-real-secret") //nolint:gosec

	currencyUID := postgrestest.SeedCurrency(t, pool, "CTC", "ConfigTableChangeGuard")

	_, err := appPool.Exec(ctx, "UPDATE currencies SET is_active = false WHERE uid = $1::uuid", currencyUID)
	require.NoError(t, err, "is_active is on 003's whitelist -- this must still work")

	var oldActive, newActive bool
	var changedBy string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT (old_row->>'is_active')::boolean, (new_row->>'is_active')::boolean, changed_by
		FROM config_table_changes WHERE table_name = 'currencies' AND old_row->>'uid' = $1
	`, currencyUID).Scan(&oldActive, &newActive, &changedBy))
	assert.True(t, oldActive)
	assert.False(t, newActive)
	assert.Equal(t, "ledger_app", changedBy)

	t.Run("a rejected attack leaves no audit row, because it leaves no committed row", func(t *testing.T) {
		var before int
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM config_table_changes").Scan(&before))
		_, err := appPool.Exec(ctx, "UPDATE currencies SET exponent = 0 WHERE uid = $1::uuid", currencyUID)
		require.Error(t, err, "003's guard still refuses this")
		var after int
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM config_table_changes").Scan(&after))
		assert.Equal(t, before, after, "the rejected statement never committed, so the AFTER trigger never ran -- this is the residual gap noted in migration 006's header, not something this migration claims to close")
	})

	t.Run("the audit trail itself cannot be tampered with", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "UPDATE config_table_changes SET changed_by = 'nobody'")
		assertPermissionDenied(t, err)
		_, err = appPool.Exec(ctx, "DELETE FROM config_table_changes")
		assertPermissionDenied(t, err)
	})
}

// TestLedgerRoCannotReadWebhookSecret pins migration 007: ledger_ro held
// blanket SELECT on webhook_subscribers, including the HMAC secret every
// outbound event delivery signs with (service/delivery/webhook.go). A
// read-only BI credential could read a key that lets it forge signed
// deliveries to any subscriber -- confirmed against a real database before
// migration 007 existed.
func TestLedgerRoCannotReadWebhookSecret(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	roPool := newRoPool(t, pool, "roles-test-ro-webhook-not-a-real-secret") //nolint:gosec

	_, err := pool.Exec(ctx, "INSERT INTO webhook_subscribers (name, url, secret) VALUES ('acme', 'https://acme.test/hook', 'super-secret-hmac-key')")
	require.NoError(t, err)

	t.Run("cannot select the secret column directly", func(t *testing.T) {
		var secret string
		err := roPool.QueryRow(ctx, "SELECT secret FROM webhook_subscribers WHERE name = 'acme'").Scan(&secret)
		assertPermissionDenied(t, err)
	})

	t.Run("cannot select * either", func(t *testing.T) {
		// pgx v5 defers query execution: Query() itself rarely returns the
		// server's permission-denied error (it is lazy about round-tripping),
		// which only surfaces once rows are pulled -- Next() returns false
		// and Err() carries it.
		rows, err := roPool.Query(ctx, "SELECT * FROM webhook_subscribers WHERE name = 'acme'")
		require.NoError(t, err)
		defer rows.Close()
		require.False(t, rows.Next(), "must not be able to iterate any row through a column that includes secret")
		assertPermissionDenied(t, rows.Err())
	})

	t.Run("every other column stays readable", func(t *testing.T) {
		var name, url string
		require.NoError(t, roPool.QueryRow(ctx, "SELECT name, url FROM webhook_subscribers WHERE name = 'acme'").Scan(&name, &url))
		assert.Equal(t, "acme", name)
	})
}

// TestRoleAttributeHardeningResetsPreExistingPrivileges pins migration 007's
// fix for the Minor finding that CREATE ROLE IF NOT EXISTS trusts whatever a
// shared cluster already has under these names: it simulates installing onto
// a cluster where ledger_app was previously (by another tenant, a manual
// grant, an old install) made SUPERUSER/CREATEROLE, and confirms the
// migration's unconditional ALTER ROLE resets it regardless.
func TestRoleAttributeHardeningResetsPreExistingPrivileges(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	_, err := pool.Exec(ctx, "ALTER ROLE ledger_app SUPERUSER CREATEROLE CREATEDB REPLICATION")
	require.NoError(t, err, "simulating a role this install did not create with elevated attributes already set")

	var super, createRole, createDB, replication bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT rolsuper, rolcreaterole, rolcreatedb, rolreplication FROM pg_roles WHERE rolname = 'ledger_app'
	`).Scan(&super, &createRole, &createDB, &replication))
	require.True(t, super && createRole && createDB && replication, "sanity: the simulated over-privileged state must actually be in place")

	// This is migration 007's own statement, re-applied -- what a fresh
	// install onto this already-populated cluster would run.
	_, err = pool.Exec(ctx, "ALTER ROLE ledger_app NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS")
	require.NoError(t, err)

	require.NoError(t, pool.QueryRow(ctx, `
		SELECT rolsuper, rolcreaterole, rolcreatedb, rolreplication FROM pg_roles WHERE rolname = 'ledger_app'
	`).Scan(&super, &createRole, &createDB, &replication))
	assert.False(t, super, "I-22 must hold even on a cluster where ledger_app pre-existed with SUPERUSER")
	assert.False(t, createRole)
	assert.False(t, createDB)
	assert.False(t, replication)
}

// TestPartitionMaintenanceRejectsUnshapedPartitionNames pins
// ledger_create_monthly_partition's own input validation (migration 007):
// the function's name argument is interpolated into DDL via format(%I), so
// EXECUTE on it is itself a ledger_app-reachable capability that must not
// accept an arbitrary identifier.
func TestPartitionMaintenanceRejectsUnshapedPartitionNames(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "roles-test-app-partname-not-a-real-secret") //nolint:gosec

	t.Run("rejects a name that does not match the monthly-partition shape", func(t *testing.T) {
		var created bool
		err := appPool.QueryRow(ctx, "SELECT ledger_create_monthly_partition('webhook_subscribers', '2035-01-01', '2035-02-01')").Scan(&created)
		require.Error(t, err, "must not be able to hijack an existing table's name through this function")
	})

	t.Run("rejects a name carrying a statement terminator", func(t *testing.T) {
		var created bool
		err := appPool.QueryRow(ctx, "SELECT ledger_create_monthly_partition('evil; DROP TABLE journal_entries; --', '2035-01-01', '2035-02-01')").Scan(&created)
		require.Error(t, err)
	})
}
