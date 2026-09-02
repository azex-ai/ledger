.PHONY: build test test-short test-submodules test-e2e vet lint sqlc sqlc-diff db openapi-check

build:
	go build ./...

# -count=1 disables the test result cache (F-m5, 2026-09-02 audit): without
# it, `go test` can print a fully green run without touching Postgres at
# all -- a cached PASS from a previous invocation, not evidence the current
# tree passes. ci.yml's test job has always passed -count=1; this target
# didn't, so it was a weaker gate than CI under the same name. `./...` also
# does not cross module boundaries (go.work notwithstanding) -- see
# test-submodules for chains/evm and anchors/r2, which this target does not
# touch, same as CI's separate steps for them.
test:
	go test -race -timeout 15m -count=1 ./...

test-short:
	go test -short -race -count=1 ./...

# chains/evm and anchors/r2 are separate Go modules (kept out of the root
# module's dependency graph deliberately -- see CLAUDE.md's Gotchas); `go
# test ./...` from the repo root never sees them. Mirrors ci.yml's test job.
test-submodules:
	cd chains/evm && go test -race -timeout 15m -count=1 ./...
	cd anchors/r2 && go test -race -timeout 15m -count=1 ./...

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
db:
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
