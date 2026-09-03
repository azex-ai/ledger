package postgres_test

// Pin for m1 of the 2026-09-03 independent review: migration 007's
// fail-closed install error must name the attribute the role HOLDS, not the
// clause that clears it.
//
// Measured before the fix, on a cluster where ledger_app already had
// SUPERUSER and the migration credential could not strip it:
//
//	ledger: role ledger_app already exists on this cluster with the
//	NOSUPERUSER attribute and this migration credential cannot remove it.
//
// The role holds SUPERUSER. The remedy half of the sentence (`ALTER ROLE
// ledger_app NOSUPERUSER`) was correct; the diagnostic half printed the same
// array and so said the opposite of the truth. For a message whose entire
// value is being actionable during an install that has just stopped, that is
// worth pinning.
//
// This is a structural pin on the migration text rather than a behavioural
// one. Reproducing the message needs a shared cluster where the three roles
// pre-exist with a privilege attribute AND the migration credential cannot
// remove it -- a cluster-wide role state that would race every other ACL
// assertion in this package (see holdACLGuard in roles_test.go for why that
// matters). What can regress here is printing the wrong array, and that is
// exactly what this reads.

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var roleAttrArray = regexp.MustCompile(`(?m)^\s*(attrs|held_as|clauses)\s+CONSTANT text\[\] := ARRAY\[([^\]]*)\];`)

func TestMigration007_DiagnosticNamesTheHeldAttributeNotTheClearingClause(t *testing.T) {
	raw, err := os.ReadFile("sql/migrations/007_role_hardening_and_partition_security_definer.up.sql")
	require.NoError(t, err)
	sql := string(raw)

	arrays := map[string][]string{}
	for _, m := range roleAttrArray.FindAllStringSubmatch(sql, -1) {
		var items []string
		for _, item := range strings.Split(m[2], ",") {
			items = append(items, strings.Trim(strings.TrimSpace(item), "'"))
		}
		arrays[m[1]] = items
	}

	for _, name := range []string{"attrs", "held_as", "clauses"} {
		require.Containsf(t, arrays, name,
			"migration 007 must declare a %s array -- three parallel arrays is what keeps the held attribute and the clearing clause from being the same word", name)
	}
	require.Equal(t, len(arrays["attrs"]), len(arrays["held_as"]), "attrs and held_as must stay parallel")
	require.Equal(t, len(arrays["attrs"]), len(arrays["clauses"]), "attrs and clauses must stay parallel")
	require.NotEmpty(t, arrays["attrs"], "sanity: the attribute list is not empty")

	for i, held := range arrays["held_as"] {
		assert.Equal(t, "NO"+held, arrays["clauses"][i],
			"held_as[%d]=%q and clauses[%d]=%q must be the same attribute in its two forms; if they diverge the message is naming two different privileges",
			i, held, i, arrays["clauses"][i])
		// pg_authid abbreviates (rolsuper for SUPERUSER, but rolcreatedb for
		// CREATEDB), so the column is a prefix of the attribute rather than
		// equal to it. Prefix is still enough to catch a pair that has
		// drifted onto two different privileges, which is what would make the
		// message report an attribute the loop never tested.
		column := strings.TrimPrefix(arrays["attrs"][i], "rol")
		assert.True(t, strings.HasPrefix(arrays["attrs"][i], "rol") && strings.HasPrefix(strings.ToLower(held), column),
			"attrs[%d]=%q and held_as[%d]=%q must name the same privilege -- otherwise the message reports an attribute the loop did not test",
			i, arrays["attrs"][i], i, held)
	}

	// The two positions in the RAISE. The diagnostic ("already exists ... with
	// the % attribute") must read held_as; the remedy ("ALTER ROLE % %") must
	// read clauses. Asserted on the argument list because that is where the
	// inversion lived: the sentence itself never changed.
	raiseArgs := regexp.MustCompile(`(?s)RAISE EXCEPTION\s*\n\s*'ledger: role % already exists on this cluster.*?',\s*\n\s*([^\n]*)\n`).FindStringSubmatch(sql)
	require.Len(t, raiseArgs, 2, "the fail-closed RAISE must still be there in a shape this pin can read")
	args := strings.TrimSpace(raiseArgs[1])
	assert.Equal(t, "role_name, held_as[i], role_name, role_name, clauses[i]", args,
		"the diagnostic position must be held_as[i] (what the role HAS) and the remedy position clauses[i] (what removes it); before m1 both were clauses[i] and the message said the opposite of the truth")
}
