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

### `core.BookingReader` gains `BookingsForDepositIdentity`

**Landed (Wave 5 re-check, W5-onchain-ops-2; audit
`docs/audits/2026-09-03-independent-review/recheck/money-out.md` N-1;
invariant I-71.)**

    core.BookingReader + BookingsForDepositIdentity(ctx, chainID int64, txHash string, txLogSeq int32) ([]Booking, error)

**What a consumer must do.** Using `postgres.NewBookingStore` (what
`ledger.New` does): nothing, it implements the method. Implementing
`core.BookingReader` by hand: return every booking whose `metadata` carries
that `(chain_id, tx_hash, txlog_seq)` triple, unpaginated and unfiltered by
status. Returning nothing unconditionally disables a mint fence -- the
deposit path asks this before crediting, to find out whether another booking
already holds the transfer it is about to book.

**Schema, in the same change**: migration `032` adds
`uq_bookings_deposit_identity`, a unique index over those three metadata
values. A deployment carrying duplicate deposit identities will fail to
apply it -- those rows are the defect it exists to prevent; the migration
header carries the query that finds them.

### Migration 029 (`029_insert_path_guards`): appended rows are guarded and recorded

**Landed (Wave 5, `docs/plans/2026-09-03-wave5-contract.md` §0 R3-1;
invariants I-66, I-67).** Schema / data break: a migration a consumer must
run, plus writes the database now refuses that it previously accepted.

**Why it exists.** Every guard and every audit trigger in migrations 001-028
fired on `UPDATE` or `DELETE`; the only INSERT-time trigger in the whole
schema was the per-currency balance check. So for every configuration and
state table, an appended row of legal shape was neither refused nor recorded.
Measured as a real `ledger_app` over a socket during the 2026-09-03
independent review, two appended `entry_template_lines` made every subsequent
honest deposit render at twice its amount with a genuine signature on it,
while reconciliation, `verify` and `SolvencyCheck` all reported healthy and
`config_table_changes` stayed empty.

**What a consumer must do.** Run the migration. Then, if you write to any of
these tables outside this library -- fixtures, seed scripts, backfills,
repair runbooks -- adjust them:

| Table | New rule at INSERT | What to change |
|---|---|---|
| `entry_template_lines` | a line may only be written by the transaction that created its `entry_templates` row | write the template and its lines together (what `TemplateStore.CreateTemplate` already does). An owner-run repair must set `ledger.repair_template_lines` to `on` transaction-locally first |
| `bookings` | `status` = the classification lifecycle's `initial`; `journal_id` NULL; `settled_amount` 0 | create the booking, then `UPDATE` it into the state you want -- the guard on UPDATE already permits exactly that |
| `reservations` | `status` = `active`; `settled_amount` 0; `journal_id` NULL | same |
| `ledger_attestations` | `seq` = chain head + 1; `prev_root` = the head's `root_hash` (seq 1 links to `core.GenesisRoot`); `prev_root` / `root_hash` / `batch_digest` 32 bytes; `signature` and `key_id` non-empty | build attestations through `AttestationService` / `postgres.AttestationStore`; placeholder or out-of-order rows are refused |
| `chain_cursors` | `last_scanned_block` never decreases and never advances more than 100,000 in one write; no other column may change | advance through `SetChainCursor`; rewind through `ledger_rewind_chain_cursor(chain_id, to_block, reason)`, which is owner-only |

**Capacity.** Twelve tables now write a `config_table_changes` row when a row
is CREATED, not only when it changes -- including `bookings`, `events` and
`reservations` at business rate. `journals` is the named exception. If you
size or retain that table, re-size it.

