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

### `core.Metrics.BalanceDrift`, `core.Metrics.NegativeBalanceDetected`, `core.Metrics.ReconcileGap`, `core.Metrics.ReservedAmount`

**Planned in this remediation wave (H-M9), not yet landed.** These four methods
take `currencyID int64` — an internal `currencies.id`. I-18 forbids an internal
primary key in a `core` interface, and the library's own Prometheus
implementation publishes it as the label `currency_id`, welding operator
dashboards to a key that does not survive a dump/restore.

The parameter becomes the currency **uid**. Consumers implementing
`core.Metrics` must update those four method signatures; consumers embedding
`core.NoopMetrics` (the documented way) only need to update the methods they
override. Prometheus series carrying `currency_id` will be replaced by
`currency_uid` — dashboards and alert rules that group on that label must be
updated.

`core.TestNoInternalIDsInCoreInterfaceSignatures` currently records these four
in `knownInterfaceInternalIDLeaks`; that list must be emptied in the same
commit as the fix.

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
