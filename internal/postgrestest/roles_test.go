package postgrestest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	ledgerpg "github.com/azex-ai/ledger/postgres"
)

// readAll returns every up and down script, keyed "<version>.up"/".down",
// read through src.
func readAll(t *testing.T, src interface {
	First() (uint, error)
	Next(uint) (uint, error)
	ReadUp(uint) (io.ReadCloser, string, error)
	ReadDown(uint) (io.ReadCloser, string, error)
}) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	v, err := src.First()
	require.NoError(t, err)
	for {
		for dir, read := range map[string]func(uint) (io.ReadCloser, string, error){"up": src.ReadUp, "down": src.ReadDown} {
			r, _, err := read(v)
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
				continue
			}
			require.NoError(t, err)
			b, err := io.ReadAll(r)
			require.NoError(t, err)
			require.NoError(t, r.Close())
			out[fmt.Sprintf("%d.%s", v, dir)] = b
		}
		next, err := src.Next(v)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			break
		}
		require.NoError(t, err)
		v = next
	}
	require.NotEmpty(t, out)
	return out
}

// TestRoleRewritingSource_RenamesOnlyTheThreeRoles pins the mechanism the
// destructive roundtrip relies on to run on a shared server: every role
// token is renamed, nothing else changes, and the production source itself
// is untouched.
func TestRoleRewritingSource_RenamesOnlyTheThreeRoles(t *testing.T) {
	prod, err := ledgerpg.NewMigrationSource()
	require.NoError(t, err)
	original := readAll(t, prod)

	const prefix = "ledger_test_1_2"
	rewritingSrc, err := NewRoleRewritingMigrationSource(prefix)
	require.NoError(t, err)
	rewritten := readAll(t, rewritingSrc)
	require.Equal(t, len(original), len(rewritten))

	anyDefault := regexp.MustCompile(`\bledger_(owner|app|ro)\b`)
	totalTokens := 0
	for key, orig := range original {
		got := rewritten[key]
		totalTokens += len(anyDefault.FindAll(orig, -1))
		require.Falsef(t, anyDefault.Match(got), "%s still names a shared ledger role after the rewrite", key)
		// Reversing the three private names restores the original byte for
		// byte: the rewrite touched nothing but the role tokens.
		back := regexp.MustCompile(`\b`+prefix+`_(owner|app|ro)\b`).ReplaceAll(got, []byte("ledger_${1}"))
		require.Truef(t, bytes.Equal(orig, back), "%s: rewrite changed more than the role names", key)
		// The identity prefix is a no-op.
		require.Truef(t, bytes.Equal(orig, RewriteRoleNames(orig, "ledger")), "%s: identity rewrite changed bytes", key)
	}
	require.Greater(t, totalTokens, 100, "sanity: the migrations reference the roles throughout")

	// The production source still serves the shared names.
	prodAgain, err := ledgerpg.NewMigrationSource()
	require.NoError(t, err)
	for key, b := range readAll(t, prodAgain) {
		require.Truef(t, bytes.Equal(original[key], b), "%s: production source output changed", key)
	}
}

func TestNewRoleRewritingMigrationSource_RejectsUnsafePrefix(t *testing.T) {
	for _, bad := range []string{"", "ledger", "ledger_test_", "Ledger", "x; DROP ROLE dev", `ledger_test_x"y`, "app"} {
		_, err := NewRoleRewritingMigrationSource(bad)
		require.Errorf(t, err, "prefix %q", bad)
	}
}
