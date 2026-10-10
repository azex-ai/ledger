#!/usr/bin/env bash
# Candidate-source checks, not proof that any module version is published.
set -euo pipefail
IFS=$'\n\t'

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
consumer_dir=$(mktemp -d "${TMPDIR:-/tmp}/ledger-consumer.XXXXXX")
trap 'rm -rf -- "$consumer_dir"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
consumer_dir=$(CDPATH= cd -- "$consumer_dir" && pwd -P)
case "$consumer_dir" in
  "$repo_dir"|"$repo_dir"/*)
    echo "Consumer hosts must be outside the repository; set TMPDIR accordingly." >&2
    exit 1
    ;;
esac
export GOWORK=off
go_version=$(cd "$repo_dir" && go list -m -f '{{.GoVersion}}')
module=github.com/azex-ai/ledger

# Each host owns a new go.mod/go.sum. No workspace or dependency go.mod
# replacement can silently supply a sibling module to it.
for consumer in root evm r2; do
  mkdir "$consumer_dir/$consumer"
  cd "$consumer_dir/$consumer"
  echo "Consumer $consumer: GOWORK=off, fresh host at $PWD"
  go mod init "example/ledger-consumer-$consumer"
  go mod edit "-go=$go_version" "-require=$module@v0.0.0" "-replace=$module=$repo_dir"
  case "$consumer" in
    evm)
      go mod edit "-require=$module/chains/evm@v0.0.0" "-replace=$module/chains/evm=$repo_dir/chains/evm"
      ;;
    r2)
      go mod edit "-require=$module/anchors/r2@v0.0.0" "-replace=$module/anchors/r2=$repo_dir/anchors/r2"
      # tidy loads dependency tests too. This is an explicit candidate-only
      # replacement, never a production import or a claim of publishability.
      go mod edit "-replace=$module/anchors/r2/internal/miniotest=$repo_dir/anchors/r2/internal/miniotest"
      ;;
  esac
  cp "$repo_dir/scripts/consumer-$consumer.go.txt" main.go
  cat go.mod
  go mod tidy
  go build -mod=readonly ./...
  go run -mod=readonly ./...
  go list -mod=readonly -deps ./... > deps.txt
  while IFS= read -r dependency; do
    case "$dependency" in
      */internal/miniotest|*/internal/miniotest/*|*/internal/postgrestest|*/internal/postgrestest/*|github.com/testcontainers/*|github.com/moby/*|github.com/docker/*)
        echo "Consumer $consumer: test-only dependency reached production imports: $dependency" >&2
        exit 1
        ;;
    esac
    # The root host must not pull optional adapters or their SDKs.
    if [[ "$consumer" == root ]]; then
      case "$dependency" in
        "$module/chains/evm"|"$module/chains/evm"/*|"$module/anchors/r2"|"$module/anchors/r2"/*|github.com/ethereum/go-ethereum|github.com/ethereum/go-ethereum/*|github.com/aws/aws-sdk-go-v2|github.com/aws/aws-sdk-go-v2/*)
          echo "Root consumer unexpectedly imports optional dependency: $dependency" >&2
          exit 1
          ;;
      esac
    fi
  done < deps.txt
  echo "Consumer $consumer: tidy/build/run and production import checks passed."
done
echo "All three candidate consumers passed; remote release availability was not checked."
