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

# Local pre-push check for docs/openapi.yaml: catches YAML syntax errors and
# schema.ts drift before they reach CI. Mirrors the "Contract gate" step in
# .github/workflows/ledger-react.yml (npm run -w @azex/ledger-react
# codegen:check) exactly -- same command, just runnable locally. Requires
# `npm ci` to have been run in web/ at least once.
openapi-check:
	cd web && npm run -w @azex/ledger-react codegen:check
