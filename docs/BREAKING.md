# Breaking changes

This file is part of the release process, not a retrospective. Every change
that can break a consumer at build time or at run time gets an entry here, in
the same commit as the change.

Three kinds of break are in scope:

1. **Go exported surface** — a removed or re-signed symbol, or a new method on
   an exported interface (Go has no sealed interfaces: a consumer's own
   implementation stops compiling). The surface is snapshotted in
   [`api-surface.txt`](api-surface.txt); `TestAPISurface_MatchesSnapshot` makes
   changing it deliberate, and `TestAPISurface_BreakingChangesAreDocumented`
   fails when a removed or changed symbol is not named in this file.
2. **HTTP wire** — a changed field name, type, nullability, status code or
   ordering. `docs/openapi.yaml` is the machine contract; the gates in
   `server/openapi_*_test.go` hold it against the Go handlers in both
   directions.
3. **Schema / data** — a migration a consumer must run, or historical rows
   whose meaning changes. `deployment.md`'s expand → migrate → contract applies;
   an entry must say what the consumer has to do, not only what changed.

`chains/evm` and `anchors/r2` are separate Go modules with their own version
lines; breaks in those are recorded here too, prefixed with the module path.

> **Format.** One `###` heading per symbol or endpoint, so the gate can find
> the symbol name by substring. Say what a consumer must DO, not just what
> moved.

---

## [Unreleased]

### `anchordev.NewLocalFileAnchor` -> `anchordev.NewLocalFileAnchorForDevelopment`

Renamed, no compatibility shim. The local-file anchor writes to the same
machine as the database it exists to be independent of, so a deployment that
reached for the shortest constructor name got an anchor that a compromised
host can rewrite alongside the ledger. The name now says so at every call
site, which is the only place the mistake is visible.

Consumers: rename the call. Nothing else changes.

### `anchortest.Check`, `anchortest.RunConformance`, `anchortest.Skipped`

Each gains a trailing `opts ...Option`. Source-compatible for existing calls
(a variadic added to the end); a consumer that stored one of these in a
function variable of the old type must widen that type.

The options exist so an anchor implementation can hand the suite an
out-of-band write (`WithOutOfBandWrite`) and have the tamper and
head-rollback phases actually run instead of being skipped -- a conformance
run that skips the phases that matter must not read as a pass.

### `postgres.Migrate` and `ledger.Migrate` gain `opts ...MigrateOption`

`Migrate(databaseURL string)` becomes
`Migrate(databaseURL string, opts ...MigrateOption)`, with a new
`postgres.MigrateContext(ctx, databaseURL, opts...)`,
`WithMigrateLogger` and `WithMigrateLockBudget`. Source-compatible.

Run-time behaviour DOES change: the cluster migration lock is now a bounded
poll (5-minute default budget) rather than a blocking `pg_advisory_lock`, so
a stuck `Migrate` elsewhere in the cluster now fails with an explanatory
error instead of hanging every other `Migrate` forever.

Consumers: no action for a normal install. A migration window longer than
five minutes needs `WithMigrateLockBudget`; routing "waiting for the cluster
migration lock" into your own logs needs `WithMigrateLogger`.

Also part of the same install path (D-M2): `Migrate` now runs `001` on its
own and then grants `ledger_owner` to the migrating credential for the rest
of the run, revoking on every exit path. Most deployments need no action,
but running `Migrate` as a third-party role with no ADMIN OPTION on
`ledger_owner` now fails up front with the three ways out listed, where it
previously died at `002` with a bare `42501` and left the database marked
dirty.

### `core.Metrics.BalanceDrift`, `core.Metrics.NegativeBalanceDetected`, `core.Metrics.ReconcileGap`, `core.Metrics.ReservedAmount` take a currency uid

**Landed (H-M9 / I-M1).** These four methods took `currencyID int64` — an
internal `currencies.id`. I-18 forbids an internal primary key in a `core`
interface, and the library's own Prometheus implementation published it as the
label `currency_id`, welding operator dashboards to a key that does not
survive a dump/restore. The parameter is now the currency **uid**.

