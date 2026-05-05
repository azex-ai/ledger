# azex-ai/ledger

Production-grade classification-driven double-entry ledger engine for Go.
Dual-mode: importable library or standalone HTTP service.

## Features

Five-dimensional banking coverage — 冲/提/费/安全/审计:

```
冲 (Deposit)    Pending two-phase API · EVM channel adapter · tolerance settlement
提 (Withdrawal) Lifecycle state machine · fund locking · fee templates
费 (Fee)        First-class fee classification · fee_charge template
安全 (Security) 10-check reconciliation · solvency check · advisory lock leader election
审计 (Audit)    Balance trends · booking trace · reversal chain · OTEL trace propagation
```

Core engine capabilities:

- **Double-entry accounting** -- every journal enforces `total_debit = total_credit` at the database level
- **Classification-driven design** -- account classifications (科目) are the primary entity; deposit/withdrawal are preset configurations, not hardcoded types
- **Lifecycle state machines** -- attach a generic state machine to any classification; bookings transition through declared states with audit-tracked events
- **Atomic event-journal model** -- booking transitions and journal posts can share one transaction via `RunInTx`; pass `EventID` when posting the journal to backfill `events.journal_id` and `bookings.journal_id`
- **Entry templates** -- reusable debit/credit recipes; `ExecuteTemplate` for single posts, `ExecuteTemplateBatch` for atomic multi-step plans
- **Checkpoint + delta balances** -- materialised checkpoints plus incremental rollup; balance reads run inside `REPEATABLE READ` for snapshot consistency
- **Reserve / Settle / Release** -- per-(holder, currency) advisory-lock serialisation with in-lock balance check (TOCTOU-safe)
- **Pending two-phase deposits** -- `AddPending` → `ConfirmPending` / `CancelPending` for in-flight deposit tracking
- **Channel adapters** -- pluggable inbound webhook handlers (HMAC-verified) for external systems such as on-chain deposit indexers
- **Webhook delivery** -- outbound event delivery with per-attempt exponential backoff and dead-letter handling
- **In-process event subscription** -- `Worker.Subscribe` for library-mode event callbacks without a webhook server
- **Transaction composition** -- `RunInTx` lets callers combine ledger writes with their own DB writes in one atomic transaction
- **Extended preset catalogue** -- deposit, withdrawal, transfer, fee, capital, settlement, card-topup, and spread bundles ship out-of-the-box
- **10-check reconciliation engine** -- accounting-equation verification, orphan detection, solvency check, idempotency audit, and stale-rollup detection
- **Balance trends + audit queries** -- time-series trends, reversal chains, booking traces for customer support and compliance
- **Platform solvency API** -- `PlatformBalanceReader` + `SolvencyChecker` read from the `system_rollups` materialised view in O(1)
- **Sparse daily snapshots** -- historical balance snapshots; startup backfill with advisory-lock guard for multi-replica safety
- **Prometheus / OTEL observability** -- `observability.NewPrometheusMetrics()` + OTEL trace propagation on journal/booking paths
- **Idempotent writes** -- every mutation requires an idempotency key; duplicates return the original result without side effects
- **Async rollup worker** -- background checkpoint materialisation with `SKIP LOCKED` queue and leader election
- **NO NULL policy** -- all DB columns `NOT NULL` with meaningful defaults; all Go fields are value types

## Quick Start -- As a Library

**1. Install**

```bash
go get github.com/azex-ai/ledger@latest
```

**2. Connect and migrate**

```go
pool, _ := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
ledger.Migrate("postgres://user:pass@host/db?sslmode=disable")
```

**3. Wire and use**

