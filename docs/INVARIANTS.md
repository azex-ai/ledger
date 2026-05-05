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

**Enforced by**:
- The `journals.reversal_of` FK column (added in migration `014`).
- A partial unique index `uq_journals_reversal_of` ensures any given journal
  can be reversed **at most once**: `CREATE UNIQUE INDEX ... WHERE reversal_of IS NOT NULL`.

**Pinned by**:
- `postgres.TestLedgerStore_ReverseJournal_AlreadyReversed`
- `postgres.TestReversalChainIntegrity` (full A → ¬A → ¬¬A blocked path)

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

`available = total_balance - SUM(active reservations on same dimension)`. A
reservation request for `amount > available` is rejected with
`ErrInsufficientBalance`.

**Why**: the obvious one — overdraft prevention. The non-obvious part: this
must be checked **inside** the advisory lock (see I-4), not before.

**Enforced by**: `postgres.ReserverStore.Reserve` (lock → check → insert).

**Pinned by**:
- `postgres.TestReserverStore_Reserve_Concurrent`

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

**Pinned by**:
- `postgres.TestPartitionBoundary_DefaultCatches`
- `postgres.TestPartitionBoundary_GetBalanceUnionsPartitions`

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