Re-signed with them, because an interface change is a change to every
implementation of it: `core.NoopMetrics.BalanceDrift`,
`core.NoopMetrics.NegativeBalanceDetected`, `core.NoopMetrics.ReconcileGap`,
`core.NoopMetrics.ReservedAmount`,
`observability.PrometheusMetrics.BalanceDrift`,
`observability.PrometheusMetrics.NegativeBalanceDetected`,
`observability.PrometheusMetrics.ReconcileGap` and
`observability.PrometheusMetrics.ReservedAmount`.

Consumers implementing `core.Metrics` by hand must update those four
signatures; consumers embedding `core.NoopMetrics` (the documented way) only
need to update the methods they actually override.

**Operators**: the Prometheus series for these four now carry `currency_uid`
instead of `currency_id`. Dashboards, recording rules and alert rules that
group or filter on that label must be updated — the old label simply
disappears, so a rule referencing it silently matches nothing.

### `core.Metrics` grows from 32 to 41 methods

**Landed (I-M1 / I-M8 / I-M10 / B-m10).** New: `JobTickCompleted`,
`JobTickFailed`, `JobTickSkippedLocked`, `JobPanicked`, `StuckRollups`,
`PendingEvents`, `AttestationBatchResult`, `AnchorPublishResult`,
`AnchorLagSeqs`.

Go has no sealed interfaces, so a hand-written `core.Metrics` implementation
stops compiling until all nine are added. **Embed `core.NoopMetrics`** — that
is what it is for, and it is why this class of change is survivable at all.

Also here: `LedgerStore` / `ReserverStore` / `BookingStore` gain a chained
`WithMetrics(core.Metrics)` (defaulting to `core.NopMetrics()`), which
`ledger.New` wires automatically. Two of the new methods
(`PendingEvents`, `ReservedAmount`) have no production call site yet and are
registered as such in `observability/emission_coverage_test.go` — they emit
nothing today, so they carry no alerting surface.

### `delivery.NewLocalDispatcher` and `service.NewLockedJob` take a `core.Metrics`

**Landed (I-N12 / I-M8).**

    delivery.NewLocalDispatcher(poller, logger)              -> (poller, logger, metrics)
    service.NewLockedJob(name, fn, pool, logger)             -> (..., logger, metrics)

`nil` is accepted and means `core.NopMetrics()`. Only direct constructor
callers are affected — going through `ledger.Service.Worker()` /
`service.Worker.Subscribe` needs no change, since the facade supplies it.

### `metadata` and `source` now have upper bounds (lead addendum)

`core.MaxSourceLen` (256), `core.MaxMetadataKeys` (64),
`core.MaxMetadataKeyLen` (128), `core.MaxMetadataValueLen` (2048),
`core.MaxMetadataTotalLen` (16384). Every write input that carries those two
free-form fields (`JournalInput`, `CreateBookingInput`, `TransitionInput`,
the three pending inputs) rejects a value over the bound with
`core.ErrInvalidInput` / HTTP 400.

Purely additive symbols, but a **behavior change**: a library-mode consumer
that was storing very large metadata now gets an error where it previously
got a write. The HTTP surface was already bounded by `Config.MaxBodyBytes`,
so an HTTP caller within the body limit is unaffected. The bounds are
generous by design — nothing in the presets or examples comes within an
order of magnitude — because they exist against pathology on the
append-only tables, not as a business rule.

### `core.HolderReader.ListHolderHolds` is paginated (H-m9)

`ListHolderHolds(ctx, holder)` → `ListHolderHolds(ctx, holder, cursor string,
limit int32) ([]HolderHold, string, error)`. It returned every outstanding
hold with no `LIMIT` and no cursor, so one holder with a runaway number of
active reservations produced an unbounded response body from an unbounded
scan — a collection endpoint outside api-contract.md §6's shape entirely.

Consumers implementing `core.HolderReader` must update the method, and the
adapter method `postgres.LedgerStore.ListHolderHolds` changed with it (a
consumer calling the store directly rather than through the port passes the
two new arguments and reads the extra return value).
`GET /holder/holds` gains `cursor` and `limit` query parameters (defaults 20,
max 100) and its `next_cursor` is now a real value instead of always null —
a client that read the whole list in one call must page.