```go
import "github.com/azex-ai/ledger"

// One call wires every postgres-backed store.
svc, err := ledger.New(pool)
if err != nil { return err }

// Install the full preset catalogue (idempotent — safe to call every startup).
if err := svc.InstallExtendedPresets(ctx); err != nil { return err }

// Book a deposit
booking, err := svc.Booker().CreateBooking(ctx, core.CreateBookingInput{
    ClassificationCode: "deposit",
    AccountHolder:      userID,
    CurrencyID:         usdtID,
    Amount:             amount,
    IdempotencyKey:     ledger.NewIdempotencyKey("deposit"),
    ChannelName:        "evm",
})

// Drive the lifecycle
_, err = svc.Booker().Transition(ctx, core.TransitionInput{
    BookingID: booking.ID,
    ToStatus:  "confirming",
    Source:    "api",
})

// Query balance (snapshot-consistent)
balance, err := svc.BalanceReader().GetBalance(ctx, userID, usdtID, classID)
```

**4. Run the background worker**

```go
worker := svc.Worker(service.DefaultWorkerConfig())
go worker.Run(ctx) // rollup, expiration, reconcile, snapshot, event delivery
```

**5. Add observability (optional)**

```go
import "github.com/azex-ai/ledger/observability"

prom := observability.NewPrometheusMetrics()
svc, _ := ledger.New(pool, ledger.WithMetrics(prom))
http.Handle("/metrics", prom.Handler())
```

## Quick Start -- As a Service

```bash
git clone https://github.com/azex-ai/ledger.git
cd ledger
docker compose up --build
```

- API: <http://localhost:8080/api/v1/system/health>
- Frontend: <http://localhost:3000>

## API Surface

All accessors return interfaces from `core/` so your application code depends only on the domain layer.

### Core operations

| Method | Interface | Description |
|--------|-----------|-------------|
| `svc.Booker()` | `core.Booker` | Create bookings, drive lifecycle transitions |
| `svc.BookingReader()` | `core.BookingReader` | Read / list bookings |
| `svc.JournalWriter()` | `core.JournalWriter` | Post, reverse, and template-execute journals |
| `svc.TemplateBatchExecutor()` | `core.TemplateBatchExecutor` | Execute multiple templates atomically |
| `svc.BalanceReader()` | `core.BalanceReader` | Get balance, batch balances |
| `svc.Reserver()` | `core.Reserver` | Reserve / settle / release funds |
| `svc.EventReader()` | `core.EventReader` | Read / list events |

### Deposit / pending

| Method | Interface | Description |
|--------|-----------|-------------|
| `svc.PendingBalanceWriter()` | `core.PendingBalanceWriter` | AddPending / ConfirmPending / CancelPending |
| `svc.PendingTimeoutSweeper()` | `core.PendingTimeoutSweeper` | Expire stale pending deposits |

### Analytics and audit

| Method | Interface | Description |
|--------|-----------|-------------|
| `svc.BalanceTrends()` | `core.BalanceTrendReader` | Daily balance trends with inflow/outflow |
| `svc.Audit()` | `core.AuditQuerier` | Journal lists, booking trace, reversal chain |
| `svc.PlatformBalanceReader()` | `core.PlatformBalanceReader` | Per-classification platform-wide balances |
| `svc.SolvencyChecker()` | `core.SolvencyChecker` | Custodial vs user liability check |

### Integrity and operations

| Method | Interface | Description |
|--------|-----------|-------------|
| `svc.FullReconciler(cfg)` | `core.FullReconciler` | 10-check reconciliation suite |
| `svc.SnapshotBackfiller()` | `core.SnapshotBackfiller` | Fill historical snapshot gaps |
| `svc.Worker(cfg)` | `*service.Worker` | Background jobs (rollup, expiry, reconcile, snapshots) |

### Metadata stores

| Method | Interface |
|--------|-----------|
| `svc.Classifications()` | `core.ClassificationStore` |
| `svc.JournalTypes()` | `core.JournalTypeStore` |
| `svc.Templates()` | `core.TemplateStore` |
| `svc.Currencies()` | `core.CurrencyStore` |
| `svc.Queries()` | `core.QueryProvider` |

### Infrastructure helpers

