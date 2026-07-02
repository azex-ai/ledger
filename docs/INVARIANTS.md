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
- `core.JournalInput.Validate` (Go side, `core/journal.go:93`).
- `chk_journal_currency_balance` deferred constraint trigger
  (`postgres/sql/migrations/004_ledger.up.sql:33`).
- `chk_journal_balance` table-level CHECK on `journals.total_debit = total_credit`
  (covers global totals as a defense-in-depth check).

**Pinned by**:
- `core.TestJournalInvariant_BalancedRandomEntries` (200 random trials)
- `core.TestJournalInvariant_MultiCurrencyEachMustBalance`
- `core.TestJournalInvariant_UnbalancedAlwaysRejected` (100 random drift trials)
- `core.FuzzJournalValidate` (Go fuzz target)

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
  reversals; the per-dimension cumulative check runs under that lock
  (`postgres/reversal_fraction_store.go`). Migration `029` replaced the old
  "at most once" unique index with this application-level conservation rule.

**Pinned by**:
- `postgres.TestLedgerStore_ReverseJournal_AlreadyReversed`
- `postgres.TestReversalChainIntegrity` (full A → ¬A → ¬¬A blocked path)
- `postgres.TestReverseJournalFraction_ConservationAndRemainderCompletion`
- `postgres.TestReverseJournalFraction_OverReversalRejected`
- `postgres.TestReverseJournalFraction_ConcurrentConservation`
- `postgres.TestReverseJournal_MutualExclusionWithFraction`

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

**Pinned by**:
- `core.TestJournalInput_Validate_NoIdempotencyKey`
- `postgres.TestLedgerStore_PostJournal_Idempotent`
- `postgres.TestPendingStore_AddPending_Idempotent`
- `postgres.TestReserverStore_Reserve_Idempotent`
- `postgres.TestIdempotency_ConcurrentSameKey` (100 goroutines, same key)

## I-4: TOCTOU-safe reserve/settle

Reservation creation atomically (a) takes a per-(holder, currency) advisory
lock, (b) re-checks `available = total_balance - SUM(active reservations)`, and
(c) inserts the reservation. Settle and Release transition the same row under
its own row lock.

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

**Three exceptions**, all FK-target columns where `0` is not a valid sentinel
because PostgreSQL needs a real `NULL` to skip referential-integrity enforcement:

- `journals.reversal_of` — null when the journal is original (not a reversal).
- `bookings.journal_id` — null until accounting is posted.
- `bookings.reservation_id` — null until / unless a reservation is linked.
- `events.journal_id` — null until an event has caused a journal posting.

**Why**: NOT NULL eliminates a category of "missing vs zero" ambiguities.
Where it would conflict with FK enforcement, `NULL` is documented and the Go
field is `*int64`.

**Enforced by**:
- Migration `017_no_null_cleanup` for the bulk move.
- Migration `018_restore_referential_integrity` for the four exceptions.

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

`available = total_balance - SUM(outstanding holds on same dimension)`. A
reservation request for `amount > available` is rejected with
`ErrInsufficientBalance`. An `active` reservation holds its full
`reserved_amount`; a `settling` one (partially settled via `SettlePartial`,
since migration `029`'s companion changes) holds its unsettled remainder
(`reserved_amount - settled_amount`) — dropping that remainder from the sum
would let a concurrent Reserve over-commit the moment the first partial
settlement lands.

**Why**: the obvious one — overdraft prevention. The non-obvious part: this
must be checked **inside** the advisory lock (see I-4), not before.

**Enforced by**: `postgres.ReserverStore.Reserve` (lock → check → insert).

**Pinned by**:
- `postgres.TestReserverStore_Reserve_Concurrent`
- `postgres.TestReserverStore_SettlePartial_RemainderStillHeld`

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

**Current state**: only the default partition exists today. Named,
date-bounded partitions and any rolling creation / archival automation are
not implemented — the `PARTITION BY RANGE` declaration is schema groundwork
for that future work, not an active rollover process. Every row currently
lands in `journal_entries_default`.

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
- `postgres.TestPartitionBoundary_DefaultCatches`
- `postgres.TestPartitionBoundary_GetBalanceUnionsPartitions`

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
- `postgres.validateSingleAmountPrecision`, called from `ReserverStore.Reserve`
  — the one write path that does **not** flow through `PostJournal` and so
  needs its own enforcement point.
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
