# Testing the checkout

Run `make test` for the root module's uncached race suite. Integration tests use
real PostgreSQL 17. The normal fixture shares a server but creates and cleans up
a separate database for each test. Docker is the default; `DATABASE_URL` can
point to an externally provided **test** server instead.

## Destructive migration roundtrip

`TestMigrations_FullDownChainAndReapply` executes Up → full Down → Up, including
dropping cluster-wide ledger roles. A separate database on the normal test
server is insufficient. `SetupIsolatedRawDB` therefore starts a separate temporary
PostgreSQL 17 container and terminates it with `t.Cleanup`, even when ordinary
tests use an external `DATABASE_URL`.

To run the complete suite without Docker, provide **two independently initialized
PostgreSQL clusters**:

```sh
DATABASE_URL=postgres://test_admin:password@127.0.0.1:55432/postgres?sslmode=disable \
LEDGER_TEST_ISOLATED_DATABASE_URL=postgres://test_admin:password@127.0.0.1:55433/postgres?sslmode=disable \
make test
```

These are test-only inputs, never production connection strings. The isolated
cluster must be dedicated to this destructive test and must not be used by a
concurrent test run. Both test administrators need database creation/cleanup
privileges; the full down chain is a privileged DBA operation and should use a
test superuser. The fixture must also be able to read
`pg_control_system().system_identifier` on both clusters. Matching cluster IDs,
unreadable identity or unavailable infrastructure fail the test. Different URL
strings, database names, host aliases or ports alone do not establish isolation.
Physical clones retaining the same system identifier are conservatively rejected;
use separate `initdb` clusters. The identity is cluster-wide, as documented by
[PostgreSQL 17](https://www.postgresql.org/docs/17/functions-info.html#FUNCTIONS-CONTROLDATA).

Container startup uses the existing cross-process flock and the installed
testcontainers PostgreSQL module's `BasicWaitStrategies`: final readiness logs
and the mapped listening port. Startup and cleanup have explicit time limits.
The ordinary fixture's canary row, table owner and role OID remain live throughout
the isolated roundtrip and must be unchanged afterwards.

Only `make test-short` / `go test -short` explicitly skips PostgreSQL tests.
Missing Docker or a bad isolation configuration must never turn a full suite
into a successful skip. Neither global package serialization nor retries are a
substitute for isolation.

## Modules and consumers

The root `./...` pattern does not cross Go module boundaries. Verify all four:

```sh
for module in . chains/evm anchors/r2 anchors/r2/internal/miniotest; do
  (cd "$module" && go build ./... && go vet ./... && go test -race -timeout 15m -count=1 ./...)
done
make test-consumer
```

The R2 tests additionally start MinIO containers. EVM end-to-end tests need
Foundry's `anvil`; `make test-e2e` opts into their build tag. The Linux CI workflow
also runs three bounded 30-second fuzz jobs. Local unit or package checks do not
stand in for those jobs.

For the web workspace, install independent dependencies with `npm ci` in `web/`,
build `@azex/ledger-react` before its tests, run its typecheck and codegen check,
and run the host tests with `npx vitest run --config test/vitest.config.mts`.
`npm run build` checks the actual Next.js host. A packed SDK consumer check should
install the tarball in a temporary directory outside the checkout, without the
optional HeroUI peer, and verify the six non-Hero entrypoints; do not publish a
package merely to validate consumption.
