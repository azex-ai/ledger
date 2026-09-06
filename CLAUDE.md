# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

# azex-ai/ledger

Production-grade double-entry ledger engine for Go. Classification-driven architecture — deposit/withdrawal are preset configurations, not hardcoded types. Dual-mode: importable library or standalone HTTP service.

## Tech Stack

- Go 1.26+, chi v5, pgx v5, sqlc, shopspring/decimal
- PostgreSQL 17 (only supported DB)
- Next.js 16 + shadcn/ui + Tailwind v4 (web dashboard in `web/`)

## Architecture

Hexagonal: `core/` (pure domain) -> `postgres/` (adapter) -> `service/` (orchestration) -> `server/` (optional HTTP layer the consumer mounts)

- **Two consumption modes share one composition core:**
  - **Library mode** — root package `ledger` (`ledger.go`) is the facade. `ledger.New(pool *pgxpool.Pool)` returns a `*ledger.Service`; pull interfaces via `svc.Booker()`, `svc.BalanceReader()`, `svc.JournalWriter()`, etc. Consumers depend only on `core/*` interfaces, never on the `postgres` adapter directly.
  - **HTTP layer** — `server.NewFromDeps(cfg, server.Deps{...})` returns the same pieces behind chi handlers as an `http.Handler` (prefer it over the positional `server.New` / `server.NewWithConfig`, which take twenty-one same-shaped interfaces and panic on an invalid config). This repository ships no binary; the consumer mounts it in theirs. `examples/fullstack` is a complete assembly.
  - `svc.RunInTx(ctx, func(tx *ledger.Service) error {...})` composes ledger writes with the caller's own DB writes in one atomic pgx transaction. The `*Service` passed in is a short-lived clone — do not retain it past the callback.
- `core/` — zero external dependencies. No net/http, pgx, slog, chi imports allowed.
- `pkg/` — boundary adapters kept out of `core/`: `bizcode` (error-code taxonomy), `httpx` (HTTP response envelope), `otel` (tracing), `slogadapter` (logging). Error-code mapping happens at the handler boundary, not in the domain.
- Interfaces defined in `core/interfaces.go`, consumer-side, -er suffix.
- Account dimensions: `(AccountHolder int64, CurrencyID int64, ClassificationID int64)`. Positive holder = user, negative = system counterpart.
- All amounts: `shopspring/decimal.Decimal` in Go, `NUMERIC(30,18)` in SQL, string in JSON.
- **No NULL**: All DB columns NOT NULL with meaningful defaults (0, '', 'epoch', '{}'). **Exceptions** (FK target columns where 0 means "absent" must be nullable so PostgreSQL can enforce referential integrity): `journals.reversal_of`, `bookings.journal_id`, `bookings.reservation_id`, `events.journal_id`, `reservations.journal_id`, `journals.event_id`.
- **Single-direction data flow**: Ledger never calls external systems. Commands in, events out.
- **Event-Journal atomicity**: When a booking transition causes accounting, compose `Booker().Transition` + `JournalWriter().PostJournal/ExecuteTemplate` inside `Service.RunInTx`, and pass `EventID` so `events.journal_id` and `bookings.journal_id` are linked atomically. `bookings.journal_id` is **set-once** — each booking's lifecycle may have at most one journal-bearing transition (the one that triggers settlement). Subsequent EventID-bearing journals on the same booking will fail with `ErrConflict`. Use multiple events without `EventID` (or rely on `events.journal_id` per event) when modeling lifecycles where every transition records accounting.

### Core Concepts

- **Classification** — the primary entity. Each classification can have a Lifecycle (state machine).
- **Booking** — an instance of a classification's lifecycle (replaces v1 Deposit/Withdrawal). "Book a deposit", "book a withdrawal" is standard banking terminology.
- **Event** — atomic record of state transitions, written with the booking update.
- **Journal** — double-entry accounting record, linked to the triggering event via `event_id`.
- **Reservation** — cross-classification fund locking mechanism.
- **Presets** — deposit/withdrawal are pre-built classification lifecycle configs in `presets/`.
- **Channel Adapter** — inbound webhook parsing for external systems (in `channel/`).

## Key Commands

Prefer the `Makefile` targets — they encode the canonical flags:

