package postgrestest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTemplateCopyMatchesTheTemplate pins SetupDB's template copy against
// the migrated template it was copied from, on the two pieces of state that
// live on the DATABASE object and that CREATE DATABASE ... TEMPLATE does not
// copy: its ACL (pg_database.datacl -- migration 030 revokes TEMPORARY from
// PUBLIC there) and its ALTER DATABASE settings (pg_db_role_setting).
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

	state := func(name string) (acl, settings string) {
		t.Helper()
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT coalesce(d.datacl::text, ''),
			       coalesce((SELECT string_agg(s.setrole::text || ':' || s.setconfig::text, ';' ORDER BY s.setrole)
			                 FROM pg_db_role_setting s WHERE s.setdatabase = d.oid), '')
			FROM pg_database d WHERE d.datname = $1`, name).Scan(&acl, &settings))
		return acl, settings
	}
	templateACL, templateSettings := state(templateName)
	copyACL, copySettings := state(copyName)
	require.NotEmpty(t, templateACL, "the migrated template has the default (NULL) database ACL -- migration 030's "+
		"REVOKE TEMPORARY ON DATABASE no longer reaches it, so this comparison would not detect a missing re-apply")
	require.Equal(t, templateACL, copyACL, "database ACL of a SetupDB copy differs from the migrated template's")
	require.Equal(t, templateSettings, copySettings, "ALTER DATABASE settings of a SetupDB copy differ from the migrated template's")
}
