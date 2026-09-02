package postgres_test

// The layer grant_coverage_test.go has never read: EXECUTE privileges on
// functions, and the ACL of journal_entries' partitions.
//
// grant_coverage_test.go is the strictest gate in this schema -- a new table
// that lands in none of its three classifications fails outright -- and it
// queries pg_class for relkind IN ('r','p') with NOT relispartition, plus
// pg_class for sequences. It never touches pg_proc, and it excludes every
// partition. Both blind spots hid real findings for two audit rounds:
// migration 007 handed ledger_app EXECUTE on two SECURITY DEFINER functions
// owned by the bootstrap credential (D-M1), and I-22's claim that ledger_app
// "cannot create any object, anywhere in the schema" was falsified by exactly
// those two grants without anything going red (D-m2).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
)

// TestFunctionExecuteACL_IsExactlyTheDocumentedWhitelist asserts that the set
// of functions each serving role can call is exactly the set the schema means
// it to call -- not "at least", and not "whatever the default happened to be".
//
// Before migration 021 the answer was "all of them": a new function is
// EXECUTE-able by PUBLIC unless a migration says otherwise, and only 007, 009
// and 013 ever said otherwise. So ledger_app could call ledger_block_mutation()
// and every other guard and audit function directly. None of that is
// exploitable on its own (the guards raise, the audit writers need a trigger
// context), but "not exploitable today" is how the two partition functions sat
// unnoticed, and the ACL is the layer that is supposed to say what is
// reachable. 021 revokes PUBLIC across the catalogue and grants back exactly
// the list below.
//
// ⚠️ A new function is reachable by nobody until a migration names a grantee,
// and a new grantee has to be added here. That direction -- closed by default,
// opened by review -- is the point.
func TestFunctionExecuteACL_IsExactlyTheDocumentedWhitelist(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	// ledger_app calls the two partition entry points from
	// postgres/partition_store.go (migration 007's whole purpose: partition
	// maintenance without a ledger_owner connection, I-35), and the three sign
	// helpers appear inside ordinary balance/trend/reconcile/holder queries it
	// runs (migration 009, I-43).
	//
	// ledger_ro runs the same read queries and so needs the same three
	// helpers, and must NOT reach the partition functions: they are DDL, and
	// a BI credential creating tables is the opposite of what read-only means.
	want := map[string][]string{
		"ledger_app": {
			"ledger_create_monthly_partition(text,date,date)",
			"ledger_rebalance_default_partition(date,date)",
			"ledger_reject_unknown_normal_side(text)",
			"ledger_signed_amount(text,text,numeric)",
			"ledger_signed_delta(text,numeric,numeric)",
		},
		"ledger_ro": {
			"ledger_reject_unknown_normal_side(text)",
			"ledger_signed_amount(text,text,numeric)",
			"ledger_signed_delta(text,numeric,numeric)",
		},
	}

	for role, expected := range want {
		role, expected := role, expected
		t.Run(role, func(t *testing.T) {
			rows, err := pool.Query(ctx, `
				SELECT p.oid::regprocedure::text
				FROM pg_proc p
				JOIN pg_namespace n ON n.oid = p.pronamespace
				WHERE n.nspname = 'public'
				  AND p.prokind IN ('f', 'p')
				  AND has_function_privilege($1, p.oid, 'EXECUTE')
				ORDER BY 1
			`, role)
			require.NoError(t, err)
			defer rows.Close()

			var got []string
			for rows.Next() {
				var sig string
				require.NoError(t, rows.Scan(&sig))
				got = append(got, sig)
			}
			require.NoError(t, rows.Err())
			assert.ElementsMatch(t, expected, got,
				"%s's EXECUTE set must be exactly the reviewed whitelist -- a function reachable by default is a capability nobody decided to grant", role)
		})
	}
}

// TestPartitionACL_EveryPartitionCarriesTheParentShape closes
// grant_coverage_test.go's `NOT c.relispartition` exclusion.
//
// Migration 008 narrowed ledger_app's INSERT on journal_entries to a column
// list that omits `id`, so the shared sequence is the only source of a row's
// id (I-42) -- a partitioned table's (id, created_at) primary key only
// guarantees uniqueness within one partition. A partition inherits the
// parent's ACL at creation, but nothing stops a later migration or an operator
// from granting on one partition directly, and the main grant-coverage query
// cannot see it: it filters partitions out entirely.
//
// The assertion is derived from the parent, not restated: whatever
// journal_entries grants, each partition must grant, and no more.
func TestPartitionACL_EveryPartitionCarriesTheParentShape(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	privilegesOn := func(relation, role string) []string {
		rows, err := pool.Query(ctx, `
			SELECT DISTINCT a.privilege_type
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			CROSS JOIN LATERAL aclexplode(c.relacl) AS a(grantor, grantee, privilege_type, is_grantable)
			WHERE n.nspname = 'public' AND c.relname = $1 AND a.grantee::regrole::text = $2
			ORDER BY 1
		`, relation, role)
		require.NoError(t, err)
		defer rows.Close()
		var got []string
		for rows.Next() {
			var p string
			require.NoError(t, rows.Scan(&p))
			got = append(got, p)
		}
		require.NoError(t, rows.Err())
		return got
	}

	rows, err := pool.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_inherits i ON i.inhrelid = c.oid
		JOIN pg_class parent ON parent.oid = i.inhparent
		JOIN pg_namespace n ON n.oid = parent.relnamespace
		WHERE n.nspname = 'public' AND parent.relname = 'journal_entries'
		ORDER BY c.relname
	`)
	require.NoError(t, err)
	var partitions []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		partitions = append(partitions, name)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.NotEmpty(t, partitions, "sanity: journal_entries is partitioned and has at least the default partition")

	for _, role := range []string{"ledger_app", "ledger_ro"} {
		parent := privilegesOn("journal_entries", role)
		for _, part := range partitions {
			assert.ElementsMatch(t, parent, privilegesOn(part, role),
				"partition %s must carry exactly journal_entries' %s grants -- a partition granted separately is invisible to grant_coverage_test.go", part, role)
		}
	}
}
