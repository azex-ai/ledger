package postgrestest

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
