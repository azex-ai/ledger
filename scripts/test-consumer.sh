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
    "errors"
    "github.com/azex-ai/ledger"
    "github.com/azex-ai/ledger/core"
    "github.com/azex-ai/ledger/presets"
    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/shopspring/decimal"
)

func configure(ctx context.Context, pool *pgxpool.Pool) error {
    svc, err := ledger.New(pool)
    if err != nil { return err }
    var _ core.TemplateBatchExecutor = svc.TemplateBatchExecutor()
    var _ core.Reserver = svc.Reserver()
    _ = func(requests []core.TemplateExecutionRequest) error {
        return svc.RunInTx(ctx, func(tx *ledger.Service) error {
            return tx.LockForTemplates(ctx, requests, "host-request:reserve")
        })
    }
    return presets.InstallTemplateBundle(ctx, svc.Classifications(), svc.JournalTypes(), svc.Templates(), presets.DepositBundle())
}

func main() {
    _ = ledger.NewIdempotencyKey("host-request")
    credits := core.Currency{Code: "CREDITS", Exponent: 6}
    for _, tc := range []struct{ source, quantity, rate, want string; exponent int32 }{
        {"USDC", "1", "1000", "1000", 6},
        {"INPUT_TOKEN", "10000", "0.002", "20", 0},
        {"ROSE", "3", "10", "30", 0},
    } {
        source := core.Currency{Code: tc.source, Exponent: tc.exponent}
        rate := core.FixedRate{SourceCode: tc.source, TargetCode: credits.Code,
            Rate: decimal.RequireFromString(tc.rate), Version: "host-v1", Rounding: core.RoundHalfUp}
        amount, err := rate.Convert(decimal.RequireFromString(tc.quantity), source, credits)
        if err != nil { panic(err) }
        if !amount.Equal(decimal.RequireFromString(tc.want)) { panic("incorrect configured conversion") }
        if _, err := rate.Convert(decimal.NewFromInt(1), credits, source); !errors.Is(err, core.ErrInvalidInput) {
            panic("reversed pair was accepted")
        }
    }
}
GO

go mod tidy
go build ./...
go run -mod=readonly ./...
go list -deps ./... > deps.txt
while IFS= read -r dependency; do
  case "$dependency" in
    github.com/testcontainers/*|github.com/moby/*|github.com/docker/*)
      echo "test-only dependency reached production imports: $dependency" >&2
      exit 1
      ;;
  esac
done < deps.txt
echo "External consumer: tidy/build and configured conversions pass; production imports exclude Docker/testcontainers."
