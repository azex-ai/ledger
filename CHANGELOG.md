# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Two independently versioned artifacts live in this repo:

- the **Go module** `github.com/azex-ai/ledger`, tagged `vX.Y.Z`;
- the **npm package** `@azex/ledger-react`, tagged `ledger-react-vX.Y.Z`.

Entries below note which artifact a change affects.

## [Unreleased]

## [0.3.1] - 2026-07-03

### Fixed
- CI: period-close reopen test compared a nanosecond Go timestamp against its
  microsecond Postgres round-trip (passed on Darwin, failed on Linux runners);
  timestamps are now truncated to microseconds in tests.
- `@azex/ledger-react`: regenerated `src/client/schema.ts` from the v0.3
  `docs/openapi.yaml` (the codegen:check CI gate was failing).

## [0.3.0] - 2026-07-03

### Go module — Added (financial-core hardening, design: docs/plans/2026-07-02-financial-core-hardening-design.md)
- **Effective date** (migration 025): `journals.effective_at` / `journal_entries.effective_at`
  separate business date from posting date. Backdating allowed (future rejected, 5min
  tolerance); real-time balances stay posting-ordered; as-of reads (`ListBalancesAt`,
  trends, snapshots) switch to the effective axis.
- **Accounting period close** (migration 026): append-only `period_closes` line;
  posting before the line fails with `ErrPeriodClosed` (14009); reopen = append an
  earlier line (latest-row-wins, audited). `POST /periods/close`, `GET /periods/closes`.
- **Trial balance**: `GET /reports/trial-balance` + `TrialBalanceReader` +
  `ledger-cli trial-balance`, on the effective axis.
- **Currency exponent & money primitives** (migration 027): `currencies.exponent`
  (JPY=0 … wei=18); every write path rejects over-precise amounts with
  `ErrPrecisionExceeded` (14006) — never silently rounds. `core.Round` (4 modes),
  `core.Allocate` (largest-remainder, sum-preserving), `core.ConvertAt`.
  HTTP currency creation requires an explicit exponent (pointer DTO — 0 is legal).
- **Account policies** (migration 028): per-(holder[,currency[,classification]])
  freeze/close + `min_balance` floor (negative = credit limit, 0 = no overdraft,
  positive = dust floor). Frozen blocks consumption (Reserve + net-negative journals)
  while pending-deposit confirmation still lands; closed blocks both directions.
  `ErrAccountFrozen` (14007) / `ErrAccountClosed` (14008). Enforced inside the
  existing per-dimension advisory locks; policy changes are audit-logged.
- **Partial reversal** (migration 029): `ReverseJournalFraction(num/den)` — multiple
  partial reversals per journal, per-currency balanced via `Allocate`, cumulative
  conservation enforced under the original's row lock; `num==den` reverses the exact
  remainder. `POST /journals/{id}/reverse-partial`.
- **Partial settlement**: `SettlePartial` / `FinalizeSettlement` activate the
  `settling` reservation state; the unsettled remainder stays held against the
  balance (over-commit window closed); expired settling reservations auto-finalize.
  `POST /reservations/{id}/settle-partial`, `POST /reservations/{id}/finalize`.
- Invariants I-14 (effective-date consistency), I-15 (close line is a hard write
  barrier), I-16 (precision bounded by exponent), I-17 (account policy enforcement);
  I-2 revised for cumulative partial reversals; I-11 extended to settling holds.
- **Inbound webhook replay cache** (migration 030): identical callbacks resent
  inside the signature timestamp window are rejected with 409 (previously relied
  solely on downstream transition idempotency). Wired in service mode via
  `Server.SetWebhookNonceRecorder`; optional for library consumers.
- `Lifecycle.Version` field (0/1 equivalent today) — a hook for future
  lifecycle-shape evolution.

### Go module — Breaking (v0.3 cleanups)
- All `Metadata` fields are now `map[string]string` (`Booking`, `TransitionInput`,
  `Event`, `channel.CallbackPayload`) — matching journals/pending. Pre-existing
  JSONB rows with non-string values are read back as their compact JSON text.
- `Reserver.Settle` / `Reserver.SettlePartial` take `SettleInput` /
  `SettlePartialInput` structs (Input + Validate discipline).
- `@azex/ledger-react`: `createCurrency` requires `exponent`; the currencies
  form gains a required decimal-places field (0 is legal — JPY — so the field
  cannot default).

### Go module — Added
- **Audit / platform reads over HTTP** — the read capabilities previously only
  reachable via the library facade and `ledger-cli` are now HTTP endpoints:
  `GET /audit/journals` (by account or time range), `GET /audit/bookings/{id}/trace`,
  `GET /audit/journals/{id}/reversals`, `GET /platform/balances`,
  `GET /platform/solvency`, `GET /balances/trends`. All documented in
  `docs/openapi.yaml`.
- **Full reconciliation is now runnable in service mode** — the 10-check suite is
  wired into the background worker (`FULL_RECONCILE_INTERVAL`, default 1h, leader-
  elected) and exposed as `POST /reconcile/full`. Check #2 (fleet-wide
  checkpoint-vs-entries scan) is now a real keyset-paginated scan with a scan
  limit + timeout guard that reports partial coverage instead of false passes.
