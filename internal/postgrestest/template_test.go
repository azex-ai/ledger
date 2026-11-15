package postgrestest

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/pgtestenv"
)

// TestTemplateCopyMatchesTheTemplate pins SetupDB's template copy against
// the migrated template it was copied from, on the two pieces of state that
// live on the DATABASE object and that CREATE DATABASE ... TEMPLATE does not
// copy: its ACL (pg_database.datacl -- migration 030 revokes TEMPORARY from
// PUBLIC there), its ALTER DATABASE settings (pg_db_role_setting), its owner
// and its COMMENT ON DATABASE.
// applyDatabaseLevelMigrationState re-applies what the migrations set today;
// a future migration that adds database-level state makes the copy differ
// from the template, and this test fails instead of every SetupDB test
// running on a database weaker than the one Migrate produces.
func TestTemplateCopyMatchesTheTemplate(t *testing.T) {
	pool := SetupDB(t)
	ctx := context.Background()
	var copyName string
	require.NoError(t, pool.QueryRow(ctx, "SELECT current_database()").Scan(&copyName))
	templateName := migrated.name
	require.NotEmpty(t, templateName)
	require.NotEqual(t, templateName, copyName)

	state := func(name string) (acl, settings, owner, comment string) {
		t.Helper()
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT coalesce(d.datacl::text, ''),
			       coalesce((SELECT string_agg(s.setrole::text || ':' || s.setconfig::text, ';' ORDER BY s.setrole)
			                 FROM pg_db_role_setting s WHERE s.setdatabase = d.oid), ''),
			       d.datdba::regrole::text,
			       coalesce(pg_catalog.shobj_description(d.oid, 'pg_database'), '')
			FROM pg_database d WHERE d.datname = $1`, name).Scan(&acl, &settings, &owner, &comment))
		return acl, settings, owner, comment
	}
	templateACL, templateSettings, templateOwner, templateComment := state(templateName)
	copyACL, copySettings, copyOwner, copyComment := state(copyName)
	require.NotEmpty(t, templateACL, "the migrated template has the default (NULL) database ACL -- migration 030's "+
		"REVOKE TEMPORARY ON DATABASE no longer reaches it, so this comparison would not detect a missing re-apply")
	require.Equal(t, templateACL, copyACL, "database ACL of a SetupDB copy differs from the migrated template's")
	require.Equal(t, templateSettings, copySettings, "ALTER DATABASE settings of a SetupDB copy differ from the migrated template's")
	require.Equal(t, templateOwner, copyOwner, "owner of a SetupDB copy differs from the migrated template's")
	require.Equal(t, templateComment, copyComment, "COMMENT ON DATABASE of a SetupDB copy differs from the migrated template's")
}

// TestReclaimOrphanTemplates pins the orphan-template rule: a template whose
// process lock nobody holds is dropped, one whose lock is held (a live
// process) is kept. The fake pids are far above any real pid on the machine.
func TestReclaimOrphanTemplates(t *testing.T) {
	base := baseConnection(t)
	ctx := context.Background()
	const orphanPID, livePID = 2147480001, 2147480002
	orphan, live := pgtestenv.TemplateDatabaseName(orphanPID), pgtestenv.TemplateDatabaseName(livePID)

	maint, err := pgtestenv.MaintenanceURL(base)
	require.NoError(t, err)
	admin, err := pgx.Connect(ctx, maint)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	for _, db := range []string{orphan, live} {
		_, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize())
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)")
		})
	}

	holder, err := pgx.Connect(ctx, maint)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Close(context.Background()) })
	require.NoError(t, pgtestenv.HoldProcessLock(ctx, holder, livePID))

	require.NoError(t, pgtestenv.ReclaimOrphanTemplates(ctx, base, os.Getpid()))

	exists := func(db string) bool {
		var ok bool
		require.NoError(t, admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", db).Scan(&ok))
		return ok
	}
	require.False(t, exists(orphan), "template of an exited process (process lock free) must be reclaimed")
	require.True(t, exists(live), "template of a live process (process lock held) must be kept")
}