```bash
make build      # go build ./...
make test       # go test -race -timeout 15m -count=1 ./...   (needs PostgreSQL — testcontainers, no mocks; refuses to run when Docker is unreachable and DATABASE_URL is unset)
make test-short # go test -short -race -count=1 ./...
make test-submodules # chains/evm + anchors/r2 (separate modules — `./...` never reaches them)
make test-e2e   # chains/evm's `//go:build e2e` tests (needs anvil on PATH)
make vet        # go vet ./...
make lint       # golangci-lint run
make sqlc       # cd postgres && sqlc generate
make sqlc-diff  # cd postgres && sqlc diff   (CI gate: generated code must match queries)
make db         # docker compose up -d postgres   (local dev database only)
make openapi-check # cd web && npm run -w @azex/ledger-react codegen:check (docs/openapi.yaml vs. web/packages/ledger-react's generated schema.ts; needs `npm ci` in web/ once)

# Unit tests only (no DB needed)
go test ./core/... ./presets/... ./channel/... ./service/delivery/... -count=1

# Run a single test
go test ./postgres/ -run TestName -race -count=1
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
7. DI wiring in the consumer's composition root (`examples/fullstack/backend/main.go` shows one)
```

## Code Conventions

- Struct JSON tags: snake_case, all exported fields must have tags
- Error wrapping: `fmt.Errorf("module: action: %w", err)`
- Never discard errors (except in tests)
- **No NULL**: All DB columns NOT NULL, all Go fields are value types (int64, string, time.Time), never pointers. Use 0/''/epoch/{} as defaults. **Exceptions** (FK target columns where 0 means "absent" must be nullable so PostgreSQL can enforce referential integrity): `journals.reversal_of`, `bookings.journal_id`, `bookings.reservation_id`, `events.journal_id`, `reservations.journal_id`, `journals.event_id`. At the `postgres` adapter layer these are `pgtype.Int8`; `core` exposes each as an empty-string-means-absent `string` field (`core.Booking.JournalUID`, `core.Event.JournalUID`, `core.Reservation.JournalUID`, `core.Journal.EventUID`), never `*int64`.
- Idempotency: every mutation requires an `idempotency_key` (UNIQUE index); same key + same payload must resolve to the original result, while same key + different payload must raise `ErrConflict`
- Journal entries: append-only, corrections via reversal journal only
- Balance: `checkpoint.balance + SUM(entries WHERE id > checkpoint.last_entry_id)`
- Concurrency: `SELECT FOR UPDATE` on balance writes, advisory locks for reservations
- DB transactions: no external API calls inside a transaction

## Testing

- Integration tests use `testcontainers-go` with real PostgreSQL — no mocked DB.
- Test files: `postgres/*_test.go` for store tests, `service/*_test.go` for service tests.
- Unit tests: `core/*_test.go`, `presets/*_test.go`, `channel/onchain/*_test.go`.
- CI runs (`.github/workflows/go-verify.yml`, shared by `ci.yml` and `go-release.yml`): `go vet`, `golangci-lint`, `go test -race -timeout 15m -count=1` (root + `chains/evm` + `anchors/r2`), the three `core` fuzz targets for 30s each, `chains/evm`'s e2e tests, `govulncheck`, `sqlc diff`, `go build`.

## File Layout Quick Reference

| Path | Purpose |
|------|---------|
| `ledger.go` (root pkg) | Library facade: `ledger.New(pool)` -> `Service` + accessors + `RunInTx` |
| `idempotency.go` (root pkg) | Idempotency key helpers for library consumers |
| `pkg/bizcode/` | Error-code taxonomy (mapped to HTTP at handler boundary) |
| `pkg/httpx/` | HTTP response envelope |
| `pkg/otel/`, `pkg/slogadapter/` | Tracing + logging adapters |
| `core/types.go` | Currency, Classification + Lifecycle, JournalType, Balance, Status |
| `core/booking.go` | Booking, CreateBookingInput, TransitionInput |
| `core/event.go` | Event, EventFilter |
| `core/journal.go` | Journal, Entry, JournalInput + validation |
| `core/template.go` | EntryTemplate, Render() |
| `core/reserve.go` | Reservation state machine |
| `core/checkpoint.go` | BalanceCheckpoint, RollupQueueItem, BalanceSnapshot |
| `core/interfaces.go` | Booker, EventReader, JournalWriter, BalanceReader, etc. |
| `presets/` | Deposit + Withdrawal + Transfer + Fee + Capital + Settlement + Spread + FX bundles |
| `presets/fx.go` | Cross-currency FX preset (sell + buy templates, settlement absorbs net) |
| `presets/devcredit.go` | Developer-mode credit preset: `dev_credit` classification + template. Excluded from `InstallExtendedPresets` — opt in via `svc.InstallDevCreditPreset` |
| `channel/adapter.go` | ChannelAdapter interface for inbound webhooks |
| `channel/onchain/evm.go` | Demo EVM adapter with HMAC verification |
| `postgres/sql/migrations/` | Schema migrations (embed.FS) |
| `postgres/sql/queries/` | sqlc query files |
| `postgres/sqlcgen/` | Generated code (do not edit) |
| `postgres/booking_store.go` | Booker + BookingReader implementation |
| `postgres/event_store.go` | EventReader + delivery polling |
| `postgres/invariants_test.go` | Postgres-backed pins for I-2 / I-3 / I-12 / I-13 |
| `postgres/benchmarks_test.go` | Bench: PostJournal / GetBalance / Reserve+Settle |
| `anchortest/conformance.go` | Reusable conformance suite for `core.Anchor` implementations — any anchor self-tests in one line (I-48) |
| `anchordev/local_file.go` | Local-file Anchor. **Dev/test only** — same machine as the database it must be independent of |
| `anchors/r2/` | Cloudflare R2 + Object Lock anchor. **Separate Go module** so its S3 SDK stays out of the root `go.mod` (same reason as `chains/evm`) |
| `observability/prometheus.go` | core.Metrics impl on prometheus/client_golang |
| `server/routes.go` | All endpoint definitions |
| `server/handler_bookings.go` | Unified booking endpoints |
| `server/handler_webhooks.go` | Inbound channel callbacks |
| `server/handler_devcredit.go` | Developer-mode credit endpoint (gated by `Config.DevCreditEnabled`, ENV=dev only) |
| `server/handler_events.go` | Event query endpoints |
| `server/handler_dead_letters.go` | Deposit dead-letter queue: read-only list + the replay that re-drives one sighting through `IngestDeposit` (gated by `CapabilityDepositReview`; `SetDeadLetterService`, off by default) |
| `service/delivery/` | Event delivery: callback (library) + webhook (service) |
| `service/worker.go` | Background job runner |
| `cmd/ledger-cli/` | Investigation CLI (balance, balances, journals, journal, trace, reconcile, solvency, trial-balance, health, verify, currencies, classifications, rollup, dead-letters, reorgs, config-history), read-only except `reconcile --full`'s resume cursor, `rollup reset-claim` and `reorgs resolve` (see the package doc) |
| `examples/` | Runnable library-mode examples: `embed` (minimum-viable), `billing` (reserve→metered deduction→release), `credits-topup` (buy/bonus/spend/cash-out), `crypto-deposit` (end-to-end EVM deposit), `event-subscribe` (Worker.Subscribe), `tx-compose` (caller write + journal in one tx), `tamper-evident` (signing + attestation + the withdrawal gate, forges a row to show what it stops), `fullstack` (chi scaffold serving the ledger HTTP API + Next.js scaffold rendering `@azex/ledger-react`) |
| `web/packages/ledger-react/` | `@azex/ledger-react` npm package (published via `ledger-react-v*` release tag). Nine JS entry points in `package.json`'s `exports` (two skins × operator/wallet surface, each with a recharts-isolating `/charts` split where it has chart pages, plus `./headless` and a server-only `./server`) — the package README and `docs/frontend.md` carry the full table; do not re-list them here. Two skins: root = shadcn-style (self-contained scoped preflight + tokens in `dist/styles.css`), `./heroui` = HeroUI v3 (optional peer `@heroui/react`, host owns theme, layout classes in `dist/heroui.css`). Both share one headless core (`./headless`); page logic must stay mirrored (a11y annotation *style* may differ per skin -- `<Label htmlFor>` vs `aria-label` -- the skin-parity gate compares behaviour and a hardening census, not markup) |
| `docs/INVARIANTS.md` | The invariants the ledger guarantees (canonical contract; numbered gaplessly from I-1, count is gated by `core.TestInvariantsDocIsOrderedAndGapless`, not maintained by hand) |
| `docs/RUNBOOK.md` | Operational guide for on-call engineers |
| `docs/DR.md` | Backup & disaster recovery: PITR, RPO/RTO, restore drill, invariant-based verification |
| `docs/CAPACITY.md` | Benchmark baseline, sizing guide, suggested SLOs, scaling signals |
| `docs/openapi.yaml` | Machine-readable OpenAPI 3.1 spec |
| `docs/frontend.md` | @azex/ledger-react usage guide + full API reference |
| `docs/audits/2026-08-25-financial-engineering/` | Eight-territory financial-engineering audit. Snapshot of what it found; its README carries the disposition. Read it before re-litigating a design choice it already examined |
| `docs/audits/2026-09-02-deep-audit/` | Ten-territory whole-repo deep audit (round 2) + lead cross-analysis. Same shape: territory reports are the snapshot, its README and `lead-verification.md` are the two files kept current with disposition |
| `docs/audits/2026-09-03-independent-review/` | Five zero-context agents re-auditing the remediated tree (round 3), plus `recheck/`. Its headline finding — nothing guarded the INSERT path — is what migrations 029-032 answer |
| `docs/api.md` | Long-form HTTP API reference (envelope, error codes, per-endpoint request/response examples) |
| `docs/BREAKING.md` | Per-symbol / per-endpoint breaking-change log; `TestAPISurface_BreakingChangesAreDocumented` fails when a changed exported symbol is missing here |
| `docs/COOKBOOK.md` | Business recipes: buy credits (FX rate), discounts, multi-currency, reserve→settle, cash-out, expiry/insufficient edges |

## HTTP API Quick Reference

```
# Bookings (unified — replaces deposits + withdrawals)
POST   /api/v1/bookings                    — Create booking
POST   /api/v1/bookings/{uid}/transition   — State transition
GET    /api/v1/bookings/{uid}              — Get booking
GET    /api/v1/bookings                    — List bookings

# Webhooks (inbound channel callbacks)
POST   /api/v1/webhooks/{channel}            — Receive channel callback

# Events (outbound)
GET    /api/v1/events/{uid}                  — Get event
GET    /api/v1/events                        — List events

# Developer mode (off by default; ENV=dev + DEV_CREDIT_ENABLED=true only)
POST   /api/v1/dev/credits                   — Credit a holder with no custodied asset

# Journals, Entries, Balances, Reservations — unchanged from v1
# Classifications, Journal Types, Templates, Currencies — unchanged
# Reconciliation, Snapshots, System — unchanged

# Added since v1 (see server/routes.go for the authoritative list, docs/api.md for the reference)
GET    /api/v1/audit/*                       — journal lists, booking trace, reversal chain
GET    /api/v1/platform/{balances,solvency}  — platform-wide balances + solvency
GET    /api/v1/balances/trends               — daily balance series
POST   /api/v1/periods/close, GET /periods/closes, GET /reports/trial-balance
PUT    /api/v1/accounts/{holder}/policy, GET /accounts/{holder}/policies
POST   /api/v1/holder-tokens, GET|POST /api/v1/holder/*   — holder-token wallet surface
*      /api/v1/{holders/{holder}/deposit-address,deposits/reviews,deposits/dead-letters}
       — crypto-deposit add-on; 503/18102 until the consumer wires it
```

## Gotchas

- `go.work` spans four modules: root `.`, `chains/evm`, `anchors/r2`, and `anchors/r2/internal/miniotest`. The PostgreSQL fixture is the root's `internal/postgrestest` package, imported only by tests. Keeping it in the root removes the unpublishable zero pseudo-version that broke `go mod tidy` for external consumers. Testcontainers/Docker dependencies appear in test/module metadata but do not enter a production `go list -deps github.com/azex-ai/ledger`; verify consumers with `make test-consumer`. R2 and its MinIO fixture still have separate module/release constraints (README).
- `postgres/sqlcgen/` is generated — never edit manually, always `sqlc generate`.
- sqlc config is at `postgres/sqlc.yaml`, run sqlc from `postgres/` dir.
- Migrations use `golang-migrate/migrate/v4` with embedded FS.
- `web/` is a separate Next.js project with its own `CLAUDE.md`; `@azex/ledger-react` lives in `web/packages/ledger-react/`.
- Releases: Go module via `go-release.yml` on version tags; `@azex/ledger-react` published to npm by `ledger-react-publish.yml` on `ledger-react-v*` tags (version taken from `package.json`).
- To consume the local checkout from a sibling module, use a parent-directory `go.work` (see README "Local Development with go.work") — no `replace` directives.
- Lifecycle is optional on Classification — nil means label-only (no bookings).
- `failed` is NOT terminal in withdrawal preset (has retry path to `reserved`).
- `chains/evm`'s e2e tests are tagged `//go:build e2e` — `make test-e2e` or an explicit `-tags e2e` is required to compile/run them; plain `make test` / `go test ./...` never touches them.
