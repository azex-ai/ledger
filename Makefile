.PHONY: build test test-clean-db test-short test-submodules test-r2-worm test-e2e test-consumer vet lint sqlc sqlc-diff db db-docker openapi-check

build:
	go build ./...

# Three fresh external hosts: root, chains/evm, anchors/r2. Candidate-source
# replacements are explicit; see docs/CONSUMING.md for the release boundary.
test-consumer:
	bash scripts/test-consumer.sh

# -count=1 disables the test result cache (F-m5, 2026-09-02 audit): without
# it, `go test` can print a fully green run without touching Postgres at
# all -- a cached PASS from a previous invocation, not evidence the current
# tree passes. ci.yml's test job has always passed -count=1; this target
# didn't, so it was a weaker gate than CI under the same name. `./...` also
# does not cross module boundaries (go.work notwithstanding) -- see
# test-submodules for chains/evm and anchors/r2, which this target does not
# touch, same as CI's separate steps for them.
# PostgreSQL: one server, no Docker. The integration tests connect to
# DATABASE_URL, or by default to the shared local instance
# postgres://dev:dev@127.0.0.1:5432/postgres (internal/pgtestenv), and isolate
# by database name -- every test creates and drops its own -- and, for the
# destructive full-migration roundtrip, by role name (docs/TESTING.md). An
# unreachable server FAILS the run (F-9, 2026-09-03 independent review: a
# suite that did not execute is not a suite that passed); `make test-short` is
# the explicit way to run without one.
# pgtestclean runs afterwards, whatever go test returned. Unless another
# ledger test process still holds the run lock (then that run cleans up), it
# drops leftover ledger test databases/roles and the shared
# ledger_owner/ledger_app/ledger_ro roles once nothing on the server depends
# on them.
# The recipe exits with go test's status, or pgtestclean's if go test passed.
test:
	status=0; go test -race -timeout 15m -count=1 ./... || status=$$?; \
	go run ./internal/pgtestenv/pgtestclean || { [ $$status -ne 0 ] || status=1; }; \
	exit $$status

# Removes leftover ledger test databases/roles from the test server without
# running the suite (e.g. after a killed run or a direct `go test`).
test-clean-db:
	go run ./internal/pgtestenv/pgtestclean

test-short:
	go test -short -race -count=1 ./...

# chains/evm and anchors/r2 are separate Go modules (kept out of the root
# module's dependency graph deliberately -- see CLAUDE.md's Gotchas); `go
# test ./...` from the repo root never sees them. Mirrors ci.yml's test job.
test-submodules:
	cd chains/evm && go test -race -timeout 15m -count=1 ./...
	cd anchors/r2 && go test -race -timeout 15m -count=1 ./...

# anchors/r2's Object Lock (WORM) assertion, behind `//go:build s3worm`: it
# needs a REAL S3-compatible server with Object Lock (MinIO, or R2), which the
# in-process fake every other r2 test uses does not implement.
# LEDGER_TEST_S3_ENDPOINT / _ACCESS_KEY_ID / _SECRET_ACCESS_KEY select the
# server (default: MinIO on http://127.0.0.1:9000, minioadmin/minioadmin); an
# unreachable server fails the run. docs/TESTING.md.
test-r2-worm:
	cd anchors/r2 && go test -tags s3worm -race -timeout 5m -count=1 -run TestAnchor_ObjectLock ./...

# chains/evm/e2e_test.go and e2e_artifacts.go carry `//go:build e2e` and are
# excluded from every other target above -- `go build`/`go vet`/`go test`
# without `-tags e2e` do not even compile them (F-M5, 2026-09-02 audit: the
# files sat uncompiled long enough that nothing would have noticed a broken
# signature). Requires anvil (foundry) on PATH; skips itself otherwise.
test-e2e:
	cd chains/evm && go test -tags e2e -race -timeout 5m -count=1 -run TestE2E ./...

vet:
	go vet ./...

lint:
	golangci-lint run

sqlc:
	cd postgres && sqlc generate

sqlc-diff:
	cd postgres && sqlc diff

# Local development database only -- this repository ships no server.
# Creates (idempotently) the `ledger` database and the ledger_owner /
# ledger_app / ledger_ro roles on the machine's shared PostgreSQL
# (127.0.0.1:5432, or LEDGER_DEV_ADMIN_URL); no container. See
# scripts/dev-db.sh.
db:
	bash scripts/dev-db.sh

# The container alternative to `make db`: docker-compose.yml's PostgreSQL.
# Explicit opt-in only; it listens on 5432 too, so stop the shared instance
# first.
db-docker:
	docker compose up -d postgres

# Local check for docs/openapi.yaml: catches YAML syntax errors and
# schema.ts drift. Mirrors the "Contract gate" step in
# .github/workflows/ledger-react.yml (npm run -w @azex/ledger-react
# codegen:check) exactly -- same command, just runnable locally.
#
# NOTE (26-08-23): CI runs this same check on push/PR, but whether a red
# result there actually blocks a merge depends on this repo's GitHub branch
# protection settings, which are configuration outside this codebase and
# unverifiable from here. A live instance of this exact drift (schema.ts
# missing the /dev/credits path added in 4eb1d9b) sat on main despite
# ledger-react.yml having run against it -- so treat CI's codegen:check as
# reporting, not guaranteed-blocking, until branch protection is confirmed.
# `make openapi-check` is therefore the one gate an author can rely on before
# pushing. Requires `npm ci` to have been run in web/ at least once.
openapi-check:
	cd web && npm run -w @azex/ledger-react codegen:check
