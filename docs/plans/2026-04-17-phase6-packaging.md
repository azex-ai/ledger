# Phase 6: Example, Docker, Documentation

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Complete the project with a crypto deposit example, Docker Compose deployment, CI pipeline, and documentation. Make it ready for public release.

**Architecture:** Example demonstrates full flow. Docker Compose wires all components. README enables 3-step quickstart.

**Tech Stack:** Docker, GitHub Actions, golang-migrate

**Depends on:** Phase 1-5

**Can parallelize with:** Phase 5 (partially)

---

### Task 1: Crypto Deposit Example

**Files:**
- Create: `examples/crypto-deposit/main.go`
- Create: `examples/crypto-deposit/README.md`

**What it demonstrates:**
1. Connect to PostgreSQL, run migrations
2. Create currency (USDT), classifications (MainWallet, Custodial, Suspense, Pending, Fees)
3. Create journal type + entry template for deposit flow
4. Simulate EVM CREATE2 deposit:
   - InitDeposit (pending)
   - ConfirmingDeposit (simulated chain confirmation)
   - ConfirmDeposit (with actual amount, channel_ref = tx_hash)
5. Query balance — show MainWallet updated
6. Reserve → Settle flow (simulate a spend)
7. Print final balances + run reconciliation check

**Step 1: Write main.go** — self-contained, uses `ledger/core` + `ledger/postgres` as a library

**Step 2: Write README.md** — explains what the example does, how to run it

```bash
cd examples/crypto-deposit
DATABASE_URL=postgres://... go run main.go
```

**Commit:** `docs(examples): add crypto-deposit example with full EVM CREATE2 flow`

---

### Task 2: Docker Compose

**Files:**
- Create: `Dockerfile` (Go backend — multi-stage build)
- Create: `docker-compose.yml`
- Create: `.env.example`

**docker-compose.yml:**
```yaml
services:
  postgres:
    image: postgres:17
    environment:
      POSTGRES_DB: ledger
      POSTGRES_USER: ledger
      POSTGRES_PASSWORD: ledger
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data

  ledgerd:
    build: .
    depends_on:
      - postgres
    environment:
      DATABASE_URL: postgres://ledger:ledger@postgres:5432/ledger?sslmode=disable
      HTTP_PORT: "8080"
    ports:
      - "8080:8080"

  web:
    build: ./web
    depends_on:
      - ledgerd
    environment:
      NEXT_PUBLIC_API_URL: http://ledgerd:8080
    ports:
      - "3000:3000"

volumes:
  pgdata:
```

**Dockerfile (backend):**
```dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o ledgerd ./cmd/ledgerd

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/ledgerd /usr/local/bin/
ENTRYPOINT ["ledgerd"]
CMD ["serve"]
```

**Step 1: Test locally**

```bash
docker compose up --build
# Verify: http://localhost:8080/api/v1/system/health
# Verify: http://localhost:3000 (frontend)
```

**Commit:** `feat: add Docker Compose for one-command deployment`

---

### Task 3: GitHub Actions CI

**Files:**
- Create: `.github/workflows/ci.yml`

**Pipeline:**
```yaml
name: CI
on: [push, pull_request]
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.25' }
      - run: go vet ./...
      - uses: golangci/golangci-lint-action@v6

  test:
    runs-on: ubuntu-latest
    services:
      postgres:
        image: postgres:17
        env:
          POSTGRES_DB: ledger_test
          POSTGRES_USER: test
          POSTGRES_PASSWORD: test
        ports: ['5432:5432']
        options: >-
          --health-cmd pg_isready
          --health-interval 10s
          --health-timeout 5s
          --health-retries 5
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.25' }
      - run: go test ./... -race -count=1
        env:
          DATABASE_URL: postgres://test:test@localhost:5432/ledger_test?sslmode=disable

  sqlc-diff:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: sqlc-dev/setup-sqlc@v4
      - run: sqlc diff

  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.25' }
      - run: go build ./...
```

**Commit:** `ci: add GitHub Actions pipeline — lint, test, sqlc diff, build`

---

### Task 4: README

**Files:**
- Create: `README.md`

**Structure:**
```markdown
# azex-ai/ledger

> Production-grade double-entry ledger engine for Go.

## Features
(bullet list of core capabilities)

## Quick Start — As a Library
(3 steps: go get → NewEngine → PostJournal)

## Quick Start — As a Service
(docker compose up → open http://localhost:3000)

## Architecture
(link to docs/plans/design.md, package diagram)

## API Reference
(link to docs/api.md or inline summary)

## Examples
(link to examples/crypto-deposit/)

## Configuration
(env vars table)

## Contributing
(standard OSS contributing guide)

## License
MIT
```

**Commit:** `docs: add README with quickstart and architecture overview`

---

### Task 5: API Documentation

**Files:**
- Create: `docs/api.md`

Auto-generate from the routes defined in Phase 4. Include request/response examples for each endpoint.

**Commit:** `docs: add API reference documentation`

---

### Task 6: Final Integration Test + Tag

**Step 1: Run full test suite**

```bash
cd /Users/aaron/azex/ledger
go test ./... -race -count=1
cd web && npm run build
docker compose up --build -d
# Smoke test API + frontend
docker compose down
```

**Step 2: Tag release**

```bash
git tag v0.1.0
```

**Commit:** (no commit, just tag)
