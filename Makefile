.PHONY: build test test-short vet lint sqlc sqlc-diff docker openapi-check

build:
	go build ./...

test:
	go test -race -timeout 5m ./...

test-short:
	go test -short -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run

sqlc:
	cd postgres && sqlc generate

sqlc-diff:
	cd postgres && sqlc diff

docker:
	docker compose up --build

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