**New owner-only operations** (no `ledger_app` grant, by design -- a repair
capability in the leaked credential's hands is the attack with a nicer name):

    ledger_rewind_chain_cursor(chain_id bigint, to_block bigint, reason text)
    ledger_discard_attestations_from(seq bigint, reason text) -> bigint

Both require a non-empty reason, both write a `config_table_changes` row, and
both fail loudly rather than reporting success having done nothing.

### `service.WithRescanLookback` changes the forward scan's window by default

**Landed (Wave 5; invariant I-67).** Behavioural, not a compile break.

Every forward-scan tick now re-covers the last **128 blocks** below the safe
tip regardless of where `chain_cursors.last_scanned_block` stands. Previously
the window was exactly `cursor+1 .. safeTip`, and a caught-up chain scanned
nothing at all.

**What a consumer must do.** Nothing, unless the extra RPC matters: this is
one additional bounded `getLogs` per tick per chain, and re-ingesting a
sighting is a no-op (`IngestDeposit` is idempotent on
`deposit-{chain}-{tx}-{seq}`). Set `service.WithRescanLookback(0)` to restore
the old window, or a larger value to widen the recovery. `ledger.EnableOnchain`
consumers pass it the same way as any other `service.OnchainOption`.

**Why it is on by default.** The scan never looked back, so a single forged
cursor advance made every real deposit in the skipped window permanently
invisible -- no booking, event, journal or entry, therefore nothing any
reconciliation check can see, while the funds are still swept into treasury.
The database can bound and record such an advance (029) but cannot undo one.

### `core.Metrics` gains four onchain-operability methods

**Landed (Wave 5, W5-onchain-ops; audit
`docs/audits/2026-09-03-independent-review/onchain-ops.md` C-2 / M-3 / M-8).**

    core.Metrics.DepositIngestDeadLettered(chainID int64, reason string)
    core.Metrics.DeadLetterBacklog(count int64, oldestAge time.Duration)
    core.Metrics.SweepOrphanedBroadcast(chainID int64)
    core.Metrics.ChainCursorAdvanceAge(chainID int64, age time.Duration)

**What a consumer must do.** Embedding `core.NoopMetrics` (the pattern
`core.Metrics`' doc comment recommends): nothing, the four no-op methods come
with it. Implementing the interface by hand: add the four methods, or switch
to embedding. `observability.PrometheusMetrics` already implements them and
registers `ledger_deposit_ingest_dead_lettered_total`,
`ledger_dead_letters_unbooked`, `ledger_dead_letter_oldest_age_seconds`,
`ledger_sweep_orphaned_broadcast_total` and
`ledger_chain_cursor_advance_age_seconds`.

**Why they exist.** The first two make a dead-lettered deposit — a real
on-chain transfer this ledger decided never to book — visible at all; before
them its only trace was a row nothing read plus a log line that lands in
`core.NopLogger()` by default. The third counts the one condition that blocks
a chain's outbound collection indefinitely. The fourth is the watcher
liveness signal `chain_cursor_lag_blocks` cannot be, because that gauge
freezes rather than climbs when the RPC endpoint is down. All four have
triage entries in `docs/RUNBOOK.md` §14 / §18.

### `service.DeadLetterRecorder` gains a read half, and `postgres.IngestDeadLetterStore.ListDeadLetters` is paginated

**Landed (Wave 5, W5-onchain-ops; audit C-2).**

    service.DeadLetterRecorder + GetDeadLetter(ctx, uid) (core.IngestDeadLetter, error)
                               + ListDeadLetters(ctx, cursor string, limit int32) ([]core.IngestDeadLetter, string, error)
                               + CountUnbookedDeadLetters(ctx) (int64, time.Time, error)

    postgres.IngestDeadLetterStore.ListDeadLetters(ctx, limit int32) ([]core.IngestDeadLetter, error)
    -> postgres.IngestDeadLetterStore.ListDeadLetters(ctx, cursor string, limit int32) ([]core.IngestDeadLetter, string, error)

**What a consumer must do.** Passing `postgres.NewIngestDeadLetterStore(pool)`
(what `ledger.EnableOnchain` does): nothing but update call sites of
`ListDeadLetters`, which now takes a cursor and returns a next-cursor
(`""` = first page / exhausted). Implementing `DeadLetterRecorder` by hand:
implement the three new methods — they are what makes the queue readable,
countable and replayable.

`core.IngestDeadLetter` also gains two fields, `Booked bool` (recomputed per
read: whether the deposit was credited in the end) and
`Sighting core.DepositSighting` (the recorded payload, which is what a replay
re-drives). Additive; only a consumer constructing the struct by hand with
positional fields is affected.

### `service.Onchain.RunPendingRecheckOnce` / `RunReorgRecheckOnce` return an error

**Landed (Wave 5, W5-onchain-ops; audit M-9).**

    (*service.Onchain).RunPendingRecheckOnce(ctx)  ->  ... error
    (*service.Onchain).RunReorgRecheckOnce(ctx)    ->  ... error

**What a consumer must do.** Handle (or explicitly discard) the returned
error. Both used to return nothing, which made an ops-triggered recheck that
failed indistinguishable from one that found nothing to do — and, since the
library's default logger is `core.NopLogger()`, usually completely silent.
`RunWatchOnce` and `RunSweepOnce` already returned errors; this makes the
four consistent.

### `core.TokenConfig.Validate` refuses `Decimals > 18` (was 36)

**Landed (Wave 5, W5-onchain-ops; audit M-2).**

**What a consumer must do.** Nothing, unless a `TokenConfig` really is
configured above 18 — in which case that configuration was never usable: a
ledger currency's `Exponent` caps at 18 (`core.CurrencyInput.Validate`), so
every deposit of such a token carrying a fraction was refused with
`ErrPrecisionExceeded`, dead-lettered, and scanned past. `Onchain.Run` also
now refuses to start when a token's `Decimals` exceeds the exponent of the
currency it books into (`service.Onchain.ValidateTokenPrecision`, additive);
a push-only consumer that never calls `Run()` should call that method at
startup.

### `service.ReconcileQuerier` gains `CorruptReversalLinks`

**Landed (Wave 5, W5-money-misc; independent review `money-out.md` M-2;
invariant I-51).**

    CorruptReversalLinks(ctx context.Context, pageLimit int) ([]CorruptReversalLink, error)

**Who this breaks**: only a consumer that implements `service.ReconcileQuerier`
itself — a test double, or a store that is not this repo's
`postgres.ReconcileAdapter`. Consumers that build the reconciliation service
the ordinary way (`ledger.New`, or `postgres.NewReconcileAdapter`) get the new
method with the rest of the adapter and need to do nothing.

**What to do**: implement it. It backs the new `reversal_chain_integrity`
check, which scans for journals carrying `reversal_of = O` that are not
reversals of `O` — the forged link that made `ReverseJournalFraction(J, 1, 1)`
reverse half of `J` and return `nil`. Return up to `pageLimit` violations,
oldest-first is not required. Returning an error is honest and safe: the check
reports it as a Finding with `Passed=false, Complete=false`, never as a pass.
**Do not stub it as `return nil, nil`** — that reads as "every reversal chain
in this deployment is sound", which is the one answer an implementation that
cannot look must not give.

The check itself is additive: `RunFullReconciliation` now returns 16 checks
without an `AuthVerifier` wired and 17 with one. A consumer asserting on the
number of checks in a report has to move that number.

### Every amount-bearing `Validate()` refuses an amount `NUMERIC(30,18)` cannot store

**Landed (Wave 5, invariant I-70).** Not a signature change: a runtime one.
Any amount needing more than 12 integer digits or more than 18 fractional
digits is now `core.ErrInvalidInput` at the `core` boundary, from
`JournalInput`, `ReserveInput`, `SettleInput`, `SettlePartialInput`,
`AddPendingInput`, `ConfirmPendingInput`, `CancelPendingInput`,
`CreateBookingInput`, `TransitionInput`, `AccountPolicyInput`,
`DepositSighting`, `SweepPolicy` and `TokenConfig` — and therefore from
`ExecuteTemplate`, whose amounts reach `JournalInput.Validate` through
`EntryTemplate.Render`.

**What a consumer must do.** Almost certainly nothing: the bound is the
storage column's own width, so every amount that previously round-tripped
still passes. Two cases change:

- An amount that used to be accepted by `Validate` and then rejected by
  Postgres (`numeric field overflow`) is now rejected earlier, by `core`,
  with `ErrInvalidInput` instead of a driver error. Code matching on the
  Postgres error text needs to match `core.ErrInvalidInput` instead.
- An amount in scientific notation with a large exponent used to be
  accepted by `Validate` and then **not return at all**. `1E999999999`
  parses in microseconds because `shopspring/decimal` is lazy, and only
  expands when something renders it — measured, `PostJournal` did not
  return after ninety seconds, burning CPU in `math/big`. That is now a
  58µs `ErrInvalidInput`. If a consumer was sending such values, they were
  hanging, not succeeding.

Found by `FuzzJournalValidate` inside the 30-second budget CI already runs.
Reachable in library mode with nothing in front of it, and over HTTP through
the inbound webhook adapter (`channel/onchain.EVMAdapter.ParseSighting`
parses the raw body's amount directly; `server.parseWireAmount`, which
refuses `e`/`E` as a wire-format rule, is not in that path).

### `postgres.NewPendingStore` takes a `core.VerifiedBalanceReader`, and `ConfirmPending` refuses to run inside `RunInTx` when it is set

**Landed (Wave 4, contract §7.20; invariant I-64).**

    postgres.NewPendingStore(pool, ledger, classStore)
    -> postgres.NewPendingStore(pool, ledger, classStore, verifiedBalance core.VerifiedBalanceReader)

**What a consumer must do.** Through `ledger.New`: nothing. It supplies
`postgres.VerifiedBalanceStore` exactly when `ledger.WithAttestor` was called,
and `nil` otherwise. Constructing `PendingStore` directly: pass the same
reader you pass `postgres.NewReserverStore`, or `nil` to keep the previous
entries-only behaviour. The parameter is positional rather than a chained
option deliberately -- an option can be forgotten, and forgetting this one
silently disables the only check between a forged pending entry and spendable
balance.

**The run-time half, which is the part that can break a working deployment.**
With a `core.Attestor` configured, `ConfirmPending` called on a
transaction-bound clone -- i.e. from inside a `ledger.Service.RunInTx`
callback -- now returns `core.ErrInvalidInput` instead of posting. Verifying
the pending dimension may be a remote call, and `financial.md` forbids those
inside an open transaction; degrading to the ungated path instead would make
the same call gated or not depending on how it was composed, with nothing in
the result saying which.

    // before: worked, ungated
    svc.RunInTx(ctx, func(tx *ledger.Service) error {
        _, err := tx.PendingBalanceWriter().ConfirmPending(ctx, in)
        return err
    })

    // after: confirm first, then open the transaction for your own writes
    if _, err := svc.PendingBalanceWriter().ConfirmPending(ctx, in); err != nil {
        return err
    }
    svc.RunInTx(ctx, func(tx *ledger.Service) error { /* your writes */ })

`CancelPending` is unaffected and still composes inside `RunInTx` (it creates
no spendable balance, so it has no verification term). Deployments that never
call `ledger.WithAttestor` are unaffected entirely. Same guard, same reason as
`ReserverStore.Reserve`'s `RequireVerifiedBalance`.

### `(*ledger.Service).Worker(cfg)` returns `(*service.Worker, error)`

**Landed (E-M5 / B-M6).**

    worker := svc.Worker(cfg)
    -> worker, err := svc.Worker(cfg)

**What a consumer must do.** Capture the error. The only case that errors is
calling `Worker` from inside a `RunInTx` callback -- previously this built a
worker half-bound to the transaction, silently, and it started, logged
"worker: started", and failed every expiration tick against a closed
transaction forever. `docs/api-surface.txt` records the new signature
(`ledger.Service.Worker = method (cfg service.WorkerConfig) (*service.Worker,
error)`); README's own Quick Start went uncorrected across this change
(2026-09-03 consumer audit F-C2) -- if you copied `worker := svc.Worker(...)`
from an older revision of this repository's README rather than from this
file, that is why it no longer compiles.

### `(*service.Worker).Subscribe(handler)` returns `error`

**Landed (E-M1).**

    worker.Subscribe(handler)
    -> if err := worker.Subscribe(handler); err != nil { ... }

**What a consumer must do.** Check it. The only case that errors is calling
`Subscribe` after `Run` has already started -- previously this only logged
(Error level), and under the library's default `core.NopLogger` that line
went nowhere, so a handler registered after `Run` silently never fired.
`docs/api-surface.txt` records the new signature (`service.Worker.Subscribe
= method (handler func(context.Context, core.Event) error) error`).

### `(*service.Worker).Run` refuses to start under the default silent logger

**Landed (E-M1 / I-M11).** A **runtime behavior change**, not a signature
change -- no entry in `api-surface.txt`, but load-bearing for any deployment
that never called `ledger.WithLogger`.

A `Worker` built from a `Service` constructed `ledger.New(pool)` with no
`WithLogger` option now has `Run` return an error immediately, before
starting any job:

    service: worker: refusing to start with the default silent logger: ...

**What a consumer must do.** Pass `ledger.WithLogger(...)` at `ledger.New`
(recommended -- every worker signal, including this one, travels over
`core.Logger` and nowhere else, so a worker booted under `NopLogger` used to
be indistinguishable from outside from one that never started at all), or
opt into the previous silent behavior explicitly with
`ledger.WithSilentWorker()` / `(*service.Worker).AllowSilent()`. A consumer
wired per an older revision of this repository's README Quick Start (no
`WithLogger`) previously ran with zero output and background jobs that
never advanced; it now fails fast at `Run` instead (2026-09-03 consumer
audit F-C2b).

### `server.New`, `server.NewWithConfig` drop the `snapshotter` and `systemRollup` parameters

**Landed (2026-08-29 review, MJ-5).**

    server.New(..., reconciler, snapshotter, systemRollup, queries, ...)
    -> server.New(..., reconciler, queries, ...)

`server.Deps.Snapshotter` and `server.Deps.SystemRollup` are removed with
them. The HTTP layer never consumed either one -- both drive the background
Worker, which a composition root assembles separately -- so the parameters
were dependencies the constructor demanded and dropped on the floor. A caller
of the positional constructors deletes the two arguments; a caller of
`server.NewFromDeps` deletes the two struct fields. Nothing else changes: the
Worker still takes them where it always did.

Prefer `server.NewFromDeps(cfg, deps)` at new call sites -- twenty-one
positional interface parameters give the compiler nothing to catch a
transposition with, which is why `Deps` exists.

### `onchain.New` returns an error

**Landed (2026-09-02 deep audit, backend remediation).**

    onchain.New(signingKey []byte) *EVMAdapter
    -> onchain.New(signingKey []byte) (*EVMAdapter, error)

The adapter used to accept a signing key of any length, including empty, and
then verify inbound webhook HMACs against it -- an empty key makes every
forged callback verify. It now refuses a key shorter than the minimum and
says so at construction, which is the only place a deployment can still fix
it. Callers add error handling; a deployment whose key was too short was
never actually verifying anything and must now supply a real one.

### `core.JournalQuerier.ListRecentJournals`, `core.Sweeper.ReplacementGasPrice`, `service.AttestationStore.RecordAnchorObservation` / `HighestObservedAnchorSeq`, `service.ReconcileQuerier.PeriodCloseViolations`, `service.RollupQueuer.CountStuckRollups`

**Landed (2026-09-02 deep audit: W1-onchain, D-tamper, D-lock, D-ops).** Six
methods added to five existing exported interfaces. Go has no sealed
interfaces, so a hand-written implementation of any of them stops compiling
until the method is added; the `postgres` adapters this repository ships
implement all six, so a consumer using them needs no change.

What each is for, since an implementor has to make it mean something:

- `ListRecentJournals(ctx, limit) ([]Journal, error)` -- newest-first sample
  of journals. Required by attestation verification and reconciliation, both
  of which must see the most recent rows; the ascending `ListJournals` page
  they used before could never contain a freshly forged one.
- `ReplacementGasPrice(ctx, chainID, signerNonce, priorTxHash)` -- the gas
  price a replacement sweep transaction must pay for the given nonce.
  Returning a price that does not exceed the stuck transaction's leaves the
  sweep stuck.
- `RecordAnchorObservation(ctx, seq, head)` / `HighestObservedAnchorSeq(ctx)`
  -- the local record of anchor sequence numbers already seen, which is what
  makes an anchor rollback detectable. An implementation that forgets
  (returns 0) disables rollback detection silently.
- `PeriodCloseViolations(ctx, pageLimit)` -- journals dated into a closed
  period. Reconciliation reports it as a check; an implementation returning
  an empty slice reports a clean ledger it did not verify.
- `CountStuckRollups(ctx)` -- balance-rollup queue items that exhausted their
  retry budget, exported as a gauge. Zero means "the queue is healthy", so a
  stub that returns zero is an alert that never fires.

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

Also part of the same install path (D-M2, amended by M-5): `Migrate` now runs
`001` on its own and then runs `002..N` on a single connection it has switched
to `ledger_owner` (`SET ROLE`) — not by granting the migrating credential
`ledger_owner`'s privileges, which would elevate every other session holding
that credential for the length of the run. Where the credential cannot yet
switch roles, `Migrate` grants itself `ledger_owner WITH SET TRUE, INHERIT
FALSE` for the run and revokes it on every exit path.

⚠️ **`Migrate` now refuses to run while another session is connected as the
migration credential** (non-superuser path only). A credential that can act as
`ledger_owner` is one any session holding it can `SET ROLE` to, so a
single-credential deployment is refused rather than silently tolerated: the
error names the session count from `pg_stat_activity` and the remedy. **This
breaks in-process migration on a live pod that shares one credential with its
own pool** — the shape every `examples/*/main.go` warns about. Fix it with a
separate `MIGRATE_DATABASE_URL`, by migrating as a deploy step before the
application starts, or by migrating on a superuser / `ledger_owner`
connection, which arranges nothing and is not subject to the check.

Two more shapes need action: running `Migrate` as a third-party role that can
neither `SET ROLE ledger_owner` nor grant itself that (no ADMIN OPTION) now
fails up front with the three ways out listed, where it previously died at
`002` with a bare `42501` and left the database marked dirty; and on the
non-superuser path `002..N` execute *as* `ledger_owner`, so migration `007`
can no longer use the runner's own `CREATEROLE` to strip a privileged
attribute off a pre-existing `ledger_owner`/`ledger_app`/`ledger_ro` — it
stops the install and asks for a superuser connection instead. A fresh install
is unaffected by the last.

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

### Migration `028`: signed reservation discharge claims (schema + gated-hold behaviour)

**Landed (2026-09-03, Wave 4, remediation contract §7.18;
`docs/INVARIANTS.md` I-65.)** No Go symbol was removed or re-signed --
`postgres.ReserverStore.WithAuth` and `.SetLogger` are additive, and
`ledger.WithAttestor` keeps its signature. This entry is here under kind 3
(schema / data) and for one **run-time** behaviour change.

**What a consumer must do.** Run migration `028`. It adds `auth_digest`,
`auth_signature` and `auth_key_id` (`NOT NULL DEFAULT ''`) to
`reservation_operation_receipts` and `reservation_settlement_legs`. Existing
rows are not backfilled and need no backfill -- an empty signature means
"this claim is not trusted", which produces exactly the pre-028 hold. Both
tables' `created_at` is now written explicitly by the application when the
claim is signed (the digest covers it) instead of defaulting to `now()`;
same instant to microsecond resolution, but a consumer that queried those
columns expecting a strictly server-assigned clock should know it is now
application-assigned on the signed path. Nothing outside those two tables
changes, and no HTTP field changes.

**The run-time change, and who sees it.** Only a consumer that BOTH calls
`ledger.WithAttestor` AND sets `core.ReserveInput.RequireVerifiedBalance`:
for those calls, a legitimate `Settle` / `SettlePartial` / `Release` /
`FinalizeSettlement` now discharges the hold **immediately** instead of at
`ExpiresAt`. That is strictly more permissive than the rule migration `025`
shipped -- so if you have code that depended on the funds staying held until
expiry (a de-facto cooldown, say), it no longer does. Everyone else is
unaffected: without `WithAttestor` the gate runs the same query with the
same result, and the ungated `Reserve`, `HeldAmount` and
`GetBalanceBreakdown` were never involved.

**One asymmetry worth planning around.** A discharge claim written from
inside your own transaction (`Settle`/`Release` on a store bound by
`ledger.Service.RunInTx`) cannot be signed -- there is no safe point to call
a possibly-remote `Attestor` inside an open transaction -- so those
reservations keep holding until expiry even with signing on. Call those four
operations on the top-level `Service` if you need the funds back at once.

Rolling `028` back drops the columns and returns every gated hold to the
conservative rule; safe in the direction that matters (it can only hold more
money, never release more).

### Migration `030`: `TEMPORARY` is revoked from `PUBLIC`, and the migration credential must be able to do it

**Landed (Wave 5; invariant I-68; audit `install-roles.md` C1/C2).**

Migration `030` closes the two Critical findings that let a leaked
`ledger_app` credential commit a one-sided journal entry: it pins
`search_path = public, pg_temp` on every function in the schema, rewrites the
balance guard so it keeps no dedup state in `pg_temp`, and -- the part that
can break an install -- runs

    REVOKE TEMPORARY ON DATABASE <the ledger database> FROM PUBLIC;

**What a consumer must do.**

1. **If anything you run against this database creates temporary tables on the
   `ledger_app` or `ledger_ro` credential, it will stop working.** Nothing in
   this repository does after `030` (the balance guard's dedup set was the
   only production `CREATE TEMP` and `030` deletes it), and the privilege is
   what both C1 and C2 needed, so it goes. If you genuinely need it for one
   role, grant it back narrowly -- `GRANT TEMPORARY ON DATABASE <db> TO
   <role>` -- and understand that you are re-opening the vector the pins
   `postgres.TestBalanceGuard_SurvivesPgTempRelationShadowing` and
   `postgres.TestBalanceGuard_DedupSetCannotBePreSeeded` cover. Those two
   deliberately run WITH the privilege granted back, so the guard is proven to
   hold without this layer; it is depth, not the wall.

2. **The migration credential must be the database owner or a superuser**, or
   `030` stops with a named remedy instead of running. Only a database owner
   can revoke a database privilege, and migrations `002+` run as
   `ledger_owner`, which is not it -- `030` drops back to the session user
   (`SET LOCAL ROLE NONE`, restored at COMMIT) to make the attempt. The
   RUNBOOK's main path (bootstrap credential owns the database) satisfies
   this. The one documented shape that does not is "migrate an
   already-installed database using the `ledger_owner` credential itself"; for
   that deployment, have a DBA run the `REVOKE` above once and re-run the
   migration -- `030` treats already-revoked as done, so the second run
   proceeds.

   The failure is loud on purpose. Logging a warning and carrying on would
   leave a database that reports migrated while the privilege behind two
   Critical findings is still granted, which is exactly the "not run reads as
   passed" shape this repo keeps finding.

Rolling `030` back restores `TEMPORARY` to `PUBLIC` (best-effort, with a
`WARNING` if the credential cannot), unpins the ten functions and restores the
`pg_temp`-dedup balance guard -- i.e. it re-opens C1 and C2, which is what
going back to the previous release means.

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
