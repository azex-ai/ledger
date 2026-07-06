# azex-ai/ledger

Production-grade classification-driven double-entry ledger engine for Go.
Dual-mode: importable library or standalone HTTP service.

## Features

Five-dimensional banking coverage:

```
Deposit       Pending two-phase API · EVM channel adapter · tolerance settlement
Withdrawal    Lifecycle state machine · fund locking · fee templates
Fee           First-class fee classification · fee_charge template
Security      10-check reconciliation · solvency check · advisory lock leader election
Audit         Balance trends · booking trace · reversal chain · OTEL trace propagation
```

Core engine capabilities:

- **Double-entry accounting** -- every journal enforces `total_debit = total_credit` at the database level
- **Classification-driven design** -- account classifications are the primary entity; deposit/withdrawal are preset configurations, not hardcoded types
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
- **Extended preset catalogue** -- deposit, withdrawal, transfer, fee, capital, settlement, and spread bundles ship out-of-the-box
- **10-check reconciliation engine** -- accounting-equation verification, orphan detection, solvency check, idempotency audit, and stale-rollup detection
- **Balance trends + audit queries** -- time-series trends, reversal chains, booking traces for customer support and compliance
- **Platform solvency API** -- `PlatformBalanceReader` + `SolvencyChecker` read from the `system_rollups` materialised view in O(1)
- **Sparse daily snapshots** -- historical balance snapshots; startup backfill with advisory-lock guard for multi-replica safety
- **Prometheus / OTEL observability** -- `observability.NewPrometheusMetrics()` + OTEL trace propagation on journal/booking paths
- **Idempotent writes** -- every mutation requires an idempotency key; duplicates return the original result without side effects
- **Async rollup worker** -- background checkpoint materialisation with `SKIP LOCKED` queue and leader election
- **NO NULL policy** -- all DB columns `NOT NULL` with meaningful defaults; all Go fields are value types

## Local Development with go.work

To consume the local copy of `azex-ai/ledger` from a sibling Go module without
publishing or `replace` directives, drop a workspace file at the parent
directory:

```bash
cd /path/to/parent          # e.g. /Users/aaron/azex
cat > go.work <<'EOF'
go 1.26.1

use (
    ./ledger
    ./ledger/internal/postgrestest
    ./your-consumer-module
)
EOF

# The ledger repo ships its own go.work that only sees the inner modules;
# the outer file supersedes it, so remove the inner one to avoid confusion.
rm ledger/go.work
```

In your consumer's `go.mod`, ensure `go 1.26.1` (or add `toolchain go1.26.1`).
The workspace file is git-ignored by convention, so this only affects local
builds. For published consumers, switch to `go get github.com/azex-ai/ledger@<tag>`
and delete the workspace.

## Quick Start -- As a Library

Two tiers: pick where you want to start.

### Tier 1 — Hello Ledger (raw entries, no presets)

The shortest path: post one balanced journal, read the resulting balance. Use
this when you want to understand the primitives or you have your own
chart-of-accounts and just need the engine.

```go
import (
    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/shopspring/decimal"
    "github.com/azex-ai/ledger"
    "github.com/azex-ai/ledger/core"
)

ledger.Migrate(dbURL)                       // schema only — no metadata yet
pool, _ := pgxpool.New(ctx, dbURL)
svc, _ := ledger.New(pool)

// Tier 1 still needs at least one Currency, Classification, and JournalType
// row before any post — see examples/embed/main.go for a self-contained boot.

j, err := svc.JournalWriter().PostJournal(ctx, core.JournalInput{
    JournalTypeID:  jtID,
    IdempotencyKey: ledger.NewIdempotencyKey("hello"),
    Entries: []core.EntryInput{
        {AccountHolder: -42, CurrencyID: 1, ClassificationID: clsID, EntryType: core.EntryTypeDebit,  Amount: decimal.NewFromInt(100)},
        {AccountHolder:  42, CurrencyID: 1, ClassificationID: clsID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
    },
    Source: "api",
})

bal, _ := svc.BalanceReader().GetBalance(ctx, 42, 1, clsID)
```

### Tier 2 — With Built-in Presets (recommended)

Install the preset bundles and you immediately get classifications, journal
types, and templates for deposits, withdrawals, fees, transfers, and more —
all idempotent on every boot.

