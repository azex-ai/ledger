# azex-ai/ledger

Production-grade classification-driven double-entry ledger engine for Go.
Dual-mode: importable library or standalone HTTP service.

## Features

Five-dimensional banking coverage:

```
Deposit       Pending two-phase API · EVM channel adapter · tolerance settlement
Withdrawal    Lifecycle state machine · fund locking · fee templates
Fee           First-class fee classification · fee_charge template
Security      Full reconciliation suite · solvency check · advisory lock leader election
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
- **Pending two-phase deposits** -- `AddPending` → `ConfirmPending` / `CancelPending` for in-flight deposit tracking (install separately: `presets.InstallPendingBundle`, not part of `InstallDefaultPresets`/`InstallExtendedPresets`)
- **Channel adapters** -- pluggable inbound webhook handlers (HMAC-verified) for external systems such as on-chain deposit indexers
- **Webhook delivery** -- outbound event delivery with per-attempt exponential backoff and dead-letter handling
- **In-process event subscription** -- `Worker.Subscribe` for library-mode event callbacks without a webhook server
- **Transaction composition** -- `RunInTx` lets callers combine ledger writes with their own DB writes in one atomic transaction
- **Extended preset catalogue** -- deposit, withdrawal, transfer, fee, capital, settlement, spread, and FX bundles ship out-of-the-box
- **Full reconciliation engine** -- accounting-equation verification, orphan detection, solvency check, idempotency audit, stale-rollup detection, and entries-based checkpoint/system_rollup/snapshot integrity
- **Balance trends + audit queries** -- time-series trends, reversal chains, booking traces for customer support and compliance
- **Platform solvency API** -- `PlatformBalanceReader` + `SolvencyChecker` read from the `system_rollups` materialised view in O(1)
- **Sparse daily snapshots** -- historical balance snapshots; startup backfill with advisory-lock guard for multi-replica safety
- **Prometheus / OTEL observability** -- `observability.NewPrometheusMetrics()` + OTEL trace propagation on journal/booking paths
- **Idempotent writes** -- every mutation requires an idempotency key, `Transition` included (see `docs/INVARIANTS.md` I-3); duplicates return the original result without side effects, mismatched payloads return `ErrConflict`
- **Async rollup worker** -- background checkpoint materialisation with `SKIP LOCKED` queue and leader election
- **NO NULL policy** -- all DB columns `NOT NULL` with meaningful defaults; all Go fields are value types

## Local Development with go.work

To consume the local copy of `azex-ai/ledger` from a sibling Go module, drop a
workspace file at the parent directory that supersedes the one this repo
ships (which only sees its own five modules — root, `chains/evm`,
`anchors/r2`, `anchors/r2/internal/miniotest`, `internal/postgrestest`):

```bash
cd /path/to/parent          # e.g. /Users/aaron/azex
cat > go.work <<'EOF'
go 1.26.6

