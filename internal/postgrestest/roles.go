package postgrestest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4/source"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	ledgerpg "github.com/azex-ai/ledger/postgres"
)

// defaultRoleToken matches exactly the three cluster-wide role names the
// production migrations are written against, as whole identifiers: it does
// not touch ledger_ prefixed functions, tables or other identifiers.
var defaultRoleToken = regexp.MustCompile(`\bledger_(owner|app|ro)\b`)

// rolePrefixShape is what RewriteRoleNames accepts as a prefix: a plain,
// unquoted-safe lower-case identifier stem.
var rolePrefixShape = regexp.MustCompile(`^[a-z][a-z0-9_]{0,50}$`)

// RoleNames are the three role names a migration run is written against.
type RoleNames struct {
	Owner, App, RO string
}

// RoleNamesFor returns the names RewriteRoleNames(_, prefix) substitutes for
// ledger_owner / ledger_app / ledger_ro.
func RoleNamesFor(prefix string) RoleNames {
	return RoleNames{Owner: prefix + "_owner", App: prefix + "_app", RO: prefix + "_ro"}
}

// RewriteRoleNames replaces every ledger_owner / ledger_app / ledger_ro token
// in a migration script with <prefix>_owner / _app / _ro. With prefix
// "ledger" it is the identity. Test-only: production migrations are never
// rewritten (postgres.NewMigrationSource serves the embedded files verbatim).
func RewriteRoleNames(sql []byte, prefix string) []byte {
	return defaultRoleToken.ReplaceAll(sql, []byte(prefix+"_${1}"))
}

// roleRewritingSource serves the embedded production migrations with the
// role names rewritten. Everything except ReadUp / ReadDown passes through.
type roleRewritingSource struct {
	source.Driver
	prefix string
}

func (s roleRewritingSource) ReadUp(version uint) (io.ReadCloser, string, error) {
	r, id, err := s.Driver.ReadUp(version)
	if err != nil {
		return r, id, err
	}
	return s.rewrite(r, id)
}

func (s roleRewritingSource) ReadDown(version uint) (io.ReadCloser, string, error) {
	r, id, err := s.Driver.ReadDown(version)
	if err != nil {
		return r, id, err
	}
	return s.rewrite(r, id)
}

func (s roleRewritingSource) rewrite(r io.ReadCloser, id string) (io.ReadCloser, string, error) {
	raw, readErr := io.ReadAll(r)
	closeErr := r.Close()
	if readErr != nil {
		return nil, id, fmt.Errorf("postgrestest: read migration %s: %w", id, readErr)
	}
	if closeErr != nil {
		return nil, id, fmt.Errorf("postgrestest: close migration %s: %w", id, closeErr)
	}
	return io.NopCloser(bytes.NewReader(RewriteRoleNames(raw, s.prefix))), id, nil
}

// NewRoleRewritingMigrationSource returns the production migration source
// with ledger_owner / ledger_app / ledger_ro renamed to <prefix>_owner /
// _app / _ro. For tests only.
func NewRoleRewritingMigrationSource(prefix string) (source.Driver, error) {
	if !rolePrefixShape.MatchString(prefix) {
		return nil, fmt.Errorf("postgrestest: role prefix %q must match %s", prefix, rolePrefixShape)
	}
	src, err := ledgerpg.NewMigrationSource()
	if err != nil {
		return nil, err
	}
	return roleRewritingSource{Driver: src, prefix: prefix}, nil
}

// RoleIsolatedDB is an unmigrated database plus the private role names its
// migrations must run under.
type RoleIsolatedDB struct {
	// ConnString is a postgres:// URL for the fresh database.
	ConnString string
	// Prefix is the role-name prefix; pass it to
	// NewRoleRewritingMigrationSource.
	Prefix string
	// Roles are the names that replace ledger_owner / ledger_app / ledger_ro.
	Roles RoleNames
}

// SetupRoleIsolatedRawDB creates an unmigrated database for tests that drop
// cluster-wide roles -- the full migration down chain -- on the SAME server as
// every other test. Isolation comes from names, not from a second cluster:
// the caller must migrate it through NewRoleRewritingMigrationSource(Prefix),
// so the roles it creates and later drops are this test's own
// (ledger_test_<pid>_<n>_{owner,app,ro}) and the shared ledger_owner /
// ledger_app / ledger_ro that ordinary tests (and concurrent runs) depend on
// are never named. Cleanup drops the database and then whichever of the three
// private roles still exist.
func SetupRoleIsolatedRawDB(t testing.TB) RoleIsolatedDB {
	t.Helper()
	base := baseConnection(t)
	name := nextDatabaseName()
	roles := RoleNamesFor(name)

	// Registered before the database exists so that, cleanups being LIFO, it
	// runs after the database (which holds the roles' dependencies) is gone.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		admin, err := pgxpool.New(ctx, base)
		if err != nil {
			t.Errorf("connect for role cleanup: %v", err)
			return
		}
		defer admin.Close()
		for _, role := range []string{roles.Owner, roles.App, roles.RO} {
			if _, err := admin.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Errorf("drop test role %s: %v", role, err)
			}
		}
	})
	connStr := createNamedDatabase(t, base, name)
	return RoleIsolatedDB{ConnString: connStr, Prefix: name, Roles: roles}
}
