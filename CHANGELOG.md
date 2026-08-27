# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Two artifacts live in this repo:

- the **Go module** `github.com/azex-ai/ledger`, tagged `vX.Y.Z`;
- the **npm package** `@azex/ledger-react`, tagged `ledger-react-vX.Y.Z`.

From 0.5.0 onward the two are **version-aligned**: a coordinated release
tags both at the same X.Y.Z (npm 0.4.0 / 0.5.0 were never published; the
npm line jumps 0.3.0 → 0.5.1 to converge with the Go module). Entries
below note which artifact a change affects.

## [Unreleased]

Two release-sized bodies of work landed this cycle. **Tamper-evident ledger**
(2026-08-21, `docs/plans/2026-08-21-tamper-evident-ledger-design.md`) answers
the question a leaked database credential raises: if an attacker gets DB
write access, what stops them from crediting themselves with a perfectly
balanced, forged journal? The answer is now "checkpoints are an untrusted
cache, per-journal writes are cryptographically signed, and withdrawals only
pay out against a verified balance" — P0 through P7 below, plus two follow-on
waves. **Crypto deposit + sweep**
(`docs/plans/2026-07-11-crypto-deposit-sweep-design.md`) turns the
CREATE2-derived shared-address deposit flow into an optional, library-shipped
default, with M3 compensating controls (threshold gate + dual-provider
reconciliation → human review) and a full wallet-side frontend.

### Go module — Removed

- **Breaking**: the standalone service binary is gone, along with `cmd/ledgerd`,
  the Helm chart, the Grafana dashboard, the Dockerfile and the compose
  services that ran them. This is a library; it now ships nothing to deploy.
  `docker-compose.yml` starts PostgreSQL alone for local development, and
  `make docker` is `make db`.

  `server/` stays. It is not a composition root -- it takes already-assembled
  dependencies and returns an `http.Handler` -- so a consumer that wants the
  HTTP surface mounts it in their own binary, as
  [`examples/fullstack`](examples/fullstack/) does. `docs/openapi.yaml` remains
  the wire contract that surface implements and that `@azex/ledger-react`
  generates its types from.

  Why it went: the binary was a second composition root that assembled a
  different set of capabilities than the library facade. It never enabled
  per-journal signing, never scheduled batch attestation, and exposed no way
  to require a verified balance -- so everything under "Security" below was
  inert in the only deployment form this repo shipped, and no test noticed,
  because nothing tested that binary.

### Go module — Fixed (accounting correctness)

Two of these move real money and are worth reading before upgrading.

- **Transfers and fees ran in the wrong direction.** `transfer_out`,
  `transfer_in` and `fee_charge` had their holder leg inverted against
  `main_wallet`'s declared polarity, so a peer-to-peer transfer of 100 left
  the sender 100 richer and the receiver 100 in debt, and charging a fee paid
  the payer. `deposit_confirm` and `checkout_settlement` were always correct;
  these three disagreed with them. **If you have posted through these
  templates, the resulting journals are wrong and need reversing.**

  The preset tests passed throughout: both legs draw on the same amount key,
  so "total debits equal total credits" holds whichever side each
  classification lands on.

- **"Reverse everything remaining" under-reversed when a journal had two
  entries on the same dimension.** Prior reversals are tracked per dimension
  while the remainder was computed per entry, so each entry subtracted the
  whole dimension's prior total. A 60 + 40 journal reversed by half and then
  "the rest" left 40 on the books and returned success. **Check any journal
  you reversed in fractions that carried repeated dimensions.**

- Inbound webhooks failed on every request in any deployment that used the
  role separation the schema installs: the replay-cache prune is a `DELETE`,
  `ledger_app` had none, and the error was returned rather than tolerated.
  Migration 002 grants that one `DELETE` -- which the schema's own comment
  already called "the one sanctioned DELETE" -- and the prune now tolerates a
  refused privilege without failing the request.

- A cached attestation-time `Authorized` verdict no longer lets
  `VerifiedBalance` skip the live check. The verdict answers whether a journal
  was authorized *when attested*; the withdrawal gate needs whether it is
  authorized *now*, and the canonical digest covers every entry's amount, so
  an amount edited after attestation was invisible to the gate. Verification
  cost on that path returns to its pre-optimization level, deliberately.

