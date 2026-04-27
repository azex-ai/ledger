.PHONY: build test test-short vet lint sqlc sqlc-diff docker

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
