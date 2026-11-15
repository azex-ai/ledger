// Package s3test provides an in-process S3-compatible server for the
// anchors/r2 tests: gofakes3 over its in-memory backend, served by
// net/http/httptest. No container, no external service, no Docker -- every
// call to Fixture gets its own empty server, torn down by t.Cleanup.
//
// It is a SEPARATE module (its own go.mod) for the same reason
// internal/postgrestest's dependencies stay out of production imports:
// gofakes3 and its transitive graph are test-only, and keeping them out of
// anchors/r2's own go.mod means a consumer importing
// github.com/azex-ai/ledger/anchors/r2 (the production P6 anchor carrier)
// never gets them as a direct dependency of the module they import. See the
// root module's CLAUDE.md "go.work" gotcha for the precise SBOM/lockfile
// consequences of this split -- they apply here too.
//
// The boundary is plain strings: Fixture returns only an endpoint and a
// credential pair, so no gofakes3 type crosses back into the r2 test package.
//
// What this server does NOT implement is Object Lock (bucket lock
// configuration, retention, refusing to delete a retained version). That
// guarantee is a property of the real server, not of anchors/r2's code, so
// it is asserted against a real S3-compatible server by the `s3server`-tagged
// test in anchors/r2 (`make test-r2-s3server`, docs/TESTING.md), never against
// this fake.
package s3test

import (
	"net/http/httptest"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// The fake server accepts any credential; these only have to be non-empty so
// the SDK signs its requests the way it would against a real server.
const (
	accessKey = "s3test-access-key"
	secretKey = "s3test-secret-key" //nolint:gosec // in-process fake server, not a real secret
)

// Fixture starts an in-process S3-compatible server and returns its endpoint
// plus a credential pair for it. It skips only in -short mode, like every
// other integration fixture in this repository; there is no external
// dependency that could be unavailable.
func Fixture(t *testing.T) (endpoint, accessKeyID, secretAccessKey string) {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode: skipping S3-backed integration test")
	}
	server := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	t.Cleanup(server.Close)
	return server.URL, accessKey, secretKey
}
