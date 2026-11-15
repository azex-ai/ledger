package postgrestest

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/pgtestenv"
)

// TestDevDBScriptUsesSharedRolePassword pins scripts/dev-db.sh (`make db`)
// to SharedRolePassword. The local development database and the integration
// suite share one server and therefore the cluster-wide ledger_* roles; the
// script and a running suite can each (re)set those roles' password at any
// time without breaking the other only while both set the same value.
func TestDevDBScriptUsesSharedRolePassword(t *testing.T) {
	raw, err := os.ReadFile("../../scripts/dev-db.sh")
	require.NoError(t, err, "read scripts/dev-db.sh")
	require.Truef(t, strings.Contains(string(raw), "\npassword="+SharedRolePassword+" "),
		"scripts/dev-db.sh must set the shared ledger_* roles' password to postgrestest.SharedRolePassword (%q); "+
			"any other value makes `make db` and a concurrently running test suite overwrite each other's credential",
		SharedRolePassword)
}

// TestDevDBScriptTakesTheSharedRolePasswordLock pins scripts/dev-db.sh to the
// lock SetSharedRolePassword takes around the same ALTER ROLE.
func TestDevDBScriptTakesTheSharedRolePasswordLock(t *testing.T) {
	raw, err := os.ReadFile("../../scripts/dev-db.sh")
	require.NoError(t, err, "read scripts/dev-db.sh")
	require.Truef(t, strings.Contains(string(raw), fmt.Sprintf("pg_advisory_xact_lock(%d)", pgtestenv.SharedRolePasswordLockKey)),
		"scripts/dev-db.sh must take pgtestenv.SharedRolePasswordLockKey (%d) around its ALTER ROLE ... PASSWORD",
		pgtestenv.SharedRolePasswordLockKey)
}
