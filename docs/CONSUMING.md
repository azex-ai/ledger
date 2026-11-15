# Consuming the Go modules

The root library and the optional EVM and R2 adapters are separate Go modules.
`make test-consumer` checks the current checkout from three fresh external host
modules. It validates candidate source integration; it does not validate a
published tag, create a release, or promise that remote `go get` will succeed.

## Run the candidate gate

Use the Go version declared in the root `go.mod`, Bash, and access to the
dependencies' module proxy or a populated Go module cache:

```sh
make test-consumer
# Equivalent, also callable by absolute path from another directory:
bash scripts/test-consumer.sh
```

The script creates separate `root`, `evm`, and `r2` hosts under a new directory
in `${TMPDIR:-/tmp}`. It refuses a temporary location inside this checkout,
sets `GOWORK=off`, and generates each host's own `go.mod` and `go.sum`. It runs
`go mod tidy`, `go build -mod=readonly ./...`, `go run -mod=readonly ./...`, and
`go list -mod=readonly -deps ./...` in each host. Failure stops the gate and
returns nonzero; an exit trap removes only that invocation's temporary directory.
Normal Go dependency and build caches remain available for reuse.

| Host | Imported public surface | Explicit candidate replacements |
| --- | --- | --- |
| Root | `ledger`, `core`, `presets`; facade wiring and three fixed conversion checks | `github.com/azex-ai/ledger` → checkout root |
| EVM | `chains/evm` constructors wired to `core.ChainReader`, `core.ChainScanner`, `core.Sweeper` | Root, plus `github.com/azex-ai/ledger/chains/evm` → `chains/evm` |
| R2 | `anchors/r2.New` wired to `core.Anchor` | Root, plus `github.com/azex-ai/ledger/anchors/r2` → `anchors/r2`, and its test fixture module → `anchors/r2/internal/miniotest` |

The script prints these host `go.mod` files before resolution. Its `v0.0.0`
requirements are local placeholders paired with explicit replacements, not
published version claims. The source probes live in `scripts/consumer-*.go.txt`;
each is copied to its host as `main.go`. Optional adapter wiring is compiled
without invoking it. The root executable retains the USDC / INPUT_TOKEN / ROSE
to CREDITS conversion checks. No PostgreSQL, RPC node, signer, R2 credentials,
MinIO, or Docker daemon is needed, and no financial operation is submitted.

The production package graph must exclude `internal/miniotest`,
`internal/postgrestest`, testcontainers, Docker, and moby. The root graph also
excludes the EVM / R2 adapters and their Ethereum / AWS SDK packages. This is a
package-import assertion: test-only modules may still appear in `go.mod`,
`go.sum`, or downloaded cache entries after tidy.

`make test-consumer` is called by the build job in
`.github/workflows/go-verify.yml`, the reusable gate shared by CI and the Go
release workflow. Repository builds and adapter tests still run separately;
the consumer gate checks an external host's dependency resolution and compilation.

## Why R2 needs an explicit fixture replacement

With `GOWORK=off`, the host's `go.mod` controls replacement resolution;
replacements in dependency modules do not apply. `go mod tidy` also loads
imports from dependency tests, so R2's test import of `internal/miniotest`
must resolve even though it is absent from the production package graph.
These are documented Go behaviors, not custom loader rules. See the official
[replacement rules](https://go.dev/ref/mod#go-mod-file-replace),
[tidy behavior](https://go.dev/ref/mod#go-mod-tidy), and
[GOWORK setting](https://go.dev/ref/mod#environment-variables).

The checked-in EVM and R2 modules currently require the root at a zero
pseudo-version; R2 also requires its MinIO fixture at a zero pseudo-version.
Their local replacements support repository development. This gate supplies
the necessary replacements explicitly in each new host, including R2's fixture,
so a workspace cannot conceal those requirements. It does not remove the
separate release constraints already described in the README's anchor section.

## Remote consumption is a separate check

A host consuming published modules must pin actual downloadable tags or commit
revisions and verify them in a fresh module with `GOWORK=off` and **no local
replacements**, then run tidy and build. Nested modules also need resolvable
root and test dependencies. Passing the candidate gate alone is insufficient
evidence for that claim. This iteration neither publishes tags nor checks the
current remote availability of the optional modules; release validation must
record the exact revisions and proxy used when it is performed.
