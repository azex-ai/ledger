# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Two independently versioned artifacts live in this repo:

- the **Go module** `github.com/azex-ai/ledger`, tagged `vX.Y.Z`;
- the **npm package** `@azex/ledger-react`, tagged `ledger-react-vX.Y.Z`.

Entries below note which artifact a change affects.

## [Unreleased]

Holder-scoped wallet surface (2026-07-08,
`docs/plans/2026-07-08-holder-scoped-wallet-surface.md`): the end-user
wallet — balances / translated transactions / holds for ONE holder — as a
library capability, so consumer products stop hand-rolling the projection.

### Go module — Added (wallet surface)

- Holder read projections (`core.HolderReader` on the ledger store):
  `ListHolderBalances` (per-currency BalanceBreakdown + currency code),
  `ListHolderTransactions` ((journal, holder, currency) net aggregation over
  role-bearing classifications, user-language kind/label/direction, cursor at
  journal granularity), `ListHolderHolds`.
- Holder tokens: stateless HMAC (`lht_` prefix), single-holder, read-only,
  TTL-bound; `server.MintHolderToken` for in-process minting,
  `POST /api/v1/holder-tokens` (write scope) over HTTP.
- `server.HolderHandler`: mountable sub-router with exactly the three read
  endpoints (no admin routes) for library hosts; ledgerd exposes the same
  surface behind `HOLDER_TOKEN_SECRET`.
- `display_label` on classifications + journal types (migration 038,
  expand-only) with preset-seeded user-facing defaults; `SetDisplayLabelIfEmpty`
  never clobbers operator overrides.

### npm package — Added (wallet surface)

- `@azex/ledger-react/wallet` (shadcn skin), `/wallet/heroui` (HeroUI v3),
  `/wallet/headless` (client + hooks): `WalletPanel`, `WalletBalances`,
  `WalletBalanceCard`, `TransactionList`; `getToken` callback auth with
  single 401 refresh-retry; rendered-surface tests pin that no double-entry
  vocabulary reaches the DOM. Version 0.4.0.

Production-hardening batch (2026-07-06): closes the operational gaps between
"code-complete" and "runnable in production" — credential model, dashboard
auth, backup/DR, alerting closure, data lifecycle, deploy hygiene.

### Go module — Added
- **Scoped API keys** (BREAKING): `API_KEYS` is now comma-separated
  `name:scope:secret` triples, scope `read` < `write` < `admin`; every route
  group enforces `requireScope`, insufficient scope returns bizcode `10150`
  (403), and the key *name* is attached to access-log lines for audit.
  Malformed `API_KEYS` fails boot. (`server/middleware_auth.go`, docs/api.md)
- **Active monthly partitioning** (BREAKING migration 037): journal_entries
  rows move from the default partition into named monthly partitions
  (`journal_entries_yYYYYmMM`); a new advisory-locked worker `partition` job
  keeps `PartitionMonthsAhead` (default 3) months pre-created and rebalances
  stranded default rows (fallback gated on SQLSTATE 23514). I-13 is now an
  active process; archival guidance in RUNBOOK §11.
- **OTLP tracing bootstrap**: `OTEL_EXPORTER_OTLP_ENDPOINT` enables an
  OTLP/HTTP batching exporter behind pkg/otel (flushed on shutdown);
  unset = no-op as before. (`cmd/ledgerd/tracing.go`)
- **MIGRATE_MODE** (`auto`|`only`|`off`) decouples schema migrations from
  pod startup; Helm `migrations.job.enabled` runs them from a
  pre-install/pre-upgrade hook Job so serving pods need no DDL privileges.

### Go module — Fixed
- `BenchmarkReserveSettle` seeded funds into a role-less classification, so
  `Reserve` (role=available only) always failed — bench-only, CI never runs
  benchmarks. Now seeds a dedicated available-role wallet.

### Go module — Security
- **Trusted-proxy client-IP resolution** (BREAKING): replaced the
  `TRUST_PROXY_HEADERS` boolean with `TRUSTED_PROXY_CIDRS`, a comma-separated
  list of trusted edge-proxy CIDR ranges. Proxy headers (`X-Forwarded-For`
  walked right-to-left skipping trusted hops, then `X-Real-IP` /
  `True-Client-IP`) are honored **only** when the socket peer is inside a
  configured range, and every candidate is `netip`-validated — so a direct
  caller can no longer spoof its IP past the rate limiter or into access logs,
  and non-IP garbage can no longer create unbounded rate-limiter buckets.
  Migration: deployments that set `TRUST_PROXY_HEADERS=true` must set
  `TRUSTED_PROXY_CIDRS` to their ingress/proxy ranges instead; an invalid value
  fails boot. (`server/middleware_realip.go`, `server/server.go`,
  deploy/helm, docs/RUNBOOK.md, README.md)

### @azex/ledger-react + web — Changed
- **Dashboard credential model** (BREAKING): `NEXT_PUBLIC_API_KEY` /
  `NEXT_PUBLIC_API_URL` are gone. The browser talks to a same-origin BFF
  proxy (`/api/v1/[...path]`) that holds `LEDGER_API_KEY` server-side;
  pages are gated by `DASHBOARD_PASSWORD` login (HMAC session cookie,
  `proxy.ts`), with sign-out in the sidebar (`Sidebar` gains an optional
  `footer` slot — additive).