use (
    ./ledger
    ./ledger/chains/evm
    ./ledger/anchors/r2
    ./ledger/anchors/r2/internal/miniotest
    ./ledger/internal/postgrestest
    ./your-consumer-module
)
EOF
```

`go` in the outer `go.work` must match the `go` directive in
[`ledger/go.mod`](go.mod) (currently `1.26.6`) — an older toolchain version
here fails immediately with `requires go >= 1.26.6`. There is no need to
remove `ledger/go.work` — both `go.work` and `go.work.sum` are tracked in
this repo (not git-ignored), and an outer workspace file always supersedes an
inner one, so the two coexist without conflict.

A plain `replace github.com/azex-ai/ledger => ../ledger` in your consumer's
`go.mod` does **not** work as a substitute for the workspace file above: the
root module's own `internal/postgrestest` requirement uses a relative
`replace`, which is not transitive to your module, so `go mod tidy` fails
with `invalid version: unknown revision 000000000000`. This only affects
local development against an unpublished checkout — `go get
github.com/azex-ai/ledger@<tag>` in a fresh module works normally and needs
no workspace file at all.

## Quick Start -- As a Library

**Prerequisite**: the connection you pass to `ledger.Migrate` must be able to
`CREATE ROLE` (superuser, or a role with the `CREATEROLE` attribute) the
first time it runs against a fresh database — the baseline schema creates
`ledger_owner`/`ledger_app`/`ledger_ro` and locks down `PUBLIC` as part of
installing the schema (`docs/RUNBOOK.md` §9 "Database roles"). Every
migration after that runs as `ledger_owner` and needs no elevated privilege.
A local Postgres superuser, or the default user in a fresh managed-Postgres
instance, satisfies this.

Two tiers: pick where you want to start.

### Tier 1 — Hello Ledger (raw entries, no presets)

The shortest path: post one balanced journal, read the resulting balance. Use
this when you want to understand the primitives or you have your own
chart-of-accounts and just need the engine.

```go
import (
    "fmt"
    "log"

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
// currencyUID / walletUID / custodyUID / jtUID are the uids those rows return;
// every dimension on EntryInput/JournalInput is referenced by uid, never by
// the internal BIGSERIAL id (api-contract.md §3: uid is the only identifier
// exposed anywhere, including this Go API).

j, err := svc.JournalWriter().PostJournal(ctx, core.JournalInput{
    JournalTypeUID: jtUID,
    IdempotencyKey: ledger.NewIdempotencyKey("hello"),
    Entries: []core.EntryInput{
        {AccountHolder: -42, CurrencyUID: currencyUID, ClassificationUID: custodyUID, EntryType: core.EntryTypeDebit,  Amount: decimal.NewFromInt(100)},
        {AccountHolder:  42, CurrencyUID: currencyUID, ClassificationUID: walletUID,  EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
    },
    Source: "api",
})
if err != nil {
    log.Fatal(err)
}

bal, _ := svc.BalanceReader().GetBalance(ctx, 42, currencyUID, walletUID)
fmt.Println("uid:", j.UID, "balance:", bal)
```

### Tier 2 — With Built-in Presets (recommended)

Install the preset bundles and you immediately get classifications, journal
types, and templates for deposits, withdrawals, fees, transfers, and more —
all idempotent on every boot.

```go
svc, _ := ledger.New(pool)
svc.InstallExtendedPresets(ctx)              // 8 bundles, see "Built-in Presets" below

// Post a deposit confirmation by template — no entry-list assembly needed.
if _, err := svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
    HolderID:       42,
    CurrencyUID:    currencyUID,
    Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
    IdempotencyKey: ledger.NewIdempotencyKey("deposit-confirm"),
    Source:         "api",
}); err != nil {
    log.Fatal(err)
}

// Or model a long-lived flow with a Booking and lifecycle transitions.
// InstallExtendedPresets installs accounting templates, not lifecycles — the
// "deposit" classification it references ships label-only, so attach a
// lifecycle before creating bookings against it. SetLifecycleIfEmpty is
// idempotent and never clobbers a lifecycle an operator has customised.
depositClass, err := svc.Classifications().GetByCode(ctx, "deposit")
if err != nil {
    log.Fatal(err)
}
if err := svc.Classifications().SetLifecycleIfEmpty(ctx, depositClass.UID, presets.DepositLifecycle); err != nil {
    log.Fatal(err)
}