| Method / function | Description |
|-------------------|-------------|
| `svc.RunInTx(ctx, fn)` | Combine ledger writes + your writes in one PostgreSQL transaction |
| `svc.Pool()` | Underlying `*pgxpool.Pool` for custom queries |
| `svc.RegisterChannel(name, adapter)` | Register inbound webhook channel adapter |
| `svc.Channels()` | Snapshot of registered adapters |
| `svc.InstallDefaultPresets(ctx)` | Install deposit + withdrawal bundles |
| `svc.InstallExtendedPresets(ctx)` | Install all 8 preset bundles |
| `svc.Ping(ctx)` | DB connectivity check (`SELECT 1`) |
| `ledger.Migrate(databaseURL)` | Run pending schema migrations |
| `ledger.NewIdempotencyKey(scope)` | Generate `scope:<16-byte-hex>` key via `crypto/rand` |

## Architecture

Hexagonal: `core/` (pure domain) → `postgres/` (adapter) → `service/` (orchestration) → `server/` (HTTP) → `cmd/ledgerd/` (entry).

```
ledger/
  core/                Pure domain layer (zero external dependencies)
    types.go             Currency, Classification + Lifecycle, JournalType, Balance, Status
    booking.go           Booking, CreateBookingInput, TransitionInput
    event.go             Event (+ ActorID, Source fields), EventFilter
    journal.go           Journal, Entry, JournalInput + validation
    template.go          EntryTemplate, Render(), TemplateExecutionRequest
    reserve.go           Reservation state machine
    checkpoint.go        BalanceCheckpoint, RollupQueueItem, BalanceSnapshot
    pending.go           PendingBalanceWriter, PendingTimeoutSweeper + inputs
    audit.go             BalanceTrendReader, AuditQuerier, BookingTrace
    platform_balance.go  PlatformBalanceReader, SolvencyChecker, SolvencyReport
    reconcile_extra.go   FullReconciler, ReconcileReport, CheckResult
    snapshot_extra.go    SnapshotBackfiller, BackfillResult
    interfaces.go        All consumer-side interfaces (-er suffix)

  postgres/            pgx v5 + sqlc adapter (only supported DB)
    sql/migrations/      Schema migrations (embed.FS)
    sql/queries/         sqlc query files
    sqlcgen/             Generated code (do not edit)
    ledger_store.go      JournalWriter + BalanceReader + TemplateBatchExecutor
    booking_store.go     Booker + BookingReader
    event_store.go       EventReader + delivery polling
    reserver_store.go    Reserver (advisory lock serialisation)
    pending_store.go     PendingBalanceWriter + PendingTimeoutSweeper
    audit_store.go       AuditQuerier
    balance_trends_store.go  BalanceTrendReader
    platform_balance_store.go  PlatformBalanceReader + SolvencyChecker
    reconcile_adapter.go ReconcileQuerier (10-check suite queries)
    snapshot_extra_store.go  SparseSnapshotter + LiveBalanceMerger

  presets/             Out-of-the-box classification configs
    deposit.go           pending → confirming → confirmed | failed lifecycle
    withdrawal.go        locked → reserved → reviewing → processing → confirmed | failed
    templates.go         Default deposit/withdrawal templates; InstallExtendedPresets
    tolerance.go         Deposit tolerance: confirm-pending + release-shortfall (atomic batch)
    fee.go, transfer.go, capital.go, settlement.go, card.go, spread.go, fx.go

  channel/             Inbound channel adapters
    adapter.go           ChannelAdapter interface (parse + verify webhooks)
    onchain/evm.go       EVM adapter with HMAC-SHA256 verification

  service/             Business orchestration
    delivery/            Event delivery: callback (library) + webhook (service)
    rollup.go            Async checkpoint materialisation
    reconcile.go         Basic + 10-check FullReconciliationService
    snapshot.go          Daily balance snapshots (advisory-lock guard)
    expiration.go        Booking + reservation expiry sweeper
    worker.go            Background worker loop (leader election via pg_try_advisory_lock)

  observability/       Prometheus metrics + OTEL trace support
    prometheus.go        PrometheusMetrics — implements core.Metrics

  server/              HTTP API (chi v5)
    routes.go            All endpoint definitions
    handler_bookings.go  Unified booking endpoints
    handler_webhooks.go  Inbound channel callbacks (1 MB body cap)
    handler_events.go    Outbound event query endpoints

  web/                 Next.js 16 management dashboard (shadcn/ui, viem-based BigInt utils)

  cmd/ledgerd/         Service entry point
  cmd/ledger-cli/      Read-only investigation CLI (balance, journals, trace, reconcile, solvency)

  deploy/helm/ledger/  Kubernetes Helm chart (deployment + service + ingress + secrets)

  ledger.go            Top-level Service facade
  idempotency.go       NewIdempotencyKey helper
```

