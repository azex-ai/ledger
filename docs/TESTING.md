# Testing the checkout

Run `make test` for the root module's uncached race suite. Integration tests use
real PostgreSQL — no mocked DB — on **one server**, with no Docker,
testcontainers or second cluster:

- `DATABASE_URL` unset: the shared local instance
  `postgres://dev:dev@127.0.0.1:5432/postgres?sslmode=disable`
  (`internal/pgtestenv.DefaultDatabaseURL`; Homebrew `postgresql@18`).
- `DATABASE_URL` set: that **test** server instead (CI uses a service container).

The connecting user must be able to `CREATE DATABASE`, `CREATE ROLE` and run the
migrations' privileged statements; use a test superuser. An unreachable server
fails the run. Only `make test-short` / `go test -short` explicitly skips
PostgreSQL tests; neither a missing server nor a missing Docker daemon may turn
a full suite into a successful skip.

## Isolation on a shared server

- **Databases.** Every test creates its own database
  `ledger_test_<pid>_<n>` and drops it in `t.Cleanup`. The pid keeps names
  unique across the package binaries of one `go test ./...` and across
  concurrent runs from different worktrees on the same server.
- **Migrated template.** Migrations run once per test process, not per
  test: the first `postgrestest.SetupDB` migrates `ledger_test_<pid>_0` with
  `postgres.Migrate`, and every `SetupDB` copies it (`CREATE DATABASE ...
  TEMPLATE`), then re-applies the state migrations set on the database object
  itself, which a copy does not inherit (migration 030's `REVOKE TEMPORARY`;
  pinned by `TestTemplateCopyMatchesTheTemplate`). Per-test `Migrate` made
  every test queue on the cluster-wide migration lock behind every other test
  process on the server. The template lives as long as its process; the
  cleanup below removes it, so a direct `go test` leaves one per package
  binary until `make test-clean-db`. `SetupRawDB` still hands out an empty,
  unmigrated database.
- **Shared roles.** The production migrations create the cluster-wide roles
  `ledger_owner` / `ledger_app` / `ledger_ro` idempotently, exactly as a
  second ledger database on a production cluster would. Ordinary tests share
  them. Tests never change the shared roles' attributes (the hardening test
  uses private role names), and a test that logs in as `ledger_app` /
  `ledger_ro` sets only `postgrestest.SharedRolePassword`, so concurrent
  processes cannot invalidate each other's credentials. `make db`
  (`scripts/dev-db.sh`, the local development database `ledger` on the same
  server) sets that same password on the three roles, so it and a running
  suite never break each other's logins. Until `ledger` is migrated nothing
  depends on the roles, and the cleanup below may drop them; re-run
  `make db` then.
- **Destructive roundtrip.** `TestMigrations_FullDownChainAndReapply` runs
  Up → full Down → Up, and the baseline's down script drops its roles. It runs
  on the same server through `postgrestest.SetupRoleIsolatedRawDB` and
  `postgrestest.NewRoleRewritingMigrationSource`: the embedded migrations are
  served with `ledger_owner` / `ledger_app` / `ledger_ro` rewritten to private
  `ledger_test_<pid>_<n>_{owner,app,ro}`, so the down chain drops only this
  test's roles. Production never goes through the rewrite:
  `postgres.NewMigrationSource` and `postgres.Migrate` serve the embedded files
  verbatim. `TestRoleRewritingSource_RenamesOnlyTheThreeRoles` pins that the
  rewrite renames every role token and nothing else.
  The test keeps an ordinary fixture (shared roles) alive throughout and
  asserts that its canary row, table owner and the shared `ledger_owner` OID
  are unchanged, that the private roles exist after each Up and are gone after
  the full Down, and that no roundtrip object is owned by or granted to a
  shared role.
- **Run lock and cleanup.** Every test process holds advisory lock
  `pgtestenv.RunLockKey` in shared mode (on the server's `postgres` database)
  from its first PostgreSQL test until it exits. `make test` then runs
  `go run ./internal/pgtestenv/pgtestclean`, whatever `go test` returned. It
  takes the cluster migration lock and the run lock exclusively, without
  waiting, and does nothing if either is busy — some ledger test process (any
  run, any worktree, any machine on that server) is still alive, and that
  run's own cleanup finishes the job. Holding both, it drops leftover
  `ledger_test_<pid>_<n>` databases, the suite's own test roles
  (`ledger_test_<pid>_<n>_{owner,app,ro}`, `ledger_noadmin_<n>`,
  `ledger_bootstrap_<n>`), and the shared `ledger_owner` / `ledger_app` /
  `ledger_ro` only when nothing in any database on the server depends on them
  (another project that installed the ledger into its own database keeps
  them). It touches no other names. A direct `go test` does not clean up;
  `make test-clean-db` runs the cleanup alone.

To check the server afterwards:

```sh
psql -h 127.0.0.1 -U dev -d postgres -c \
  "SELECT datname FROM pg_database WHERE datname LIKE 'ledger\_test\_%';" -c \
  "SELECT rolname FROM pg_roles WHERE rolname LIKE 'ledger\_%';"
```

Neither global package serialization nor retries are a substitute for
isolation.

## Modules and consumers

The root `./...` pattern does not cross Go module boundaries. Verify all four:

```sh
for module in . chains/evm anchors/r2 anchors/r2/internal/s3test; do
  (cd "$module" && go build ./... && go vet ./... && go test -race -timeout 15m -count=1 ./...)
done
make test-consumer
```

The R2 tests run against an in-process fake S3 server (`internal/s3test`,
gofakes3) -- no container, nothing to start. The one R2 assertion the fake
cannot serve, Object Lock refusing to delete a retained version, is behind
`-tags s3worm` and run by `make test-r2-worm` against a real S3-compatible
server: `LEDGER_TEST_S3_ENDPOINT`, `LEDGER_TEST_S3_ACCESS_KEY_ID` and
`LEDGER_TEST_S3_SECRET_ACCESS_KEY`, by default a native MinIO on this machine
(`http://127.0.0.1:9000`, `minioadmin` / `minioadmin`; CI starts one in a
container). An unreachable server fails that target. It reuses one bucket,
`ledger-test-worm`, with a random key prefix per run: the objects carry a
one-day COMPLIANCE retention, so neither they nor a bucket holding them can be
deleted before it expires -- a bucket per run would leave one behind every
time. EVM end-to-end tests need
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
