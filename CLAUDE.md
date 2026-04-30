# azex-ai/ledger

Production-grade double-entry ledger engine for Go. Classification-driven architecture — deposit/withdrawal are preset configurations, not hardcoded types. Dual-mode: importable library or standalone HTTP service.

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
- **No NULL**: All DB columns NOT NULL with meaningful defaults (0, '', 'epoch', '{}'). **Exceptions** (FK target columns where 0 means "absent" must be nullable so PostgreSQL can enforce referential integrity): `journals.reversal_of`, `bookings.journal_id`, `bookings.reservation_id`, `events.journal_id`.
- **Single-direction data flow**: Ledger never calls external systems. Commands in, events out.
- **Event-Journal atomicity**: Events and journals are written in the same transaction. Events are the "reason" a journal exists.

### Core Concepts

- **Classification (科目)** — the primary entity. Each classification can have a Lifecycle (state machine).
- **Booking** — an instance of a classification's lifecycle (replaces v1 Deposit/Withdrawal). "Book a deposit", "book a withdrawal" is standard banking terminology.
- **Event** — atomic record of state transitions, written with the booking update.
- **Journal** — double-entry accounting record, linked to the triggering event via `event_id`.
- **Reservation** — cross-classification fund locking mechanism.
- **Presets** — deposit/withdrawal are pre-built classification lifecycle configs in `presets/`.
- **Channel Adapter** — inbound webhook parsing for external systems (in `channel/`).

## Key Commands

```bash
# Build
go build ./...

# Test (requires PostgreSQL — uses testcontainers, no mocks)
go test ./... -race -count=1

# Unit tests only (no DB needed)
go test ./core/... ./presets/... ./channel/... ./service/delivery/... -count=1

# sqlc (run from postgres/ directory)
cd postgres && sqlc generate

# Lint
go vet ./...

# Docker (full stack)
docker compose up --build
```

## Workflow: Adding a New Classification

```
1. Define lifecycle in presets/ (or register at runtime via API)
2. Create classification via API or Go code with lifecycle JSON
3. Create bookings against that classification
4. Transition bookings through lifecycle states
5. Post journals when accounting is needed (caller orchestrates)
6. Events are emitted automatically on every transition
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
- **No NULL**: All DB columns NOT NULL, all Go fields are value types (int64, string, time.Time), never pointers. Use 0/''/epoch/{} as defaults. **Exceptions** (FK target columns where 0 means "absent" must be nullable so PostgreSQL can enforce referential integrity): `journals.reversal_of`, `bookings.journal_id`, `bookings.reservation_id`, `events.journal_id`. The corresponding Go fields on `core.Booking`, `core.Event`, `core.Reservation` are `*int64`.
- Idempotency: every mutation requires an `idempotency_key` (UNIQUE index)
- Journal entries: append-only, corrections via reversal journal only
- Balance: `checkpoint.balance + SUM(entries WHERE id > checkpoint.last_entry_id)`
- Concurrency: `SELECT FOR UPDATE` on balance writes, advisory locks for reservations
- DB transactions: no external API calls inside a transaction

## Testing

- Integration tests use `testcontainers-go` with real PostgreSQL — no mocked DB.
- Test files: `postgres/*_test.go` for store tests, `service/*_test.go` for service tests.
- Unit tests: `core/*_test.go`, `presets/*_test.go`, `channel/onchain/*_test.go`.
- CI runs: `go vet`, `golangci-lint`, `go test -race`, `sqlc diff`, `go build`.

## File Layout Quick Reference

| Path | Purpose |
|------|---------|
| `core/types.go` | Currency, Classification + Lifecycle, JournalType, Balance, Status |
| `core/booking.go` | Booking, CreateBookingInput, TransitionInput |
| `core/event.go` | Event, EventFilter |
| `core/journal.go` | Journal, Entry, JournalInput + validation |
| `core/template.go` | EntryTemplate, Render() |
| `core/reserve.go` | Reservation state machine |
| `core/checkpoint.go` | BalanceCheckpoint, RollupQueueItem, BalanceSnapshot |
| `core/interfaces.go` | Booker, EventReader, JournalWriter, BalanceReader, etc. |
| `presets/` | Deposit + Withdrawal + Transfer + Fee + Capital + Settlement + Card + Spread + FX bundles |
| `presets/fx.go` | Cross-currency FX preset (sell + buy templates, settlement absorbs net) |
| `channel/adapter.go` | ChannelAdapter interface for inbound webhooks |
| `channel/onchain/evm.go` | Demo EVM adapter with HMAC verification |
| `postgres/sql/migrations/` | Schema migrations (embed.FS) |
| `postgres/sql/queries/` | sqlc query files |
| `postgres/sqlcgen/` | Generated code (do not edit) |
| `postgres/booking_store.go` | Booker + BookingReader implementation |
| `postgres/event_store.go` | EventReader + delivery polling |
| `postgres/invariants_test.go` | Postgres-backed pins for I-2 / I-3 / I-12 / I-13 |
| `postgres/benchmarks_test.go` | Bench: PostJournal / GetBalance / Reserve+Settle |
| `observability/prometheus.go` | core.Metrics impl on prometheus/client_golang |
| `server/routes.go` | All endpoint definitions |
| `server/handler_bookings.go` | Unified booking endpoints |
| `server/handler_webhooks.go` | Inbound channel callbacks |
| `server/handler_events.go` | Event query endpoints |
| `service/delivery/` | Event delivery: callback (library) + webhook (service) |
| `service/worker.go` | Background job runner |
| `cmd/ledgerd/` | HTTP service entry point |
| `cmd/ledger-cli/` | Read-only investigation CLI (balance, journals, trace, reconcile, solvency) |
| `deploy/helm/ledger/` | Kubernetes Helm chart |
| `docs/INVARIANTS.md` | The 13 invariants the ledger guarantees (canonical contract) |
| `docs/RUNBOOK.md` | Operational guide for on-call engineers |
| `docs/openapi.yaml` | Machine-readable OpenAPI 3.1 spec |

## HTTP API Quick Reference

```
# Bookings (unified — replaces deposits + withdrawals)
POST   /api/v1/bookings                    — Create booking
POST   /api/v1/bookings/{id}/transition    — State transition
GET    /api/v1/bookings/{id}               — Get booking
GET    /api/v1/bookings                    — List bookings

# Webhooks (inbound channel callbacks)
POST   /api/v1/webhooks/{channel}            — Receive channel callback

# Events (outbound)
GET    /api/v1/events/{id}                   — Get event
GET    /api/v1/events                        — List events

# Journals, Entries, Balances, Reservations — unchanged from v1
# Classifications, Journal Types, Templates, Currencies — unchanged
# Reconciliation, Snapshots, System — unchanged
```

## Gotchas

- `postgres/sqlcgen/` is generated — never edit manually, always `sqlc generate`.
- sqlc config is at `postgres/sqlc.yaml`, run sqlc from `postgres/` dir.
- Migrations use `golang-migrate/migrate/v4` with embedded FS.
- `web/` is a separate Next.js project with its own `CLAUDE.md`.
- Lifecycle is optional on Classification — nil means label-only (no bookings).
- `failed` is NOT terminal in withdrawal preset (has retry path to `reserved`).
