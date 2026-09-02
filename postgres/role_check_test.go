package postgres_test

// Pin for A-N2 (2026-09-02 deep audit): the deployment discipline every
// ACL-enforced invariant rests on is checkable.
//
// I-42 says journal_entries.id is sourced from the sequence alone, and the
// balance equation's checkpoint+delta scan needs that to be true. What makes
// it true is migration 008's column-level INSERT grant -- against ledger_app,
// by name. The same is true of the append-only guards' REVOKEs, of 014's
// webhook_subscribers narrowing, and of 021's function whitelist.
//
// Connect the serving pool as ledger_owner instead and none of them is
// violated: they are absent. That is the failure mode worth pinning, and the
// pin has to assert the shape of it -- as the owner the forbidden INSERT
// SUCCEEDS -- rather than assert a refusal, which is what a test written from
// the invariant's own wording would have done.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

func TestAssertRuntimeRole_CatchesAConnectionTheACLsDoNotConstrain(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	t.Run("as ledger_app the check passes", func(t *testing.T) {
		appPool := newAppPool(t, pool, "role-check-app-not-a-real-secret") //nolint:gosec
		svc, err := ledger.New(appPool)
		require.NoError(t, err)
		require.NoError(t, svc.AssertRuntimeRole(ctx))
	})

	t.Run("as any other role it reports the mismatch, naming both", func(t *testing.T) {
		// The test container's own superuser stands in for "whatever the
		// operator actually wired": the point is not which role it is, only
		// that it is not the one the grants name.
		svc, err := ledger.New(pool)
		require.NoError(t, err)

		err = svc.AssertRuntimeRole(ctx)
		require.Error(t, err, "I-42's load-bearing prerequisite is a role name; nothing checked it before this")
		require.ErrorIs(t, err, core.ErrInvalidInput)
		assert.Contains(t, err.Error(), postgres.AppRole, "the message has to name the role the operator should be using")
	})

	t.Run("and the reason it matters: the invariant is absent, not violated", func(t *testing.T) {
		// I-42 in one statement. As ledger_app this INSERT is refused by the
		// column-level grant (pinned in
		// TestJournalEntries_DuplicateIDAcrossPartitions_Rejected). As the
		// owner it goes straight through -- no error, no warning, and a
		// journal_entries row whose id the sequence never issued, which is
		// exactly what the balance equation assumes cannot exist.
		//
		// This subtest is the argument for AssertRuntimeRole existing at all:
		// the library cannot make this refusal happen on the wrong connection,
		// so the only thing it can do is let a consumer find out.
		var currencyID, classID, typeID, journalID int64
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO currencies (uid, code, name) VALUES (gen_random_uuid(), 'RCK', 'role check') RETURNING id
		`).Scan(&currencyID))
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO classifications (uid, code, name, normal_side, is_system)
			VALUES (gen_random_uuid(), 'role_check', 'role check', 'debit', false) RETURNING id
		`).Scan(&classID))
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO journal_types (uid, code, name) VALUES (gen_random_uuid(), 'role_check', 'role check') RETURNING id
		`).Scan(&typeID))
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO journals (uid, journal_type_id, idempotency_key, effective_at, total_debit, total_credit)
			VALUES (gen_random_uuid(), $1, 'role-check-journal', now(), 1, 1)
			RETURNING id
		`, typeID).Scan(&journalID))

		// Both legs, so the deferred per-journal balance trigger is satisfied at
		// commit and the only thing under test is the id column. Ids the sequence
		// never issued: as ledger_app the first of these is refused at the ACL
		// layer with 42501 (TestJournalEntries_DuplicateIDAcrossPartitions_Rejected).
		_, err := pool.Exec(ctx, `
			INSERT INTO journal_entries (id, journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at)
			VALUES (9000000000000001, $1, 950101, $2, $3, 'debit', 1, now(), now()),
			       (9000000000000002, $1, 950101, $2, $3, 'credit', 1, now(), now())
		`, journalID, currencyID, classID)
		require.NoError(t, err,
			"as the owner, an explicit journal_entries.id is accepted -- I-42 is not enforced on this connection, which is the whole point")

		var last int64
		require.NoError(t, pool.QueryRow(ctx, "SELECT last_value FROM journal_entries_id_seq").Scan(&last))
		assert.Less(t, last, int64(9000000000000001),
			"and the sequence never issued them, so the next checkpoint+delta scan sees rows it will never reach again")
	})
}

// TestCheckRuntimeRole_IsUsableWithoutTheFacade covers the other consumption
// mode: a caller assembling stores directly (server.NewWithConfig, or a
// composition root that never builds a ledger.Service) still needs the check.
func TestCheckRuntimeRole_IsUsableWithoutTheFacade(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	require.Error(t, postgres.CheckRuntimeRole(ctx, pool, postgres.AppRole))

	appPool := newAppPool(t, pool, "role-check-direct-not-a-real-secret") //nolint:gosec
	require.NoError(t, postgres.CheckRuntimeRole(ctx, appPool, postgres.AppRole))
}