### Go module — Security (configuration integrity)

- Migration 003 puts a column whitelist on `currencies`, `classifications`,
  `journal_types`, `entry_templates`, `entry_template_lines` and
  `deposit_addresses`. A per-journal signature authenticates what the
  application *read*, not what happened -- so an application credential that
  could rewrite a deposit address's holder, or a template line's direction,
  made the application sign a correct journal about the wrong facts, and the
  result verified. Only `is_active`, `display_label`, `lifecycle` and
  `balance_role`'s one-way upgrade stay mutable; `currencies.exponent`,
  `classifications.normal_side` and `.code`, and every column of a template
  line do not.

- Migration 004 refuses promoting a classification to `balance_role`
  `'available'` once it has journal entries. `available` is the only bucket
  `Reserve` spends from, so promoting one that already holds balances turns
  them into withdrawable funds in a single statement -- the shipped
  `fee_expense` is debited on every withdrawal fee. `'pending'` and
  `'locked'` stay unrestricted; neither is spendable.

### Go module — Security (tamper-evident ledger, P0–P7 + Waves 2–3)

- **P0 — Reconciliation coverage fixed**: the negative (system) holder range
  was never scanned by check #2 (its keyset cursor started at `(0,0)`, and
  `account_holder > cursor` permanently excludes negatives); a scan that hit
  `Check2ScanLimit`, timed out, or covered a check the schema didn't support
  yet was still reported `Passed=true`. `CheckResult` now carries a
  `Complete` flag (zero value = not completed, fail-closed) and
  `ReconcileReport` a `FullCoverage` flag; the `ReconcileCheckResult` metric
  only reports green when a check both ran and finished.
- **P1 — DB role least privilege**: `ledger_owner` (DDL-capable, owns every
  object), `ledger_app` (read/write, no DDL), and `ledger_ro` roles; schema
  ownership transferred to `ledger_owner` and `PUBLIC` access revoked. This
  is the prerequisite every later DB-layer guard below depends on — without
  it, any of those triggers could simply be dropped by the credential they
  exist to constrain. The ownership sweep originally covered tables and
  sequences only, leaving all five guard functions — including the two that
  enforce append-only on `journals` and `journal_entries` — owned by whoever
  ran the migration. A function's owner can `CREATE OR REPLACE` its body, so
  that is a silent way to disable every append-only guarantee while leaving
  the triggers in place and emitting no DDL an audit would flag. The baseline
  sweeps tables, sequences, views and routines, and asks the catalogue what
  exists rather than working from a list; a CI check now fails if any object
  is not owned by `ledger_owner`.
- **P2 — Checkpoints are an untrusted cache**:
  balances can now be recomputed entirely from `journal_entries`
  (`RebuildCheckpoint`), with a resumable scan cursor and an append-only
  `checkpoint_rebuilds` audit trail. `system_rollups`/`balance_snapshots`
  drift detection no longer inherits checkpoint corruption.
- **P3 — Per-journal balance enforced at the DB layer again**: a deferred
  constraint trigger rejects any journal whose entries don't balance
  per-currency, independent of the application. This closes a
  real regression — an equivalent trigger existed early on, was dropped
  during a later refactor and never replaced, so pure-SQL access could insert
  unbalanced entries for several months of the project's history.
- **P4 — Mutation guards on non-journal balance tables**:
  `classifications.normal_side`/`balance_role`, `reservations`, and
  `period_closes` can no longer be mutated outside their one legitimate
  entry point. `journals.event_id` gets the same set-once protection —
  see the breaking schema/type change under Changed below.
- **P5 — Per-journal authorization signing** (plus a follow-up fix): a new
  `core.Attestor` port (distinct from the on-chain
  `core.Signer` — different key, different blast radius) signs every
  journal, reversal, and template batch at write time, including writes
  composed through `RunInTx` (a gap in the initial cut where those weren't
  being signed is now closed).
- **P6 — Batch attestation chain + external anchor**:
  journals are chained into signed, gapless batches with an externally
  anchorable head, so a compromised DB owner can't silently truncate the
  tail without `verify` noticing.