booking, err := svc.Booker().CreateBooking(ctx, core.CreateBookingInput{
    ClassificationCode: "deposit",
    AccountHolder:      42,
    CurrencyUID:        currencyUID,
    Amount:             decimal.NewFromInt(100),
    IdempotencyKey:     ledger.NewIdempotencyKey("deposit"),
    ChannelName:        "evm",
})
if err != nil {
    log.Fatal(err)
}
if _, err := svc.Booker().Transition(ctx, core.TransitionInput{
    BookingUID:     booking.UID,
    ToStatus:       "confirming",
    Source:         "api",
    IdempotencyKey: ledger.NewIdempotencyKey("deposit-confirming"), // REQUIRED — I-3, see docs/INVARIANTS.md
}); err != nil {
    log.Fatal(err)
}
```

Background worker (rollup, expiry, reconcile, snapshots, partition
management, and — for anything you separately opted into — in-process event
subscription and the P6 batch attestation chain):

```go
worker := svc.Worker(service.DefaultWorkerConfig())
go worker.Run(ctx)
```

`svc.Worker(cfg)` wires everything it can build from the Service alone:
rollup/expiry/reconcile/snapshot/partition always run; `worker.Subscribe(fn)`
(in-process event callbacks, no webhook server needed) works with no extra
wiring call; and if this Service was constructed `WithAttestor`, the P6
batch attestation job runs too.
Two jobs still need an explicit call because they need something this
constructor cannot see: outbound **webhook** delivery
(`worker.SetEventDeliverer(...)`, needs a `delivery.SubscriberLister`) and
the fleet-wide **full reconciliation suite**
(`worker.SetFullReconciler(svc.FullReconciler(cfg))`, deliberately opt-in —
it is a heavier scan than the lightweight accounting-equation check that
always runs). `worker.Run` logs, once at startup, which optional jobs are
enabled.

Observability (logger / metrics / tracing) is opt-in — see [Observability](#observability) below.

## Quick Start -- Serving the HTTP API

This repository ships **no server binary**. It is a library: the domain, the
Postgres adapter, the background worker, and an optional HTTP layer you mount
in your own binary. There is nothing here to deploy.

`server.NewFromDeps` returns the full ledger API as an `http.Handler`, so a
consumer that wants the HTTP surface wires it alongside their own routes:

```go
srv, err := server.NewFromDeps(cfg, server.Deps{
    Journals: svc.JournalWriter(), Balances: svc.BalanceReader(), /* ... the rest of the stores and services you assembled ... */
})
if err != nil {
    return err // invalid cfg -- you decide how to fail, not the library
}
http.ListenAndServe(":8080", srv.Handler())
```

Prefer `NewFromDeps` over the older `server.New` / `server.NewWithConfig`:
those take twenty-one same-shaped `core` interfaces as positional
parameters, so two swapped arguments of matching interface shape compile
clean and fail at runtime — `Deps` names each dependency by field instead.
`NewFromDeps` also returns an error on an invalid `Config` rather than
panicking. See [`examples/fullstack`](examples/fullstack/) for the complete
field list.

A complete, runnable assembly -- chi router, API-key auth, the worker, and a
Next.js frontend against it -- is in [`examples/fullstack`](examples/fullstack/).
[`docs/openapi.yaml`](docs/openapi.yaml) is the wire contract that surface
implements, and what [`@azex/ledger-react`](web/packages/ledger-react/)
generates its types from.

`docker-compose.yml` here starts only PostgreSQL, for local development
(`make db`). Its `POSTGRES_USER` is the image's initial superuser, which
already satisfies the `CREATE ROLE` prerequisite above.

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
| **Classification** | Account type — "main_wallet", "pending", "fees", "equity", … Has `NormalSide` (debit-normal vs credit-normal), a `BalanceRole` (see below), and an optional `Lifecycle` state machine. Positive holder = user-side, negative = system counterpart. | `core.Classification` / `classifications` table |
| **Journal Type** | Categorises journals by intent — "deposit_confirm", "fee", "transfer". Required metadata before any post; think of it as the journal-entry kind in a chart of accounts. | `core.JournalType` / `journal_types` table |
| **Entry Template** | Reusable recipe for a balanced journal: a list of `(classification, debit/credit, holder_role, amount_key)` lines. Render with `TemplateParams` to produce a `JournalInput`. | `core.EntryTemplate` / `entry_templates` table |
| **Booking + Lifecycle** | Long-lived process record (e.g. a deposit attempt) tied to a Classification. Each `Transition` writes an Event and may post a Journal. | `core.Booking`, `core.Lifecycle` / `bookings` + `events` |

**`BalanceRole`** — every non-system Classification (`IsSystem: false`) must
declare one explicitly (`core.ClassificationInput.Validate` rejects `""`);
`is_system` classifications leave it unset. It decides what a holder's
balance breakdown, and `Reserve`, do with the money in that bucket:

| Value | Meaning |
|-------|---------|
| `core.BalanceRoleAvailable` | Immediately spendable. The **only** bucket `Reserve` draws from. |
| `core.BalanceRolePending` | Inbound funds awaiting confirmation — counted in the holder's total, not spendable yet. |
| `core.BalanceRoleLocked` | Journal-locked funds (e.g. a withdrawal in flight) — counted, not spendable. |
| `core.BalanceRoleMemo` | Deliberately excluded from the holder's spendable-money view and not a liability the platform owes back (e.g. `fee_expense`: money already paid, tracked per-holder for reporting only). |

Picking `memo` when you meant `available` makes that money invisible to
`Reserve` forever; picking `available` for a reporting-only account makes it
withdrawable. See [`docs/INVARIANTS.md`](docs/INVARIANTS.md) I-25 / I-37 for
the full contract.

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

The library ships eight preset bundles. Each is a self-contained set of
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
svc.InstallExtendedPresets(ctx)   // All 8 bundles
```

Neither installer includes the pending two-phase deposit bundle or the
developer-mode credit bundle — both are opt-in on purpose (the former adds a
whole extra deposit path, the latter mints balance against no custodied
asset) and are installed separately:

