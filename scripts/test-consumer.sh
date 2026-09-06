#!/usr/bin/env bash
# Build the root library from a fresh host module, outside this go.work.
set -euo pipefail

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
consumer_dir=$(mktemp -d "${TMPDIR:-/tmp}/ledger-consumer.XXXXXX")
trap 'rm -rf "$consumer_dir"' EXIT
export GOWORK=off
go_version=$(cd "$repo_dir" && go list -m -f '{{.GoVersion}}')
cd "$consumer_dir"
go mod init ledger-consumer-check
go mod edit "-go=$go_version"
go mod edit -require=github.com/azex-ai/ledger@v0.0.0
go mod edit "-replace=github.com/azex-ai/ledger=$repo_dir"

cat > main.go <<'GO'
package main

import (
    "context"
    "github.com/azex-ai/ledger"
    "github.com/azex-ai/ledger/core"
    "github.com/azex-ai/ledger/presets"
    "github.com/jackc/pgx/v5/pgxpool"
)

func configure(ctx context.Context, pool *pgxpool.Pool) error {
    svc, err := ledger.New(pool)
    if err != nil { return err }
    var _ core.TemplateBatchExecutor = svc.TemplateBatchExecutor()
    var _ core.Reserver = svc.Reserver()
    return presets.InstallTemplateBundle(ctx, svc.Classifications(), svc.JournalTypes(), svc.Templates(), presets.DepositBundle())
}

func main() { _ = ledger.NewIdempotencyKey("host-request") }
GO

go mod tidy
go build ./...
go list -deps ./... > deps.txt
while IFS= read -r dependency; do
  case "$dependency" in
    github.com/testcontainers/*|github.com/moby/*|github.com/docker/*)
      echo "test-only dependency reached production imports: $dependency" >&2
      exit 1
      ;;
  esac
done < deps.txt
echo "External consumer: tidy/build pass; production imports exclude Docker/testcontainers."