- **P7 — RFC 6962 Merkle tree**: batch digests gained a
  Merkle root plus inclusion proofs, so `verify` can localize *which*
  entries were tampered with, not just detect that a batch changed.
- **Withdrawal-time verified balance** (Wave 2): `ReserveInput.RequireVerifiedBalance`
  gates a reservation on a balance computed only from signed/authorized
  journals — an unsigned or forged journal can no longer fund a withdrawal
  even if it's balanced and passes every other check.
- **Attestation-batched authorization verdicts** (Wave 3): signature
  verification cost is amortized across a batch at attestation time instead
  of being re-derived on every read. A cached verdict may only ever be
  trusted, never re-derived, and is contractually at least as strict as a
  live check.
- **Deposit-review separation of duties** (Wave 3): resolving a deposit
  stuck in `review` now requires a capability that no existing API-key
  scope implies on its own (an `admin` key alone can no longer both create
  the anomaly and clear it); a persistently unreachable second confirmation
  source escalates through alerting instead of leaving the review queue
  stuck indefinitely.
- New invariants **I-19 through I-34** in `docs/INVARIANTS.md` pin all of the
  above; `examples/tamper-evident` is a runnable end-to-end demonstration —
  it forges a row and shows exactly what stops the money from leaving.

### Go module — Added (crypto deposit + sweep)

- CREATE2-derived shared deposit addresses
  (`svc.Onchain().EnsureDepositAddress`), a pull-based EVM watcher + sweeper
  (`chains/evm`, a separate Go module so consumers who don't need on-chain
  support never pull in go-ethereum), and a push-based webhook ingestion
  path — both converge on `svc.Onchain().IngestDeposit`.
- Address registration performs a full historical rescan before returning
  and tracks a durable per-chain scan cursor (`registration_rescans`) that
  survives process restarts and resumes in bounded block windows — no
  fire-and-forget scanning, no unbounded genesis-to-tip replays.
- **M3 compensating controls**: an `AutoCreditCeiling` threshold gate plus
  dual-provider (`DepositConfirmer`) reconciliation routes anomalous or
  unconfirmed deposits into a `review` state with zero ledger effect (I-21)
  instead of auto-crediting; human approve/reject endpoints post the
  compensating journal. Secure by default — an unconfigured ceiling fails
  startup instead of silently defaulting to unlimited auto-credit.
- Ops hardening: stuck sweeps are automatically revived and sweeps are
  serialized per chain to avoid double-submission.
- Developer-mode credit preset (`dev_credit`): credits a holder against no
  custodied asset, gated by `ENV=dev` + `DEV_CREDIT_ENABLED=true`. For local
  development and demos only — not installed by `InstallExtendedPresets`.

### Go module — Changed

- **Breaking**: `NewReserverStore(pool, ledger, verifiedBalance)` gained a
  third parameter (`*postgres.VerifiedBalanceStore`), needed for the
  withdrawal-time verified-balance gate above. Callers constructing a
  `ReserverStore` directly must update the call site.
- **Breaking**: `journals.event_id` is now nullable with a real foreign key
  to `events(id)` — previously `NOT NULL DEFAULT 0`, using
  `0` as an unenforced sentinel for "no event". The generated
  `postgres/sqlcgen.Journal.EventID` field changes from `int64` to
  `pgtype.Int8` accordingly. The public library surface
  (`core.Journal.EventUID string`) is unaffected; only code reading the
  generated struct or the raw column directly needs to switch from
  `== 0` to `.Valid`.
- **Breaking (installation)**: the 53 migrations accumulated since the
  project's first commit are squashed into a single `001_baseline`. A new
  install runs one migration instead of fifty-three, and gets the locked-down
  role configuration in that one step rather than through a staged rollout.
  **The connection that runs it must be able to `CREATE ROLE`** — a superuser
  or any role with `CREATEROLE`, which every managed-Postgres master user
  has. That credential is install-time only and should be rotated or retired
  afterwards. Equivalence with the old chain is enforced by diffing
  `pg_dump --schema-only` between a database built by each, which reports no
  difference. Migration numbers referenced anywhere else in these notes are
  historical: the released artifact contains `001_baseline` plus `002`
  through `004`, the three this release adds on top of it, and nothing else.

