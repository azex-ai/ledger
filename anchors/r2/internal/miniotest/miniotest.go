// Package miniotest provides a MinIO (S3-compatible) testcontainer fixture for
// the anchors/r2 tests. It is a SEPARATE module (its own go.mod) for the same
// reason internal/postgrestest is: testcontainers-go/modules/minio pulls in the
// Docker SDK, moby, and gopsutil, and keeping them out of anchors/r2's own
// go.mod means a consumer importing github.com/azex-ai/ledger/anchors/r2 (the
// production P6 anchor carrier) never gets those as a direct dependency of the
// module they import. See the root module's CLAUDE.md "go.work" gotcha for the
// precise SBOM/lockfile consequences of this split — they apply here too.
//
// The boundary is deliberately testcontainers-free at its surface: Fixture
// returns only plain strings, so no testcontainers type crosses back into the
// r2 test package and pulls the dependency along.
package miniotest

import (
	"context"
	"strings"
	"testing"

	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
)

// Fixture starts a real MinIO container and returns its S3 endpoint plus the
// access key / secret to reach it. It skips (not fails) in -short mode or when
// no Docker daemon is reachable, and terminates the container via t.Cleanup.
func Fixture(t *testing.T) (endpoint, accessKey, secret string) {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode: skipping MinIO-backed integration test")
	}

	ctx := context.Background()
	container, err := tcminio.Run(ctx, "minio/minio:RELEASE.2024-01-16T16-07-38Z")
	if err != nil {
		if strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
			t.Skip("Docker daemon not running, skipping integration test")
		}
		t.Fatalf("start minio container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate minio container: %v", err)
		}
	})

	endpoint, err = container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("minio connection string: %v", err)
	}
	return "http://" + endpoint, container.Username, container.Password
}
