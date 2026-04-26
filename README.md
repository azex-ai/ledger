# azex-ai/ledger

Production-grade classification-driven double-entry ledger engine for Go.
Dual-mode: importable library or standalone HTTP service.

## Features

- **Double-entry accounting** -- every journal enforces `total_debit = total_credit` at the database level
- **Classification-driven design** -- account classifications (科目) are the primary entity; deposit/withdrawal are preset configurations, not hardcoded types
- **Lifecycle state machines** -- attach a generic state machine to any classification; bookings transition through declared states with audit-tracked events
- **Atomic event–journal model** -- events and journals are written in the same transaction; events are the "reason" a journal exists
- **Entry templates** -- reusable debit/credit recipes; `ExecuteTemplate` for single posts, `ExecuteTemplateBatch` for atomic multi-step plans (e.g. deposit tolerance)
- **Checkpoint + delta balances** -- materialised checkpoints plus incremental rollup; balance reads run inside `REPEATABLE READ` for snapshot consistency
- **Reserve / Settle / Release** -- per-(holder, currency) advisory-lock serialisation with in-lock balance check (TOCTOU-safe)
- **Channel adapters** -- pluggable inbound webhook handlers (HMAC-verified) for external systems such as on-chain deposit indexers
- **Webhook delivery** -- outbound event delivery with per-attempt exponential backoff and dead-letter handling
- **Presets** -- `deposit` and `withdrawal` ship as out-of-the-box classification + lifecycle + template bundles in `presets/`
- **Reconciliation engine** -- accounting-equation verification and per-account drift detection
- **Daily snapshots** -- historical balance snapshots for reporting
- **Idempotent writes** -- every mutation requires an idempotency key; duplicates return the original result without side effects
- **Async rollup worker** -- background checkpoint materialisation with `SKIP LOCKED` queue
- **NO NULL policy** -- all DB columns `NOT NULL` with meaningful defaults; all Go fields are value types

## Quick Start -- As a Library

**1. Install**

```bash
go get github.com/azex-ai/ledger@latest
```

**2. Connect and migrate**

```go
pool, _ := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
postgres.Migrate("pgx5://user:pass@host/db?sslmode=disable")
```

**3. Install presets and use**

```go
q := sqlcgen.New(pool)
ledgerStore  := postgres.NewLedgerStore(pool)
bookingStore := postgres.NewBookingStore(pool, q)
classStore   := postgres.NewClassificationStore(pool)
jtStore      := postgres.NewJournalTypeStore(pool)
tmplStore    := postgres.NewTemplateStore(pool)

// Install built-in classifications, journal types, and templates (idempotent).
// Deposit + withdrawal lifecycles are also exposed as presets.DepositLifecycle / presets.WithdrawalLifecycle.
if err := presets.InstallDefaultTemplatePresets(ctx, classStore, jtStore, tmplStore); err != nil {
    return err
}

// Book a deposit
booking, err := bookingStore.CreateBooking(ctx, core.CreateBookingInput{
    ClassificationCode: "deposit",
    AccountHolder:      userID,
    CurrencyID:         usdtID,
    Amount:             amount,
    IdempotencyKey:     txHash,
    ChannelName:        "evm",
    Metadata:           map[string]any{"chain": "ethereum"},
})

// Drive the lifecycle (each transition emits an event in the same tx as the journal)
_, err = bookingStore.Transition(ctx, core.TransitionInput{
    BookingID: booking.ID,
    ToStatus:  "confirming",
    ActorID:   0, // system actor
})

// Query balance (snapshot-consistent)
balance, err := ledgerStore.GetBalance(ctx, userID, usdtID, booking.ClassificationID)
```

## Quick Start -- As a Service

```bash
git clone https://github.com/azex-ai/ledger.git
cd ledger
docker compose up --build
```

- API: <http://localhost:8080/api/v1/system/health>
- Frontend: <http://localhost:3000>

## Architecture

Hexagonal: `core/` (pure domain) → `postgres/` (adapter) → `service/` (orchestration) → `server/` (HTTP) → `cmd/ledgerd/` (entry).