### `GET /journals`, `GET /entries`, `GET /audit/journals` now page newest first (H-m3)

`ListJournalsCursor`, `ListEntriesByAccount`, `ListJournalsByAccount` and
`ListJournalsByTimeRange` changed from `id > cursor ORDER BY id ASC` to
`id < cursor ORDER BY id DESC`. docs/openapi.yaml already described
`GET /journals` as "descending id", and the holder surface's own journal
pagination always was descending — so the documented direction was the one
nothing implemented, and a consumer building a "most recent activity" list
got the oldest page with the cursor walking away from the present.

**Run-time behavior change, not a compile break.** A consumer that renders
these pages in arrival order will see the order invert. Cursor handling does
not change (still an opaque string from `next_cursor`); a consumer that
decoded the cursor and reasoned about it as "the largest id seen" must stop
(it was never a documented value). The queue-shaped lists
(`GET /bookings`, `GET /events`, `GET /deposits/reviews`) deliberately stay
oldest-first, and every endpoint's direction is now stated in its openapi
summary and in docs/api.md's Pagination section.

### `server.PagedResponse` on `GET /holder/balances` and `GET /holder/holds` (H-m4)

Both routes used to answer `{"list": [...]}` with **no** `next_cursor` key.
They now answer `{"list": [...], "next_cursor": null}`, like every other list
endpoint (api-contract.md §6 wants one comparable sentinel). A consumer that
distinguished these two routes by the absence of the key must switch to
`next_cursor === null`.

### `POST /journals/{uid}/reverse` rejects `idempotency_key` (H-M3)

The body field was documented as `required` and silently discarded: the real
key is derived server-side as `reversal:{uid}:{reason}`. Sending it — in the
body, or via the `Idempotency-Key` header, which the alias middleware folds
into the body — is now answered with `400` instead of accepted and ignored.
Callers that need to choose the key use `POST /journals/{uid}/reverse-partial`
with `num` = `den` = 1.

### `Booking.expires_at` is `null`, not `""` (H-M2)

