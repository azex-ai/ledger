package postgres_test

// Structural pin for I-57 (docs/INVARIANTS.md) and migration 019: every
// relation and every routine in `public` belongs to ledger_owner.
//
// Why this exists: 001_baseline transfers ownership with a catalogue sweep at
// the bottom of the file, and its own comment argues that sweeping the
// catalogue "is what makes both classes impossible here". It made them
// impossible inside 001. Nothing looked across files, and a full-repo grep for
// relowner/proowner/OWNER TO returned only 001's own up/down pair -- so every
// object built by 002 through 018 stayed owned by whichever credential ran the
// migration: 4 tables, 4 sequences and 9 functions, measured on a clean
// install of 001-015.
//
// Two of those nine functions are SECURITY DEFINER with EXECUTE granted to
// ledger_app, so they were running as the bootstrap credential rather than as
// ledger_owner -- the premise migration 007's header uses to argue its blast
// radius shrinks. The other seven are the guard and audit trigger functions,
// whose owner can CREATE OR REPLACE the body of ledger_block_mutation() and
// turn every append-only guarantee in this schema off while leaving all the
// triggers in place, firing, and doing nothing.
//
// ⚠️ Expected to go red the moment a migration creates an object and does not
// end with `SELECT ledger_resweep_ownership();` -- that is this pin doing its
// job. The fix is the call, not an exception here.
//
// M-7 (W3 adversarial review of the gates): every check in this family --
// here, function_acl_test.go, roles_test.go, grant_coverage_test.go -- was
// scoped `WHERE nspname = 'public'`. The reviewer put a SECURITY DEFINER
// function that grants ledger_app ALL ON ALL TABLES IN SCHEMA public into a
// NEW schema, owned by the migration runner (a superuser in the common
// install), granted EXECUTE to ledger_app, and every one of them stayed
// green: one call and the leaked credential owns the database.
//
// Two changes close it. The sweeps below now cover every non-system schema.
// And TestObjectOwnership_NoUnregisteredSchemas asserts that no other schema
// exists at all unless it is registered -- which is what makes the remaining
// `nspname = 'public'` filters elsewhere sound rather than merely narrow: a
// second schema is red before anything in it needs auditing.

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
)

func TestObjectOwnership_EverythingInPublicBelongsToLedgerOwner(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	// Relations: ordinary tables, partitioned tables, partitions (NOT excluded
	// -- ALTER TABLE ... OWNER TO does not recurse into them and
	// ledger_rebalance_default_partition creates them at runtime), sequences,
	// views and materialized views. Indexes and TOAST tables always follow
	// their parent's owner and cannot be altered independently, so they are
	// not enumerable failures.
	rows, err := pool.Query(ctx, `
		SELECT n.nspname || '.' || c.relname, c.relkind, pg_get_userbyid(c.relowner)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg\_%'
		  AND c.relkind IN ('r', 'p', 'S', 'v', 'm')
		  AND pg_get_userbyid(c.relowner) <> 'ledger_owner'
		ORDER BY c.relname
	`)
	require.NoError(t, err)
	var offenders []string
	for rows.Next() {
		var name, kind, owner string
		require.NoError(t, rows.Scan(&name, &kind, &owner))
		offenders = append(offenders, fmt.Sprintf("%s (relkind %s) owned by %s", name, kind, owner))
	}
	require.NoError(t, rows.Err())
	rows.Close()
	assert.Empty(t, offenders,
		"every relation in public must be owned by ledger_owner; the migration that created these has to end with SELECT ledger_resweep_ownership()")

	rows, err = pool.Query(ctx, `
		SELECT p.oid::regprocedure::text, pg_get_userbyid(p.proowner)
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg\_%'
		  AND p.prokind IN ('f', 'p')
		  AND pg_get_userbyid(p.proowner) <> 'ledger_owner'
		ORDER BY 1
	`)
	require.NoError(t, err)
	offenders = nil
	for rows.Next() {
		var sig, owner string
		require.NoError(t, rows.Scan(&sig, &owner))
		offenders = append(offenders, fmt.Sprintf("%s owned by %s", sig, owner))
	}
	require.NoError(t, rows.Err())
	rows.Close()
	assert.Empty(t, offenders,
		"every routine in public must be owned by ledger_owner: its owner can CREATE OR REPLACE the body, which is how a guard is disabled without touching a trigger")
}

// TestObjectOwnership_SecurityDefinerFunctionsRunAsLedgerOwner is the sharp
// end of the pin above, stated as the property that actually matters rather
// than as a catalogue sweep. A SECURITY DEFINER function runs with its
// OWNER's privileges, and both of these are EXECUTE-able by ledger_app -- the
// credential the whole threat model assumes is leaked. While they belonged to
// the bootstrap credential (which 001 records as holding a permanent ADMIN
// OPTION on ledger_owner, and which is a superuser in the common install),
// that leaked credential held two entry points into a far larger privilege
// than I-35 claims for it.
func TestObjectOwnership_SecurityDefinerFunctionsRunAsLedgerOwner(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT p.oid::regprocedure::text, pg_get_userbyid(p.proowner)
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg\_%'
		  AND p.prosecdef
		ORDER BY 1
	`)
	require.NoError(t, err)
	found := map[string]string{}
	for rows.Next() {
		var sig, owner string
		require.NoError(t, rows.Scan(&sig, &owner))
		found[sig] = owner
	}
	require.NoError(t, rows.Err())
	rows.Close()

	require.NotEmpty(t, found, "sanity: the two partition entry points are SECURITY DEFINER")
	for sig, owner := range found {
		assert.Equal(t, "ledger_owner", owner,
			"%s runs with its owner's privileges and ledger_app can call it (I-35)", sig)
	}
}

// registeredSchemas maps every schema this deployment is allowed to have to
// why it exists. `public` is the ledger. Anything else must be argued for
// here, because every other privilege gate in this package reads `public`
// alone, and a schema nobody registered is a place to hide an object those
// gates never look at (M-7: a SECURITY DEFINER escalation function in a
// second schema was invisible to all of them).
var registeredSchemas = map[string]string{
	"public": "the ledger schema; 001_baseline builds it and ledger_resweep_ownership() keeps every object in it owned by ledger_owner",
}

func TestObjectOwnership_NoUnregisteredSchemas(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT n.nspname, pg_get_userbyid(n.nspowner)
		FROM pg_namespace n
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg\_%'
		ORDER BY 1
	`)
	require.NoError(t, err)
	defer rows.Close()

	found := map[string]string{}
	for rows.Next() {
		var name, owner string
		require.NoError(t, rows.Scan(&name, &owner))
		found[name] = owner
	}
	require.NoError(t, rows.Err())

	require.Contains(t, found, "public", "sanity: the ledger schema must exist, or this gate is reading an empty catalogue")
	for name, owner := range found {
		assert.Containsf(t, registeredSchemas, name,
			"schema %q (owned by %s) is not registered. Every privilege gate in this package is written against `public`: "+
				"an object in another schema -- a SECURITY DEFINER function that grants ledger_app ALL ON ALL TABLES, say -- "+
				"is checked by none of them. Either drop the schema or add it to registeredSchemas with the reason it exists "+
				"AND extend those gates to cover it", name, owner)
	}
	for name, reason := range registeredSchemas {
		assert.Containsf(t, found, name, "registered schema %q (%s) does not exist -- delete the entry", name, reason)
	}
}