```
ledger/
  core/                Pure domain layer (zero external dependencies)
    types.go             Currency, Classification + Lifecycle, JournalType, Balance, Status
    booking.go           Booking, CreateBookingInput, TransitionInput
    event.go             Event, EventFilter
    journal.go           Journal, Entry, JournalInput + validation
    template.go          EntryTemplate, Render(), TemplateExecutionRequest
    reserve.go           Reservation state machine
    checkpoint.go        BalanceCheckpoint, RollupQueueItem, BalanceSnapshot
    interfaces.go        Booker, JournalWriter, TemplateBatchExecutor, BalanceReader, ...

  postgres/            pgx v5 + sqlc adapter (the only officially supported DB)
    sql/migrations/      Schema migrations (embed.FS)
    sql/queries/         sqlc query files
    sqlcgen/             Generated code (do not edit)
    ledger_store.go      JournalWriter + TemplateBatchExecutor + BalanceReader
    booking_store.go     Booker + BookingReader (state-machine transitions, lifecycle expiry)
    event_store.go       EventReader + delivery polling
    reserver_store.go    Reserver (per-(holder, currency) advisory locks)

  presets/             Out-of-the-box classification configs
    deposit.go           pending → confirming → confirmed | failed lifecycle
    withdrawal.go        locked → reserved → reviewing → processing → confirmed | failed
    templates.go         Default deposit/withdrawal entry templates
    tolerance.go         Deposit tolerance: confirm-pending + release-shortfall as one atomic batch

  channel/             Inbound channel adapters
    adapter.go           ChannelAdapter interface (parse + verify webhooks)
    onchain/evm.go       Demo EVM adapter with HMAC verification

  service/             Business orchestration
    delivery/            Event delivery: callback (library mode) + webhook (service mode)
    rollup.go            Async checkpoint materialisation
    reconcile.go         Accounting-equation + per-account verification
    snapshot.go          Daily balance snapshots
    expiration.go        Booking expiry sweeper
    worker.go            Background worker loop

  server/              HTTP API (chi v5)
    routes.go            All endpoint definitions
    handler_bookings.go  Unified booking endpoints
    handler_webhooks.go  Inbound channel callbacks (1MB body cap)
    handler_events.go    Outbound event query endpoints

  web/                 Next.js 16 management dashboard (shadcn/ui, viem-based BigInt utils)

  cmd/ledgerd/         Service entry point
```

**Account dimensions** are fixed at three: `(AccountHolder, CurrencyID, ClassificationID)`.
Positive holder IDs are users; negative IDs are system counterparts (`-userID`).

**Single-direction data flow**: the ledger never calls external systems. Commands in, events out.

For the v2 design rationale, see [docs/plans/2026-04-22-ledger-v2-design.md](docs/plans/2026-04-22-ledger-v2-design.md).

## HTTP API Quick Reference

```
# Bookings (unified -- replaces v1 deposits + withdrawals)
POST   /api/v1/bookings                   Create booking
POST   /api/v1/bookings/{id}/transition   State transition
GET    /api/v1/bookings/{id}              Get booking
GET    /api/v1/bookings                   List bookings

# Webhooks (inbound channel callbacks, HMAC-verified, 1MB cap)
POST   /api/v1/webhooks/{channel}         Receive channel callback

# Events (outbound)
GET    /api/v1/events/{id}
GET    /api/v1/events

# Plus: journals, entries, balances, reservations, classifications, journal types,
#       templates, currencies, reconciliation, snapshots, system health.
```

All list endpoints use cursor-based pagination (`?cursor=...&limit=50`).
Error responses use a consistent envelope: `{"error": {"code": "...", "message": "..."}}`.

See [docs/api.md](docs/api.md) for the complete reference with request/response examples.

## Examples

- [**crypto-deposit**](examples/crypto-deposit/) -- Full EVM CREATE2 deposit lifecycle: classification install, booking creation, channel-adapter webhook, template-based journaling, reserve/settle, balance queries, and reconciliation.

## Configuration

The service entry point reads:

| Variable | Description | Default |
|----------|-------------|---------|
| `DATABASE_URL` | PostgreSQL connection string (`postgres://` or `postgresql://`) | (required) |
| `HTTP_PORT` | HTTP server listen port | `8080` |
| `CORS_ALLOWED_ORIGIN` | Allowed CORS origin; logs a warning when unset | `*` |

Other timing parameters (rollup interval, reservation TTL, reconcile / snapshot cadences,
withdrawal review threshold) are configured in code at `cmd/ledgerd/main.go` and can be
exposed as env vars when needed -- there is intentionally no default magic.

## Testing

Integration tests use `testcontainers-go` against real PostgreSQL -- no mocked DB.

```bash
# Full suite (requires Docker)
go test ./... -race -count=1

# Unit-only (no DB)
go test ./core/... ./presets/... ./channel/... ./service/delivery/... -count=1
```

## License

MIT