```go
svc, _ := ledger.New(pool)
svc.InstallExtendedPresets(ctx)              // 9 bundles, see "Built-in Presets" below

// Post a deposit confirmation by template — no entry-list assembly needed.
_, err := svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
    HolderID:       42,
    CurrencyID:     1,
    Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
    IdempotencyKey: ledger.NewIdempotencyKey("deposit-confirm"),
    Source:         "api",
})

// Or model a long-lived flow with a Booking and lifecycle transitions.
booking, _ := svc.Booker().CreateBooking(ctx, core.CreateBookingInput{
    ClassificationCode: "deposit",
    AccountHolder:      42,
    CurrencyID:         1,
    Amount:             decimal.NewFromInt(100),
    IdempotencyKey:     ledger.NewIdempotencyKey("deposit"),
    ChannelName:        "evm",
})
svc.Booker().Transition(ctx, core.TransitionInput{
    BookingID: booking.ID,
    ToStatus:  "confirming",
    Source:    "api",
})
```

Background worker (rollup, expiry, reconcile, snapshots, event delivery):

```go
worker := svc.Worker(service.DefaultWorkerConfig())
go worker.Run(ctx)
```

Observability (logger / metrics / tracing) is opt-in — see [Observability](#observability) below.

## Quick Start -- As a Service

```bash
git clone https://github.com/azex-ai/ledger.git
cd ledger

# API keys are name:scope:secret triples (scope: read|write|admin).
# Generate one per consumer — the name shows up in access logs (audit):
export API_KEYS="ops:admin:$(openssl rand -hex 32),app:write:$(openssl rand -hex 32)"

export ENV=dev
export POSTGRES_PASSWORD=$(openssl rand -hex 16)
export CORS_ALLOWED_ORIGIN=http://localhost:3000
export EVM_WEBHOOK_SECRET=$(openssl rand -hex 32)

docker compose up --build
```

- API: <http://localhost:8080/api/v1/system/health>
- Frontend: <http://localhost:3000>

Every endpoint except the health probes requires `Authorization: Bearer
<secret>` — scope semantics and the full key spec live in
[docs/api.md](docs/api.md#authentication):

```bash
curl -H "Authorization: Bearer <the-admin-secret>" http://localhost:8080/api/v1/currencies
```

## Quick Start -- Frontend (React)

The [`@azex/ledger-react`](web/packages/ledger-react/) package ships typed
TanStack Query hooks, admin page components, and an all-in-one `<LedgerAdmin/>`
shell for the HTTP API. It is published to the public npm registry — no
registry config or auth token needed:

```bash
npm install @azex/ledger-react @tanstack/react-query
```

```tsx
import { LedgerProvider, LedgerAdmin } from "@azex/ledger-react";
import "@azex/ledger-react/styles.css";

export default function Admin() {
  return (
    <LedgerProvider config={{ baseUrl: "https://ledger.example.com" }}>
      <LedgerAdmin />
    </LedgerProvider>
  );
}
```

See [docs/frontend.md](docs/frontend.md) for the full guide: individual page
components wired to your router, headless hooks, RSC server prefetch, theming,
and the complete API reference. The [`web/`](web/) app is the working
reference integration.

## Core Concepts

The ledger is built on five primitives. Knowing them is enough to model any
banking flow.

| Primitive | What it is | Where it lives |
|-----------|-----------|----------------|
| **Currency** | Unit of value (USD, USDT, EUR, …). Has a precision. | `core.Currency` / `currencies` table |
| **Classification** | Account type — "main_wallet", "pending", "fees", "equity", … Has `NormalSide` (debit-normal vs credit-normal) and an optional `Lifecycle` state machine. Positive holder = user-side, negative = system counterpart. | `core.Classification` / `classifications` table |
| **Journal Type** | Categorises journals by intent — "deposit_confirm", "fee", "transfer". Required metadata before any post; think of it as the journal-entry kind in a chart of accounts. | `core.JournalType` / `journal_types` table |
| **Entry Template** | Reusable recipe for a balanced journal: a list of `(classification, debit/credit, holder_role, amount_key)` lines. Render with `TemplateParams` to produce a `JournalInput`. | `core.EntryTemplate` / `entry_templates` table |
| **Booking + Lifecycle** | Long-lived process record (e.g. a deposit attempt) tied to a Classification. Each `Transition` writes an Event and may post a Journal. | `core.Booking`, `core.Lifecycle` / `bookings` + `events` |

When a journal is posted:

- **Journal**: header row with idempotency key, totals, metadata.
- **Entry**: each individual debit / credit line on the journal.
- All entries must satisfy `SUM(debit) = SUM(credit)` per currency. Enforced by DB trigger.

Before posting any journal, the database must contain at least:

1. One **Currency**
2. One **Classification**
3. One **Journal Type**
4. (Optional) **Entry Template** if you want `ExecuteTemplate` instead of building entries manually.

Installing a preset bundle (next section) creates all of these in one call.

## Built-in Presets

The library ships nine preset bundles. Each is a self-contained set of
classifications, journal types, and templates that wire one accounting flow
end-to-end.

| Bundle | Classifications introduced | Journal types | Templates | Purpose |
|--------|---------------------------|---------------|-----------|---------|
| `DepositBundle()` | `pending`, `main_wallet`, `suspense`, `custodial` | `deposit_pending`, `deposit_confirm`, `deposit_confirm_pending`, `deposit_release_pending`, `deposit_record_overage`, `deposit_resolve_overage`, `deposit_release_overage` | matching templates, one per journal type | Two-phase deposit (pending → confirmed) with tolerance & overage handling |
| `WithdrawalBundle()` | `locked`, `fee_expense`, `fee_revenue` | `lock_funds`, `unlock_funds`, `withdraw_confirm`, `withdraw_fee` | `lock_funds`, `unlock_funds`, `withdraw_confirm`, `withdraw_fee` | Lock → reserve → confirm; fee templates |
| `TransferBundle()` | `settlement` (system) | `transfer` | `transfer_out`, `transfer_in` | User-to-user via shared settlement pool (sender leg + receiver leg) |
| `FeeBundle()` | `fees` (system) | `fee` | `fee_charge` | Generic platform fee: DR user main_wallet, CR system fees |
| `CapitalBundle()` | `equity` (system) | `capital_injection`, `capital_withdraw` | matching | Platform equity movements |
| `SettlementBundle()` | `settlement` (system), `fees` (system) | `checkout_settlement` | `checkout_settlement_gross`, `checkout_settlement_net` | Checkout settlement (gross or net-of-fee) into user wallet |
| `SpreadBundle()` | `spread` (system) | (none) | (none) | Registers the `spread` classification only — caller posts via `PostJournal` |
| `FXBundle()` | (shared only) | `fx_sell`, `fx_buy` | matching | Per-currency FX leg pair sharing the settlement pool |

Two convenience installers:

```go
svc.InstallDefaultPresets(ctx)    // Deposit + Withdrawal only
svc.InstallExtendedPresets(ctx)   // All 9 bundles
```

Or install one bundle at a time:

```go
import "github.com/azex-ai/ledger/presets"

presets.InstallTemplateBundle(ctx,
    svc.Classifications(), svc.JournalTypes(), svc.Templates(),
    presets.FeeBundle(),
)
```

All installers are idempotent — safe to run on every startup. Existing rows
are validated against the bundle and reused; mismatched `NormalSide` /
`IsSystem` raise an error so a renamed preset cannot silently change semantics.

Preset lifecycles (state machines for `Booker.Transition`):

```go
presets.DepositLifecycle      // pending → confirming → confirmed | failed | expired
presets.WithdrawalLifecycle   // locked → reserved → reviewing → processing → confirmed | failed
```

## Recording Accounting

Three ways to post a journal, in increasing order of abstraction.

### 1. Direct — `PostJournal`

You assemble the entry list. No template required. Use for one-off journals
that don't have a reusable shape.

```go
svc.JournalWriter().PostJournal(ctx, core.JournalInput{
    JournalTypeID:  jtID,
    IdempotencyKey: key,
    Entries: []core.EntryInput{
        {AccountHolder:  42, CurrencyID: 1, ClassificationID: walletID, EntryType: core.EntryTypeDebit,  Amount: amt},
        {AccountHolder: -42, CurrencyID: 1, ClassificationID: feesID,   EntryType: core.EntryTypeCredit, Amount: amt},
    },
    ActorID: 99, Source: "api",
})
```

### 2. Template — `ExecuteTemplate`

Renders a stored `EntryTemplate` (preset or your own) using `TemplateParams`,
then calls `PostJournal`. Most application code lives here.

```go
svc.JournalWriter().ExecuteTemplate(ctx, "fee_charge", core.TemplateParams{
    HolderID:       42,
    CurrencyID:     1,
    Amounts:        map[string]decimal.Decimal{"amount": amt},
    IdempotencyKey: key,
    Source:         "billing",
})
```

`AmountKey` on each template line picks the value out of `Amounts`. Multiple
keys per template (e.g. `amount` + `fee` for `withdraw_fee`) let one template
encode multi-amount flows.

### 3. Atomic Multi-Template — `ExecuteTemplateBatch`

Runs several templates inside one transaction. All commit or all roll back
together. Use for compound flows like "lock + charge fee" or "confirm pending +
record overage".

```go
svc.TemplateBatchExecutor().ExecuteTemplateBatch(ctx, []core.TemplateExecutionRequest{
    {TemplateCode: "lock_funds",   Params: lockParams},
    {TemplateCode: "withdraw_fee", Params: feeParams},
})
```

### Picking the right API

| You have… | Use |
|----------|-----|
| One reusable shape, single currency, single holder | `ExecuteTemplate` |
| Several reusable shapes that must succeed together | `ExecuteTemplateBatch` |
| Cross-currency, cross-holder, or one-off entries | `PostJournal` directly |
| A whole flow tied to a long-lived state | `Booker.Transition` (with optional `EventID` linkage to a journal) |

## Extending the Ledger

You write data, not code — the same primitives the presets use are public.

### Add a custom classification

```go
clsStore := svc.Classifications()
clsStore.CreateClassification(ctx, core.ClassificationInput{
    Code:       "promotion_credit",
    Name:       "Promotion Credit",
    NormalSide: core.NormalSideCredit,
    IsSystem:   true,
    Lifecycle:  nil,                  // label-only; pass non-nil to attach an FSM
})
```

### Add a custom journal type

```go
svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{
    Code: "promo_grant",
    Name: "Promotion Grant",
})
```

### Add a custom entry template

```go
svc.Templates().CreateTemplate(ctx, core.TemplateInput{
    Code:          "promo_grant",
    Name:          "Promotion Grant",
    JournalTypeID: jtID,
    Lines: []core.TemplateLineInput{
        {ClassificationID: equityID, EntryType: core.EntryTypeDebit,  HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
        {ClassificationID: walletID, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser,   AmountKey: "amount", SortOrder: 2},
    },
})
```

You can now `ExecuteTemplate(ctx, "promo_grant", …)` from anywhere.

### Add a custom lifecycle (state machine)

A lifecycle is JSON attached to a classification. Bookings against that
classification can only transition along the declared edges.

```go
svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
    Code:       "kyc_review",
    Name:       "KYC Review",
    NormalSide: core.NormalSideDebit,
    Lifecycle: &core.Lifecycle{
        Initial:  "submitted",
        Terminal: []core.Status{"approved", "rejected"},
        Transitions: map[core.Status][]core.Status{
            "submitted": {"reviewing", "rejected"},
            "reviewing": {"approved", "rejected"},
        },
    },
})
```

`svc.Booker().Transition` will validate against this FSM. Invalid transitions
return `core.ErrInvalidTransition`.

### Add a custom channel adapter (inbound webhooks)

Implement `channel.Adapter` for any external system that needs to drive
booking transitions via signed webhooks:

```go
type StripeAdapter struct{ secret string }

func (a *StripeAdapter) Name() string { return "stripe" }

func (a *StripeAdapter) VerifySignature(h http.Header, body []byte) error {
    // verify Stripe-Signature header...
}

func (a *StripeAdapter) ParseCallback(h http.Header, body []byte) (*channel.CallbackPayload, error) {
    // unmarshal body, return BookingID + ChannelRef + Status + ActualAmount
}

svc.RegisterChannel("stripe", &StripeAdapter{secret: os.Getenv("STRIPE_SECRET")})
```

`POST /api/v1/webhooks/stripe` will now route through your adapter.

### Compose ledger writes with your own DB writes — `RunInTx`

When the ledger journal must succeed or fail atomically with rows in your own
schema, hand the ledger and the raw `pgx.Tx` to one transaction:

```go
err := svc.RunInTx(ctx, func(tx *ledger.Service) error {
    if _, err := tx.JournalWriter().ExecuteTemplate(ctx, "transfer", params); err != nil {
        return err
    }
    _, err := tx.DBTX().Exec(ctx, "INSERT INTO my_table (...) VALUES (...)")
    return err
})
```

Use `tx.DBTX()` (not `Pool()`) inside the callback — `Pool` ignores the
surrounding transaction and would commit out-of-band.

### What changes when you add what

| Want to add… | Change |
|--------------|--------|
| New classification | Insert one row via `Classifications()` |
| New journal type | Insert one row via `JournalTypes()` |
| New reusable journal shape | Insert template via `Templates()` |
| New stateful flow (e.g. KYC) | Add classification with `Lifecycle` JSON; use `Booker` |
| New webhook source | Implement `channel.Adapter`; `RegisterChannel` at boot |
| Entry-line semantics not expressible by templates | Drop to `PostJournal` directly — templates are not Turing-complete by design |
| New balance metric | Implement `core.Metrics`, pass via `WithMetrics(...)`; or wrap the Prometheus adapter |
| Non-Postgres persistence | Implement the relevant `core/*` interfaces; the domain layer does not assume Postgres |

## Observability

Three pluggable surfaces. All three default to no-op — you opt in.

### Logger

Implement `core.Logger` (`Info` / `Warn` / `Error`) and inject:

```go
import "log/slog"

type slogAdapter struct{ l *slog.Logger }
func (s slogAdapter) Info(m string, a ...any)  { s.l.Info(m, a...) }
func (s slogAdapter) Warn(m string, a ...any)  { s.l.Warn(m, a...) }
func (s slogAdapter) Error(m string, a ...any) { s.l.Error(m, a...) }

logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
svc, _ := ledger.New(pool, ledger.WithLogger(slogAdapter{l: logger}))
```

Same shape works for `zap`, `zerolog`, or any structured logger.

### Metrics

Implement `core.Metrics` (counters / histograms / gauges, full surface in
[`core/metrics.go`](core/metrics.go)) and inject. The library ships one
production-ready impl:

```go
import "github.com/azex-ai/ledger/observability"

prom := observability.NewPrometheusMetrics()
svc, _ := ledger.New(pool, ledger.WithMetrics(prom))

http.Handle("/metrics", prom.Handler())
go http.ListenAndServe(":9090", nil)
```

Exposed metrics include `ledger_journals_posted_total`,
`ledger_journal_latency_seconds`, `ledger_reservations_active`,
`ledger_pending_rollups`, `ledger_balance_drift`, `ledger_reconcile_gap`, and
more. Cardinality is bounded by design: `journalTypeCode` and `classCode` are
stable enums, currency IDs are small integers.

For OpenTelemetry, DataDog, or any other backend, write a thin adapter
against `core.Metrics`. The interface is intentionally narrow (~20 methods).

### Distributed tracing

OTEL trace propagation is automatic on the journal / booking write paths —
spans named `ledger.ledger.post_journal`, `ledger.booking.transition`, etc.,
are emitted whenever the active context has a tracer. No injection needed;
just configure the global tracer provider before calling `ledger.New`:

```go
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/sdk/trace"
)

tp := trace.NewTracerProvider(/* exporter, sampler, ... */)
otel.SetTracerProvider(tp)

// All ledger operations now emit spans into your collector.
```

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
    fee.go, transfer.go, capital.go, settlement.go, spread.go, fx.go

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

The v0.x series had hardcoded `deposit` / `withdrawal` resource types. v2 introduces classification-driven design: deposit and withdrawal are preset configurations of the generic booking lifecycle. This enables arbitrary account types (fee, capital, settlement, spread, …) without any code change in the engine. The public API is backwards-compatible; callers using the v2 facade (`ledger.New`) did not need to change.

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
- [**DR.md**](docs/DR.md) -- Backup & disaster recovery: PITR strategy, RPO/RTO targets, restore procedure, and invariant-based backup verification (quarterly drill).
- [**CAPACITY.md**](docs/CAPACITY.md) -- Benchmark baseline, sizing guide (pool/replicas/DB), suggested SLOs, and scaling signals.
- [**openapi.yaml**](docs/openapi.yaml) -- OpenAPI 3.1 contract (32 paths, 34 schemas).
- [**api.md**](docs/api.md) -- Long-form HTTP API reference with examples.
- [**frontend.md**](docs/frontend.md) -- React UI + data-layer (`@azex/ledger-react`): hooks, page components, RSC prefetch, theming, full API reference.
- [**COOKBOOK.md**](docs/COOKBOOK.md) -- Business recipes: buy credits at a 1:100 rate (FX two-leg), discounts (price / bonus / promo), adding currencies, spending via reserve→settle, cashing out, and expiry/insufficient-funds edges.

## Examples

- [**embed**](examples/embed/) -- Minimum-viable library embed: PostJournal + GetBalance with no templates, no presets, no HTTP layer.
- [**crypto-deposit**](examples/crypto-deposit/) -- Full EVM CREATE2 deposit lifecycle: classification install, booking creation, channel-adapter webhook, template-based journaling, reserve/settle, balance queries, and reconciliation.
- [**billing**](examples/billing/) -- SaaS-style metered billing: top-up wallet, reserve budget, deduct actual cost, release remainder.
- [**credits-topup**](examples/credits-topup/) -- Buy / bonus / spend / cash-out credits as a second currency; runs Cookbook recipes 1, 2b, 4, 5 end-to-end.
- [**event-subscribe**](examples/event-subscribe/) -- In-process event subscription: Worker.Subscribe, graceful shutdown.
- [**tx-compose**](examples/tx-compose/) -- Transactional composition: ledger journal + caller's own DB write in one PostgreSQL transaction; rollback on error.

## SemVer / Stability Policy

The current release series is **v0.x**. No API stability guarantees are made between minor versions while the library is in active development.

**v1.0 milestone criteria**:
- All five dimensions (deposit / withdrawal / fee / security / audit) have been exercised in at least one production deployment
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
| `API_KEYS` | Comma-separated `name:scope:secret` bearer keys (scope: `read`\|`write`\|`admin`). Required on every endpoint except probes/webhooks. | (none) |
| `MAX_BODY_BYTES` | Maximum inbound request body size in bytes | `262144` (256 KB) |
| `EVM_WEBHOOK_SECRET` | HMAC-SHA256 signing key for the EVM block-scanner webhook adapter | (channel disabled when empty) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP/HTTP collector endpoint; setting it enables trace export (standard `OTEL_EXPORTER_OTLP_*` vars apply, `OTEL_SERVICE_NAME` defaults to `ledgerd`) | (tracing disabled) |
| `MIGRATE_MODE` | Migration behavior at startup: `auto` (run, then serve), `only` (run and exit — for pre-deploy migration jobs), `off` (skip; another process owns migrations) | `auto` |
| `TRUST_PROXY_HEADERS` | `true` = derive client IP from `X-Real-IP` / last `X-Forwarded-For` hop for rate limiting and logs. Enable ONLY behind a trusted edge proxy that sets those headers; otherwise callers can spoof their IP. | `false` (socket peer) |

Other timing parameters (rollup interval, reservation TTL, reconcile / snapshot cadences, withdrawal review threshold) are set in `cmd/ledgerd/main.go`.

### Security notes

- **Authentication**: bearer-token API keys via `Authorization: Bearer <key>`, required on every endpoint (probes and webhook callbacks excepted). Keys are `name:scope:secret` triples — scope `read` < `write` < `admin`; the key name lands in access logs for audit. Constant-time compare.
- **Rate limits**: in-memory per-IP token bucket -- 100 req/min mutations, 1000 req/min reads. Single-instance only.
- **Body size**: every request is capped at `MAX_BODY_BYTES`; webhooks have an additional 1 MB cap enforced in the handler.
- **Webhook replay**: HMAC payload is `<timestamp>.<body>`; timestamps outside ±5 minutes are rejected.
- **Health vs. readiness**: `/api/v1/system/health` returns 503 on DB failure; `/api/v1/system/ready` returns 503 until migrations + worker have booted.
- **⚠️ GET endpoints are unauthenticated by design.** `authMiddleware` only enforces the bearer key on `POST`/`PUT`/`PATCH`/`DELETE` (see `server/middleware_auth.go`); every `GET` -- including balances, journals, audit trails, and the platform-wide `/platform/balances` / `/platform/solvency` endpoints -- is open to anyone who can reach the port. Per-key holder scoping for reads is a future item, not implemented today. **You must run standalone-service deployments behind a private network or a gateway that authenticates/authorizes reads** (VPC-only, mTLS, or an API gateway in front) -- do not expose the HTTP port directly to the public internet. This does not apply to library-mode consumption, where your own application owns the auth boundary.

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