- `bizcode.Retryable(code)` + a `retryable` field on the HTTP error envelope —
  machine-readable retry semantics (retry only with the same idempotency key);
  contract documented in `docs/api.md`.
- Per-subscriber webhook delivery health: `webhook_subscribers` gains
  `last_status_code` / `last_error` / `last_attempt_at` (migration 024), written
  after every delivery attempt.
- Delivery / reconcile / rollup observability: new `core.Metrics` methods
  `EventDelivered`, `EventDeliveryFailed`, `EventDead`, `RollupItemFailed`,
  `ReconcileCheckResult`, implemented by `observability.PrometheusMetrics`.
- `journal_entries` primary key `(id, created_at)` (migration 022) and a
  covering index for `ListReservationsByAccount` (migration 023).

### Go module — Fixed
- `JournalInput.Validate` now rejects non-positive `currency_id` /
  `classification_id` at the domain boundary (previously only the DB FK caught it).
- `Settle` rejects non-positive and over-reserved amounts with
  `core.ErrInvalidInput` before hitting the DB constraint.
- `Lifecycle.Validate` rejects states unreachable from `Initial` (island states).
- Worker cleanup paths (`ReleaseRollupClaim`, advisory-lock release) now run on a
  detached 5s context so shutdown no longer strands claims until lease expiry.
- Expiration scans process the earliest-expiring items first
  (`ORDER BY expires_at`); expected multi-replica transition races log at Info.
- Added the missing down migration for 020.

### Go module — Breaking (v0.x)
- `server.New` / `server.NewWithConfig` take five new trailing dependencies
  (audit, platform balances, solvency, balance trends, full reconciler).
- `core.Metrics` has five new methods — implementations written from scratch
  must add them (embedding implementations are unaffected).
- `delivery.NewWebhookDeliverer` takes a `core.Metrics` argument;
  `delivery.SubscriberLister` gains `RecordDeliveryStatus`.

### Documentation
- `docs/RUNBOOK.md`: webhook delivery contract (at-least-once, retries reorder,
  consumers must dedupe on `X-Ledger-Event-ID`), fixed the subscriber-health
  troubleshooting SQL, and a new "unauthenticated reads" deployment-boundary
  section (also in `README.md`).
- `docs/INVARIANTS.md`: idempotency-key lifecycle note (I-3) and partition
  rollout status (I-13).
- `channel.Adapter`: replay-protection responsibility split documented.
- `docs/openapi.yaml` `info.version` now tracks the Go module version.

## [0.2.0] - 2026-07-02

### Go module — Added
- `Reserver.HeldAmount(ctx, holder, currencyID)` — returns the sum of the holder's
  active reservations in a currency (the figure Reserve subtracts from balance to
  check availability). Consumers can now derive `available = balance − held`
  through the interface instead of querying the `reservations` table directly.

### Documentation
- `docs/COOKBOOK.md` — business recipes: buy credits at a fixed rate (FX two-leg),
  discounts (price / bonus / promo), adding currencies, reserve→settle spend,
  cash-out, and expiry/insufficient-funds edges.
- `examples/credits-topup` — runnable end-to-end program for the above.

### Build / CI
- Toolchain aligned to latest: golangci-lint **v2.12.2** (v1.62 was built with
  Go 1.23 and could not load Go 1.26 projects), sqlc **1.31.1**, CI Go **1.26.x**,
  Docker base `golang:1.26-alpine`. Added `.golangci.yml`.
- Fixed `docker-build`: the main module's `replace` of the test-only
  `internal/postgrestest` submodule now resolves in the builder (its `go.mod` is
  allowed through `.dockerignore` and copied before `go mod download`).
- Cleared pre-existing lint debt surfaced once golangci-lint could finally run.

## [0.1.0] - 2026-07-02

First tagged release. Establishes the public consumption contract for both the
Go library and the React package. API is **v0.x** — no stability guarantees
between minor versions while under active development (see SemVer policy in
`README.md`).

### Go module — Added
- Root `ledger` facade: `ledger.New(pool, ...Option)` returns a `*ledger.Service`
  exposing `core` interfaces (`Booker`, `BalanceReader`, `JournalWriter`,
  `Reserver`, `EventReader`, …) — consumers depend only on `core/*`, never on the
  `postgres` adapter directly.
- `Service.RunInTx` composes ledger writes with the caller's own DB writes in one
  atomic pgx transaction.
- `ledger.Migrate(databaseURL)` runs the embedded schema migrations.
- `WithLogger` / `WithMetrics` options for injecting observability; both optional.
- Preset bundles installable via `InstallDefaultPresets` / `InstallExtendedPresets`
  (deposit, withdrawal, transfer, fee, capital, settlement, card, spread, FX).
- Inbound channel adapters via `RegisterChannel`; background jobs via `Worker`.
- Standalone HTTP service `cmd/ledgerd` and read-only investigation CLI
  `cmd/ledger-cli`.

### npm package `@azex/ledger-react` — Added
- Initial release: hooks, page components, RSC prefetch helpers, and theming for
  consuming the ledger HTTP API. Entry points `.`, `./charts`, `./server`,
  `./styles.css`.
- Published to the public npm registry.

[Unreleased]: https://github.com/azex-ai/ledger/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/azex-ai/ledger/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/azex-ai/ledger/releases/tag/v0.1.0
