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

// minioImage is the server the fixture runs. MinIO removed minio/minio from
// Docker Hub in September 2026 (and Quay stopped serving anonymous pulls
// shortly after), so the upstream tag this fixture pinned since its creation
// became "pull access denied" on every CI run. pgsty/minio is a community
// build of the archived MinIO source with the same image layout as upstream
// (docker-entrypoint.sh, `server /data`, root user), which is what the
// testcontainers minio module drives; the Chainguard build runs as uid 65532
// and only ships a floating tag, and the Bitnami legacy image has its own
// entrypoint. Pinned to a dated release tag, never `latest`, so the next
// registry change breaks here deliberately rather than silently.
//
// The tag is also pinned by digest: a tag is mutable and the image comes
// from a community organization, so the digest is what fixes the bytes CI
// runs. It is the multi-arch OCI *index* digest (one value valid on amd64
// and arm64), not a per-platform manifest digest. To re-derive it when
// moving to a new release tag:
//
//	docker buildx imagetools inspect pgsty/minio:<tag>
//
// and copy the top-level "Digest:" line (MediaType
// application/vnd.oci.image.index.v1+json), never one of the per-platform
// entries listed under "Manifests:".
const minioImage = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z@sha256:b6bfe7239bfc83fb90d31612d9704d86039dd714f7904b3f1ad68f211e602372"

// Fixture starts a real MinIO container and returns its S3 endpoint plus the
// access key / secret to reach it. It skips (not fails) in -short mode or when
// no Docker daemon is reachable, and terminates the container via t.Cleanup.
func Fixture(t *testing.T) (endpoint, accessKey, secret string) {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode: skipping MinIO-backed integration test")
	}

	ctx := context.Background()
	container, err := tcminio.Run(ctx, minioImage)
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