### Ops & docs
- `docs/DR.md`: PITR strategy, RPO/RTO targets, restore drill with
  invariant-based verification (reconcile full + solvency), quarterly drill.
- `docs/CAPACITY.md`: measured baseline (PostJournal ~2.5ms, GetBalance
  ~0.7ms, Reserve→Settle ~2.7ms serial), sizing rules, suggested SLOs.
- Helm: ServiceMonitor + PrometheusRule (alerts mapped 1:1 to RUNBOOK
  scenarios), PDB (on by default), HPA + NetworkPolicy (opt-in); Grafana
  dashboard in `deploy/grafana/`.
- CI: govulncheck (pinned), Trivy image scan (fails on fixed
  CRITICAL/HIGH), SPDX SBOM artifact, dependabot (gomod/npm/actions).
- RUNBOOK: §10 rewritten for the scoped-key model, §11 partition
  management/archival, §5 dead-letter handling + events retention policy.

## [0.4.1] - 2026-07-03

### Go module — Fixed
- **Migrations 025 and 031 failed on any database that already contained
  rows** — the class of database every upgrading library consumer has. Plain
  CI only ever migrates empty databases, where both bugs are invisible
  (caught by armatrix's upgrade rehearsal):
  - 025's `journal_entries.effective_at` backfill UPDATE was rejected by the
    018 append-only row trigger (0 rows = trigger never fires). The backfill
    now disables/re-enables the trigger around the one-time statement.
  - 031's `ADD COLUMN uid UUID NOT NULL` (no DEFAULT) fails on non-empty
    tables. Every table now uses add → `gen_random_uuid()` backfill →
    `SET NOT NULL`. Pre-existing rows get v4 uids (uniqueness is the only
    property the contract needs; v7 time-ordering remains a Go-side nicety
    for new rows).
- New pin: `TestMigrate_PopulatedDatabase` migrates to v24, seeds rows into
  every entity table, then runs the rest — the populated-database upgrade
  path is now CI-covered. `postgres.NewMigrationSource` and
  `postgrestest.SetupRawDB` are exposed for such stepwise migration tests.

## [0.4.0] - 2026-07-03

API-contract alignment (design: docs/plans/2026-07-03-api-contract-alignment-design.md).
No-legacy premise (Aaron's 2026-07-03 ruling): the ledger is treated as a brand-new
library — single-step breaking migration, no compatibility shims, no backfill.

### Go module — Changed (BREAKING: uid-only identity)
- **uid (UUIDv7) is the only externally visible identifier** (migration 031). Every
  entity table gains `uid UUID NOT NULL` + unique index; uids are generated Go-side
  (UUIDv7) on insert — no DB default, so a write path that forgets one fails loudly.
  Internal BIGSERIAL ids survive only inside storage (PKs/FKs/locks/keyset cursors)
  and appear in **no public contract**, including the library-mode Go API.
- `core` entity structs expose `UID string`; all cross-references are `*UID string`
  (`Journal.EventUID`, `Booking.JournalUID`, `Entry.ClassificationUID`, …; "" = not
  linked). All interface signatures take/return uids (`GetJournal(uid string)`,
  `ReverseJournal(uid string, …)`, `GetBalance(holder, currencyUID, classificationUID)`, …).
- **Pagination**: every list interface returns `(items, nextCursor string, error)`;
  cursors are opaque base64 strings produced by the store (`AuditFilter.Cursor`,
  `BookingFilter.Cursor`, `EventFilter.Cursor` are now strings). HTTP responses carry
  `next_cursor` populated by the store, not recomputed by handlers.
- **HTTP API**: all path params are `{uid}`; query params renamed
  (`currency_id`→`currency_uid`, `classification_id`→`classification_uid`,
  `booking_id`→`booking_uid`); no request or response body carries an internal id.
  Pinned by a mechanical source scan (server contract test, invariant I-18).
- `channel.CallbackPayload.BookingID int64` → `BookingUID string`; EVM adapter
  parses `booking_uid` from webhook JSON.
- `service.ClassificationLister` is now a dimension port
  (`ClassificationDims` / `CurrencyIDByUID` / `CurrencyUIDByID`), implemented by
  `postgres.RollupAdapter`; rollup/reconcile math stays on internal ids and converts
  at the boundary.
- `ledger-cli` flags take uids (`--currency <uid>`, `--uid`, `--booking-uid`); the
  internal-id escape hatch was not kept — use psql for storage-level investigation.
- `postgrestest` seed helpers return uid strings; new `InternalID` helper resolves a
  uid back to the bigint id for raw-SQL test assertions.
- `docs/openapi.yaml` rewritten for the uid contract (info.version 0.4.0).

### @azex/ledger-react — Changed (BREAKING, 0.2.0)
- Regenerated `schema.ts`; all hand-written types use `uid: string` / `*_uid: string`
  ("" = not linked, no more `number | null`); `Entry` and `TemplateLine` no longer
  carry row ids. Client methods, hooks, and admin pages take uid strings; journal
  detail routing matches opaque uids (`/journals/{uid}`).

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