**Account dimensions** are fixed at three: `(AccountHolder, CurrencyID, ClassificationID)`.
Positive holder IDs are users; negative IDs are system counterparts (`-userID`).

**Single-direction data flow**: the ledger never calls external systems. Commands in, events out.

**What's new since v0.x**

The v0.x series had hardcoded `deposit` / `withdrawal` resource types. v2 introduces classification-driven design: deposit and withdrawal are preset configurations of the generic booking lifecycle. This enables arbitrary account types (fee, capital, settlement, spread, card topup, …) without any code change in the engine. The public API is backwards-compatible; callers using the v2 facade (`ledger.New`) did not need to change.

For the design rationale, see [docs/plans/2026-04-22-ledger-v2-design.md](docs/plans/2026-04-22-ledger-v2-design.md).

## HTTP API Quick Reference

```
# Bookings (unified -- replaces v1 deposits + withdrawals)
POST   /api/v1/bookings                   Create booking
POST   /api/v1/bookings/{id}/transition   State transition
GET    /api/v1/bookings/{id}              Get booking
GET    /api/v1/bookings                   List bookings

# Webhooks (inbound channel callbacks, HMAC-verified, 1 MB cap)
POST   /api/v1/webhooks/{channel}         Receive channel callback

# Events (outbound)
GET    /api/v1/events/{id}
GET    /api/v1/events

# Plus: journals, entries, balances, reservations, classifications, journal types,
#       templates, currencies, reconciliation, snapshots, system health.
```

All list endpoints use cursor-based pagination (`?cursor=...&limit=50`).
Error responses use a consistent envelope: `{"code": <int>, "message": "..."}`.

See [docs/api.md](docs/api.md) for the complete reference with request/response examples, and [docs/openapi.yaml](docs/openapi.yaml) for the machine-readable OpenAPI 3.1 schema.

## Documentation

- [**INVARIANTS.md**](docs/INVARIANTS.md) -- The 13 invariants the ledger guarantees (per-currency balance, append-only, idempotency, TOCTOU-safe reserve, money conservation, partition coverage, …) with `Why / Enforced by / Pinned by` for each.
- [**RUNBOOK.md**](docs/RUNBOOK.md) -- Operational guide for on-call: reconciliation failure, solvency alert, rollup backlog, webhook backlog, idempotency collision, emergency stop.
- [**openapi.yaml**](docs/openapi.yaml) -- OpenAPI 3.1 contract (32 paths, 34 schemas).
- [**api.md**](docs/api.md) -- Long-form HTTP API reference with examples.

## Examples

- [**embed**](examples/embed/) -- Minimum-viable library embed: PostJournal + GetBalance with no templates, no presets, no HTTP layer.
- [**crypto-deposit**](examples/crypto-deposit/) -- Full EVM CREATE2 deposit lifecycle: classification install, booking creation, channel-adapter webhook, template-based journaling, reserve/settle, balance queries, and reconciliation.
- [**billing**](examples/billing/) -- SaaS-style metered billing: top-up wallet, reserve budget, deduct actual cost, release remainder.
- [**event-subscribe**](examples/event-subscribe/) -- In-process event subscription: Worker.Subscribe, graceful shutdown.
- [**tx-compose**](examples/tx-compose/) -- Transactional composition: ledger journal + caller's own DB write in one PostgreSQL transaction; rollback on error.

