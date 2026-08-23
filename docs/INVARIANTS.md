# Ledger Invariants

This document collects the rules the ledger guarantees, where each is enforced,
and how to verify it. If you're auditing this codebase, embedding it as a
library, or building a sister product on top, this is the contract.

Every invariant listed here has at least one regression test. Search the
codebase for the **Pinned by** identifier to find the test that pins it.

## Notation

- **Holder dimension**: signed `int64`. `holder > 0` is user-side, `holder < 0`
  is the platform mirror, `holder == 0` is reserved/invalid.
- **Account dimension**: the tuple `(holder, currency_id, classification_id)`.
- **Amount**: `decimal.Decimal` in Go, `NUMERIC(30,18)` in Postgres, JSON string.
- **Journal**: a balanced set of debit/credit entries. Append-only. Identified
  by a unique `idempotency_key`.

---

## I-1: Per-currency journal balance

Every journal must have, **for each currency it touches**, total debits equal to
total credits.

**Why**: a multi-currency journal that balances "globally" but skews per-currency
is meaningless — debits and credits in different currencies are not comparable.

**Enforced by**:
- `core.JournalInput.Validate` (Go side, `core/journal.go:93`) — rejects
  malformed input before any DB call.
- `postgres.LedgerStore.postJournalWithQueries`'s `VerifyJournalBalanced`
  query (`postgres/sql/queries/journals.sql`) — one query per posted
  journal, in the same transaction as the entry inserts, before commit.
- `check_journal_currency_balance()` deferred constraint trigger
  (`postgres/sql/migrations/044_journal_balance_trigger.up.sql`) — the
  DB-layer backstop for direct SQL / a compromised app credential that
  bypasses everything above. Migration 004 first shipped this as a per-row
  trigger that re-scanned every entry of the journal on every row (O(N^2));
  018 dropped it for exactly that reason, leaving the DB layer unenforced
  until 044 restored it with a transaction-scoped per-journal dedup (O(N)
  overall — see the migration file for the mechanism). **044 is not
  retroactive**: rows written between 018 and 044 are not covered by the
  trigger, which is why the fleet-wide "journal_dr_cr" reconcile check
  (I-24) exists as an independent, bulk-scanning complement.
- `chk_journal_balance` table-level CHECK on `journals.total_debit = total_credit`
  (covers global totals as a defense-in-depth check).

**Pinned by**:
- `core.TestJournalInvariant_BalancedRandomEntries` (200 random trials)
- `core.TestJournalInvariant_MultiCurrencyEachMustBalance`
- `core.TestJournalInvariant_UnbalancedAlwaysRejected` (100 random drift trials)
- `core.FuzzJournalValidate` (Go fuzz target)
- `postgres.TestJournalBalanceTrigger_RejectsDirectSQLImbalance` — migrates to
  schema v41 (pre-044), proves a direct SQL insert that unbalances an
  existing journal by currency succeeds with nothing to stop it, then
  migrates up through 044 and proves the identical attack on a fresh journal
  now fails at commit.

## I-2: Append-only journals; corrections via reversal only

Once a journal is written, it is never updated or deleted. To correct a mistake,
post a **reversal journal** that points at the original via `journals.reversal_of`.

**Why**: an immutable audit trail is the basis of every regulator-readable
ledger. Allowing `UPDATE` would let a bug or bad actor edit history silently.

A journal may be reversed **in fractions** (`ReverseJournalFraction`, since
migration `029`): several reversal journals may point at the same original.
The conservation rule is: **cumulative reversed amount per original entry
never exceeds that entry's amount** — over-reversal is rejected with
`ErrConflict`. A full `ReverseJournal` is only allowed while the journal has
no reversal history; a reversal itself can never be reversed.

**Enforced by**:
- The `journals.reversal_of` FK column (added in migration `014`).
- `SELECT ... FOR UPDATE` on the original journal row serialises concurrent
  reversals — BOTH `ReverseJournalFraction` and the full `ReverseJournal`
  take it inside one transaction with their history check and insert; the
  per-dimension cumulative check runs under that lock. Migration `029`
  replaced the old "at most once" unique index with this application-level
  conservation rule, which makes the row lock load-bearing: without it two
  concurrent full reversals (different reasons → different idempotency keys)
  would both see "no history" and together post a 200% reversal.