```go
presets.InstallPendingBundle(ctx, svc.Classifications(), svc.JournalTypes(), svc.Templates())
svc.InstallDevCreditPreset(ctx) // ENV=dev + DEV_CREDIT_ENABLED=true only
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
    JournalTypeUID: jtUID,
    IdempotencyKey: key,
    Entries: []core.EntryInput{
        {AccountHolder:  42, CurrencyUID: currencyUID, ClassificationUID: walletUID, EntryType: core.EntryTypeDebit,  Amount: amt},
        {AccountHolder: -42, CurrencyUID: currencyUID, ClassificationUID: feesUID,   EntryType: core.EntryTypeCredit, Amount: amt},
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
    CurrencyUID:    currencyUID,
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
    Code:           "promo_grant",
    Name:           "Promotion Grant",
    JournalTypeUID: jtUID,
    Lines: []core.TemplateLineInput{
        {ClassificationUID: equityUID, EntryType: core.EntryTypeDebit,  HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
        {ClassificationUID: walletUID, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser,   AmountKey: "amount", SortOrder: 2},
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
    IsSystem:   false,
    // kyc_review is a process-tracking classification, not a money bucket —
    // it never carries a spendable balance, so it is the BalanceRoleMemo
    // case: "deliberately excluded", not "nobody tagged this yet". Every
    // non-system classification must declare BalanceRole explicitly (see
    // "Core Concepts" above) — CreateClassification rejects "" here.
    BalanceRole: core.BalanceRoleMemo,
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
    // verify Stripe-Signature header against a.secret...
    return nil
}

func (a *StripeAdapter) ParseCallback(h http.Header, body []byte) (*channel.CallbackPayload, error) {
    // unmarshal body, return BookingUID + ChannelRef + Status + ActualAmount
    return &channel.CallbackPayload{BookingUID: "...", ChannelRef: "...", Status: "confirmed"}, nil
}

// RegisterChannel takes the adapter alone — Name() is what it registers under.
svc.RegisterChannel(&StripeAdapter{secret: os.Getenv("STRIPE_SECRET")})
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

If you've configured `WithAttestor` (per-journal authorization signing),
note that `JournalWriter` calls made *inside* this callback never sign —
there is no point inside an already-open transaction where calling out to
the Attestor wouldn't violate the "no external calls inside a DB
transaction" rule the whole signing feature depends on. To get a signed
journal out of a `RunInTx` composition, call `svc.Authorize` (or
`svc.AuthorizeTemplate` for a template-driven journal) *before* opening
`RunInTx`, then post the result via `tx.JournalWriter().PostAuthorized(...)`
instead of `PostJournal`/`ExecuteTemplate` inside the callback. Every
journal's `auth_status` column records which of the two paths was taken
(`signed`, or `unsigned_tx_mode` if you skip this step), so this is
observable after the fact rather than a silent gap.

Calling `RunInTx` again on the `*Service` your callback receives is
rejected (an error, not a second independent transaction). `AttestationService`,
`VerifyLedger`, and `EnableOnchain` are likewise rejected when called from
inside a `RunInTx` callback — each needs the top-level Service (they read or
write through the pool directly, or would set state on a clone `RunInTx`
discards when the callback returns).

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
against `core.Metrics`. The interface is intentionally wide (32 methods, one
per emitted signal, not grouped into a handful of generic Counter/Gauge/
Histogram calls) so each call site names what it means — embed
`core.NoopMetrics` and override only the handful of methods you care about
rather than writing every method body by hand:

```go
type myMetrics struct{ core.NoopMetrics }
func (m *myMetrics) JournalPosted(code string) { /* ... */ }
```

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
| `svc.HolderReader()` | `core.HolderReader` | Holder-scoped wallet read surface (balances, translated transactions, holds) — feeds `server.HolderHandler` or consume directly |
| `svc.AccountPolicies()` | `core.AccountPolicyStore` | Per-account freeze/close + balance-floor overrides |

### Deposit / pending

Requires `presets.InstallPendingBundle(ctx, ...)` — unlike most of the API
Surface below, this one is **not** included in `InstallDefaultPresets` or
`InstallExtendedPresets`; install it explicitly before using either accessor
(the calls below fail with `core.ErrNotFound` until you do).

| Method | Interface | Description |
|--------|-----------|-------------|
| `svc.PendingBalanceWriter()` | `core.PendingBalanceWriter` | AddPending / ConfirmPending / CancelPending |
| `svc.PendingTimeoutSweeper()` | `core.PendingTimeoutSweeper` | Expire stale pending deposits |

### Onchain (crypto deposit + sweep, optional)

| Method | Interface | Description |
|--------|-----------|-------------|
| `svc.EnableOnchain(chains, reader, scanner, sweeper, opts...)` | `(*service.Onchain, error)` | Wires the CREATE2 deposit + sweep subsystem (docs/plans/2026-07-11-crypto-deposit-sweep-design.md); `reader`/`scanner`/`sweeper` may each be nil to disable the corresponding background job. Call once; validates the `AutoCreditCeiling` and `ReconcileFailureLimit` fences before handing back an instance — see `examples/crypto-deposit` |
| `svc.Onchain()` | `*service.Onchain` | The subsystem `EnableOnchain` wired, or nil if it was never called |
| `svc.InstallDevCreditPreset(ctx)` | `error` | Installs the developer-credit bundle (mint balance with no custodied asset behind it) — deliberately absent from both `InstallDefaultPresets` and `InstallExtendedPresets`, opt in explicitly |

### Analytics and audit

| Method | Interface | Description |
|--------|-----------|-------------|
| `svc.BalanceTrends()` | `core.BalanceTrendReader` | Daily balance trends with inflow/outflow |
| `svc.Audit()` | `core.AuditQuerier` | Journal lists, booking trace, reversal chain |
| `svc.ConfigHistory()` | `core.ConfigChangeReader` | Forensic trail: who changed the config/lifecycle tables, the reconciliation scan cursors, or an account policy — and when |
| `svc.AssertRuntimeRole(ctx)` | `error` | Startup check: this connection authenticates as `ledger_app`, the role every ACL-enforced invariant is written against |
| `svc.PlatformBalanceReader()` | `core.PlatformBalanceReader` | Per-classification platform-wide balances |
| `svc.SolvencyChecker()` | `core.SolvencyChecker` | Custodial vs user liability check |
| `svc.PeriodCloser()` | `core.PeriodCloser` | Manages the accounting period close line |
| `svc.TrialBalanceReader()` | `core.TrialBalanceReader` | Computes a trial balance report |

### Integrity and operations

| Method | Interface | Description |
|--------|-----------|-------------|
| `svc.Reconciler()` | `core.Reconciler` | Basic accounting-equation / per-account checks (backs the HTTP reconcile endpoints) |
| `svc.FullReconciler(cfg)` | `core.FullReconciler` | Full reconciliation suite |
| `svc.SnapshotBackfiller()` | `core.SnapshotBackfiller` | Fill historical snapshot gaps |
| `svc.Worker(cfg)` | `*service.Worker` | Background jobs (rollup, expiry, reconcile, snapshots) |
| `svc.CheckpointIntegrity()` | `core.CheckpointIntegrityStore` | Trusted, entries-only balance API (`RecomputeBalance` / `RebuildCheckpoint`) that never consults `balance_checkpoints`. **Withdrawal / large-amount paths must call `RecomputeBalance` instead of `BalanceReader.GetBalance`** — see `core.CheckpointIntegrityStore`'s godoc |
| `svc.VerifiedBalanceReader()` | `core.VerifiedBalanceReader` | Withdrawal-time authorization-gated balance reader. A mechanism the library offers, not a policy it imposes — nothing calls it automatically (`Reserve` does not), so a consumer that never calls this accessor sees no behavior change. Check the returned error before trusting the amount: it can report UNDEFINED |
| `svc.AuthVerifier()` | `core.AuthVerifier` | The verifier passed to `WithAttestor`, or nil if it was never called — reach the same verifier the composition root wired in from a withdrawal gate, reconcile check, or `ledger-cli verify` |
| `svc.AttestationService(anchor)` | `(*service.AttestationService, error)` | Batch attestation over per-journal signatures, anchored externally. Errors if `WithAttestor` was never called. For `anchor`, see [Anchoring in production](#anchoring-in-production) |
| `svc.VerifyLedger(ctx, anchor, cfg)` | `service.VerifyReport` | Fail-closed tamper-evidence check against the attestation chain — see `examples/tamper-evident` |

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
| `svc.RunInTxWithOptions(ctx, opts, fn)` | `RunInTx` with explicit `pgx.TxOptions` (e.g. `pgx.Serializable`) |
| `svc.Authorize(ctx, input)` | Compute a journal's canonical digest and sign it **outside** any transaction, so a `RunInTx` write can still land signed (KMS signing is an external call; `financial.md` forbids those inside a transaction) |
| `svc.AuthorizeTemplate(ctx, req)` | `Authorize` for a template execution |
| `svc.DBTX()` | The active `pgx` executor — the pool, or the transaction when called on a `RunInTx` clone |
| `svc.Pool()` | Underlying `*pgxpool.Pool` for custom queries |
| `svc.RegisterChannel(adapter)` | Register inbound webhook channel adapter (registers under `adapter.Name()`) |
| `svc.Channels()` | Snapshot of registered adapters |
| `svc.InstallDefaultPresets(ctx)` | Install deposit + withdrawal bundles |
| `svc.InstallExtendedPresets(ctx)` | Install all 8 preset bundles |
| `svc.Ping(ctx)` | DB connectivity check (`SELECT 1`) |
| `ledger.Migrate(databaseURL)` | Run pending schema migrations |
| `ledger.NewIdempotencyKey(scope)` | Generate `scope:<16-byte-hex>` key via `crypto/rand` |
| `ledger.RetryIdempotent(ctx, scope, attempts, fn)` | Retry `fn` with the same idempotency key on every attempt — see "Retrying a failed write" below |

### Retrying a failed write

`core.IsRetryable(err)` reports whether resubmitting is safe — but only if
the retry reuses the **same** idempotency key. A fresh key on retry does not
replay the original write, it posts a second, independent one
(`api-contract.md` §9); a first attempt that times out after the journal
landed, retried with a new key, is a silent double entry. This holds
identically in library mode and HTTP mode, and is true whether or not you use
the helper below — the key must outlive the loop either way.

```go
key := ledger.NewIdempotencyKey("deposit")
for attempt := 0; attempt < 3; attempt++ {
    _, err = svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
        IdempotencyKey: key, // the SAME key every attempt
        // ...
    })
    if err == nil || !core.IsRetryable(err) {
        break
    }
}
```

`ledger.RetryIdempotent` does exactly that and makes the "regenerate the key
on retry" mistake unexpressible:

```go
err := ledger.RetryIdempotent(ctx, "deposit", 3, func(ctx context.Context, key string) error {
    _, err := svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
        IdempotencyKey: key,
        // ...
    })
    return err
})
```

It stops immediately on any non-retryable error — a rejected input or an
insufficient balance does not become correct by being sent again — and backs
off starting at 50ms, doubling, capped at 2s. Nothing in this library calls
it; it exists for callers who don't already have their own retry machinery.

## Architecture

Hexagonal: `core/` (pure domain) → `postgres/` (adapter) → `service/` (orchestration) → `server/` (optional HTTP layer, mounted by the consumer).

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
    reconcile_queries.go  ReconcileQuerier (full reconciliation suite queries)
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
    reconcile.go         Basic + full-suite FullReconciliationService
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

  cmd/ledger-cli/      Read-only investigation CLI (balance, journals, trace, reconcile, solvency)

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
Every response uses `{code,message,data}`: success carries `message:null`; failures carry `message:{text,fields?}` and `data:null`.

See [docs/api.md](docs/api.md) for the complete reference with request/response examples, and [docs/openapi.yaml](docs/openapi.yaml) for the machine-readable OpenAPI 3.1 schema.

## Documentation

- [**INVARIANTS.md**](docs/INVARIANTS.md) -- Every invariant the ledger guarantees (per-currency balance, append-only, idempotency, TOCTOU-safe reserve, money conservation, partition coverage, tamper-evident signing and attestation, …) with `Why / Enforced by / Pinned by` for each. The count grows as invariants are added — read the doc for the current list rather than a number here, which would just go stale again.
- [**RUNBOOK.md**](docs/RUNBOOK.md) -- Operational guide for on-call: reconciliation failure, solvency alert, rollup backlog, webhook backlog, idempotency collision, emergency stop.
- [**DR.md**](docs/DR.md) -- Backup & disaster recovery: PITR strategy, RPO/RTO targets, restore procedure, and invariant-based backup verification (quarterly drill).
- [**CAPACITY.md**](docs/CAPACITY.md) -- Benchmark baseline, sizing guide (pool/replicas/DB), suggested SLOs, and scaling signals.
- [**openapi.yaml**](docs/openapi.yaml) -- OpenAPI 3.1 contract (59 paths, 97 schemas).
- [**api.md**](docs/api.md) -- Long-form HTTP API reference with examples.
- [**frontend.md**](docs/frontend.md) -- React UI + data-layer (`@azex/ledger-react`): hooks, page components, RSC prefetch, theming, full API reference.
- [**COOKBOOK.md**](docs/COOKBOOK.md) -- Business recipes: buy credits at a 1:100 rate (FX two-leg), discounts (price / bonus / promo), adding currencies, spending via reserve→settle, cashing out, and expiry/insufficient-funds edges.

## Examples

- [**fullstack**](examples/fullstack/) -- Full-stack quickstart: a chi scaffold serving the complete ledger HTTP API next to its own routes, plus Next.js scaffolds rendering the `@azex/ledger-react` admin dashboard against it in both flavors (default shadcn-style skin and the `/heroui` skin).
- [**embed**](examples/embed/) -- Minimum-viable library embed: PostJournal + GetBalance with no templates, no presets, no HTTP layer.
- [**crypto-deposit**](examples/crypto-deposit/) -- Full EVM CREATE2 deposit lifecycle: classification install, booking creation, channel-adapter webhook, template-based journaling, reserve/settle, balance queries, and reconciliation.
- [**billing**](examples/billing/) -- SaaS-style metered billing: top-up wallet, reserve budget, deduct actual cost, release remainder.
- [**credits-topup**](examples/credits-topup/) -- Buy / bonus / spend / cash-out credits as a second currency; runs Cookbook recipes 1, 2b, 4, 5 end-to-end.
- [**event-subscribe**](examples/event-subscribe/) -- In-process event subscription: Worker.Subscribe, graceful shutdown.
- [**tamper-evident**](examples/tamper-evident/) -- Per-journal signing, batch attestation to an external anchor, and the
  `RequireVerifiedBalance` withdrawal gate. Forges a balanced journal by direct SQL -- the way a stolen `DATABASE_URL`
  would -- and shows the gate refusing to pay it out while an ungated reserve happily does. Expects an empty database.
  It anchors to `anchordev`, a local file, which is **not** tamper-evident storage -- see
  [Anchoring in production](#anchoring-in-production) for what to swap it for.
- [**tx-compose**](examples/tx-compose/) -- Transactional composition: ledger journal + caller's own DB write in one PostgreSQL transaction; rollback on error.

## Tamper-evident signing (optional)

`ledger.WithAttestor(attestor core.Attestor, authVerifier core.AuthVerifier)`
is the single entry point for the whole per-journal signing / batch
attestation / verified-balance chain below — without it every journal is
posted `auth_status=unsigned_no_attestor` and none of this section applies.
`authdev.NewLocalAttestor` is the dev/test implementation (ed25519 key held
in memory); a production `core.Attestor` needs the signing key to live
outside the failure domain `DATABASE_URL` lives in — see
`examples/tamper-evident` for a complete, runnable walkthrough (forges a row,
shows the gate refusing to pay it out).

`ledger.WithSilentWorker()` opts a `Worker` out of `Run`'s refusal to start
when its logger is `core.NopLogger` — see [Observability](#observability)
above; most consumers should inject a real logger instead.

`ledger.WithCustodialClassCodes(codes ...string)` overrides which
classification codes `SolvencyChecker` treats as custodial assets (default:
`custodial`, `settlement`) — set it if your deployment's custodial
classification isn't named `custodial`.

## Anchoring in production

`core.Anchor` is the outermost link of the tamper-evidence chain: it publishes
each attestation head somewhere this ledger's own database credentials cannot
reach. Everything below it -- per-journal signatures, batch attestation, the
`RequireVerifiedBalance` gate -- is verified *against* the anchor, so an anchor
the attacker can also rewrite makes the rest decorative.

**What ships:**

| Package | Use |
|---|---|
| `anchordev` | Local file. **Dev and tests only** -- same machine, same user as the database it is supposed to be independent of. |
| `anchors/r2` | Cloudflare R2 with Object Lock, in a separate module so its S3 SDK never enters your dependency graph. Deployment steps -- separate account, bucket configuration, and the two credential scopes -- are in `docs/RUNBOOK.md`. **Not yet independently `go get`-able** (`docs/RUNBOOK.md`'s "Consuming the submodule today"): consume it from a local checkout via the parent-directory `go.work` above, not `go get github.com/azex-ai/ledger/anchors/r2@<tag>` -- that does not yet resolve. |

**Writing your own.** Object storage with a compliance-mode retention lock, a
public chain, an RFC 3161 timestamp authority and an append-only database in a
different failure domain are all defensible carriers; they differ in whether
they can also prove your books to a third party, and in what they cost per
publish. `docs/RUNBOOK.md` ("Choosing an Anchor carrier") lists the four
properties any carrier must have. Whatever you pick, it has to hold under one
question: *if the attacker holds the ledger's database credentials, can they
also alter what has already been published?* If yes, it is not an anchor.

**Verify it before you trust it.** The contract -- idempotent publish per seq,
mismatched bytes rejected, `Head` read from the carrier and never from the
ledger database -- is machine-checkable, and any implementation can self-test
in one line:

```go
func TestMyAnchorConformance(t *testing.T) {
    // The factory must hand out fresh clients pointed at ONE carrier -- the
    // suite constructs a second client to check that what the first published
    // is really on the carrier and not in its memory. Resolve the bucket (or
    // path, or table) once, outside the closure.
    bucket := newTestBucket(t)
    anchortest.RunConformance(t, func() core.Anchor { return newMyAnchor(bucket) })
}
```

`anchors/r2` runs it against a real Object-Lock bucket in its own tests. An
anchor that has not passed it is an unverified assumption sitting at the point
the whole chain terminates.

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

`server.LoadConfig()` reads (used by `server.New`; `server.NewFromDeps` and
`server.NewWithConfig` take an explicit `*server.Config` instead and read
nothing from the environment):

| Variable | Description | Default |
|----------|-------------|---------|
| `ENV` | Deployment environment; anything other than `dev` enables production guards | `production` |
| `CORS_ALLOWED_ORIGIN` | Allowed CORS origin. Required in non-dev `ENV` -- the service refuses to boot without it. | (required outside dev) |
| `HOLDER_TOKEN_SECRET` | HMAC signing key for holder-scoped wallet tokens (`/api/v1/holder/*`), at least 32 bytes | (holder surface disabled when empty) |
| `MAX_BODY_BYTES` | Maximum inbound request body size in bytes | `262144` (256 KB) |
| `API_KEYS` | Comma-separated `name:scope:secret` bearer keys (scope: `read`\|`write`\|`admin`). Required on every endpoint except probes/webhooks. | (none) |
| `TRUSTED_PROXY_CIDRS` | Comma-separated CIDR ranges of your trusted edge proxies (e.g. `10.0.0.0/8,172.16.0.0/12`). When set, the client IP is derived from `X-Forwarded-For` (walked right-to-left, skipping trusted hops) / `X-Real-IP` / `True-Client-IP` for rate limiting and logs — but **only** for requests whose socket peer is inside these ranges, so a direct caller cannot spoof its IP. Every candidate is IP-validated. Invalid value = startup error. | (empty; socket peer) |
| `DEV_CREDIT_ENABLED` | Enables `POST /api/v1/dev/credits` (mints holder balance against no custodied asset). Requires `ENV=dev`; boot fails otherwise. | `false` |
| `PROTECTED_TEMPLATE_CODES` | Comma-separated extra template codes to protect beyond the structural rule (any template with a leg on an `is_system` classification) and the built-in list — see "write scope and system classifications" below | (built-in list only) |
| `ALLOW_GENERIC_TEMPLATE_POST` | Comma-separated template codes exempted from the protected-template gate (`POST /journals/template` normally refuses these) | (none exempted) |

`DATABASE_URL` / `HTTP_PORT` / migration-on-boot behavior are read by your
own composition root, not by this library — see
[`examples/fullstack`](examples/fullstack/). OpenTelemetry export is
likewise your own setup: install an OTLP exporter and set the global tracer
provider before calling `ledger.New` (see [Observability](#observability)
below); the standard `OTEL_EXPORTER_OTLP_*` env vars apply to whatever
exporter you install, not to code in this repo.

Other timing parameters (rollup interval, reservation TTL, reconcile / snapshot cadences, withdrawal review threshold) live in `service.WorkerConfig` and the option functions on `ledger.New`; `examples/fullstack` shows them being set.

### Security notes

- **Authentication**: bearer-token API keys via `Authorization: Bearer <key>`, required on every endpoint (probes and webhook callbacks excepted). Keys are `name:scope:secret` triples — scope `read` < `write` < `admin`; the key name lands in access logs for audit. Constant-time compare.
- **Rate limits**: in-memory per-IP token bucket -- 100 req/min mutations, 1000 req/min reads. Single-instance only.
- **Body size**: every request is capped at `MAX_BODY_BYTES`; webhooks have an additional 1 MB cap enforced in the handler.
- **Webhook replay**: HMAC payload is `<timestamp>.<body>`; timestamps outside ±5 minutes are rejected.
- **Health vs. readiness**: `/api/v1/system/health` returns 503 on DB failure; `/api/v1/system/ready` returns 503 until migrations + worker have booted.
- **`write` scope and system classifications.** `POST /journals` accepts handwritten, per-currency-balanced entries, but by default **refuses any entry touching an `is_system` classification** (custodial, suspense, equity, …) — the handwritten-path counterpart to the protected-template guard, so a leaked `write`-scope key cannot mint deposit-shaped accounting through either endpoint (`docs/INVARIANTS.md` I-38). A deployment that legitimately hand-posts system-side journals over HTTP sets `Config.AllowSystemClassificationPost` (logs a startup warning). Non-system journals are unaffected. `write` scope still grants broad authority — don't issue it to a party that shouldn't record accounting.
- **Authentication scope.** When `API_KEYS` is set, `authMiddleware` requires a valid bearer key on **every** endpoint regardless of HTTP method -- reads included (`server/middleware_auth.go`). The only exemptions are the unauthenticated probe paths (`/system/health`, `/system/ready`) and the inbound webhook paths (which authenticate via their channel's HMAC signature instead). The holder-token surface (`/holder/*`) authenticates with a minted holder token rather than an API key. Per-key holder scoping for the platform-wide read endpoints (`/platform/balances` / `/platform/solvency`) is not implemented -- any valid `read`-scope key can call them -- so still front standalone deployments with a network boundary you control. When `API_KEYS` is empty the server logs a startup warning and serves every endpoint unauthenticated; never run that way in production. This does not apply to library-mode consumption, where your own application owns the auth boundary.

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