## SemVer / Stability Policy

The current release series is **v0.x**. No API stability guarantees are made between minor versions while the library is in active development.

**v1.0 milestone criteria**:
- All five dimensions (冲/提/费/安全/审计) have been exercised in at least one production deployment
- HTTP API at OpenAPI 3.1 full coverage — see [docs/openapi.yaml](docs/openapi.yaml) (in progress)
- The `core/` interface set is stable for at least two minor versions without breaking changes
- INVARIANTS.md complete with every invariant pinned by a regression test — see [docs/INVARIANTS.md](docs/INVARIANTS.md)

**Deprecation policy (post v1.0)**: deprecated items will carry a `// Deprecated:` godoc comment for at least one minor version before removal. Breaking changes are only made in major version bumps.

**Before v1.0**: callers should pin to a specific `vX.Y.Z` tag or commit SHA. The `go get ./...@latest` convenience works for greenfield projects that can track HEAD.

## Configuration

The service entry point reads:

| Variable | Description | Default |
|----------|-------------|---------|
| `DATABASE_URL` | PostgreSQL connection string (`postgres://` or `postgresql://`) | (required) |
| `HTTP_PORT` | HTTP server listen port | `8080` |
| `ENV` | Deployment environment; anything other than `dev` enables production guards | `production` |
| `CORS_ALLOWED_ORIGIN` | Allowed CORS origin. Required in non-dev `ENV` -- the service refuses to boot without it. | (required outside dev) |
| `API_KEYS` | Comma-separated bearer-token keys for mutating endpoints. GETs are open. | (none) |
| `MAX_BODY_BYTES` | Maximum inbound request body size in bytes | `262144` (256 KB) |
| `EVM_WEBHOOK_SECRET` | HMAC-SHA256 signing key for the EVM block-scanner webhook adapter | (channel disabled when empty) |

Other timing parameters (rollup interval, reservation TTL, reconcile / snapshot cadences, withdrawal review threshold) are set in `cmd/ledgerd/main.go`.

### Security notes

- **Authentication**: bearer-token API keys via `Authorization: Bearer <key>`. Constant-time compare; only required for state-changing methods.
- **Rate limits**: in-memory per-IP token bucket -- 100 req/min mutations, 1000 req/min reads. Single-instance only.
- **Body size**: every request is capped at `MAX_BODY_BYTES`; webhooks have an additional 1 MB cap enforced in the handler.
- **Webhook replay**: HMAC payload is `<timestamp>.<body>`; timestamps outside ±5 minutes are rejected.
- **Health vs. readiness**: `/api/v1/system/health` returns 503 on DB failure; `/api/v1/system/ready` returns 503 until migrations + worker have booted.

## Testing

Integration tests use `testcontainers-go` against real PostgreSQL -- no mocked DB.

```bash
# Full suite (requires Docker)
go test ./... -race -count=1

# Unit-only (no DB)
go test ./core/... ./presets/... ./channel/... ./service/delivery/... -count=1

# Fuzz the validators (Go 1.18+ built-in fuzzing)
go test ./core -run=^$ -fuzz=FuzzJournalValidate   -fuzztime=30s
go test ./core -run=^$ -fuzz=FuzzLifecycleValidate -fuzztime=30s

# Benchmarks (requires Docker)
go test ./postgres/ -bench=. -benchtime=3s -run=^$
```

Every invariant in [docs/INVARIANTS.md](docs/INVARIANTS.md) names the test(s) that pin it (the "Pinned by" section). When the contract changes, that doc and the named tests must change together.

## License

MIT