- The `journals_no_arbitrary_update` trigger's protected-column list includes
  every column of `journals` — including `effective_at` and `uid` (migration
  `033`; 018's original list predated both) — so history cannot be edited
  around the reversal mechanism.

**Pinned by**:
- `postgres.TestLedgerStore_ReverseJournal_AlreadyReversed`
- `postgres.TestReversalChainIntegrity` (full A → ¬A → ¬¬A blocked path)
- `postgres.TestReverseJournalFraction_ConservationAndRemainderCompletion`
- `postgres.TestReverseJournalFraction_OverReversalRejected`
- `postgres.TestReverseJournalFraction_ConcurrentConservation`
- `postgres.TestReverseJournal_ConcurrentFullReversals_OnlyOneWins`
- `postgres.TestReverseJournal_MutualExclusionWithFraction`
- `postgres.TestJournals_UpdateGuard_CoversEffectiveAtAndUID`

## I-3: Idempotency on every mutation

Every state-changing operation requires an `idempotency_key`. Replaying the
same key with the same payload returns the original result and produces no
additional side effects. Reusing the same key with a different payload is a
conflict.

**Why**: in distributed systems, every retry path needs a deterministic
"is this the same thing I already did?" answer. Without it, network-flaky
clients double-charge / double-credit users.

**Enforced by**:
- `UNIQUE` constraint on `journals.idempotency_key`.
- `UNIQUE` constraint on `reservations.idempotency_key`.
- `UNIQUE` constraint on `bookings.idempotency_key`.
- Each `Validate()` method rejects empty idempotency keys at the Go boundary.
- The store layer re-reads the persisted row after a `23505` race:
  if payload matches, it returns the original record; if payload diverges,
  it returns `ErrConflict`.

`idempotency_key` shares its lifecycle with the record it's attached to —
there is no separate TTL or expiry on the key itself. A key is only as
replayable as the row it lives on. Before ever archiving, truncating, or
otherwise removing main records (journals, reservations, bookings), the
replay semantics for their idempotency keys must be defined first: does a
retry after archival re-create the record, return `ErrConflict`, or error
outright? No such cleanup path exists in this codebase today — this note
exists so the first one that gets built doesn't skip the question.

`SettlePartial` is an accumulator (`settled_amount += x`), so its idempotency
needs a durable per-application record: each increment writes a
`reservation_settlement_legs` row keyed by the caller's idempotency key
(migration `034`). A replayed key with the same amount succeeds without
re-applying — even after the reservation is finalized — and a replayed key
with a different amount (or another reservation) is `ErrConflict`.

`ConfirmPending`/`CancelPending` re-check their idempotency key **under the
balance advisory lock**, before the pending-balance gate: a retry racing its
original request must resolve to the original journal, never to
`ErrInsufficientBalance` for a confirm that in fact succeeded.

**Pinned by**:
- `core.TestJournalInput_Validate_NoIdempotencyKey`
- `postgres.TestLedgerStore_PostJournal_Idempotent`
- `postgres.TestPendingStore_AddPending_Idempotent`
- `postgres.TestReserverStore_Reserve_Idempotent`
- `postgres.TestIdempotency_ConcurrentSameKey` (100 goroutines, same key)
- `postgres.TestSettlePartial_IdempotentReplay`
- `postgres.TestConfirmPending_ConcurrentSameKey_NeverInsufficientBalance`

## I-4: TOCTOU-safe reserve/settle

Reservation creation atomically (a) takes a per-(holder, currency) advisory
lock, (b) re-checks `available = Σ balance(role=available) - SUM(active
reservations)` (the I-11 basis), and (c) inserts the reservation. Settle and
Release transition the same row under its own row lock.

**Why**: classic time-of-check / time-of-use bug. Two concurrent reserve calls
can each read "balance is enough", then both insert reservations, leaving the
holder over-committed.

**Enforced by**:
- Advisory lock in `postgres.ReserverStore.Reserve` (acquired before balance read).
- `SELECT ... FOR UPDATE` on the reservation row in `Settle` / `Release`.
- Reservation FSM transition table in `core/reserve.go` rejects illegal moves.

**Pinned by**:
- `postgres.TestReserverStore_Reserve_Concurrent`
- `core.TestReservationStatus_AllTransitions`
- `core.TestReservationStatus_TerminalStatesAreSticky`

## I-5: Real-time balance = checkpoint + delta

Account balance is computed as
`checkpoint.balance + Σ(entries with id > checkpoint.last_entry_id)`.
The full computation runs inside a `REPEATABLE READ` transaction so the
checkpoint and the delta describe the same point in time.

**Why**: rollups can lag the journal stream. A naïve "read the checkpoint"
balance misses everything posted in the last few seconds. Deltas plus
isolation give us a balance that's consistent and current.

**Enforced by**:
- `postgres.LedgerStore.GetBalance` (transaction-wrapped).
- `postgres.PlatformBalanceStore.GetPlatformBalances` (LATERAL JOIN with delta).
- Rollup worker advances checkpoints lazily.

**Load-bearing prerequisite**: every `journal_entries` write goes through the
single choke point `postJournalWithQueries`, which holds the per
`(holder, currency)` advisory lock (I-4) from before id allocation until
commit. That serializes commit order = id order within a pair, which is what
lets the rollup use `MAX(id)` as a safe checkpoint watermark and lets
`checkpoint + Σ(id > last_entry_id)` never skip an entry. Any future write
path that inserts entries without `acquireBalanceLocks` silently reopens
this visibility race — do not add one.

**Pinned by**:
- `postgres.TestLedgerStore_GetBalance_MultipleJournals`
- `postgres.TestPlatformBalance_RealtimeReflectsUnrolledJournal`
- `postgres.TestQueryStore_GetSystemRollups_RealtimeReflectsUnrolledJournal`

## I-6: Decimal precision is `NUMERIC(30,18)`

All monetary amounts are 18 fractional digits, preserved end-to-end.
Go uses `shopspring/decimal.Decimal`. Postgres uses `NUMERIC(30,18)`. JSON
uses string encoding (`"123.456"`), never JSON number.

**Why**: float64 is not closed under decimal arithmetic; rounding noise on
financial sums is unacceptable. 18 digits accommodates Ethereum wei
(1e18 base units per ether) and is a Postgres-native scale.

**Enforced by**:
- Schema: every amount column is `NUMERIC(30,18) NOT NULL`.
- Go: every amount field is `decimal.Decimal`. No `float64` or `int64-as-amount`
  at any boundary.
- JSON: `decimal.Decimal` serialises as quoted string by default.

**Pinned by**:
- `core.TestJournalInvariant_HighPrecisionAmounts` (1e-18 round-trip)
- `pkg/httpx.TestDecode_*` (string→decimal decode path)

## I-7: NOT NULL by default; documented exceptions only

Every column is `NOT NULL` with a meaningful default (`0`, `''`, `epoch`, `'{}'`).

**Exceptions**, all FK-target columns where `0` is not a valid sentinel
because PostgreSQL needs a real `NULL` to skip referential-integrity enforcement:

- `journals.reversal_of` — null when the journal is original (not a reversal).
- `bookings.journal_id` — null until accounting is posted.
- `bookings.reservation_id` — null until / unless a reservation is linked.
- `events.journal_id` — null until an event has caused a journal posting.
- `reservations.journal_id` — null until a journal is linked (migration `035`
  restored the FK that `017` dropped and `018` forgot to restore; the `0`
  sentinel era left wrong ids silently accepted).
- `journals.event_id` — null until the journal is linked to the event that
  caused it (migration `045` converted the `014` sentinel column to this
  shape and added the FK it never had; see I-25).

**Why**: NOT NULL eliminates a category of "missing vs zero" ambiguities.
Where it would conflict with FK enforcement, `NULL` is documented and the Go
field is `*int64` (in Go structs that expose the column directly — the
`postgres` adapter's `sqlcgen.Journal.EventID` is the only current holder of
this one, as `pgtype.Int8`; `core.Journal` exposes it as `EventUID string`).

**Enforced by**:
- Migration `017_no_null_cleanup` for the bulk move.
- Migration `018_restore_referential_integrity` for the four exceptions.
- Migration `045_mutation_guards` for `journals.event_id`.

## I-8: Lifecycle FSM is well-formed

Every classification's `Lifecycle` (state machine) must satisfy:

1. Initial status has at least one outgoing transition (and may not be Terminal).
2. Terminal statuses have no outgoing transitions.
3. Every transition target is either declared as Terminal or has its own
   transition entry (no dead-end status references).

**Why**: a malformed lifecycle is a runtime time bomb — bookings could enter
states they can never leave, or transitions could resolve to undefined states.

**Enforced by**: `core.Lifecycle.Validate` (`core/types.go:22`).

**Pinned by**:
- `core.TestLifecycle_Validate`
- `core.TestLifecycle_DeadEndStatusRejected`
- `core.TestLifecycle_InitialCannotBeTerminal`
- `core.FuzzLifecycleValidate`

## I-9: System holder is the negation of user holder

`SystemAccountHolder(userID) == -userID`. `IsSystemAccount(holder) == holder < 0`.
`UserHolderFromSystem(sysHolder) == -sysHolder`. The map is reversible without
external lookup.

**Why**: keeps the library platform-agnostic. Each consuming service decides
what `userID` means (user-row id, workspace id, tenant id). Library does not
encode any platform-specific ID-space transform.

**Enforced by**: `core/types.go:108` (one helper, four lines).

**Pinned by**:
- `core.TestSystemAccountHolder_RoundTrip`
- `core.TestIsUserAccount`

## I-10: Events and journals share a transaction

When a booking transition causes accounting (a journal posting), the caller can
compose the transition and the journal post inside `ledger.Service.RunInTx`.
When the journal is posted with `JournalInput.EventID` / `TemplateParams.EventID`,
the event row and the journal row are written in the **same** Postgres
transaction, `event.journal_id` is backfilled, and the booking's `journal_id`
is linked before commit. There is no committed window where one exists without
the other.

**Why**: consumers reading the event stream need to be able to fetch the
matching journal in a follow-up query without race-window logic. Reverse
also holds: an audit trail starting from the journal can always find its
"reason" event.

**Enforced by**:
- `postgres.BookingStore.Transition` inserts the event inside the caller tx.
- `postgres.LedgerStore.PostJournal` links `events.journal_id` and `bookings.journal_id` when `EventID` is supplied.
- `ledger.Service.RunInTx` provides the shared transaction boundary.

**Pinned by**:
- `postgres.TestAudit_TraceBooking` (booking → events → journals stitch)
- `postgres.TestIntegration_FullLedgerFlow`

## I-11: Reservation cannot exceed available balance

`available = Σ balance(role=available) - SUM(outstanding holds on same
dimension)`. A reservation request for `amount > available` is rejected with
`ErrInsufficientBalance`. An `active` reservation holds its full
`reserved_amount`; a `settling` one (partially settled via `SettlePartial`,
since migration `029`'s companion changes) holds its unsettled remainder
(`reserved_amount - settled_amount`) — dropping that remainder from the sum
would let a concurrent Reserve over-commit the moment the first partial
settlement lands.

The availability **base** is the sum of the holder's classifications tagged
`balance_role = 'available'` (migration `032`) — not the sum of every
classification. Pending deposits (`role=pending`), journal-locked funds
(`role=locked`), and role-less classifications (`fee_expense` and friends)
are not reservable. The old all-classifications basis let a holder reserve
against an unconfirmed deposit (double-spend if the deposit is later
cancelled) and let a debit-normal expense classification *inflate* the
reservable figure.

The same role sums power `BalanceReader.GetBalanceBreakdown`
(`GET /balances/{holder}/{currency}/breakdown`):

    pending   = Σ balance(role=pending)
    locked    = Σ balance(role=locked) + held
    available = Σ balance(role=available) − held
    total     = available + locked + pending

so the `available` a consumer reads is exactly the figure Reserve enforces.

**Why**: the obvious one — overdraft prevention. The non-obvious part: this
must be checked **inside** the advisory lock (see I-4), not before.

**Enforced by**: `postgres.ReserverStore.Reserve` (lock → check → insert),
`postgres.LedgerStore.sumBalancesByRoleWithQueries` (shared basis),
`classifications.balance_role` CHECK constraint (migration `032`).

**Pinned by**:
- `postgres.TestReserverStore_Reserve_Concurrent`
- `postgres.TestReserverStore_SettlePartial_RemainderStillHeld`
- `postgres.TestGetBalanceBreakdown_RolesPlusHolds`
- `postgres.TestReserve_AvailableBasisExcludesPendingLockedAndRoleless`
- `postgres.TestReserve_PendingOnlyBalanceNotReservable`
- `postgres.TestInstallPresets_BalanceRoleUpgradeAndConflict` (expand-safe
  role upgrade on preset re-install)

## I-12: Money conservation across the system

The sum of all journal entries across all accounts equals zero per currency,
at all times. There is no operation in this ledger that creates or destroys
value — every debit has a matching credit.

**Why**: this is the *one* invariant the rest of the system serves. If it
fails, the ledger is broken.

**Enforced by**: I-1 + I-2 together (every journal balances; nothing is ever
deleted).

**Pinned by**:
- `postgres.TestMoneyConservation_Network` (N×M×K large-scale random journal
  sequence)
- `service.TestReconciliationService_BalancedSystem`
- `service.TestCheck4AccountingEquation_Balanced` and the per-currency variant

## I-13: Partition coverage is total

`journal_entries` is `PARTITION BY RANGE (created_at)`. A default partition
catches any row whose date falls outside named partitions. Reads via the
indexed dimension `(holder, currency, classification)` correctly union across
partitions.

**Why**: partitioning is a performance/maintenance feature; if a row falls
through the cracks (no partition match, no default), the insert fails. The
default partition prevents that, and the read invariant must hold across
partition boundaries.

**Enforced by**:
- Migration `004_ledger` declares partitioning.
- Migration `010_default_partition` creates the catch-all.
- Migration `037_journal_entries_monthly_partitions` moves historical rows
  into named monthly partitions (`journal_entries_yYYYYmMM`) and pre-creates
  a rolling horizon.
- The worker's `partition` job (`service/partition.go` +
  `postgres/partition_store.go`) keeps the horizon `PartitionMonthsAhead`
  months ahead of the clock (advisory-locked, `PartitionInterval` cadence),
  and rebalances any rows that ever strand in the default partition.

**Current state**: monthly partitions are active. The default partition
remains attached as the catch-all safety net and should always be empty —
rows appearing there are an alertable anomaly (the partition job logs an
error and rebalances them on its next run). Archival guidance for old
partitions lives in `RUNBOOK.md` §11.

**Pinned by**: `TestPartitions_MigrationCreatesHorizon`,
`TestPartitions_EnsureMonthlyPartitions`,
`TestPartitions_RebalanceStrandedDefaultRows`
(postgres/partition_store_test.go), plus the pre-existing I-13
cross-partition read pin in postgres/invariants_test.go.

## I-14: Effective date consistency

`journal_entries.effective_at` is always equal to the `effective_at` of its
parent journal (denormalized at write time, never independently set per
entry). `effective_at` is never more than a 5-minute clock-skew tolerance
ahead of the time it was written — future-dated ("scheduled") posting is not
supported.

**Why**: `effective_at` separates the business date a journal is attributed
to from `created_at` (the write date), enabling retroactive posting (late
invoices, delayed on-chain confirmations). As-of reporting (trial balance,
balance trends, daily snapshots) reads `effective_at` directly off
`journal_entries` — if it ever drifted from the parent journal's value, or a
caller could schedule a journal into the future, those reports would silently
misattribute or hide postings. See
`docs/plans/2026-07-02-financial-core-hardening-design.md` §1.

**Enforced by**:
- `core.JournalInput.Validate` rejects `effective_at` beyond the future
  tolerance.
- `postgres.LedgerStore.postJournalWithQueries` defaults a zero `effective_at`
  to `now()` and writes the same resolved value to the journal row and every
  entry row in the same transaction.
- Reversal journals (`ReverseJournal`) never copy the original journal's
  `effective_at` — they always default to "now" (open period), which is the
  standard close-then-correct pattern (see I-15).

**Pinned by**:
- `core.TestJournalInput_Validate_EffectiveAt_Zero_OK`,
  `..._Past_OK`, `..._WithinTolerance_OK`, `..._FarFuture_Rejected`
- `postgres.TestMigration025_EffectiveAtColumnsExist` (schema pin)
- `postgres.TestLedgerStore_PostJournal_EffectiveAt_DefaultsToNow`
- `postgres.TestLedgerStore_PostJournal_EffectiveAt_Backdated` (also pins
  entry/journal `effective_at` equality)
- `postgres.TestLedgerStore_PostJournal_EffectiveAt_RejectsFarFuture`
- `postgres.TestLedgerStore_ReverseJournal_EffectiveAt_DoesNotInheritOriginal`
- `postgres.TestRollupAdapter_ListBalancesAt_UsesEffectiveAt` (as-of reporting
  reads the business date, not the write date)

## I-15: The accounting period close line is a hard write barrier

There is no journal whose `effective_at` is earlier than the currently active
period-close line (`period_closes`, latest-`created_at`-row-wins). Real-time
balances (`checkpoint + delta`) are unaffected — the close line only gates
*new writes*, it never rewrites or hides history.

**Why**: without a close line, any historical report can be silently changed
by a later retroactive posting — "the books for last month are final" has no
enforcement. Reopening (appending a row with an earlier `close_before`) is a
deliberate, audited, explicit action (an append-only row), never an implicit
side effect. Corrections to a closed period are made by reversing at the
current (open) date, never by rewriting history — consistent with I-2
(corrections via reversal only).

**Enforced by**: `postgres.LedgerStore.postJournalWithQueries` reads the
active close line (`GetActivePeriodClose`) inside the same transaction as
every write path (direct `PostJournal`, `ExecuteTemplate`,
`ExecuteTemplateBatch`, and `ReverseJournal`, since they all funnel through
this method) and rejects with `core.ErrPeriodClosed` when
`effective_at < close_before`.

**Pinned by**:
- `postgres.TestMigration026_PeriodClosesTableExists` (schema pin)
- `postgres.TestPeriodCloseStore_ActiveCloseLine_NeverClosed`
- `postgres.TestLedgerStore_PostJournal_PeriodClosed_Rejected` (rejects before
  the line, accepts at/after it)
- `postgres.TestPeriodCloseStore_Reopen_LatestRowWins` (reopen restores
  postability; full close-line history is retained)
- `postgres.TestLedgerStore_ReverseJournal_AfterPeriodClose_PostsAtCurrentPeriod`
  (correction-via-reversal lands in the open period)

**Pinned by**:
- `postgres.TestLedgerStore_PostJournal_PeriodClosed_Rejected` — a posting whose
  effective date falls before the active close line is refused
- `postgres.TestPeriodCloseStore_ActiveCloseLine_NeverClosed` — nothing to
  enforce before the first close
- `postgres.TestPeriodCloseStore_Reopen_LatestRowWins` — reopening is an append,
  latest row wins
- `postgres.TestLedgerStore_ReverseJournal_AfterPeriodClose_PostsAtCurrentPeriod`
  — correction-via-reversal lands in the open period
- `postgres.TestPeriodClosesGuard_NoUpdateNoDelete` — the close log itself is
  append-only (migration 045, attack path A5)
- `core.TestClosePeriodInput_Validate_RequiresCloseBefore`

> Corrected 2026-08-21: this section previously cited two
> PartitionBoundary tests (names deliberately unquoted here so the pin checker
> does not read this note as a citation) — partition-coverage tests belonging to
> I-13's subject matter, one of which never existed under that name at all. I-15 was in fact pinned the whole time, by the tests above; the
> citation was wrong, so the invariant *read* as verified by tests that could not
> possibly verify it. Found by `core.TestInvariantsDocPinsAllExist`, added the
> same day precisely because this document's own "the Pinned by section is the
> contract" rule had nothing enforcing it.

## I-16: Amount precision is bounded by currency exponent

Every committed `journal_entries.amount` (and every `reservations.reserved_amount`)
has at most `currencies.exponent` decimal places. `NUMERIC(30,18)` is storage
precision; `exponent` is *business* precision — a currency like JPY (exponent=0)
or USD (exponent=2) legitimately rejects amounts a wei-denominated currency
(exponent=18) would accept.

**Why**: without a per-currency precision bound, a `0.001 JPY` entry is
perfectly legal today — every caller has to hand-roll its own precision
checks, and a missed check is a silent accounting error that only surfaces at
reconciliation time (or in an external settlement mismatch).

**Enforced by**:
- `currencies.exponent SMALLINT NOT NULL DEFAULT 18 CHECK (0..18)`
  (`postgres/sql/migrations/027_currency_exponent.up.sql`). Existing rows
  default to 18 (the loosest setting) so no historical data is invalidated.
- `postgres.validateEntriesPrecision` (`postgres/precision.go`), called from
  `LedgerStore.postJournalWithQueries` — the single choke point behind
  `PostJournal`, `ExecuteTemplate`, `ExecuteTemplateBatch`, and
  `ReverseJournal`. `PendingStore.AddPending/ConfirmPending/CancelPending`
  inherit the check for free because they all post through
  `LedgerStore.PostJournal` rather than writing entries directly.
- `postgres.validateSingleAmountPrecision` / `checkAmountPrecision`, called
  from every amount-bearing write path that does **not** flow through
  `PostJournal`: `ReserverStore.Reserve`, `ReserverStore.Settle`,
  `ReserverStore.SettlePartial`, `BookingStore.CreateBooking`, and
  `BookingStore.Transition` (non-zero settled amounts).
- The check is `amount.Equal(amount.Truncate(exponent))` — over-precise
  amounts are rejected with `core.ErrPrecisionExceeded` (bizcode 14006),
  **never** silently rounded or truncated. Rounding is the caller's explicit
  decision (`core.Round` / `core.ConvertAt` in `core/money.go`), not something
  the ledger does on the caller's behalf.
- `core.CurrencyInput.Validate` rejects `Exponent` outside `[0, 18]` before a
  currency is even created; the DB `CHECK` is defense-in-depth for the same
  bound.

**Not enforced by**: `core.Allocate` (`core/money.go`) — it requires its
`total` argument to already be exact at the target exponent (returns
`core.ErrInvalidInput` otherwise) and guarantees every returned share is
exact at that exponent too, but it is a pure function with no currency
lookup; the store-level check above is what actually gates what reaches the
database.

**Pinned by**:
- `postgres.TestPrecision_PostJournal_RejectsOverPrecisionAmount`
- `postgres.TestPrecision_PostJournal_AcceptsWholeYen`
- `postgres.TestPrecision_PostJournal_DefaultExponentStillAllowsFractionalAmounts`
- `postgres.TestPrecision_Reserve_RejectsOverPrecisionAmount`
- `postgres.TestPrecision_Reserve_AcceptsWholeYen`
- `postgres.TestPrecision_Pending_RejectsOverPrecisionAmount`
- `postgres.TestPrecision_Booking_RejectsOverPrecisionAmount`
- `postgres.TestPrecision_SettlePartial_RejectsOverPrecisionAmount`
- `postgres.TestCurrencyStore_CreateCurrency_RejectsInvalidExponent`
- `postgres.TestCurrencyStore_CreateCurrency_ExponentZero`
- `core.TestCurrencyInput_Validate`
- `core.TestRound_HalfUp` / `TestRound_HalfEven` / `TestRound_Down` / `TestRound_Up`
- `core.TestAllocate_SumEqualsTotal_KnownCases` and friends
  (`TestAllocate_RejectsNegativeWeight`, `TestAllocate_RejectsAllZeroWeights`,
  `TestAllocate_RejectsEmptyWeights`, `TestAllocate_RejectsOverPrecisionTotal`,
  `TestAllocate_ZeroTotal`, `TestAllocate_SingleWeightGetsEverything`,
  `TestAllocate_ExponentZero`)
- `core.TestAllocateInvariant_SumAlwaysEqualsTotal` (500 random trials)
- `core.FuzzAllocate` (Go fuzz target — sum(shares) == total for any valid
  total/weights/exponent)
- `core.TestConvertAt_MatchesHandCalculation`

---

## I-17: Account policy enforcement

An account dimension `(account_holder, currency_id, classification_id)` may
carry an optional `account_policies` override row. No row for a dimension
means today's default behaviour: `active`, unconstrained. When a row exists,
the most specific match wins — `(holder,currency,classification)` >
`(holder,currency,0)` > `(holder,0,0)` — and:

- `closed` rejects every entry touching that dimension, in either direction,
  with `ErrAccountClosed`. Checked per-entry, fail-fast — closed is absolute.
- `frozen` rejects a **net decrease** under that policy with `ErrAccountFrozen`.
  Net, not per-entry: a policy can be a currency- or holder-wide wildcard
  spanning several classifications in one journal (e.g.
  `PendingBalanceWriter.ConfirmPending` posts a decrease to the "pending"
  classification and an equal increase to "main_wallet" for the same holder),
  and deposits must still complete while frozen (design doc §4/§9-1: frozen
  blocks consumption, not the pending two-phase deposit flow). `Reserve` has
  no entries to net against — it is unconditionally a consumption entry
  point, so frozen/closed reject it outright.
- `enforce_min_balance` rejects a journal that would take the dimension's
  balance below `min_balance` (0 = no overdraft, negative = overdraft limit,
  positive = dust floor), evaluated once against the *net* delta across every
  entry the journal posts to that exact dimension — not per-entry, so an
  intermediate debit within a net-positive journal is not falsely rejected.

**Why**: without this, any direct `PostJournal` call could push a frozen or
closed account's balance around, or drive any account arbitrarily negative —
the only balance floor in the system was `Reserve`'s available-balance check,
which a direct journal post bypasses entirely.

**Enforced by**:
- `postgres.LedgerStore.enforceAccountPolicies`, called from
  `postJournalWithQueries` after the tx-scoped advisory locks for the
  journal's `(holder, currency)` pairs are held (I-4) and before any row is
  written — a rejection aborts the whole journal.
- `postgres.ReserverStore.Reserve`, same advisory lock, same policy
  resolution (`classification_id` fixed at 0 — a reservation isn't tied to a
  classification).
- `postgres.AccountPolicyStore.SetPolicy` acquires the same advisory lock for
  currency-specific policies (`currency_id != 0`) before writing, so a policy
  change is serialized against any journal/reserve in flight for that exact
  pair. A holder-wide policy (`currency_id == 0`) is **not** pinned to a
  single lock key this way — a policy change at that tier and a concurrent
  journal in an unrelated currency for the same holder are not linearized
  against each other. Flagged as a known gap, not silently assumed away.

**Pinned by**:
- `postgres.TestLedgerStore_AccountPolicy_StatusMatrix` (active/frozen/closed
  × increase/decrease/Reserve)
- `postgres.TestLedgerStore_ConfirmPending_SucceedsWhileFrozen` (explicit
  pin: deposit finalization is not consumption)
- `postgres.TestLedgerStore_AccountPolicy_MinBalance_*` (zero/negative/positive
  `min_balance`, and same-journal multi-entry netting)
- `postgres.TestLedgerStore_AccountPolicy_MatchPriority`
- `postgres.TestAccountPolicyStore_SetPolicy_ConcurrentWithPostJournal`
- `postgres.TestAccountPolicyStore_SetPolicy_AuditTrail`

---

## I-18: uid-only external identity

Every entity's externally visible identifier is its `uid` — a UUIDv7 generated
Go-side at insert time. Internal `BIGSERIAL` ids exist only inside storage
(primary keys, foreign keys, advisory-lock keys, keyset-pagination cursors) and
appear in **no public contract**: not in HTTP request or response bodies, not
in path or query parameters, and not in the library-mode Go API (`core` types
and interfaces speak uids exclusively). Pagination cursors that encode an
internal position are opaque base64 strings.

**Why**: bigserial ids leak write ordering and table cardinality, invite
enumeration, and weld consumers to a storage implementation detail. A single
identifier namespace (uid) keeps the storage layer free to change and makes
every external reference stable across dump/restore.

**Enforced by**:
- Migration 031 (`uid UUID NOT NULL` + unique index on every entity table; no
  DB default, so a write path that forgets to mint a uid fails loudly)
- `postgres/dims.go` + per-store `uidToPG`/`pgToUID` conversion at the adapter
  boundary

**Pinned by**:
- `server.TestContract_NoInternalIDKeysInJSON` (mechanical source scan: no
  internal-id JSON key in any handler request/response struct)
- `service.TestReconcileFindings_NoInternalIDPatternsInSource` (the reconcile
  report is an API response body; its free-text Description/Detail strings
  carry uids/codes, never internal ids — per-row forensics go to server logs)

---

## I-19: Sweep bookings never post a journal

A `sweep` booking (`presets.SweepClassificationCode` — the crypto-deposit
design's on-chain collection batches,
docs/plans/2026-07-11-crypto-deposit-sweep-design.md §4) exists purely for
idempotency (booking key = `sweep-{chain_id}-{token}-{signer_nonce}`), for
lifecycle bookkeeping (`pending -> sent -> confirmed | failed(-> pending
retry)`), and for an audit trail of one batch collection transaction. It is
installed with **no journal type, no entry template, and no `BalanceRole`**
(its `NormalSide` is inert, fixed to `NormalSideCredit` by convention) — a
sweep batch moving funds from a custody address to treasury is a
channel/custody movement, not a user-facing accounting event: the value it
moves was already recognized when the deposit was confirmed. Posting a
journal for it would double-count that value (`financial.md`:
"渠道/托管资金移动不进账本").

**Why**: sweep addresses are predictable (`salt = account_holder`, factory
public — design doc §5-2) and their balances are attacker-adjacent dust
targets; keeping sweep entirely outside the journal means a compromised
sweep path (forged "confirmed", stuck batch, wrong nonce) can waste gas or
force a redundant collection, but can **never** fabricate or erase a
liability, credit a holder, or unbalance a journal. The blast radius of a
sweep-side bug is bounded to "no-op or wasted gas," by construction, not by
runtime validation.

**Enforced by**:
- `presets.SweepLifecycle` / `presets.InstallSweepClassification`
  (`presets/sweep.go`) only calls `ClassificationStore.CreateClassification`
  — it is never given a `JournalTypeStore` or `TemplateStore` handle, so no
  code path exists by which installing the sweep classification could also
  install a journal type or template for it, and no journal template ever
  references classification code `sweep`.
- `service.Onchain`'s sweep orchestration (`service/onchain.go`) never calls
  `JournalWriter.PostJournal`/`ExecuteTemplate` for a sweep booking's
  transitions — only `Booker.Transition`.

**Pinned by**:
- `postgres.TestSweepBooking_NeverPostsJournal` (drives a sweep booking
  through pending → sent → confirmed, asserting `journal_uid` stays empty at
  every step)
- `presets.TestInstallCryptoDepositBundle` (asserts
  `journalStore.GetJournalTypeByCode(ctx, SweepClassificationCode)` returns
  `core.ErrNotFound` after installing the full bundle)
- `service.TestOnchain_Sweep_NonceReuseAndNoJournal` (sweep job end to end:
  nonce reuse across retries, and no journal on any transition)

---

## I-20: Deposit booking idempotency survives a reorg

A crypto deposit's booking idempotency key is
`deposit-{chain_id}-{tx_hash}-{txlog_seq}`, where `txlog_seq` is the
transfer's ordinal position **among the logs in that transaction that credit
one of our registered addresses** (tx-local, deterministic) — never the
chain's block-level `log_index` (design doc §3). A reorg that re-includes the
same transaction in a different block reassigns block-level log indices, but
never reorders the transfers within the transaction itself, so `txlog_seq`
for a given transfer is stable across the reorg while `log_index` is not.

**Why**: both ingestion paths (the chains/evm watcher's `eth_getLogs` poll
and the onchain webhook) may observe the same transfer more than once as
confirmations accrue and as the chain reorganizes around it. If the
idempotency key were built from block-level `log_index`, a reorg would
silently mint a fresh key for an already-recorded transfer — a duplicate
deposit booking and, worse, a duplicate `deposit_confirm` journal: free
money. Keying off the tx-local ordinal instead means the same transfer,
observed any number of times by either path, resolves to the same booking
(`Booker.CreateBooking`'s existing same-key/same-payload idempotency, I-3)
instead of creating a duplicate.

**Enforced by**:
- `core.DepositSighting.TxLogSeq` (`core/onchain.go`) — the field's doc
  comment and shape deliberately exclude any block-level log index; both
  ingestion paths (chains/evm watcher and the `channel/onchain` webhook
  bridge) must derive this from the transaction's internal transfer
  ordering, not from `eth_getLogs`' block-scoped `logIndex`.
- `service.Onchain`'s `depositIdempotencyKey`
  (`deposit-{chain_id}-{tx_hash}-{txlog_seq}`) is constructed once, at
  booking-creation time, by the shared `IngestDeposit` orchestration — not
  by either ingestion path individually, so both paths necessarily agree on
  it for the same transfer.
- `core.DepositSighting.BlockNumber` is also reorg-variant (the same tx can
  be re-mined into a different block) but, unlike `Confirmations`, it IS
  persisted on the booking's `Metadata` — the recheck loop needs it back to
  recompute confirmations without re-scanning the chain. To keep it from
  breaking idempotent replays, `postgres.bookingMetadataMatches`
  (`postgres/idempotency_match.go`) deliberately excludes this one metadata
  key from `CreateBooking`'s payload-equality check; every other field
  (including every other `Metadata` key) is still compared exactly.

**Pinned by**:
- `postgres.TestDepositBooking_IdempotencyKey_StableAcrossBlockNumberChurn`
  (re-ingesting the identical sighting with a DIFFERENT `block_number`,
  simulating a reorg re-mining the tx into a different block, resolves to
  the same booking — not `ErrConflict`)
- `service.TestOnchain_IngestDeposit_FullLifecycle` (end-to-end:
  re-observing the same sighting is a pure no-op; a second Transfer log in
  the same tx with a different `txlog_seq` does not collide)
- `onchain.TestEVMAdapter_ParseSighting` (the webhook bridge derives
  `TxLogSeq` from the payload's tx-local `txlog_seq` field, never a
  block-scoped index; also requires `block_number` per
  `core.DepositSighting.Validate`)

## I-21: Review holds a deposit with zero ledger effect

A crypto deposit routed to `review` instead of `confirmed`
(docs/plans/2026-07-11-crypto-deposit-sweep-design.md §9: M3 compensating
controls -- the threshold gate, `TokenConfig.AutoCreditCeiling`, or the
reconciliation gate, `OnchainDeps.DepositConfirmer` disagreeing with the
primary sighting) has **posted no journal**. Its `journal_uid` is empty for
as long as it sits in `review`, exactly like a `pending`/`confirming`
booking that has not yet reached its confirmation threshold. The account
holder's balance is unaffected. Only a human calling
`service.Onchain.ApproveReview` moves it to `confirmed` and, at that point
(and not before), posts the `deposit_confirm` journal via the same
`postDepositConfirmedJournal` path (and same `EventUID` cross-link) the
normal `confirming -> confirmed` transition uses. `RejectReview` moves it to
`failed` and never posts a journal at all.

**Why**: `review` exists specifically because a single-source "confirmed"
signal (RPC lied, webhook secret leaked) is otherwise the deposit path's
whole trust boundary (design doc §5-1) -- an unbounded free-money bug if a
sighting could reach `confirmed`, and therefore a journal, without this
compensating control ever having a chance to catch it. If routing to review
had any accounting side effect (even a provisional credit), the control
would be theater: the "free money" the gate exists to prevent would already
be sitting in the user's balance by the time a human looks at the queue.

**Enforced by**:
- `presets.DepositLifecycle` (`presets/deposit.go`) -- `review` is reachable
  only from `confirming`, and its own only outgoing edges are `confirmed`
  and `failed`; no other status can reach `review`, and `review` cannot
  reach anything but a human-driven `ApproveReview`/`RejectReview` call.
- `service.Onchain.routeToReview` (`service/onchain.go`) calls only
  `Booker.Transition` -- it never touches `TxComposer`/`JournalWriter`.
- `service.Onchain.postDepositConfirmedJournal` is the ONLY function in the
  onchain subsystem that posts a `deposit_confirm` journal; both
  `advanceConfirmation`'s normal path and `ApproveReview` call through it,
  so there is exactly one code path that can ever credit a deposit.
- `service.Onchain.RejectReview` calls only `Booker.Transition` to `failed`,
  mirroring `routeToReview` -- never `TxComposer`.

**Pinned by**:
- `service.TestOnchain_IngestDeposit_OverCeiling_RoutesToReview` (an amount
  above `AutoCreditCeiling` reaching its confirmation threshold transitions
  to `review`, not `confirmed`, with an empty `journal_uid`)
- `service.TestOnchain_IngestDeposit_ReconcileMismatch_RoutesToReview` (a
  configured `DepositConfirmer` disagreeing with the primary sighting routes
  to `review` with an empty `journal_uid`, even though the primary source
  alone would have confirmed)
- `service.TestOnchain_ApproveReview_PostsJournalWithEventLink` (approving a
  reviewed booking transitions it to `confirmed` and posts its
  `deposit_confirm` journal, cross-linked via `EventUID`, only at that point)
- `service.TestOnchain_RejectReview_NoJournal` (rejecting a reviewed booking
  transitions it to `failed` with `journal_uid` remaining empty forever)

## I-22: `ledger_app` has no DDL

The application-facing database role, `ledger_app`, can `SELECT`/`INSERT`/
`UPDATE` ordinary tables and `SELECT`/`INSERT` (never `UPDATE`/`DELETE`) on
`journal_entries` — but it cannot `DROP`, `TRUNCATE`, `ALTER`, manage
triggers, or create any object, anywhere in the schema. This is true as
soon as migration 042 applies, independent of who currently owns the
tables: `ledger_app` was never granted anything beyond
`SELECT`/`INSERT`/`UPDATE` and will never own anything.

**Why**: GRANT-based privileges alone are not a defense against a
compromised application credential — before migration 042, every
environment ran with a single connection that had unrestricted DDL, so a
leaked `DATABASE_URL` (or a SQL-injection foothold) could `DROP TRIGGER` the
append-only guards, `TRUNCATE` `journal_entries`, or silently detach a
partition (attack path A6,
docs/plans/2026-08-21-tamper-evident-ledger-design.md §2). Postgres cannot
confer `ALTER`/`DROP`/`TRUNCATE`/trigger-management rights through `GRANT` —
only object ownership (or superuser) grants them — so this invariant is only
true because `ledger_app` never owns anything.

**Note on scope**: this invariant governs what the `ledger_app` *role* can
do once an environment's `DATABASE_URL` is cut over to it. Migration 042
itself performs a **pure expand** step (`deployment.md`) — creating the
roles and issuing every grant additively, with no `REVOKE` and no ownership
transfer — and does not switch any environment's connection yet. The
counterpart `ledger_owner` role does not yet own any table after 042 alone;
migration 049, a separate, later "migrate" migration (see `docs/RUNBOOK.md`
§9), performs `REVOKE ALL ON SCHEMA public FROM PUBLIC` and the ownership
transfer, and must ship in the same release as the `DATABASE_URL` cutover —
an earlier combined version of 042 that did both in one file passed every
test connecting as the new roles while actually locking the (non-superuser,
in a real managed-Postgres deployment) migration-running connection out of
its own database the instant it
committed; see `docs/RUNBOOK.md` §9 for the failure this caused and the
test that catches it.

**Note on grant coverage**: 042's GRANT loop only enumerates tables that
existed when 042 ran, and its `ALTER DEFAULT PRIVILEGES` deliberately
benefits only `ledger_owner` — every later migration that adds a table is
required to GRANT `ledger_app`/`ledger_ro` on it explicitly
(contracts.md §9 point 3). That rule was violated twice before it had a
structural check: `reconcile_scan_cursors` (043) and `checkpoint_rebuilds`
(050) were both written and merged before 042 landed and neither carried
that grant. Migration 052 closes the gap for those two tables; the pin
below (`TestGrantCoverage_*`) makes the underlying rule self-enforcing
going forward instead of depending on a migration author remembering it
(`working-agreements` §5).

**Note on ACL/trigger consistency**: the ACL and the append-only mutation
guard on a table must agree — a table protected only by a trigger, with an
ACL that still says it is updatable, is one `GRANT`-layer bypass away from
looking updatable to the next reader and one code path away from actually
being written to (the trigger still blocks it, but the two defenses no
longer say the same thing, and 042's own history — see "Note on scope"
above — is a direct example of one defense layer failing silently while a
test only exercised the other). `checkpoint_rebuilds` (050) and
`period_closes` (026, guard added by 045 A5) both carry the same
unconditional `ledger_block_mutation()` guard journal_entries does, but
were left grantable `UPDATE`; 052 corrects both. The pin derives "which
tables carry this guard" from `information_schema.triggers` (matching on
the exact `BEFORE UPDATE ... EXECUTE FUNCTION ledger_block_mutation()`
shape), not a hardcoded table list, so any future table reusing this guard
gets the matching ACL enforced automatically — and any table with only a
*partial* guard (`classifications`, `reservations`, `journals` — see A1-A4
under I-25) is correctly left with `UPDATE`, since those are legitimately
mutated through controlled paths.

**Enforced by**:
- `postgres/sql/migrations/042_ledger_roles.up.sql` — creates `ledger_owner`
  / `ledger_app` (`SELECT`/`INSERT`/`UPDATE`, no `UPDATE` on
  `journal_entries`, no DDL of any kind) / `ledger_ro`, and grants each
  additively. `REVOKE ALL ON SCHEMA public FROM PUBLIC` and the ownership
  transfer that makes `ledger_owner` DDL-capable are deliberately NOT in
  this migration (see "Note on scope" above) — they live in
  `postgres/sql/migrations/049_ledger_roles_ownership_transfer.up.sql`
  instead, which must ship in the same release as the `DATABASE_URL`
  cutover (`docs/RUNBOOK.md` §9).
- `postgres/sql/migrations/052_grant_coverage_gap.up.sql` — grants
  `ledger_app`/`ledger_ro` `SELECT`/`INSERT`(+`UPDATE` for
  `reconcile_scan_cursors`, which has no append-only guard) on
  `reconcile_scan_cursors`, `checkpoint_rebuilds`, and
  `checkpoint_rebuilds_id_seq` (see "Note on grant coverage" above); and
  `REVOKE UPDATE ON period_closes FROM ledger_app` — 042 granted it (026
  predates 042) before 045 added its append-only guard, and nothing had
  revoked it since (see "Note on ACL/trigger consistency" above).

**Pinned by**:
- `postgres.TestMigration042_LedgerAppIsLeastPrivilege` — migrates to 041
  first and confirms the single connection has *unrestricted* DDL there
  (proving the restrictions below are not vacuous), then migrates the rest
  of the way and confirms `ledger_app` cannot `TRUNCATE`/`DROP TRIGGER`/
  `ALTER TABLE`/`CREATE TABLE`/`UPDATE journal_entries`/`DELETE FROM
  journal_entries`/touch `schema_migrations`, while it can still
  `SELECT`/`INSERT`/`UPDATE` an ordinary table and `SELECT`/`INSERT`
  `journal_entries`.
- `postgres.TestMigration042_DoesNotStrandTheMigrationRunner` — migrates
  exactly to 042 through a non-superuser role that owns the database
  (simulating a managed-Postgres master user) and confirms that role can
  still write afterward. This is the regression pin for the combined-
  migration bug described above: it fails with `permission denied for
  table schema_migrations` against the old (pre-split) combined 042 and
  passes against the current (pure-expand) 042.
- `postgres.TestMigration049_StrandsTheOldConnectionByDesign` — the
  counterpart pin for 049: the same non-superuser role can still write
  after 042 alone, but loses access to business tables once 049 runs
  (`schema_migrations` is deliberately excepted -- see `docs/RUNBOOK.md`
  §9). Also proves 049 itself can apply cleanly under a non-superuser
  connection -- an earlier revision without its narrow
  schema-USAGE/schema_migrations re-grants could never successfully apply
  at all, on any non-superuser connection, regardless of `DATABASE_URL`
  cutover timing.
- `postgres.TestMigration042_LedgerAppInsertsIntoPartitionCreatedAfterGrant`
  — after manually granting `ledger_owner` ownership of `journal_entries`
  (mirroring what 049 does, scoped to just this one table so the test does
  not depend on 049's exact implementation), a partition it creates *after*
  042's grant ran is still writable by `ledger_app` through the parent
  table name.
- `postgres.TestMigration042_RoleAttributes` — pins role attributes
  (`LOGIN`, not superuser/createdb/createrole) and the exact grant set each
  role holds (`information_schema.role_table_grants`) on an ordinary table,
  `journal_entries`, and `schema_migrations`.
- `postgres.TestMigration042_DownDropsRolesAndRestoresOwnership` /
  `postgres.TestMigration049_DownRestoresOwnership` — the down migrations
  for 042 and 049 each roll back cleanly and leave the original connection
  able to operate normally.
- `postgres.TestGrantCoverage_EveryTableHasExpectedLedgerAppAndLedgerRoGrants`
  / `postgres.TestGrantCoverage_EverySequenceHasExpectedGrants` — enumerate
  every table/sequence in `public` (not a fixed list) and assert the exact
  grant shape 042's policy intends: `SELECT`/`INSERT` only for any table
  carrying a `BEFORE UPDATE ... ledger_block_mutation()` guard (derived from
  `information_schema.triggers`, not a hardcoded table list — see "Note on
  ACL/trigger consistency" above), `SELECT`/`INSERT`/`UPDATE` for every
  other table. Catches any future migration that adds an object without
  granting it, or that adds/reuses the append-only guard without a matching
  ACL. Verified red against `reconcile_scan_cursors`/`checkpoint_rebuilds`/
  `checkpoint_rebuilds_id_seq`/`period_closes` before 052, green after.

- `postgres.LedgerStore.attestJournal` / `PostJournal` (`postgres/ledger_store.go`)
  -- resolves `EffectiveAt` once, signs before `pool.Begin`, writes the
  three columns inside the transaction that also writes the journal row.
- `core.CanonicalJournalDigest` / `core.EncodeAmount` (`core/auth.go`) --
  the deterministic uid-space encoding (18-decimal fixed-point, 16-byte
  big-endian two's complement, domain-separated SHA-256) both `Sign` and
  `Verify` agree on. This encoding is the one part of P5 that cannot be
  changed later without breaking every previously-signed journal -- see
  its golden vectors below.
- Immutability of `auth_digest`/`auth_signature`/`auth_key_id` after a
  journal is signed: enforced by `ledger_journals_block_arbitrary_update()`,
  but **owned by migration 045 (P4)**, not this migration. Contracts §2
  (2026-08-21 rewrite) replaced that function's hardcoded per-migration
  column list with a generic `to_jsonb(OLD)`/`to_jsonb(NEW)` comparison
  against an explicit mutable-column whitelist, so these three columns are
  protected automatically once 045 installs it -- migration 046 does not
  (and must not) touch that function itself.
- `authdev.NewLocalAttestor` -- refuses a wrong-length seed or empty
  key_id at construction time, in the caller's own composition root,
  never silently inside the ledger.

**Pinned by** (`postgres/auth_pin_test.go` unless noted):
- `TestPostJournal_SignsWithConfiguredAttestor` -- a signed journal's stored
  digest/signature/key_id round-trip through `core.VerifyJournalAuth`
  successfully.
- `TestPostJournal_UnsignedWithoutAttestor` -- `Attestor == nil` leaves
  `PostJournal` byte-for-byte unchanged from before P5 (expand-safe).
- `TestPostJournal_IdempotentReplayDoesNotResign` -- a replayed post with
  the same idempotency key triggers exactly one `Attestor.Sign` call, not
  two.
- `TestPostJournal_AttestorErrorRejectsPost` -- a `Sign` error rejects the
  whole write; nothing is persisted.
- `TestForgedDirectSQLJournalIsUnauthorized` -- the M5 scenario itself: a
  balanced journal inserted directly via SQL (bypassing `PostJournal`
  entirely) passes a live per-journal balance check and still fails
  `core.VerifyJournalAuth`.
- `core.TestCanonicalJournalDigest_GoldenVector` / `TestEncodeAmount_GoldenVectors`
  (`core/auth_test.go`) -- pin the exact byte layout against independently
  computed values; any diff is a breaking encoding change.
- `core.TestVerifyJournalAuth_RejectsEmptyStoredDigest` /
  `RejectsMismatchedDigest` / `RejectsEmptySignature` -- each isolates one
  of `VerifyJournalAuth`'s three guard clauses; removing any one of them
  was verified, by hand, to make its corresponding test fail (the
  mismatch-check removal reaches a nil `AuthVerifier` and panics; the
  digest-emptiness removal is independently caught by the mismatch check,
  demonstrating defense-in-depth rather than a redundant no-op check).
- `authdev.TestNewLocalAttestor_DeterministicFromSameSeed` /
  `TestNewLocalVerifier_StandaloneFromPublicKey` -- the default Attestor
  implementation itself: same seed signs identically, and a verify-only
  process (holding only the public key) can check a signature it never
  had the private key to produce.

## I-23: checkpoint / system_rollups / balance_snapshots are exactly recomputable from entries; detection never auto-repairs

`balance_checkpoints` is an unreliable cache, not a source of truth — an
attacker with DB write access can set it to any value. Three things follow:

1. **A trusted, entries-only recompute path exists and is what money-leaving
   paths must use.** `core.CheckpointIntegrityStore.RecomputeBalance` sums
   `journal_entries` from entry 0 for a dimension; `balance_checkpoints` never
   appears in its query (`postgres/sql/queries/integrity_checkpoint.sql`,
   `RecomputeCheckpointFromEntries`). It is slow (full history scan) — that
   cost is the reason `BalanceReader.GetBalance` (checkpoint + delta) stays
   the default for ordinary reads; withdrawal / large-amount paths must call
   `RecomputeBalance` instead, precisely because checkpoint tampering has zero
   influence on a value that never reads the checkpoint.
2. **A poisoned checkpoint has a trusted repair path that is NOT automatic.**
   `CheckpointIntegrityStore.RebuildCheckpoint` takes the `(holder,
   currency_id)` advisory lock (the same lock space `PostJournal`/`Reserve`
   use), refuses with `core.ErrRollupPending` if a `rollup_queue` item is
   still pending/claimed for the dimension (a worker holding a stale
   checkpoint snapshot in memory could otherwise re-clobber the fix the
   moment its write lands), recomputes from entry 0, and unconditionally
   overwrites the checkpoint row (`RebuildBalanceCheckpoint`, which — unlike
   `UpsertBalanceCheckpoint` — has no monotonic `last_entry_id` guard, because
   that guard is exactly what makes an ordinary upsert unable to repair a
   checkpoint whose `last_entry_id` was tampered to look "ahead" of the true
   watermark). Detection (reconcile's `checkpoint_balance`,
   `system_rollup_integrity`, `snapshot_integrity` checks) and correction
   (`RebuildCheckpoint`) are deliberately separate calls: nothing in this
   library invokes `RebuildCheckpoint` automatically, because auto-correcting
   while an attack may still be in progress would destroy the forensic
   evidence the drift represents. A **manual** repair has that exact same
   evidence-destroying property — the drift vanishes from
   `balance_checkpoints` the instant it's overwritten, and a log line is not
   durable enough (rotation, retention limits) to stand in for it. So every
   call, in the same transaction as the overwrite, durably records the
   before/after balances, watermarks, and resulting drift in the append-only
   `checkpoint_rebuilds` table (migration `050`; the same
   `ledger_block_mutation()` no-UPDATE/no-DELETE guard 018 put on
   `journals`/`journal_entries`) — a repair can never commit without leaving
   forensic evidence, and the evidence can never exist without the repair
   having happened.
3. **`system_rollups` and `balance_snapshots` must be checked against entries
   directly, never against checkpoints as an "independent" basis.**
   `SystemRollupService.RefreshSystemRollups` populates `system_rollups` via
   `AggregateCheckpointsByClassification`, which sums `balance_checkpoints` —
   so `system_rollups` inherits any checkpoint tampering wholesale if the
   only thing verifying it is itself or the checkpoints it was built from.
   The `system_rollup_integrity` check instead compares it against
   `AccountingEquationRows`, the same entries-only recompute the
   `accounting_equation` check already performs, and flags a `system_rollups`
   row with **no** matching entries at all (the M5 fabrication scenario: a
   rollup entry manufactured out of nothing) rather than treating it as
   "unknown, skip". The `snapshot_integrity` check does the entries-based
   equivalent for `balance_snapshots`, scoped to the most recent
   `snapshot_date` to bound cost; historical dates can be re-verified and
   repaired on demand via `SnapshotBackfillService.BackfillSnapshots`, which
   already recomputes from entries for an explicit date range.

**Why**: a `balance_checkpoints` row is exactly as trustworthy as the
attacker's most recent `UPDATE`. Comparing it against anything derived from
itself (a self-check, or `system_rollups` built by summing it) can never
detect tampering — only an independent recompute straight from
`journal_entries` can. And once drift is *detected*, silently overwriting it
during an active incident destroys the evidence needed to scope the breach —
so correction is a distinct, explicit, operator-invoked action, never a side
effect of running reconcile.

**Enforced by**:
- `postgres.CheckpointIntegrityStore` (`postgres/checkpoint_integrity_store.go`) —
  `RecomputeBalance`/`RebuildCheckpoint`, backed by
  `RecomputeCheckpointFromEntries` and `RebuildBalanceCheckpoint`
  (`postgres/sql/queries/integrity_checkpoint.sql`,
  `postgres/sql/queries/checkpoints.sql`).
- `checkpoint_rebuilds` table (migration `050`) + `InsertCheckpointRebuildAudit`
  (`postgres/sql/queries/integrity_checkpoint.sql`), written atomically with
  `RebuildBalanceCheckpoint` inside the same transaction;
  `checkpoint_rebuilds_no_update` / `checkpoint_rebuilds_no_delete` triggers
  make the record itself tamper-evident.
- `service.FullReconciliationService.runCheck2GlobalBalance` (checkpoint
  vs. entries, per account) — persists its fleet-wide scan cursor and a
  `lap_dirty` flag in `reconcile_scan_cursors` (migration `043`) so a
  violation found partway through a multi-run scan cannot be buried by a
  later, cleaner segment of the same lap (C4b).
- `service.FullReconciliationService.runCheckSystemRollupIntegrity` /
  `runCheckSnapshotIntegrity` (system_rollups / balance_snapshots vs.
  entries).

**Pinned by**:
- `postgres.TestCheckpointIntegrity_RecomputeBalance_IgnoresCheckpointTampering`
  (RecomputeBalance returns the true balance while GetBalance still reads a
  poisoned checkpoint on the same dimension)
- `postgres.TestCheckpointIntegrity_RebuildCheckpoint_OvercomesMonotonicGuard`
  (poisons both balance AND `last_entry_id`; demonstrates the normal
  monotonic-guarded upsert CANNOT repair it, then that `RebuildCheckpoint`
  does)
- `postgres.TestCheckpointIntegrity_RebuildCheckpoint_RefusesWhenRollupPending`
  (a pending `rollup_queue` item for the dimension makes `RebuildCheckpoint`
  return `core.ErrRollupPending`)
- `postgres.TestCheckpointIntegrity_RebuildCheckpoint_RecordsAuditRow` (the
  before/after balances and drift land in `checkpoint_rebuilds`, matching the
  injected poison amount)
- `postgres.TestCheckpointIntegrity_CheckpointRebuilds_IsAppendOnly` (`UPDATE`
  and `DELETE` against `checkpoint_rebuilds` are both rejected)
- `service.TestCheck2GlobalBalance_ResumesFromPersistedCursor`,
  `service.TestCheck2GlobalBalance_LapDirtyPersistsAcrossRuns`,
  `service.TestCheck2GlobalBalance_PartialRunPersistsLapDirty`,
  `service.TestFullReconciliation_Check2ResumesAcrossRuns` (DB-backed: a
  3-pair fleet scanned 1 pair per run across 4 calls resumes correctly, and
  the run that completes the lap still reports `Passed=false` for a drift
  found two runs earlier)
- `service.TestCheckSystemRollupIntegrity_DetectsDrift`,
  `service.TestCheckSystemRollupIntegrity_FabricatedRowWithNoEntries`,
  `service.TestFullReconciliation_DetectsSystemRollupDriftFromPoisonedCheckpoint`
  (DB-backed: poisons a checkpoint, refreshes `system_rollups` from it, and
  requires the check to catch the drift against entries)
- `service.TestCheckSnapshotIntegrity_DetectsDrift`,
  `service.TestCheckSnapshotIntegrity_PageLimitReportsIncomplete`,
  `service.TestFullReconciliation_DetectsSnapshotDrift`

---

## I-24: Per-journal balance is enforced at the DB layer, independent of the application

Every journal's per-currency balance (I-1) is enforced by **two independent
mechanisms that do not trust each other**: the application layer
(`core.JournalInput.Validate` + `VerifyJournalBalanced`, both bypassable by
direct SQL against the database) and the DB layer (a deferred constraint
trigger on `journal_entries`, restored by migration 044 after migration 018
dropped its predecessor). Additionally, a fleet-wide reconcile check
("journal_dr_cr") scans every journal individually in bulk, independent of
the DB trigger's write-time enforcement.

**Why**: C1 (docs/plans/2026-08-21-tamper-evident-ledger-design.md §2) —
an attacker with a leaked app DB credential, or a bug that bypasses
`postJournalWithQueries`, can issue a direct SQL `INSERT` into
`journal_entries`. Before this invariant, nothing in the database itself
would stop an unbalanced insert; the only enforcement lived in application
code the attacker had already bypassed. Separately, M1 (design doc §2) —
the pre-existing "journal_dr_cr" reconcile check computed a GLOBAL
debit==credit equality, which cannot see two journals that are each
individually unbalanced but net to zero in aggregate.

**Enforced by**:
- `check_journal_currency_balance()` deferred constraint trigger,
  `AFTER INSERT OR UPDATE OR DELETE ON journal_entries FOR EACH ROW`,
  `DEFERRABLE INITIALLY DEFERRED` (`postgres/sql/migrations/044_journal_balance_trigger.up.sql`).
  Dedupes by `journal_id` within the transaction via a `pg_temp` table
  (`ON COMMIT DELETE ROWS`) so the actual aggregate check runs once per
  journal touched by the transaction, not once per row — O(N) overall, not
  004's O(N^2).
- `service.FullReconciliationService.runCheck11JournalBalance`
  ("journal_dr_cr" — `service/reconcile.go`), backed by
  `IntegrityUnbalancedJournalsCount`/`Sample`
  (`postgres/sql/queries/integrity_balance.sql`): a bulk, fleet-wide scan
  independent of the trigger, catching what the trigger cannot (rows
  written before 044 existed, or any future bypass of it).
- The pre-existing "journal_dr_cr" global-equality behavior is kept as an
  independent check, renamed `global_dr_cr_equality`
  (`service.FullReconciliationService.runCheck1JournalBalance`) — the two
  checks catch different failure modes and neither substitutes for the
  other.

**Pinned by**:
- `postgres.TestJournalBalanceTrigger_RejectsDirectSQLImbalance` — migrates
  to schema v41, proves a direct-SQL unbalancing insert succeeds with no DB
  guard; migrates up through 044 and proves the identical attack now fails.
- `postgres.TestUnbalancedJournalsFleetScan_CatchesWhatGlobalEqualityMisses` —
  crafts two journals (pre-044, via direct SQL) that are each individually
  unbalanced by currency but net to zero globally; proves the global
  equality query reports "balanced" while `IntegrityUnbalancedJournalsCount`
  reports both violations.
- `service.TestFullReconciliation_JournalBalance_DetectsPerJournalDrift` —
  mock-level pin that `runCheck11JournalBalance` reports `Passed: false`,
  logs (not leaks into the report) the offending internal ids, and that a
  clean querier reports `Passed: true, Complete: true`.
---

---

## I-25: Non-journal balance-computation tables cannot be mutated outside their controlled entry points

Every table that participates in balance computation but is not itself
`journals`/`journal_entries` has a DB-level guard against post-insert
mutation, closing the five gaps in
docs/plans/2026-08-21-tamper-evident-ledger-design.md §6 (A1-A5):

- `classifications.normal_side` is immutable — no code path updates it, and
  changing it retroactively flips the sign of every historical rollup for
  that classification.
- `classifications.balance_role`'s only legal transition is the expand-style
  `'' -> <role>` upgrade `ClassificationStore.SetBalanceRole` performs.
  Switching between two non-empty roles, or reverting to `''`, is rejected —
  including when attempted through `SetBalanceRole` itself a second time.
- `reservations.account_holder`/`currency_id`/`reserved_amount`/
  `idempotency_key`/`expires_at`/`created_at`/`uid` are immutable.
  `settled_amount` may only increase (a decrease can only be tampering,
  `SettlePartial`'s own precondition already guarantees monotonic growth).
  `journal_id` is set-once (`NULL -> non-NULL` only). `status` follows the
  whitelist state machine in `core/reserve.go` (`reservationTransitions`):
  `active -> {settling, settled, released}`, `settling -> {settled,
  released}` — `settled`/`released` are terminal, with no path back to
  `active`.
- `period_closes` is now actually append-only (previously documented as such
  but unenforced) — no `UPDATE`, no `DELETE`.
- `journals.event_id` is set-once (`NULL -> non-NULL` only) and now carries
  the FK to `events(id)` it never had. Before this, the column wasn't even in
  the anti-tamper guard's comparison list — 033's and 018's trigger comments
  described a "set-once backfill WHEN clause" that had never actually been
  implemented (018:137-140 was an unconditional `BEFORE UPDATE FOR EACH ROW`
  with no `WHEN`). The guard itself changed shape in 045: instead of a
  per-migration hardcoded column list (033's pattern, which p5-authsig's
  ordering conflict proved fragile — two `CREATE OR REPLACE`s on the same
  function in numeric-ordered migrations means the later one silently
  overwrites the earlier), it now compares `to_jsonb(OLD) - mutable` against
  `to_jsonb(NEW) - mutable` for an explicit whitelist of columns allowed to
  change post-insert (`mutable := ARRAY['event_id']` as of 045). Any future
  column added to `journals` is protected by default — fail-closed by
  construction, the same reasoning P0 used for `CheckResult.Complete`'s zero
  value.

**Why**: I-1/I-24 (journal balance) and I-12 (money conservation) only cover
`journal_entries`. A writer with app DB credentials doesn't need to touch a
single journal row to change a holder's effective balance — flipping
`normal_side` reverses how the rollup reads every existing entry;
`balance_role` reclassifies `locked` funds as `available`; enlarging
`reserved_amount` or fabricating a `reservations` row changes availability
with zero accounting trail; rewriting `journals.event_id` breaks posting
provenance without touching any protected journal column.

**Enforced by**:
- `ledger_classifications_guard()` / `classifications_mutation_guard`
  trigger (`postgres/sql/migrations/045_mutation_guards.up.sql`).
- `ledger_reservations_guard()` / `reservations_mutation_guard` trigger
  (same migration).
- `period_closes_no_update` / `period_closes_no_delete` triggers, reusing
  `ledger_block_mutation()` from 018 (same migration).
- `ledger_journals_block_arbitrary_update()` (033's per-migration column
  list replaced by 045 with a generic `to_jsonb` comparison against a
  mutable-column whitelist, per
  `docs/plans/2026-08-21-integrity-hardening-contracts.md` §2; `event_id`'s
  `NULL -> non-NULL` set-once check lives in the function body, not a
  trigger `WHEN` clause) + `journals_event_id_fkey` FK.
- Depends on P1 (migration 042): without role separation, the same
  credential that would abuse these columns can `DROP TRIGGER` the guard
  itself.

**Pinned by** (`postgres/mutation_guards_test.go`, each verified to fail —
i.e. the tamper attempt would have succeeded — with its specific guard
trigger/function manually removed before this migration existed):
- `TestClassificationsGuard_NormalSideImmutable`
- `TestClassificationsGuard_BalanceRoleOnlyUpgradesFromEmpty`
- `TestReservationsGuard_DimensionColumnsImmutable`
- `TestReservationsGuard_SettledAmountMustNotDecrease`
- `TestReservationsGuard_JournalIDSetOnce`
- `TestReservationsGuard_StatusWhitelist`
- `TestPeriodClosesGuard_NoUpdateNoDelete`
- `TestJournalsGuard_EventIDSetOnce`
- `TestJournalsGuard_FutureColumnsProtectedByDefault`

---

## I-26: Every journal that carries an authorization signature has a valid one

(docs/plans/2026-08-21-tamper-evident-ledger-design.md §7 / M5, P5 of the
integrity-hardening wave; simplified from the original task brief by Team
Lead on 2026-08-21 -- see `core/auth.go`'s package doc comment.)
`journals.auth_digest` / `auth_signature` / `auth_key_id` (migration 046)
let a journal posted through `postgres.LedgerStore.PostJournal` (with a
`core.Attestor` configured via `WithAuth`, in pool mode) carry a signature
over `core.CanonicalJournalDigest`'s canonical, uid-space encoding of the
posting -- computed and signed strictly **outside** any DB transaction
(`financial.md`: no external calls inside a DB transaction is the whole
reason the digest is uid-space, not id-space; see design doc §7.2 --
benchmarking confirmed this ordering adds pure latency without extending
any lock's hold time, since it runs before any advisory lock is taken).
`core.VerifyJournalAuth` recomputes the digest from the journal's own
fields and rejects (wrapping `core.ErrUnauthorizedJournal`) any journal
whose stored digest is empty, does not match the recomputation, or whose
signature/key_id the configured `core.AuthVerifier` does not accept. Every
journal is signed once an Attestor is configured -- there is no
per-journal-type coverage decision and no KMS-failure-mode branch; a
`Sign` error simply propagates as a plain error.

**Why**: M5 is the one finding this whole wave exists to answer --
`journals`/`journal_entries` FK integrity, per-currency balance,
append-only triggers, and the account-holder sign convention are all
satisfied by a perfectly balanced, perfectly plausible **forged** journal
inserted directly via SQL by an attacker holding DB write credentials.
Every invariant I-1 through I-25 passes on that forgery. Per-journal
signing is the only mechanism that still tells it apart from a genuine
posting, because forging a valid signature additionally requires the
Attestor's private key -- which never enters the database (design doc
§0/§7.1) -- rather than a DB write credential (design doc §1's threat
model row for "app DB 凭证"). The default implementation
(`authdev.LocalAttestor`, an in-process ed25519 key loaded from an
injected seed) is production-ready for this project's actual deployment
(a monolith, not a fleet behind a remote KMS): the threat model already
concedes "app process + signing key both compromised" as out of scope
(design doc §1 non-goal 2) for ANY key custody model, local or remote, so
a local key satisfies the same guarantee.

**Scope note (honest, not silently narrowed; updated 2026-08-21, design doc
§7.5, board #12/#13)**: `PostJournal`'s tx-mode branch, `ExecuteTemplateBatch`,
and `ReverseJournal`/`ReverseJournalFraction` still never sign, because
there is no point in those call chains that is provably outside a DB
transaction the way `financial.md` requires for the Attestor's signing
call. This was ALSO true, before this fix, of every `JournalWriter` call
composed inside `ledger.Service.RunInTx` -- including
`service/onchain.go`'s `postDepositConfirmedJournal`, P5's own headline use
case (M5: forged deposit accounting), which is composed via `RunInTx` to
get its atomic event/journal cross-link (I-10). That specific gap is
closed: `Service.Authorize`/`Service.AuthorizeTemplate` (postgres:
`LedgerStore.Authorize`) run BEFORE `RunInTx` opens (the last safe point to
call the Attestor), and `JournalWriter.PostAuthorized` posts the result
from inside the callback without touching the Attestor again --
`postDepositConfirmedJournal` now uses exactly this sequence. Callers of
the OTHER never-sign paths above still get an unsigned journal -- but no
longer indistinguishable from "no Attestor configured": `journals.auth_status`
(migration 051) records `unsigned_tx_mode` for all of them (and for any
`RunInTx`-composed journal whose caller did not adopt
Authorize/PostAuthorized), `unsigned_no_attestor` when no Attestor is
configured at all, and `signed` otherwise. A withdrawal gate (a downstream
consumer treating an unsigned journal as unauthorized for withdrawal
purposes) is still **not wired by this phase** -- design doc §12's P5 row
is explicit that it is a separate, later release; `core.VerifyJournalAuth`
is the primitive that release (or `ledger-cli verify`, P6) would call, and
it can now additionally branch on `auth_status` instead of only on
"digest empty or not".

**EventUID is not part of the digest, by design (§7.2/§7.5, revised
2026-08-21)**: an earlier draft of this fix signed with `EventUID == ""`
when the real event uid was not yet known at Authorize time (exactly
`postDepositConfirmedJournal`'s situation -- its event uid is minted by
`booker.Transition` inside the transaction that follows), then attached
the real event uid to `AuthorizedJournal.Input` afterward for the FK link
without re-signing. **That draft was wrong**: `core.VerifyJournalAuth`
recomputes the digest from a caller-supplied `JournalInput`, and any
verifier that reconstructs `EventUID` from the journal's actual, persisted
`event_id` (the natural thing to do) would recompute a digest that never
matches the one signed with `EventUID == ""` -- a spurious
`ErrUnauthorizedJournal` on every legitimately signed, event-linked
journal, worse than the gap it was fixing. Team Lead's ruling: remove
`EventUID` from `CanonicalJournalDigest` entirely. Domain separator
bumped to `0x10` (`0x01`, the retired V1 layout, can never be reused;
`0x02`/`0x03` are `core/attestation.go`'s batch digest / root hash, P6 --
contracts §2.6 is the allocation table for this shared resource). No
journal was ever signed under `0x01` in a real deployment, so this was
the cheapest possible time. `AuthorizedJournal.Input.EventUID` may now be
set or changed freely, before or after `Authorize` returns, without
affecting `Digest`/`Signature` at all.

**Disclosed residual limitation**: an attacker with DB write credentials
can set `event_id` on a journal that originally had none (045 allows the
`NULL -> non-NULL` set-once transition on that column) -- forging which
event a genuine journal appears to have originated from. This **cannot
move any funds**: the amounts, accounts, currencies, and idempotency key
are all covered by the signature and cannot be forged this way. The
event/journal link was never a cryptographic guarantee -- it is I-10's
same-transaction write plus 045's set-once FK, both DB-structural, exactly
as before P5 existed. Signing cannot add atomicity to a link it does not
cover.

**Enforced by**:
- `postgres.LedgerStore.attestJournal` / `PostJournal` (`postgres/ledger_store.go`)
  -- resolves `EffectiveAt` once, signs before `pool.Begin`, writes the
  three columns inside the transaction that also writes the journal row.
- `core.CanonicalJournalDigest` / `core.EncodeAmount` (`core/auth.go`) --
  the deterministic uid-space encoding (18-decimal fixed-point, 16-byte
  big-endian two's complement, domain-separated SHA-256) both `Sign` and
  `Verify` agree on. This encoding is the one part of P5 that cannot be
  changed later without breaking every previously-signed journal -- see
  its golden vectors below.
- Immutability of `auth_digest`/`auth_signature`/`auth_key_id` after a
  journal is signed: enforced by `ledger_journals_block_arbitrary_update()`,
  but **owned by migration 045 (P4)**, not this migration. Contracts §2
  (2026-08-21 rewrite) replaced that function's hardcoded per-migration
  column list with a generic `to_jsonb(OLD)`/`to_jsonb(NEW)` comparison
  against an explicit mutable-column whitelist, so these three columns are
  protected automatically once 045 installs it -- migration 046 does not
  (and must not) touch that function itself.
- `authdev.NewLocalAttestor` -- refuses a wrong-length seed or empty
  key_id at construction time, in the caller's own composition root,
  never silently inside the ledger.
- `postgres.LedgerStore.Authorize` / `PostAuthorized`
  (`postgres/ledger_store.go`, design doc §7.5) -- `Authorize` runs
  `attestJournal` before any transaction and refuses to run at all on a
  transaction-bound store (`core.ErrInvalidInput`); `PostAuthorized` never
  calls the Attestor and refuses a `core.AuthorizedJournal` whose `Status`
  is the Go zero value. `(*ledger.Service).Authorize` /
  `AuthorizeTemplate` expose the same pair at the library facade;
  `service.TxComposer.AuthorizeTemplate` is what `service/onchain.go`'s
  `postDepositConfirmedJournal` calls before opening `RunInTx`.
- `journals.auth_status` CHECK constraint (migration 051) -- restricts the
  column to exactly `signed` / `unsigned_no_attestor` / `unsigned_tx_mode`;
  `postJournalWithQueries` additionally refuses to insert at all if
  `auth.status` is empty, a stricter Go-level backstop than the DB
  constraint (better error message, catches the bug before a query is
  even issued).

**Pinned by** (`postgres/auth_pin_test.go` unless noted):
- `TestPostJournal_SignsWithConfiguredAttestor` -- a signed journal's stored
  digest/signature/key_id round-trip through `core.VerifyJournalAuth`
  successfully.
- `TestPostJournal_UnsignedWithoutAttestor` -- `Attestor == nil` leaves
  `PostJournal` byte-for-byte unchanged from before P5 (expand-safe).
- `TestPostJournal_IdempotentReplayDoesNotResign` -- a replayed post with
  the same idempotency key triggers exactly one `Attestor.Sign` call, not
  two.
- `TestPostJournal_AttestorErrorRejectsPost` -- a `Sign` error rejects the
  whole write; nothing is persisted.
- `TestForgedDirectSQLJournalIsUnauthorized` -- the M5 scenario itself: a
  balanced journal inserted directly via SQL (bypassing `PostJournal`
  entirely) passes a live per-journal balance check and still fails
  `core.VerifyJournalAuth`.
- `core.TestCanonicalJournalDigest_GoldenVector` / `TestEncodeAmount_GoldenVectors`
  (`core/auth_test.go`) -- pin the exact byte layout against independently
  computed values; any diff is a breaking encoding change.
- `core.TestVerifyJournalAuth_RejectsEmptyStoredDigest` /
  `RejectsMismatchedDigest` / `RejectsEmptySignature` -- each isolates one
  of `VerifyJournalAuth`'s three guard clauses; removing any one of them
  was verified, by hand, to make its corresponding test fail (the
  mismatch-check removal reaches a nil `AuthVerifier` and panics; the
  digest-emptiness removal is independently caught by the mismatch check,
  demonstrating defense-in-depth rather than a redundant no-op check).
- `authdev.TestNewLocalAttestor_DeterministicFromSameSeed` /
  `TestNewLocalVerifier_StandaloneFromPublicKey` -- the default Attestor
  implementation itself: same seed signs identically, and a verify-only
  process (holding only the public key) can check a signature it never
  had the private key to produce.
- `core.TestCanonicalJournalDigest_IgnoresEventUID` (`core/auth_test.go`,
  board #12/#13) -- pins that EventUID does not affect the digest at all;
  fails loudly if it is ever re-added.
- `postgres/authorize_pin_test.go` (§7.5 fix, board #12/#13):
  `TestAuthorize_RejectsOnTransactionBoundStore`,
  `TestPostAuthorized_RejectsEmptyStatus`,
  `TestPostAuthorized_SignsFromTxMode` (the fix itself: `Authorize` outside
  a transaction + `PostAuthorized` inside a caller-owned one produces a
  verifiable signature),
  `TestPostJournal_TxMode_NeverSignsEvenWithAttestor` (the contrasting
  negative: the *old* tx-mode entry point is deliberately unchanged and
  still labeled `unsigned_tx_mode`),
  `TestPostJournal_PoolMode_AuthStatusMatchesAttestorConfiguration`,
  `TestAuthStatus_NewColumnRejectsUnknownValue`.
- `service.TestOnchain_DepositConfirm_SignsViaRunInTx`
  (`service/onchain_signing_test.go`) -- drives a real deposit through
  `IngestDeposit` to `confirmed` with an Attestor configured and asserts
  the persisted `deposit_confirm` journal is `auth_status = signed` with a
  signature that verifies both against a reconstruction with EventUID
  blank AND against one carrying the journal's real, persisted EventUID
  (pinning that verification does not depend on which EventUID a
  reconstruction happens to carry). Verified failing before this fix
  (reverting `postDepositConfirmedJournal` to its pre-§7.5
  `ExecuteTemplate`-based form reproduces `auth_status = unsigned_tx_mode`
  and an empty signature).

## I-27: The attestation chain is complete -- gapless, linked, signed, and every entry covered exactly once

(docs/plans/2026-08-21-tamper-evident-ledger-design.md §8, P6 of the
integrity-hardening wave.) `ledger_attestations` (migration 047) is a
gapless, hash-linked sequence: attestation `seq` is unique and
contiguous starting at 1; each row's `prev_root` equals the previous
row's `root_hash` (seq 1's `prev_root` is `core.GenesisRoot`, 32 zero
bytes); each row's `root_hash` is `core.AttestationRootHash(seq,
prev_root, batch_digest, entry_count)` and is signed by a
`core.Attestor` (`signature`/`key_id`, verifiable by `core.AuthVerifier`
without the private key). `entry_attestations` (a side table, not a
column on `journal_entries` -- see the migration's own comment for why)
covers every `journal_entries` row exactly once: `entry_id` is its
primary key, so double-coverage is a `UNIQUE` violation, not a logic bug
waiting to happen.

**Why**: P5's per-journal signature proves a row was authorized *when
written*; it says nothing about whether the row *still exists* or
whether the *history around it* has been rewritten. I-27 closes both
gaps. The critical failure mode (design doc §8.2, this task's explicit
brief) is a late-arriving entry: two entries from different `(holder,
currency)` pairs can commit out of id order (I-5's ordering guarantee is
scoped to one pair), so a batch boundary drawn as `to_entry_id =
MAX(id)` would let a lower-id entry that commits *after* a higher-id one
was already batched slip through a gap no seq-continuity check could
ever notice -- it would neither appear in any batch's coverage nor break
any id-range invariant. The `entry_attestations` side table turns
"covered" into a queryable fact (`LEFT JOIN ... WHERE entry_id IS
NULL`), not an id-range assumption, so the late entry is simply
"uncovered" until the next run picks it up, and is caught by
`PRIMARY KEY (entry_id)` if two runs ever tried to cover it twice.

**Enforced by**:
- `service.AttestationService.RunAttestBatch` (`service/attestation.go`)
  -- resolves the next seq/prev_root and reads uncovered entries as plain
  queries, signs `root_hash` strictly before opening any transaction
  (`financial.md`), then inserts the attestation row and its
  `entry_attestations` coverage atomically
  (`postgres.AttestationStore.InsertAttestation`).
- `core.CanonicalBatchDigest` / `core.AttestationRootHash`
  (`core/attestation.go`) -- the deterministic, domain-separated
  encoding (`0x02` / `0x03`, distinct from P5's `0x01`) both the
  attestation job and `ledger-cli verify` agree on.
- Migration 047's `entry_attestations` `PRIMARY KEY (entry_id)` -- a
  structural guarantee against double-coverage, not a runtime check that
  could be skipped.
- `postgres/sql/queries/integrity_attestations.sql`'s
  `ListUncoveredEntries` -- a plain anti-join against
  `entry_attestations`, deliberately unbounded by any id or time window
  (see its own comment).
- `service.VerifyLedger` (`service/attest_verify.go`) -- ledger-cli
  verify's steps 2-3: walks the chain checking seq continuity, prev_root
  linkage, signatures, and recomputes each batch's digest from live
  `journal_entries` content (catching both a content rewrite and a row
  deletion, since a deleted row shrinks the recount below the stored
  `entry_count`).

**Pinned by** (`service/attestation_test.go` unless noted):
- `TestAttestationService_LateArrivingEntryIsEventuallyCoveredExactlyOnce`
  -- the exact §8.2 scenario: two entries commit out of id order; the
  late one is covered on the next run, exactly once, without disturbing
  the earlier one's coverage.
- `TestNaiveIDRangeWatermark_WouldMissTheLateEntry` -- falsification
  evidence: the REJECTED alternative (a monotonic `id > watermark`
  design, no side table) is run against the identical interleaving and
  shown to structurally exclude the late entry forever, demonstrating
  why the side-table design is load-bearing, not decorative.
- `TestAttestationService_EmptyBatchStillProducesAnAttestation` --
  design doc §8.1's "空批照样出一条": a tick that finds nothing still
  produces a row, so "the job ran and found nothing" is never confused
  with "the job never ran" (`working-agreements` §3).
- `TestAttestationService_ChainLinksPrevRoot` -- seq N's `prev_root`
  equals seq N-1's `root_hash`.
- `TestAttestationService_RequiresAttestor` -- there is no "unsigned
  attestation" state in this schema (unlike P5's expand-safe empty
  columns); `RunAttestBatch` refuses to run without an `Attestor`.
- `core.TestCanonicalBatchDigest_*` / `TestAttestationRootHash_*`
  (`core/attestation_test.go`) -- golden vectors (empty batch, chained
  root, negative holder, tiny amount) cross-checked against an
  independently written Python encoder, plus structural properties
  (deterministic, order-sensitive, rejects wrong-length hashes).
- `service.TestVerifyLedger_TamperedOnBrokenChainLink` /
  `TestVerifyLedger_TamperedOnDeletedEntry` (`service/attest_verify_test.go`)
  -- both simulate this wave's actual threat model (an owner-role
  bypassing the no-UPDATE/no-DELETE trigger) and confirm `VerifyLedger`
  classifies the result `TAMPERED`, not `VERIFIED`.

## I-28: The latest external anchor head matches the DB's attestation chain

(design doc §8.3/§8.4.) The external anchor (`core.Anchor`) remembers
only the latest `(seq, root_hash)` pair (design doc: "几十字节"), but
because I-27's hash chain links every later `root_hash` back to every
earlier batch's content, that single remembered value is enough to
detect a rewrite anywhere in the history: `ledger-cli verify` compares
the anchor's head against the DB row at the same `seq` and flags a
mismatch as `TAMPERED`. An anchor that is *behind* the DB's chain (has
not yet seen the latest attestations) is a distinct, benign state --
`DRIFT`, not `TAMPERED` -- because nothing about it indicates the
history was rewritten, only that publishing has not caught up yet.

**Why**: I-27 alone is a closed system -- an attacker with DB write
access (this wave's whole threat model, design doc §1) who can rewrite
`ledger_attestations` can also recompute a self-consistent replacement
chain from scratch, since every input the chain hashes (barring the
`Attestor`'s private key) lives in the same database. I-28 is what makes
that rewrite detectable: the anchor lives "somewhere the ledger's own
database credentials cannot reach" (design doc §8.3), so a rewrite that
does not also touch the anchor is caught by comparing the two.

**Enforced by**:
- `service.VerifyLedger` -- step 1 pulls the anchor's head before
  touching anything else, and step 2's per-seq loop compares the DB row
  at `seq == anchorSeq` against it.
- `service.AttestationService.catchUpAnchor` -- the "本地重试队列" design
  doc §8.3 calls for: the gap between `core.Anchor.Head` and the DB's
  latest seq IS the retry queue (no separate table), replayed on every
  run, so a transient `Publish` failure is retried automatically and
  survives a process restart (the anchor itself is external and
  durable).
- `anchordev.LocalFileAnchor` -- the dev-only local-file `core.Anchor`
  implementation `Publish`/`Head` calls exercise directly. **Not a
  production adapter** -- see its package doc comment; the real carrier
  (an object-lock bucket in a separate cloud account, at minimum) is a
  genuinely unresolved deployment choice this library does not ship
  (integrity contracts §7).

**Pinned by**:
- `service.TestAttestationService_PublishesToAnchor` -- the happy path:
  after a successful `RunAttestBatch`, the anchor's `Head` reflects the
  new seq/root_hash.
- `service.TestAttestationService_CatchesUpAnchorAfterTransientFailure`
  -- a `Publish` failure on one run does not lose the seq; the next
  run's catch-up step republishes it before creating a new attestation.
- `service.TestVerifyLedger_DriftWhenAnchorIsBehind` -- an anchor that
  has not caught up classifies as `DRIFT`, not `TAMPERED` or `VERIFIED`.
- `service.TestVerifyLedger_NotRunWithoutAnchor` /
  `TestVerifyLedger_NotRunWithoutVerifier` /
  `TestVerifyLedger_NotRunWhenAnchorHeadErrors` -- the fail-closed red
  line (`working-agreements` §3, same discipline as P0's
  `Complete`/`FullCoverage`): a missing public key, missing anchor, or
  an anchor that errors on `Head` all produce `NOT_RUN`, never a
  folded-in `VERIFIED`.
- `anchordev.TestLocalFileAnchor_IdempotentReplay` /
  `TestLocalFileAnchor_RejectsMismatchedReplay` /
  `TestLocalFileAnchor_RejectsNonSequentialSeq` -- `core.Anchor.Publish`'s
  own idempotency contract ("re-publishing the same seq with identical
  bytes must succeed, with different bytes must return an error").

## I-29: The Merkle root over each attestation batch is bound into the signed chain, and its per-entry leaf hashes are self-consistent with it

(design doc §9/§9.1/§9.4, P7 of the integrity-hardening wave.) Migration
048 adds `ledger_attestations.merkle_root` (the RFC 6962 tree root,
`core.BuildMerkleTree`, over the same entries `batch_digest` (I-27)
covers) and `entry_attestations.leaf_hash` (each covered entry's own RFC
6962 leaf hash, as it went into that `merkle_root`). Both are computed
and persisted by `service.AttestationService.RunAttestBatch` alongside
`batch_digest`.

**Why**: the first cut of this feature added `merkle_root` as a plain
column, NOT one of the signed hash's inputs -- meaning a third party
verifying an inclusion proof would be trusting a root protected only by
this table's append-only trigger and ACL, both internal to the database,
which defeats the entire point of an inclusion proof (the verifier is not
supposed to have to trust the database). Team Lead's review (design doc
§9.4(1)) judged that a real gap, not an acceptable disclosed limitation,
and required binding `merkle_root` into the signed hash itself:

- **`core.AttestationRootHashV2`** (separator `0x11`, contracts §2.6)
  hashes `seq || prevRoot || batchDigest || merkleRoot || entryCount`.
  Every attestation created from migration 048 onward uses it.
  `core.AttestationRootHash` (v1, separator `0x03`) is unchanged and
  keeps its original meaning forever for rows created before merkle_root
  existed (`len(merkle_root) == 0` is the discriminator -- the same
  signal I-29 already used to mean "row predates P7"); a v1 row is never
  retroactively upgraded, for the same "cannot re-sign history" reason
  `batch_digest` itself was never renamed (see migration 048's header).
  `service.VerifyLedger` recomputes root_hash under whichever formula the
  row's own version uses and confirms it still equals the stored, signed
  value -- unconditionally, not only when the recomputed fields also
  happen to match live `journal_entries` content, or an edit to
  `merkle_root`/`batch_digest` that diverges from BOTH live data and the
  original signed root_hash would only be caught by the live-data checks,
  never by this one (a real bug this implementation hit and fixed --
  `TestVerifyLedger_TamperedMerkleRootAlone`'s falsification evidence).

- **`entry_attestations.leaf_hash`** closes a second, independent gap:
  even with `merkle_root` signed, the per-entry leaf hashes used to
  *localize* a mismatch (I-30) still need their own tamper-evidence,
  or a forged leaf_hash could mislead an on-call responder about which
  entry actually changed without tripping any signature check.
  `service.VerifyLedger` rebuilds a tree from the batch's stored
  `leaf_hash` values (`core.BuildMerkleTreeFromLeafHashes`) and confirms
  it still equals the batch's own (v2-signed) `merkle_root` -- editing a
  stored leaf hash changes that recomputed root without touching
  anything the signature or the live-entries checks look at, so this is
  the only check that catches it.

Empty `merkle_root` / `leaf_hash` (migration 048's expand-safe `''::bytea`
default) mean "this row predates the value being computed" -- `VerifyLedger`
skips the corresponding check rather than treating absence as a
mismatch, mirroring I-26's existing treatment of an empty `auth_key_id`
("never signed" is not "forged").

**Enforced by**:
- `service.AttestationService.RunAttestBatch` (`service/attestation.go`)
  -- computes the batch's `core.BuildMerkleTree`, signs
  `core.AttestationRootHashV2` (merkle_root bound in), and persists every
  entry's `core.MerkleTree.LeafHashes()` alongside it, all strictly
  outside any DB transaction except the one atomic insert
  (`financial.md`).
- `service.VerifyLedger` (`service/attest_verify.go`) -- three
  independent checks per seq: (1) live entries vs `batch_digest`/
  `merkle_root`, (2) stored `leaf_hash` values vs `merkle_root`, (3)
  root_hash self-consistency under the row's own version (v1 or v2).
- `postgres/sql/migrations/048_ledger_attestations_merkle_root.up.sql` --
  both columns additive, `NOT NULL DEFAULT ''::bytea`.

**Pinned by** (`core/attestation_test.go` / `service/attest_verify_merkle_test.go`):
- `core.TestAttestationRootHashV2_GenesisGoldenVector` /
  `TestAttestationRootHashV2_ChainedGoldenVector` -- independently
  computed in Python, cross-checked against the pre-existing v1 pins'
  own encoder to confirm the same byte-layout methodology.
- `core.TestAttestationRootHashV2_DiffersFromV1ForTheSameInputs` -- v1
  and v2 never collide even for the adversarial all-zero-merkleRoot edge
  case.
- `service.TestVerifyLedger_TamperedMerkleRootAlone` -- corrupting only
  `merkle_root` is caught via root_hash self-consistency (among other,
  expected, overlapping findings).
- `service.TestVerifyLedger_TamperedLeafHashAlone` -- the isolation pin:
  corrupting only a stored `leaf_hash` (journal_entries, merkle_root,
  root_hash, signature all left untouched) produces exactly one finding,
  confirmed by falsification (disabling the check reverts this exact
  test to `VERIFIED`).
- `service.TestVerifyLedger_MerkleRootCheckSkippedForLegacyEmptySentinel`
  -- a v1 row (`merkle_root` never computed) verifies clean.

## I-30: Inclusion proofs are sound and self-contained localization works without an operator-supplied reference

(design doc §9.2/§9.4(2), P7.) `core.GenerateInclusionProof` produces an
RFC 6962 audit path (sibling hashes only, ordered leaf-to-root) for one
entry within a batch's Merkle tree; `core.VerifyInclusion(leafHash,
proof, root) bool` recomputes the root from that path and reports
whether it matches -- a pure function with zero dependencies (no DB, no
`MerkleTree` instance, not even this package's other types beyond the
three arguments), so a third party can verify "this entry is in the
batch with this root" using only a value they already trust (e.g. from
`core.Anchor`) and the two things the ledger operator hands them: the
leaf hash and the path. Design doc §9.2's red line -- the path must
reveal nothing about any other entry's content -- holds structurally:
`InclusionProof.Path` is `[][]byte` (hashes only); nothing in
`GenerateInclusionProof` ever touches a sibling's raw `AttestedEntry`.

`core.LocateMismatches` gives the *localization* half of §9's brief
(§9.1): given two equal-size Merkle trees, it narrows a mismatch from
"seq N is bad" to the exact leaf index(es) that diverge, in O(k log n)
(k = number of mismatches) by pruning any subtree whose cached hash still
matches. §9.1 required a `TAMPERED` verdict to carry the altered entries'
ids; the first cut of this feature could only do that with an
operator-supplied external reference (a second full tree this schema does
not otherwise persist), and reported "no list" honestly rather than
fabricate one when no reference was available. Team Lead's review (design
doc §9.4(2)) judged that a real schema gap, not an acceptable limitation
-- on-call is this capability's consumer, and requiring them to already
hold a trusted external snapshot is least likely to be true at the exact
moment tampering is discovered.

**Fix**: `entry_attestations.leaf_hash` (I-29) makes localization
self-contained. `service.VerifyLedger` rebuilds a tree from a seq's
stored, already-verified-consistent `leaf_hash` values
(`core.BuildMerkleTreeFromLeafHashes`) and diffs it against a tree built
from live entries (`core.BuildMerkleTree`) via `core.LocateMismatches` --
no operator input required. `VerifyConfig.ReferenceEntries` (an
operator-supplied external snapshot, e.g. a separate replica) remains as
an explicit fallback, tried only when the self-contained path is
unavailable for that seq (e.g. a row created before `leaf_hash` was wired
up) -- design doc §9.4(2) keeps it, not replaces it.

**Why** (RFC 6962 correctness): without a documented, tested boundary
between "hashes only" and "actual content," an inclusion-proof
implementation is easy to get subtly wrong in a way that leaks data (RFC
6962 exists specifically so Certificate Transparency logs can prove
membership to auditors without handing over the whole log) or that IS
forgeable (CVE-2012-2459: a naive implementation that duplicates an odd
trailing leaf to pad a level produces the same root for an `n`-leaf tree
and an `n+1`-leaf tree whose extra leaf is a copy of the last one,
breaking the "root uniquely identifies this exact leaf set" property
every consumer of a Merkle root implicitly relies on).

**Enforced by**:
- `core.merkleLeafHash` / `core.merkleNodeHash` (`core/merkle.go`) --
  RFC 6962's own fixed domain bytes (`0x00` leaf / `0x01` node -- not a
  separator this package chose, unlike P5/P6/P7's `0x10`/`0x02`/`0x03`/
  `0x11`).
- `core.largestPowerOfTwoLessThan` / `core.buildMTH` -- RFC 6962's
  recursive split (not a naive level-order pairing), which is what makes
  an odd trailing leaf never get duplicated.
- `core.GenerateInclusionProof` / `core.VerifyInclusion` -- proof
  generation and the pure, dependency-free verification function.
- `core.BuildMerkleTreeFromLeafHashes` / `core.LocateMismatches` -- the
  self-contained-localization tree builder and the O(k log n)
  prune-and-descend diff.
- `service.VerifyLedger`'s self-contained localization (tried first) and
  `VerifyConfig.ReferenceEntries` hook / `locateMismatchedEntryIDs`
  helper (fallback) (`service/attest_verify.go`); `cmd/ledger-cli`'s
  `verify --reference-dir` flag is the reference implementation of
  supplying the fallback.

**Pinned by**:
- `core.TestMerkleRoot_GoldenVectors` (`core/merkle_test.go`) -- n=0..8
  leaves, cross-checked against an independently written Python
  transcription of RFC 6962's MTH algorithm, AND against
  `core.TestMerkleTree_RFC6962TestLogRoots` -- a third, independent
  implementation (Team Lead, from the spec's recursive definition) over
  the canonical eight-entry Certificate Transparency test log, closing
  the "no internet access to cross-check official vectors" limitation
  this implementation disclosed rather than papered over.
- `core.TestMerkleTree_NoDuplicationOfOddLeaf` -- the CVE-2012-2459 pin:
  a 3-leaf tree's root is confirmed to differ from the naive
  duplicate-last-leaf construction's root, both by direct byte
  comparison and against an independently computed golden value.
- `core.TestMerkleTree_InclusionProofRoundTrip_AllSizesAndIndices` -- every
  `(n, index)` pair for `n` = 1..32 round-trips through
  `GenerateInclusionProof` + `VerifyInclusion`. Falsification evidence:
  reverting `VerifyInclusion`'s sibling/direction pairing to its
  first-draft (incorrect) form was confirmed to fail this exact test
  before the fix landed.
- `core.TestVerifyInclusion_RejectsTamperedLeaf` /
  `TestVerifyInclusion_RejectsTamperedPath` /
  `TestVerifyInclusion_RejectsOutOfRangeIndex` -- a genuine proof against
  the wrong leaf, a tampered sibling hash, or an out-of-range index must
  all fail verification, not silently succeed.
- `core.TestLocateMismatches_FindsSingleTamperedLeaf` /
  `TestLocateMismatches_FindsMultipleTamperedLeaves` /
  `TestLocateMismatches_RejectsDifferentSizes` -- localization finds
  exactly the diverging leaf indices, and refuses (rather than guessing)
  when the two trees are not the same size.
- `service.TestVerifyLedger_LocalizesTamperedEntry_SelfContainedNoReferenceNeeded`
  (`service/attest_verify_merkle_test.go`) -- the headline capability:
  localization narrows to the exact tampered entry id with ZERO operator
  input, confirmed by falsification (disabling the self-contained path
  reverts this exact test to an empty result).
- `service.TestVerifyLedger_LocalizesTamperedEntryWithReference` /
  `TestVerifyLedger_NoLocalizationWithoutReference` -- the fallback path
  (self-contained data deliberately unavailable via
  `insertAttestationWithoutLeafHashes`): a supplied reference still
  narrows `TAMPERED` to the exact entry id; no reference and no
  self-contained data means no entry list, never a fabricated one.

---

> Numbering note: I-22 (P1 DB roles) is allocated in the Phase 0 contract
> (`docs/plans/2026-08-21-integrity-hardening-contracts.md` §5) to a parallel
> task that has not merged yet, so this document does not yet contain it.
> Whoever merges P1 inserts I-22 into that slot — the number is a contract,
> not a reflection of merge order.
- `postgres/sql/migrations/042_ledger_roles.up.sql` — creates `ledger_owner`
  / `ledger_app` (`SELECT`/`INSERT`/`UPDATE`, no `UPDATE` on
  `journal_entries`, no DDL of any kind) / `ledger_ro`, and grants each
  additively. `REVOKE ALL ON SCHEMA public FROM PUBLIC` and the ownership
  transfer that makes `ledger_owner` DDL-capable are deliberately NOT in
  this migration (see "Note on scope" above) — they live in
  `postgres/sql/migrations/049_ledger_roles_ownership_transfer.up.sql`
  instead, which must ship in the same release as the `DATABASE_URL`
  cutover (`docs/RUNBOOK.md` §9).

**Pinned by**:
- `postgres.TestMigration042_LedgerAppIsLeastPrivilege` — migrates to 041
  first and confirms the single connection has *unrestricted* DDL there
  (proving the restrictions below are not vacuous), then migrates the rest
  of the way and confirms `ledger_app` cannot `TRUNCATE`/`DROP TRIGGER`/
  `ALTER TABLE`/`CREATE TABLE`/`UPDATE journal_entries`/`DELETE FROM
  journal_entries`/touch `schema_migrations`, while it can still
  `SELECT`/`INSERT`/`UPDATE` an ordinary table and `SELECT`/`INSERT`
  `journal_entries`.
- `postgres.TestMigration042_DoesNotStrandTheMigrationRunner` — migrates
  exactly to 042 through a non-superuser role that owns the database
  (simulating a managed-Postgres master user) and confirms that role can
  still write afterward. This is the regression pin for the combined-
  migration bug described above: it fails with `permission denied for
  table schema_migrations` against the old (pre-split) combined 042 and
  passes against the current (pure-expand) 042.
- `postgres.TestMigration049_StrandsTheOldConnectionByDesign` — the
  counterpart pin for 049: the same non-superuser role can still write
  after 042 alone, but loses access to business tables once 049 runs
  (`schema_migrations` is deliberately excepted -- see `docs/RUNBOOK.md`
  §9). Also proves 049 itself can apply cleanly under a non-superuser
  connection -- an earlier revision without its narrow
  schema-USAGE/schema_migrations re-grants could never successfully apply
  at all, on any non-superuser connection, regardless of `DATABASE_URL`
  cutover timing.
- `postgres.TestMigration042_LedgerAppInsertsIntoPartitionCreatedAfterGrant`
  — after manually granting `ledger_owner` ownership of `journal_entries`
  (mirroring what 049 does, scoped to just this one table so the test does
  not depend on 049's exact implementation), a partition it creates *after*
  042's grant ran is still writable by `ledger_app` through the parent
  table name.
- `postgres.TestMigration042_RoleAttributes` — pins role attributes
  (`LOGIN`, not superuser/createdb/createrole) and the exact grant set each
  role holds (`information_schema.role_table_grants`) on an ordinary table,
  `journal_entries`, and `schema_migrations`.
- `postgres.TestMigration042_DownDropsRolesAndRestoresOwnership` /
  `postgres.TestMigration049_DownRestoresOwnership` — the down migrations
  for 042 and 049 each roll back cleanly and leave the original connection
  able to operate normally.

---

## I-31: Reversals and template batches sign under a configured Attestor in pool mode, not just plain journals

(docs/plans/2026-08-21-integrity-hardening-contracts.md, "Wave 2 契约层"
§W2-1/W2-2, board #15, W2-T1.) Before this fix, `ReverseJournal`,
`ReverseJournalFraction`, and `ExecuteTemplateBatch` ALWAYS posted
`journals.auth_status = unsigned_tx_mode`, in EVERY mode, even pool mode --
unconditionally, regardless of whether a `core.Attestor` was configured via
`WithAuth`. That mislabeled the reason: `unsigned_tx_mode` means "no safe
point to call the Attestor without violating `financial.md`", which is true
of a `WithDB`-bound store (a genuine caller-owned transaction), but was never
true of these three in pool mode -- they self-manage their own transaction
lifecycle exactly like `PostJournal`'s pool-mode branch, which has been able
to sign since P5 (I-26) existed. The three call chains simply never tried.

**Why this matters for W2-1's ruling**: the Wave 2 contract's verified-balance
semantics (`docs/plans/2026-08-21-integrity-hardening-contracts.md` §W2-1)
are account-level fail-closed -- any contributing journal that is not
`auth_status = signed` makes that account's verified balance UNDEFINED, not
"a smaller number." Reversals and batch-posted journals sit squarely on the
money path (a reversal, in particular, is exactly as capable of moving funds
as a forward posting -- M5's forgery scenario applies to it unchanged). As
long as these three call chains could never produce `signed` in pool mode --
the deployment's default and, before this fix, ONLY mode that could ever
sign anything -- every account with a reversal or batch-posted journal in its
history had a permanently UNDEFINED verified balance, regardless of how a
downstream consumer wired the withdrawal gate. Closing this gap is what
makes `VerifiedBalanceReader` (W2-T2) reachable for any account with a real
transaction history.

**Fix**: `core.JournalWriter.AuthorizeReversal(ctx, journalUID, num, den,
reason, idempotencyKey)` mirrors `Authorize`/`AuthorizeTemplate`'s
outside-any-transaction signing split (§7.5, I-26), extended to cover a
reversal's entries -- which are DERIVED from the original journal (read from
the DB) rather than caller-supplied, so they cannot be signed until that
read happens. `ReverseJournal` (num=1, den=1) and `ReverseJournalFraction`
(any valid num/den), in pool mode with an Attestor configured, call
`AuthorizeReversal` strictly before `pool.Begin`, then re-derive the same
entries (`reversalEntriesFor`, shared verbatim with the pre-authorization
path) fresh under the original journal's row lock and compare
`core.CanonicalJournalDigest` byte-for-byte against what was signed. A match
uses the signature verbatim (`auth_status = signed`); a mismatch --
possible only for the num==den ("reverse everything remaining") form, whose
entries subtract cumulative prior-reversal history, if a concurrent partial
reversal commits in the gap between `AuthorizeReversal` and the row lock --
rejects the post outright (`core.ErrConflict`) rather than silently posting
unsigned, silently re-signing inside the transaction (forbidden by
`financial.md` regardless), or using the now-stale signature. This is NOT a
weakening of the pre-existing conservation checks (I-2): the row lock, the
already-fully-reversed check, and the overshoot check all still run
unchanged; the digest comparison is an ADDITIONAL guard specific to signing,
layered on top. `ExecuteTemplateBatch`'s pool-mode branch renders every
template and signs every resulting input before `pool.Begin` (no new port
method needed -- it fully owns both the render and the write internally).

Pool mode with NO Attestor configured now reports `unsigned_no_attestor`
for all three (matching `PostJournal`'s own pool-mode-no-attestor label,
I-26) rather than `unsigned_tx_mode` -- "no attestor" and "no safe point
because of genuine tx mode" are different reasons, and conflating them was
part of the same mislabeling this invariant fixes (see
`core.AuthStatusUnsignedTxMode`'s updated doc comment). A store bound via
`WithDB` (participating in a caller-owned transaction, e.g. a `RunInTx`
callback) is UNCHANGED: all three still always post `unsigned_tx_mode`
there, because there genuinely is no safe point to call the Attestor inside
a transaction this code did not open -- board #15 does not add a
`PostAuthorized`-equivalent entrypoint for reversals or batches; a caller
needing that composition would have to close the gap itself the way
`postDepositConfirmedJournal` did for a plain journal (§7.5).

**Enforced by**:
- `core.JournalWriter.AuthorizeReversal` (`core/interfaces.go`) -- the port,
  with the digest-comparison contract spelled out in its doc comment.
- `postgres.LedgerStore.AuthorizeReversal` / `reversalEntriesFor`
  (`postgres/reversal_fraction_store.go`) -- the implementation and the
  shared, DB-access-free entry derivation both the unlocked
  pre-authorization call and the locked post-time call use, which is what
  makes the digest comparison meaningful (byte-identical inputs whenever
  reversal history has not changed in between).
- `postgres.LedgerStore.ReverseJournal` / `reverseJournalWithQueries`
  (`postgres/ledger_store.go`) and `ReverseJournalFraction` /
  `reverseJournalFractionWithQueries` (`postgres/reversal_fraction_store.go`)
  -- the pre-authorize-before-`Begin` sequencing and the post-time digest
  comparison / fallback-status selection.
- `postgres.LedgerStore.ExecuteTemplateBatch`
  (`postgres/ledger_store.go`) -- the pool-mode render-then-sign-then-post
  sequencing; `executeTemplateBatchWithQueries` remains the tx-mode-only
  path, unconditionally `unsigned_tx_mode`.
- `core.AuthStatusUnsignedTxMode`'s doc comment (`core/auth.go`) -- records
  the narrowed scope this invariant introduces.

**Pinned by** (`postgres/reversal_signing_pin_test.go` unless noted):
- `TestReverseJournal_SignsWithConfiguredAttestor` /
  `TestReverseJournalFraction_SignsWithConfiguredAttestor` (both the
  num!=den proportional-split branch and the num==den "reverse everything
  remaining" branch) / `TestExecuteTemplateBatch_SignsWithConfiguredAttestor`
  -- pool mode with an Attestor configured produces `auth_status = signed`
  with a signature that round-trips through `core.VerifyJournalAuth`.
  Verified failing before this fix (reverting the implementation reproduces
  `auth_status = unsigned_tx_mode` for all three).
- `TestReverseJournal_UnsignedNoAttestorInPoolMode` /
  `TestReverseJournalFraction_UnsignedNoAttestorInPoolMode` /
  `TestExecuteTemplateBatch_UnsignedNoAttestorInPoolMode` -- pool mode with
  no Attestor configured reports `unsigned_no_attestor`, not
  `unsigned_tx_mode`. Verified failing before this fix (old code reports
  `unsigned_tx_mode` unconditionally).
- `TestReverseJournal_TxMode_NeverSignsEvenWithAttestor` /
  `TestReverseJournalFraction_TxMode_NeverSignsEvenWithAttestor` /
  `TestExecuteTemplateBatch_TxMode_NeverSignsEvenWithAttestor` -- the
  negative contrast: a `WithDB`-bound store still never signs any of the
  three, even with an Attestor configured. Unchanged behavior, non-regression.
- `TestReverseJournalFraction_ConcurrentPartialReversalInvalidatesPreAuthorization`
  -- the race the num==den branch's digest comparison exists to catch: a
  `blockingAttestor` deterministically lands a real concurrent partial
  reversal inside the window between `AuthorizeReversal` computing its
  digest and `ReverseJournalFraction` opening its transaction; the stale
  pre-authorization is rejected (`core.ErrConflict`) and leaves no row
  behind. Verified failing (hanging, in fact: the pre-fix code path never
  calls the Attestor for a reversal at all, so the test's synchronization
  point is never reached) before this fix.

## How to add a new invariant

1. Write the rule down here under a new `I-N` heading.
2. Add the `Why` (the failure mode you're preventing).
3. Add the `Enforced by` (where in the code).
4. Add at least one test under `Pinned by` and reference it by name.
5. If the test is a fuzz target, run it for a few seconds in CI and commit
   any corpus seeds it discovers.

The "Pinned by" section is the contract. If a test name disappears, either
(a) the invariant is no longer being checked — fix it — or (b) the test was
renamed; update this doc.
