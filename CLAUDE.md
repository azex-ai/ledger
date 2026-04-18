# azex-ai/ledger

Production-grade double-entry ledger engine for Go. Dual-mode: importable library or standalone HTTP service.

## Tech Stack

- Go 1.26+, chi v5, pgx v5, sqlc, shopspring/decimal
- PostgreSQL 17 (only supported DB)
- Next.js 16 + shadcn/ui + Tailwind v4 (web dashboard in `web/`)

## Architecture

Hexagonal: `core/` (pure domain) -> `postgres/` (adapter) -> `service/` (orchestration) -> `server/` (HTTP) -> `cmd/ledgerd/` (entry)

- `core/` — zero external dependencies. No net/http, pgx, slog, chi imports allowed.
- Interfaces defined in `core/interfaces.go`, consumer-side, -er suffix.
- Account dimensions: `(AccountHolder int64, CurrencyID int64, ClassificationID int64)`. Positive holder = user, negative = system counterpart.
- All amounts: `shopspring/decimal.Decimal` in Go, `NUMERIC(30,18)` in SQL, string in JSON.

## Key Commands

```bash
# Build
go build ./...

# Test (requires PostgreSQL — uses testcontainers, no mocks)
go test ./... -race -count=1

# sqlc (run from postgres/ directory)
cd postgres && sqlc generate

# Check sqlc drift
sqlc diff

# Lint
go vet ./...

# Docker (full stack)
docker compose up --build
```

## Workflow: Adding Features

```
1. SQL migration in postgres/sql/migrations/
2. Queries in postgres/sql/queries/*.sql -> cd postgres && sqlc generate
3. Domain types/logic in core/
4. Store adapter in postgres/
5. Service orchestration in service/ (if needed)
6. HTTP handler in server/handler_*.go + wire in server/routes.go
7. DI wiring in cmd/ledgerd/main.go
```

## Code Conventions

- Struct JSON tags: snake_case, all exported fields must have tags
- Error wrapping: `fmt.Errorf("module: action: %w", err)`
- Never discard errors (except in tests)
- Idempotency: every mutation requires an `idempotency_key` (UNIQUE index)
- Journal entries: append-only, corrections via reversal journal only
- Balance: `checkpoint.balance + SUM(entries WHERE id > checkpoint.last_entry_id)`
- Concurrency: `SELECT FOR UPDATE` on balance writes, advisory locks for reservations
- DB transactions: no external API calls inside a transaction

## Testing

- Integration tests use `testcontainers-go` with real PostgreSQL — no mocked DB.
- Test files: `postgres/*_test.go` for store tests, `service/*_test.go` for service tests.
- CI runs: `go vet`, `golangci-lint`, `go test -race`, `sqlc diff`, `go build`.

## File Layout Quick Reference

| Path | Purpose |
|------|---------|
| `core/types.go` | Currency, Classification, JournalType, Balance |
| `core/journal.go` | Journal, Entry, JournalInput + validation |
| `core/template.go` | EntryTemplate, Render() |
| `core/reserve.go` | Reservation state machine |
| `core/deposit.go` | Deposit state machine |
| `core/withdraw.go` | Withdrawal state machine |
| `core/checkpoint.go` | BalanceCheckpoint, RollupQueueItem, BalanceSnapshot |
| `core/interfaces.go` | All store interfaces |
| `postgres/sql/migrations/` | Schema migrations (embed.FS) |
| `postgres/sql/queries/` | sqlc query files |
| `postgres/sqlcgen/` | Generated code (do not edit) |
| `server/routes.go` | All endpoint definitions |
| `server/handler_*.go` | Handlers by resource |
| `service/worker.go` | Background job runner |
| `docs/plans/2026-04-17-design.md` | Full design document |
| `docs/api.md` | HTTP API reference |

## Gotchas

- `postgres/sqlcgen/` is generated — never edit manually, always `sqlc generate`.
- sqlc config is at `postgres/sqlc.yaml`, run sqlc from `postgres/` dir.
- Migrations use `golang-migrate/migrate/v4` with embedded FS.
- No Makefile yet — commands run directly.
- `web/` is a separate Next.js project with its own `CLAUDE.md`.