### Go module — Fixed

- **`Worker.Subscribe` never delivered anything.** It built its dispatcher
  with no event poller and required a separate `SetLocalPoller` call before
  `Run` that nothing outside the test suite made — so an in-process
  subscription silently received no events, forever, while logging a failure
  on every poll tick. `ledger.Service.Worker` now wires the poller, so
  subscribing is all a consumer has to do. If you were calling
  `SetLocalPoller` yourself, keep doing so; it still works and is still
  required when you build a `service.Worker` directly rather than through the
  facade.
- A forged-but-unsigned journal could report as `VERIFIED` under some code
  paths — closed.
- `PendingStore` dropped an unread cached set of classification IDs.
- Assorted ledger-correctness and event-delivery hardening fixes surfaced
  during the integrity-hardening review pass.
- New `JournalTypeStore.SetHolderKind` and reconcile check
  `untagged_holder_kind` (M-7 follow-up, docs/INVARIANTS.md I-44): a journal
  type visible in a holder's transaction list without a `holder_kind` had no
  way to be discovered other than a user noticing `"other"` where a
  specific label was expected. Pure addition, no behavior change to what
  `kind` resolves to on the wire.

### @azex/ledger-react — Added

- End-user wallet: `WalletDepositAddressCard` (shadcn + heroui), backed by a
  holder-token-scoped `GET /api/v1/holder/deposit-address` endpoint — no
  admin key ever reaches the browser.
- M3 deposit review queue page (shadcn + heroui).
- Sweep collection monitor page (shadcn + heroui).
- Headless contract for crypto deposit (client + hooks + types), for hosts
  building their own UI on top of the deposit/sweep feature instead of the
  shipped components.

### @azex/ledger-react — Fixed

- **Breaking**: `HolderTransaction.kind` (and `TransactionList`'s
  `kindLabels` prop, keyed by it) is now one of a small,
  deployment-stable vocabulary — `"deposit" | "withdrawal" | "transfer" |
  "fee" | "adjustment" | "other"` (`core.HolderTxKind`,
  docs/INVARIANTS.md I-44) — instead of the ledger's internal journal-type
  UUID. If your app passes `kindLabels` keyed by that UUID (or, from before
  this cycle, by the journal-type *code* such as `"deposit_confirm"` — both
  prior shapes silently matched nothing once the server changed under
  them), rewrite the keys to the vocabulary above, e.g.
  `kindLabels={{ deposit: "Top up" }}`. No action needed if you only
  consumed the library's default `kind_label` text. `docs/openapi.yaml`'s
  `HolderTransaction.kind` schema is updated to a matching `enum`.
- `ErrorState` gained a Retry action across both skins (previously a dead
  end once an error rendered).
- `schema.ts` (OpenAPI-generated types) regenerated to match the current
  contract, plus a local `openapi-check` script so this can't drift
  silently again.
- Gaps between `docs/frontend.md` and the package's actual exports closed.
- `qrcode.react` dependency pinned.

### Examples

- Every example that needs only a database was run end to end against a fresh
  one. Two could not complete before: `crypto-deposit` was missing a lifecycle
  on the seeded label-only `deposit` classification (the accounting bundles
  install templates, not lifecycles) and then skipped the `confirming` state
  that `DepositLifecycle` requires; `event-subscribe` was demonstrating the
  broken `Subscribe` path above, and printed a note and exited 0 when no event
  arrived rather than failing.

### Docs

- `docs/plans/2026-08-21-tamper-evident-ledger-design.md` (design) and
  `docs/plans/2026-08-21-integrity-hardening-contracts.md` (the cross-task
  migration-number and resource-allocation contract for P1–P7 and the
  in-flight migration-baseline squash) carry the full rationale behind the
  Security entries above.
- `docs/RUNBOOK.md`, `docs/DR.md`, and `docs/openapi.yaml` updated for the
  new roles, reconcile checks, and endpoints.

## [0.5.1] - 2026-07-09

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
  vocabulary reaches the DOM. Version 0.5.1 (aligned with the Go module).

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