`bookingResponse.ExpiresAt` changed from `string` to `*string`, so a booking
with no expiry serializes `"expires_at": null` rather than an empty string. The
key is still always present (it remains in the spec's `required`). A consumer
that fed this field to a date parser was already failing on the empty string;
one that compared it to `""` to mean "no expiry" must compare to `null`.

### `GET /snapshots`, `GET /entries`, `GET /balances/{holder}` parameter names (H-M1)

No Go behavior changed — `docs/openapi.yaml` was wrong, and any client
generated from it was broken. The spec now matches the handlers:
`/entries` takes `currency_uid` (not `currency`); `/snapshots` takes
`currency_uid`, `start`, `end` (not `currency`, `from`, `to`), all four
required; `/balances/{holder}` documents its **required** `currency_uid`, which
it always demanded. Clients generated from an earlier spec must be regenerated.

### Business code `14011` added (E-M9)

`core.ErrUnknownAuthKey` used to resolve to `19999` / HTTP 500, which
`bizcode.Retryable` calls retryable while `core.IsRetryable` calls it
non-retryable. It now resolves to `14011` / HTTP 422, non-retryable in both.
Retry logic that switched on the exact code `19999` to handle this case must
match `14011`; logic that asks `bizcode.Retryable(code)` is unaffected except
that it now returns the correct answer.

### `server.Config.Logger`, `server.HolderConfig.Logger` added (I-N15)

Purely additive fields, but they change where the HTTP layer's logs go: the
access log and the holder-token / deposit-review audit lines now use the
injected `core.Logger` (default `slog.Default()`) instead of the package-level
`slog`. A log pipeline that captured them via `slog.Default()` keeps working
until a logger is injected. Also: `httpx.Error` logs 4xx at Info and 503 at
Warn instead of Error, and the holder-token mint line no longer contains the
holder id.

### Outbound webhook timestamps are UTC (H-M4)

The outbound event payload is now serialized by `internal/wirejson`, the same
encoder the HTTP surface uses. On a deployment whose process TZ is not UTC,
`occurred_at` (and any other timestamp) changes from a local offset such as
`2026-09-02T12:00:00+08:00` to `2026-09-02T04:00:00Z`. Same instant, and it is
what `api-contract.md` §5 always required; a subscriber that string-compared
the previous rendering must parse instead.

---

## 0.6.0 and earlier

Recorded here after the fact: these were tracked in an audit worklist
(`docs/audits/2026-08-25-financial-engineering/TODO.md` §10) rather than in the
release process, which is the practice this file replaces. That file keeps the
full argumentation; the entries below are what a consumer needs.

### `core.Reserver.Release`, `core.Reserver.FinalizeSettlement`, `core.SettleInput`, `core.TransitionInput`

Idempotency keys became mandatory on every reservation and booking state
change. `Release(ctx, reservationUID string)` →
`Release(ctx, core.ReleaseInput)`; `FinalizeSettlement` likewise;
`core.SettleInput.IdempotencyKey` and `core.TransitionInput.IdempotencyKey` are
now required by `Validate()`. HTTP: `POST /reservations/{uid}/settle`,
`/finalize` and `/release` all require `idempotency_key` in the body.

⚠️ Do not derive a transition key as `<booking_uid>-<to_status>`: a lifecycle
may legitimately reach the same status twice (withdrawal `failed → reserved`,
sweep `failed → pending`), and that derivation makes the second, legitimate
retry short-circuit on the first one's receipt. Derive from the source event
occurrence instead.

### `core.CheckpointIntegrityStore.RebuildCheckpoint`

Returns a uid-based `*core.BalanceCheckpoint` (`CurrencyUID` /
`ClassificationUID string`, no `LastEntryID`) instead of the id-based type of
the same name. Read `cp.CurrencyUID` / `cp.ClassificationUID`; for a watermark,
use `RecomputeBalance` or the `checkpoint_rebuilds` audit table. The id-keyed
working types moved to `service.BalanceCheckpoint` / `service.RollupQueueItem`.

### `pkg/httpx.Error`'s HTTP mapping for four `core` sentinels

`core.ErrUnauthorizedJournal`: `500`/`19999` → `422`/`14010`, and retryable
`true` → `false` (that reversal *is* the fix — a tamper rejection was being
dressed up as a transient error). `core.ErrRollupPending` and
`core.ErrAttestorUnavailable`: `500`/`19999` → `503`/`18103`/`18104`. New
sentinel `core.ErrTransient` → `503`/`18105`. Retry logic matching the literal
`19999` for these cases must match the new codes.

### `server.PagedResponse.NextCursor`, `holderTransactionsPage.NextCursor`

`string` with `omitempty` → `*string`. On the wire, an exhausted page changes
from a missing key or `""` to a literal `null`. `!body.next_cursor` still
works; `"next_cursor" in body` or `typeof next_cursor === "string"` does not.

### `GET /holder/transactions` field `kind`

Now `core.HolderTxKind`: a stable product enum — `deposit`, `withdrawal`,
`transfer`, `fee`, `adjustment`, `other` — rather than a journal-type code
(which narrated internal mechanism) or a uid (which differs per deployment).
Untagged journal types read as `other`; the wire never carries `""`. Match the
enum values, and rekey `@azex/ledger-react`'s `kindLabels` prop accordingly.
`kind_label` is unchanged. Library consumers can tag their own journal types
via `core.JournalTypeStore.SetHolderKind`.

### `GET /system/health`, `GET /system/ready` failure bodies

Hand-written `{"status":"degraded","db":"down"}` / `{"status":"starting"}` →
the standard envelope `{"code":18101,"message":{"text":"…"},"data":null}`.
Probe scripts must key on HTTP 503, not on a `status` string in the body.

### `POST /journals` `effective_at`, `POST /bookings/{uid}/transition` `source`

Documented but never read; now honored. A caller that sent junk in these
fields expecting it to be ignored must check it.

### `presets` template directions and shapes

`transfer_out` / `transfer_in` / `fee_charge` debit-credit directions were
corrected — **consumers must reverse existing journals posted with the wrong
direction**. Fractional-reversal aggregation was corrected: check existing
reversals of journals with repeated dimensions.

### Service binary and deployment surface removed

The repository ships no binary. The consumer's own composition root mounts
`server`'s `http.Handler` (see `examples/fullstack`).
