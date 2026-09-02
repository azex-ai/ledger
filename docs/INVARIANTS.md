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

That is an **upper** bound. The matching lower bound — after a `num == den`
("reverse everything remaining") reversal, the original's net amount on every
dimension is **zero** — holds only because every journal linked by
`reversal_of` really is a reversal of what it points at. That is not a
property of this invariant; it is **I-51**, and it is what makes the
cumulative arithmetic here mean what it says.

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

## I-3: Idempotency on every mutation that moves money

Every operation that moves money, or advances a money-bearing state machine,
requires an `idempotency_key`. Replaying the same key with the same payload
returns the original result and produces no additional side effects. Reusing
the same key with a different payload is a conflict.

Concretely, that is: posting a journal (including every reversal form),
`Reserve` / `Settle` / `SettlePartial` / `Release` / `FinalizeSettlement`,
`AddPending` / `ConfirmPending` / `CancelPending`, and `Booker`'s
`CreateBooking` / `Transition`.

**Configuration writes are deliberately excluded**, and this is a scope
statement, not an outstanding gap:

| Excluded | Why it needs no key |
|---|---|
| `CreateCurrency` / `CreateClassification` / `CreateJournalType` / `CreateTemplate` | Natural-key inserts: `code` is `UNIQUE`, so a replay is a duplicate-key conflict, not a second row |
| `Deactivate*` (currency / classification / journal type / template) | Idempotent by construction — a flag set to `false` twice is `false`. Since 2026-09-02 a uid matching no row is `ErrNotFound` rather than a silent success |
| `SetPolicy` (`AccountPolicyStore`) | Upsert at an exact `(holder, currency, classification)` dimension; replaying the same payload converges on the same row, and the append-only `account_policy_changes` trail records each application |
| `ClosePeriod` | Append-only and latest-row-wins: a duplicate close line at the same `close_before` leaves the active line unchanged |
| `EnsureAddress` (`AddressRegistry`) | Upsert on a deterministically derived address |

> Scope corrected 2026-09-02 (`concurrency.md` B-m9). This section used to
> open with "Every state-changing operation requires an `idempotency_key`" —
> a universal claim that every configuration write in the library falsifies,
> and has always falsified. Stated that way it read as an unfulfilled promise
> about six real code paths, which invites someone to "fix" them by bolting
> keys onto self-idempotent upserts: pure cost, no property gained. The rule
> now says what it protects (money movement) and lists what it does not.

**Why**: in distributed systems, every retry path needs a deterministic
"is this the same thing I already did?" answer. Without it, network-flaky
clients double-charge / double-credit users. Configuration writes have no
such hazard: there is no amount to apply twice.

**Enforced by**:
- `UNIQUE` constraint on `journals.idempotency_key`.
- `UNIQUE` constraint on `reservations.idempotency_key`.
- `UNIQUE` constraint on `bookings.idempotency_key`.
- `UNIQUE` constraint on `reservation_operation_receipts.idempotency_key`
  (migration `005`) — see the `Settle`/`Release`/`FinalizeSettlement`
  paragraph below.
- `UNIQUE` constraint on `booking_transition_receipts.idempotency_key`
  (migration `005`) — see the `Transition` paragraph below.
- Each `Validate()` method rejects empty idempotency keys at the Go boundary.
- The scope boundary itself is machine-checked, not maintained by hand: the
  AST gate cited among this section's pins below
  (`TestIdempotencyKeyScopeMatchesInvariantI3`) requires every `*Input` type
  in `core` to be classified one way or the other.
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

`Settle`, `Release` and `FinalizeSettlement` each move a reservation into a
**terminal** status (`settled` or `released`). Before migration `005`, none
of the three took an idempotency key at all: a lost-response retry of an
already-applied call re-ran the same status-machine check, found the row
already terminal, and returned `ErrInvalidTransition` — indistinguishable
from a genuine conflict (someone else settling a different amount, or the
reservation having been released out from under the caller). Each now
requires `IdempotencyKey` and records one row in
`reservation_operation_receipts` (`operation` ∈ `settle` / `release` /
`finalize_settlement`) on success, checked under the reservation's row lock
**before** the status-machine gate: a replay with the same reservation,
operation and amount returns the original success; a payload mismatch —
different reservation, different operation, or different amount — is
`ErrConflict`. Because the reservation's state machine makes `settled` and
`released` mutually exclusive terminal states reachable by exactly one of
these three calls, one receipt per reservation is enough to disambiguate.

`Transition` (`core.Booker`) requires `TransitionInput.IdempotencyKey` like
every other write in this package (W15-A closed the exception W1-A had
carved out here — see docs/plans/2026-08-26-audit-remediation-contracts.md
§7). `Validate()` rejects an empty key. Every call is checked against a
durable `booking_transition_receipts` row (booking, to_status, channel_ref,
amount, and the `event_id` the original call produced), under the booking's
row lock and before the lifecycle transition gate: a replay with the same
key and payload always returns the original event, even after the booking
has since moved on to a later status — the case a narrower,
state-comparison-based path (`idempotentTransitionEvent`, still present as a
secondary safeguard for two calls racing to apply the exact same occurrence
under different freshly-derived keys) cannot cover, since at that point
`lifecycle.CanTransition(current, ToStatus)` is false and the retry would
otherwise be rejected with `ErrInvalidTransition`, indistinguishable from a
genuine invalid request; the same key with a different payload is
`ErrConflict`.

Deriving that key is not simply `<booking_uid>-<to_status>`: a booking's
lifecycle can legitimately revisit the same status more than once (the
withdrawal preset's `failed` → `reserved` retry edge, or the sweep preset's
`failed` → `pending` revival edge). A key that ignores which *occurrence* of
that status a call is describing collides across two genuinely different,
legitimate transitions — the second one silently short-circuits to the
first one's receipt instead of applying, and the caller sees success while
nothing actually happened. Every system-driven call site
(`service/onchain.go`, `service/expiration.go`, the legacy webhook
transition path) derives its key from that occurrence's own identity (an
on-chain broadcast tx hash, a source event, or — where the target status is
provably reached at most once for that lifecycle — the booking's own uid);
client-driven calls (`POST /bookings/{uid}/transition`) carry a
caller-supplied key via the `Idempotency-Key` header (api-contract.md §9).

**Pinned by**:
- `core.TestIdempotencyKeyScopeMatchesInvariantI3` — the AST gate over the
  `core` package: every `*Input` type either carries an `IdempotencyKey`
  field and is on the money-path list above, or carries none and is on the
  exclusion list. A new money-path input that forgets the key turns it red;
  so does a new configuration input that nobody classified, which is what
  stops the exclusion table drifting away from the code.
- `core.TestJournalInput_Validate_NoIdempotencyKey`
- `postgres.TestLedgerStore_PostJournal_Idempotent`
- `postgres.TestPendingStore_AddPending_Idempotent`
- `postgres.TestReserverStore_Reserve_Idempotent`
- `postgres.TestIdempotency_ConcurrentSameKey` (100 goroutines, same key)
- `postgres.TestSettlePartial_IdempotentReplay`
- `postgres.TestConfirmPending_ConcurrentSameKey_NeverInsufficientBalance`
- `postgres.TestReserverStore_Settle_IdempotentReplay`
- `postgres.TestReserverStore_Release_IdempotentReplay`
- `postgres.TestReserverStore_FinalizeSettlement_IdempotentReplay`
- `postgres.TestBookingStore_Transition_IdempotencyKey_SurvivesForwardProgress`
- `postgres.TestBookingStore_Transition_RevisitingSameStatus_DistinctKeysDoNotCollide`
- `core.TestTransitionInput_Validate` (missing idempotency key)
- `service.TestOnchain_Sweep_TwoRevivalCycles_DoNotCollide`

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
- `postgres.TestReserverStore_Reserve_Concurrent_RejectsOverCommit` (10
  concurrent reserves that together exceed the funded balance -- the actual
  TOCTOU claim; mutation-tested against the advisory lock)
- `core.TestReservationStatus_AllTransitions`
- `core.TestReservationStatus_TerminalStatesAreSticky`

(`postgres.TestReserverStore_Reserve_Concurrent` was removed from this list
in the 2026-09-02 audit's F-m1: 2 concurrent reserves that together stay
under the funded balance succeed identically with or without the advisory
lock, so it is empty for this invariant's actual claim -- see that test's own
doc comment. The test itself is kept for its "doesn't crash/deadlock" value,
just not cited as a pin here.)

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
this visibility race — do not add one. This was, until 2026-08-26, a prose-only
warning with no machine gate (docs/audits/2026-08-25-financial-engineering/financial-correctness.md);
`postgres.TestInsertJournalEntry_SingleChokePoint` now makes it one — it
parses this package's AST and fails if `InsertJournalEntry` ever gets a second
call site, or if its one call site stops calling `acquireBalanceLocks`.

**Closed gap (was open through migration 007)**: the delta filter's
`id > last_entry_id` comparison assumes `id` is unique across the whole
table, but the schema's actual guarantee is narrower — `journal_entries`'
primary key is `(id, created_at)`, not `id` alone, because a partitioned
table's primary key must include the partition key (`created_at`, monthly
range partitions). `001_baseline.up.sql`'s own comment calls this "a
uniqueness backstop beyond trusting the sequence" — **that description is
wrong and is left uncorrected in the migration itself** (already-applied
migrations are never edited; see `~/.claude/rules/deployment.md`) — the
composite key only forbids the exact same `id` repeating inside the *same*
partition; nothing at the schema level stopped the same `id` from appearing
once per partition. Before migration 008, that was not a schema-level
backstop against a row inserted with an explicit, already-used `id` in a
different partition — which the `ledger_app`-credential threat model this
audit wave treats as in-scope elsewhere (see I-22) already permitted.
The pre-migration-008 form of the pin below (see git history for its prior
name and body) confirmed the consequence was real, not merely plausible: such
a forged, internally-balanced pair was
permanently invisible to `GetBalance` (its id never exceeds the watermark)
while `SumGlobalDebitCreditByCurrency`/`reconcile.sql` counted it and saw a
balanced total either way — the two views of the ledger diverged without
violating any id-range invariant or tripping the global debit==credit check.
**Migration 008 (I-42) closes this**: `ledger_app`'s INSERT on
`journal_entries` (every partition, derived from `pg_partition_tree`, plus
the parent) is now column-scoped to exclude `id`, so the shared
`journal_entries_id_seq` — one sequence for the whole partitioned table — is
the only remaining source of a row's `id`, and any INSERT statement naming
`id` explicitly is refused at the ACL layer (`42501`) regardless of what
value it supplies. See I-42 for the full argument.

**Pinned by**:
- `postgres.TestLedgerStore_GetBalance_MultipleJournals`
- `postgres.TestPlatformBalance_RealtimeReflectsUnrolledJournal`
- `postgres.TestQueryStore_GetSystemRollups_RealtimeReflectsUnrolledJournal`
- `postgres.TestInsertJournalEntry_SingleChokePoint` (load-bearing prerequisite,
  now a machine gate)
- `postgres.TestJournalEntries_DuplicateIDAcrossPartitions_Rejected` (I-42's
  pin — the same forged-id attack, now asserted refused under a real
  `ledger_app` credential)

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
- `postgres.TestSchema_NumericColumnsAreExactly30_18` (F-M4/F-P6, 2026-09-02
  audit: the schema half of this invariant had never been checked -- the two
  pins above are both Go-side and never touch the database. Queries
  `information_schema.columns` for every `numeric`-typed column in the
  `public` schema and asserts precision 30 / scale 18, mechanically derived
  from the full column set rather than a hand-maintained table allowlist.)
- `postgres.TestSchema_NoFloatTypedColumns` (financial.md's float ban,
  checked at the schema layer: no `double precision`/`real`/`float` column
  anywhere in `public`.)

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

**Pinned by** (F-P7, 2026-09-02 audit: this section had no Pinned by at all):
- `postgres.TestSchema_NullableColumnsExactlyMatchI7Exceptions` — queries
  `information_schema.columns` for the entire `public` schema and asserts
  the nullable-column set is exactly the six exceptions above, plus two
  categories this document does not yet mention (flagged separately, not
  new drift): the dead `deposits`/`withdrawals` tables (`001_baseline.up.sql`
  says outright they predate this convention) and four claim-lease columns
  (`rollup_queue`/`registration_rescans`, `claimed_until`/`processed_at`/
  `last_error`). A NOT NULL dropped anywhere else in the schema, or one of
  the six exceptions removed, goes red either direction.

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
- `postgres.TestAudit_TraceBooking` (booking → events → journals stitch --
  the `events.journal_id` direction)
- `postgres.TestIntegration_FullLedgerFlow`
- `postgres.TestJournalsGuard_EventIDSetOnce` (F-P10, 2026-09-02 audit: the
  `journals.event_id` direction -- the two pins above cover
  `events.journal_id` but neither one's assertions go red when
  `journals.event_id` is forced to always be null; this one does, because
  its `j.EventUID` comes from a genuine `journals.event_id` round-trip via
  `GetEventUIDByID`, not an echo of the input)

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

**Scope — holds bind Reserve, and only Reserve.** The subtraction above
happens in exactly one place: `Reserve`'s own availability check. A direct
journal post (`PostJournal`, `ExecuteTemplate`, any preset template such as
`lock_funds` or `transfer_out`) is **not** constrained by outstanding holds —
and neither is I-17's `min_balance` check, which reads the raw dimension
balance without netting out active reservations. So "reserved" means
"protected from other reservations", not "protected from spending": a caller
that reserves 100 and then posts a 100 journal on the same dimension will
succeed (or, with `min_balance = 0`, will drive the later settlement journal
into `ErrInsufficientBalance`, wedging the reservation until it expires).
Consumers that need reserved funds to be unspendable must route *all*
consumption through Reserve→Settle, or park held funds on a `role=locked`
classification via a journal.

**Why min_balance is deliberately NOT netted against holds** (considered and
rejected, 2026-08-29 review W-3): the obvious "fix" — subtract outstanding
holds inside the account-policy `min_balance` check so a direct journal can't
spend reserved funds — would break settlement itself. The charge a `Settle`
posts *is* a direct journal against the same dimension, spending exactly the
funds the reservation holds; a hold-netting min_balance would reject that
charge (its own reservation's hold is still outstanding at the moment it
posts), wedging every settlement. A direct `PostJournal` carries no reference
to "which reservation, if any, this relates to", so the check cannot exempt
the settling reservation. Netting holds is therefore not a safe default or a
safe opt-in at this layer — it trades a documented, bounded gap for a
money-path regression. The correct boundary stays: holds bind Reserve; direct
journals are bounded by `min_balance` on the raw balance; unspendability is
achieved by routing consumption through Reserve→Settle or a `role=locked`
parking journal, both above.

**Why**: the obvious one — overdraft prevention. The non-obvious part: this
must be checked **inside** the advisory lock (see I-4), not before.

**Enforced by**: `postgres.ReserverStore.Reserve` (lock → check → insert),
`postgres.LedgerStore.sumBalancesByRoleWithQueries` (shared basis),
`classifications.balance_role` CHECK constraint (migration `032`).

**Pinned by**:
- `postgres.TestReserverStore_Reserve_Concurrent_RejectsOverCommit` (see I-4:
  `TestReserverStore_Reserve_Concurrent` was removed from this list in the
  2026-09-02 audit's F-m1 -- empty for this invariant too, same reason)
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
- `service.TestFullReconciliation_DetectsGlobalImbalance` (F-P12, 2026-09-02
  audit: the three pins above are all happy-path "the system is balanced"
  assertions -- every other check in `RunFullReconciliation` had a sibling
  test proving it catches real drift except check1
  ("global_dr_cr_equality"), which had none. This one injects a
  currency-level debit/credit gap via a mock `GlobalSummer` and asserts
  `Passed=false` at the `RunFullReconciliation` layer.)

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

(F-m7, 2026-09-02 audit: `TestPartitions_RebalanceStrandedDefaultRows` above
calls `EnsureMonthlyPartitions`, not `RebalanceDefault` -- it was previously
the only pin cited for `RebalanceDefault` despite never calling it. Two new
pins close that gap and the service-layer gap alongside it:
`postgres.TestPartitions_RebalanceDefault_DirectCall` calls
`PartitionStore.RebalanceDefault` directly; `service.TestPartitionService_EnsureUpcoming_StrandedRowsTriggersRebalance`
covers the worker-facing `PartitionService.EnsureUpcoming` self-heal branch
that calls it, which had zero coverage of its own — see also
`service.TestPartitionService_EnsureUpcoming_HealthyHorizon` for the common
no-stranded-rows path.)

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

**As-of reads self-heal against retroactive posting (added 2026-08-26)**:
`ListBalancesAt` itself always reads live from `journal_entries` and was
never affected by this. But `balance_snapshots` is a cache of `ListBalancesAt`
computed once (`service.SnapshotService.CreateDailySnapshot`), and nothing
re-triggered that computation when a later write retroactively backdated
(`effective_at` earlier than the snapshot's own `created_at`) into an
already-snapshotted business date — the cached row stayed wrong forever, even
though the live query was always correct (see
`docs/audits/2026-08-25-financial-engineering/financial-correctness.md`
Major #2, "effective_at 回溯记账不会让已写入的历史快照失效"). Fixed at the
read boundary: `postgres.RollupAdapter.GetSnapshotBalances` now checks each
cached row against `journal_entries` for exactly that condition
(`GetMaxEntryCreatedAtForDimensionBefore` in
`postgres/sql/queries/checkpoints.sql`) and recomputes live from
`ListBalancesAt` when a row is found stale, instead of trusting the cache
unconditionally. `service/snapshot.go`'s write path is unchanged — this is a
read-time self-heal, not a write-time invalidation, so a snapshot row can
still be numerically wrong in storage; only reads through
`GetSnapshotBalances` are guaranteed correct.

**Enforced by**:
- `core.JournalInput.Validate` rejects `effective_at` beyond the future
  tolerance.
- `postgres.LedgerStore.postJournalWithQueries` defaults a zero `effective_at`
  to `now()` and writes the same resolved value to the journal row and every
  entry row in the same transaction.
- Reversal journals (`ReverseJournal`) never copy the original journal's
  `effective_at` — they always default to "now" (open period), which is the
  standard close-then-correct pattern (see I-15).
- `postgres.RollupAdapter.GetSnapshotBalances`'s staleness check and live
  recompute (see above).

**Pinned by**:
- `core.TestJournalInput_Validate_EffectiveAt_Zero_OK`,
  `..._Past_OK`, `..._WithinTolerance_OK`, `..._FarFuture_Rejected`
- `postgres.TestEffectiveAtColumnsExist` (schema pin)
- `postgres.TestLedgerStore_PostJournal_EffectiveAt_DefaultsToNow`
- `postgres.TestLedgerStore_PostJournal_EffectiveAt_Backdated` (also pins
  entry/journal `effective_at` equality)
- `postgres.TestLedgerStore_PostJournal_EffectiveAt_RejectsFarFuture`
- `postgres.TestLedgerStore_ReverseJournal_EffectiveAt_DoesNotInheritOriginal`
- `postgres.TestRollupAdapter_ListBalancesAt_UsesEffectiveAt` (as-of reporting
  reads the business date, not the write date)
- `postgres.TestRollupAdapter_GetSnapshotBalances_BackdatedEntryInvalidatesCache`
  (the Major #2 fix: a snapshot written before a backdated entry lands must
  read back the corrected total, not the stale cached one)

## I-15: The accounting period close line is a hard write barrier

No journal is *written* behind a close line that was already active when the
write happened: once a `period_closes` line is committed
(latest-`created_at`-row-wins), no transaction can afterwards land a journal
whose `effective_at` precedes it. Real-time balances
(`checkpoint + delta`) are unaffected — the close line only gates *new
writes*, it never rewrites or hides history.

> Wording corrected 2026-09-02. This section previously read "There is no
> journal whose `effective_at` is earlier than the currently active
> period-close line" — a universal claim that is false after every ordinary
> close: closing August makes every August journal's `effective_at` earlier
> than the line, which is the *point* of closing a period, not a violation.
> Stated that way the invariant was unfalsifiable in the wrong direction: any
> check written against its letter would fire on healthy fleets, which is
> presumably part of why no check was ever written. The property that is
> actually worth guaranteeing, and that the barrier below delivers, is the
> one now stated: the line is a barrier against *later writes*, not a claim
> about existing history.

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
`effective_at < close_before` — **and, since 2026-09-02, does so under the
shared half of the period-close advisory barrier, with `ClosePeriod` taking
the exclusive half.** See I-59 for that mechanism and for the
`period_close_violations` reconciliation check that can falsify it.

> Enforcement gap closed 2026-09-02 (`concurrency.md` B-M5). Reading the line
> "inside the same transaction as every write path" was the whole of the
> enforcement, and it is not exclusion: `PeriodCloseStore.ClosePeriod` took
> no lock of any kind, so under READ COMMITTED it could INSERT and COMMIT a
> new line at any point between a writer's read and that writer's COMMIT.
> The window is not microseconds — a consumer's `RunInTx` holds the
> transaction open for as long as its own callback runs, which
> `ledger.Service.RunInTx`'s doc actively encourages. Nothing in the
> reconciliation suite compared `journals.effective_at` against
> `period_closes.close_before`, so a journal that slipped through left no
> trace anywhere.

**Pinned by** (every pin below is single-threaded — it closes the period
first and then asserts a later posting is refused. That is the whole reason
B-M5 went unnoticed for as long as it did: the hole was purely a matter of
two transactions' relative timing, which no sequential test can express. The
concurrency pins live on I-59):
- `postgres.TestPeriodClosesTableExists` (schema pin)
- `postgres.TestPeriodCloseStore_ActiveCloseLine_NeverClosed` — nothing to
  enforce before the first close
- `postgres.TestLedgerStore_PostJournal_PeriodClosed_Rejected` — a posting whose
  effective date falls before the active close line is refused
- `postgres.TestPeriodCloseStore_Reopen_LatestRowWins` — reopening is an append,
  latest row wins (full close-line history is retained)
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
  The balance it evaluates is the **raw** dimension balance — active
  reservations are NOT subtracted. `min_balance = 0` therefore does not stop
  a journal from spending funds that a reservation is holding; it only stops
  the balance itself going below zero. See I-11's scope note for what holds
  do and do not protect against.

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
Go-side at insert time. Internal `BIGSERIAL`/`IDENTITY` ids exist only inside
storage (primary keys, foreign keys, advisory-lock keys, keyset-pagination
cursors) and appear in **no public contract**: not in HTTP request or
response bodies, not in path or query parameters, and not in the
library-mode Go API (`core` types and interfaces speak uids exclusively).
Pagination cursors that encode an internal position are opaque base64
strings.

The rollup/reconcile engine's internal, id-keyed working representations
(`service.BalanceCheckpoint`, `service.RollupQueueItem`, keyed on
`CurrencyID`/`ClassificationID int64` for that engine's hot path) live in
`service`, not `core` — the same convention as `service.ClassificationDim`'s
doc comment ("internal ids never leave the service"). The one place a
checkpoint crosses into the public library API —
`CheckpointIntegrityStore.RebuildCheckpoint`, reached via
`Service.CheckpointIntegrity()` — returns the uid-based
`core.BalanceCheckpoint` instead.

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
  internal-id JSON key in any handler request/response struct — the banned-key
  set is derived from `postgres/sql/migrations/*.up.sql` by
  `internal/idschema.BannedKeys`, not a hand-maintained word list, so a
  future internal-id column is caught without editing this test;
  `TestContract_NoInternalIDKeysInJSON_CatchesSchemaColumnsMissedByOldWordList`
  regression-pins the specific columns — `policy_id`, `entry_id`,
  `last_entry_id` — the old hand list missed)
- `core.TestNoInternalIDFieldsInCoreTypes` (same `internal/idschema`
  derivation, called directly by both `server` and `core`'s test packages —
  board #28 (test-credibility.md) found this used to be an independent
  ~55-line copy in each package, since `core` cannot import `server`'s test
  file without a cycle; `internal/idschema` is a dependency-free package
  neither `core` nor `server` production code imports, so both test files
  can share the ONE implementation with no cycle — scans every exported type
  declared in `core/*.go` directly, so a `core` type carrying an internal id
  is caught even before anyone wires it into an HTTP handler, matching this
  invariant's "`core` types ... speak uids exclusively" clause literally
  rather than only its HTTP-wire consequence;
  `TestNoInternalIDFieldsInCoreTypes_CatchesPlantedViolation`
  regression-pins that the scan itself still fires against a planted
  fixture)
- `service.TestReconcileFindings_NoInternalIDPatternsInSource` (the reconcile
  report is an API response body; its free-text Description/Detail strings
  carry uids/codes, never internal ids — per-row forensics go to server logs)
- `postgres.TestCheckpointIntegrity_RebuildCheckpoint_ReturnsUIDsNotInternalIDs`
  (the library-mode Go API surface: `RebuildCheckpoint`'s result echoes back
  the same uids the caller passed in, and no field on the result type carries
  an internal-id-shaped json tag)

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
- `postgres.TestSweepBooking_NeverPostsJournal_FailedAndRetryPath` (pending →
  sent → failed → pending (revive) → sent → confirmed, asserting
  `journal_uid` stays empty through the failed status and the retry --
  the branch the test above doesn't reach)
- `presets.TestInstallCryptoDepositBundle` (asserts
  `journalStore.GetJournalTypeByCode(ctx, SweepClassificationCode)` returns
  `core.ErrNotFound` after installing the full bundle)
- `service.TestOnchain_Sweep_NonceReuseAndNoJournal` (sweep job end to end:
  nonce reuse across retries, and no journal on any transition)

---

## I-20: Deposit booking idempotency survives a reorg

A crypto deposit's booking idempotency key is
`deposit-{chain_id}-{tx_hash}-{txlog_seq}`, where `txlog_seq` is the log's
zero-based position **among all logs in that transaction's receipt** — never
the chain's block-level `log_index`, and never a position within whatever
subset of logs a particular query happened to return (design doc §3).

Two properties are required of that definition, and only a
transaction-internal one has both:

1. **Independent of who is looking.** The watcher queries `eth_getLogs` for
   every registered address at once; a registration rescan queries exactly
   one address. Any definition phrased in terms of "the logs that credit one
   of *our* registered addresses" is a function of a set that differs between
   those two callers, so a transaction crediting two registered addresses
   yielded a different `txlog_seq` — and therefore a different idempotency
   key — depending on which path saw it first.
2. **Stable when the transaction is re-mined.** A reorg that re-includes the
   same transaction in a different block reassigns block-level log indices,
   but does not reorder the logs the transaction itself emits. Keying off
   `log_index` would mint a fresh key for an already-credited transfer.

⚠️ Until the 2026-09-02 remediation (G-C2) this invariant was stated with the
first property violated: `txlog_seq` was the hit's ordinal among the logs the
current call returned. The consequences were both directions of wrong — the
same transfer booked twice (rescan first, then the watcher deriving a
higher-numbered, unused key for it), or a legitimate deposit dead-lettered
forever (watcher first, then the rescan deriving a key already held by a
different holder's transfer, hitting `ensureBookingMatchesInput`'s payload
mismatch). It was reachable without an attacker — one multisend crediting two
registered addresses — and, because deposit addresses are publicly derivable
(`salt = holder`, factory address public), constructible on purpose.

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
  comment states the single admissible definition (receipt-relative
  position). Both ingestion paths must derive it that way: the chains/evm
  watcher and an external scanner feeding the `channel/onchain` webhook
  bridge.
- `evm.Reader.FetchDeposits` (`chains/evm/reader.go`) — for every
  transaction that credited a registered address in the scanned window it
  reads the transaction receipt (`eth_getTransactionReceipt`) and maps each
  hit log's block-level index to its position inside that receipt. A receipt
  that cannot be read fails the whole scan rather than falling back to a
  query-dependent number: with I-52's cursor semantics the caller then
  retries the same window, so failing closed costs a tick, while guessing
  costs a duplicate or lost deposit.
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
  (`postgres/idempotency_match.go`) deliberately excludes `block_number`
  from `CreateBooking`'s payload-equality check.
- The same exclusion list (`bookingMetadataObservationVariantKeys`) also
  covers `review_reason`, `reject_reason`, `approved_by`, and `rejected_by`
  — every one of these is written onto a booking's `Metadata` by a
  `Transition` call that happens strictly AFTER `CreateBooking`
  (`service.Onchain`'s `routeToReview` / `ApproveReview` / `RejectReview`),
  never present in the original `CreateBookingInput.Metadata` a rescan or
  retried webhook delivery re-derives. Every other field (including every
  other `Metadata` key) is still compared exactly.

**Pinned by**:
- `postgres.TestDepositBooking_IdempotencyKey_StableAcrossBlockNumberChurn`
  (re-ingesting the identical sighting with a DIFFERENT `block_number`,
  simulating a reorg re-mining the tx into a different block, resolves to
  the same booking — not `ErrConflict`)
- `postgres.TestDepositBooking_IdempotencyKey_StableAcrossReviewAuditMetadata`
  (one subtest per audit key -- `review_reason`, `approved_by`,
  `reject_reason`+`rejected_by` -- replaying the pre-Transition
  `CreateBookingInput` against a booking whose `Metadata` has since gained
  that key must resolve to the same booking, not `ErrConflict`; test-credibility.md
  flagged this as PLAUSIBLE-but-unverified since only `block_number` had a
  dedicated test -- verified real, now covered)
- `postgres.TestBookingMetadataMatches_ObservationVariantKeys_TableDriven`
  (F-P20, 2026-09-02 audit: the two pins above cover the keys someone
  remembered to write a test for; this one is derived FROM
  `bookingMetadataObservationVariantKeys` itself, at the pure-function
  `bookingMetadataMatches` layer, so a sixth key added to that list without
  test data is automatically exercised, both directions -- ignored on its
  own, still conflicts on any other field)
- `service.TestOnchain_IngestDeposit_FullLifecycle` (end-to-end:
  re-observing the same sighting is a pure no-op; a second Transfer log in
  the same tx with a different `txlog_seq` does not collide)
- `onchain.TestEVMAdapter_ParseSighting` (the webhook bridge derives
  `TxLogSeq` from the payload's tx-local `txlog_seq` field, never a
  block-scoped index; also requires `block_number` per
  `core.DepositSighting.Validate`)
- `evm.TestReader_FetchDeposits_TxLogSeqIsIndependentOfAddressFilter`
  (`chains/evm/reader_test.go`) — the pin this invariant lacked entirely: one
  transaction crediting two registered addresses, queried once with both
  addresses (the watcher) and once with one (a registration rescan), must
  derive the same `TxLogSeq` for the same transfer, and that value must be
  the receipt position rather than the filtered-result position. Every
  earlier pin listed above goes through the store, a hand-fed sighting, or
  the webhook parser — none of them through the watcher's own derivation,
  which is where the defect lived.
- `evm.TestReader_FetchDeposits_SkippedLogsLeaveATraceAndDoNotShiftSeq` (a
  log dropped for an unlisted token or a malformed payload must not renumber
  the surviving transfers — the same defect's second face)
- `evm.TestReader_FetchDeposits_UnreadableReceiptFailsClosed` (no sighting may
  be produced from a transaction whose receipt could not be read)

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
  `deposit_confirm` journal, cross-linked via `EventUID`, only at that point
  -- directly asserts `events.journal_id` resolves to the same journal
  `ApproveReview` returned, not just that some journal exists;
  test-credibility.md flagged the name as claiming this without checking it,
  now closed)
- `service.TestOnchain_RejectReview_NoJournal` (rejecting a reviewed booking
  transitions it to `failed` with `journal_uid` remaining empty forever)

## I-22: `ledger_app` has no DDL

The application-facing database role, `ledger_app`, can `SELECT`/`INSERT`/
`UPDATE` ordinary tables and `SELECT`/`INSERT` (never `UPDATE`/`DELETE`) on
`journal_entries` — but it can issue no DDL of its own: no `DROP`,
`TRUNCATE`, `ALTER` or trigger management, and it cannot create an object by
naming one. This is true from the moment the schema exists, independent of
who currently owns the tables: `ledger_app` was never granted anything beyond
`SELECT`/`INSERT`/`UPDATE` and will never own anything.

**The DDL it CAN reach, and only through**: migration 007 grants `EXECUTE` on
two `SECURITY DEFINER` functions, `ledger_create_monthly_partition` and
`ledger_rebalance_default_partition` (I-35). Between them they issue `CREATE
TABLE ... PARTITION OF`, `DETACH`/`ATTACH PARTITION` and a `TRUNCATE` of the
default partition — so the sentence above has to be read precisely: what
`ledger_app` cannot do is compose DDL. What it can do is call two functions
that each perform one fixed, argument-validated shape of it, on
`journal_entries` alone, running as `ledger_owner` rather than as itself.

This wording was corrected on 2026-09-02: the invariant previously said
`ledger_app` "cannot ... create any object, anywhere in the schema" and
"cannot `TRUNCATE`", both of which migration 007 had made false, while I-35
described the exception as an established fact. Two invariants contradicted
each other for two audit rounds, and the pin could not see it because it only
tried bare statements — "cannot do X directly" and "cannot do X" differ by a
dimension nothing was testing. The reason it matters is not the two functions
(013 and 021 constrain both arguments and both are load-bearing for partition
maintenance without an owner-privileged serving pool): it is that a reader
doing threat modelling off I-22 would conclude a leaked app credential
produces no DDL surface at all.

**The one exception, and what it is not**: migration 002 grants `DELETE` on
`webhook_nonces`. That table is a replay cache holding no financial data, and
its prune is a `DELETE` -- without the privilege the prune failed, its error
was returned, and every inbound webhook failed on it in exactly the
deployments that connect as `ledger_app`. The exception is one privilege on
one non-financial table. It does not weaken anything this invariant claims:
`ledger_app` still holds no `DELETE` on any table that records money, still
owns nothing, and still cannot perform DDL of any kind.

**Why**: GRANT-based privileges alone are not a defense against a
compromised application credential — a connection that owns its own tables
(or is superuser) can `DROP TRIGGER` the append-only guards, `TRUNCATE`
`journal_entries`, or silently detach a partition (attack path A6,
docs/plans/2026-08-21-tamper-evident-ledger-design.md §2), regardless of
what GRANT says. Postgres cannot confer `ALTER`/`DROP`/`TRUNCATE`/
trigger-management rights through `GRANT` — only object ownership (or
superuser) grants them — so this invariant is only true because `ledger_app`
never owns anything.

**Note on grant coverage**: the baseline's GRANT statements only enumerate
tables that exist at install time, and its `ALTER DEFAULT PRIVILEGES`
deliberately benefits only `ledger_owner` — every later migration that adds
a table is required to GRANT `ledger_app`/`ledger_ro` on it explicitly
(contracts.md §9 point 3). The pin below (`TestGrantCoverage_*`) makes that
rule self-enforcing going forward instead of depending on a migration
author remembering it (`working-agreements` §5).

Coverage is every **table, sequence, partition and function** in `public`, as
of 2026-09-02. The first two were covered from the start; the other two were
blind spots that each hid a real finding for two rounds. Partitions were
excluded by the main query's `NOT relispartition`, so a partition granted
separately was invisible — `TestPartitionACL_EveryPartitionCarriesTheParentShape`
now derives each partition's expected ACL from `journal_entries` itself.
Functions were not looked at at all, which is how 007's two `EXECUTE` grants
sat unexamined; `TestFunctionExecuteACL_IsExactlyTheDocumentedWhitelist`
asserts each role's `EXECUTE` set equals a written whitelist, and migration
021 revokes the `PUBLIC` default that made every guard function in the schema
callable by everyone.

**Ownership is part of the grant story, not separate from it** (2026-09-02,
migrations 019/021): every relation and routine in `public` must be owned by
`ledger_owner`. The `Why` paragraph above explains what ownership confers that
`GRANT` cannot; the corollary is that an object owned by anyone else is
outside the model this invariant describes, whether or not its ACL looks
right. That applied to everything migrations 002–018 created — including both
`SECURITY DEFINER` functions, which therefore ran with the *bootstrap*
credential's privileges rather than `ledger_owner`'s. See I-57.

**A table carrying a credential-shaped column may not be granted table-level
`SELECT` to `ledger_ro`** (2026-09-02, D-m3). Migration 007 took
`webhook_subscribers` away from `ledger_ro` because reading `secret` "does not
just disclose data, it hands a read-only credential the ability to forge signed
event deliveries to any subscriber" — but the grant-coverage pin went on
requiring table-level `SELECT` for `ledger_ro` on every other table, so the next
table with a signing key would have gone red until its author granted the key
to the BI role. The requirement is now derived from
`information_schema.columns`: a column matching
`secret|password|passwd|token|private_key|seed|hmac|credential` obliges the
table to be column-scoped, and obliges that column to be unreadable.

**Note on the configuration tables (migration 003)**: the guard set is not
limited to the tables that record money. A per-journal authorization signature
authenticates what the application *read*, not what happened -- `IngestDeposit`
resolves a holder from `deposit_addresses` and `EntryTemplate.Render` reads
`entry_template_lines` fresh on every call -- so a credential that can rewrite
those tables makes the application sign a correct journal about the wrong
facts, and the result is signed, chain-attested and reports `VERIFIED`.
Migration 003 puts a column whitelist on `currencies`, `classifications`,
`journal_types`, `entry_templates`, `entry_template_lines` and
`deposit_addresses`, leaving mutable only what has a real mutation path
(`is_active`, `display_label`, `lifecycle`, and `balance_role`'s one-way
upgrade). `currencies.exponent`, `classifications.normal_side` and `.code`,
and every column of a template line are immutable as a result.

**Note on ACL/trigger consistency**: the ACL and the append-only mutation
guard on a table must agree — a table protected only by a trigger, with an
ACL that still says it is updatable, is one `GRANT`-layer bypass away from
looking updatable to the next reader and one code path away from actually
being written to (the trigger still blocks it, but the two defenses no
longer say the same thing). The pin derives "which tables carry this guard"
from `information_schema.triggers` (matching on the exact `BEFORE UPDATE
... EXECUTE FUNCTION ledger_block_mutation()` shape), not a hardcoded table
list, so any future table reusing this guard gets the matching ACL enforced
automatically — and any table with only a *partial* guard (`classifications`,
`reservations`, `journals` — see A1-A4 under I-25) is correctly left with
`UPDATE`, since those are legitimately mutated through controlled paths.

**Enforced by**:
- `postgres/sql/migrations/001_baseline.up.sql` — creates `ledger_owner` /
  `ledger_app` (`SELECT`/`INSERT`/`UPDATE`, no `UPDATE` on
  `journal_entries`, no DDL of any kind) / `ledger_ro`, grants each,
  `REVOKE ALL ON SCHEMA public FROM PUBLIC`, and transfers every
  table/sequence to `ledger_owner` — all in the same migration that first
  creates the schema (Wave 4, `2026-08-21-integrity-hardening-contracts.md`).

**Pinned by**:
- `postgres.TestLedgerAppIsLeastPrivilege` — confirms `ledger_app` cannot `TRUNCATE`/`DROP TRIGGER`/
  `ALTER TABLE`/`CREATE TABLE`/`UPDATE journal_entries`/`DELETE FROM
  journal_entries`/touch `schema_migrations`, while it can still
  `SELECT`/`INSERT`/`UPDATE` an ordinary table and `SELECT`/`INSERT`
  `journal_entries`. Those last two subtests are what keep the refusals
  from being vacuous: a role granted nothing at all would also fail every
  forbidden operation, so the pin only means something because the
  permitted operations are asserted to succeed in the same run. Two further
  subtests (2026-09-02) close the "directly" gap described above: it CAN
  create a partition through `ledger_create_monthly_partition` — and that
  partition comes out owned by `ledger_owner` — and it CANNOT call a function
  outside the whitelist.
- `postgres.TestFunctionExecuteACL_IsExactlyTheDocumentedWhitelist` /
  `postgres.TestPartitionACL_EveryPartitionCarriesTheParentShape` — the two
  layers grant coverage never read. The first asserts each role's `EXECUTE`
  set is exactly the reviewed list, so a function reachable by the `PUBLIC`
  default is a capability nobody granted; the second derives every
  `journal_entries` partition's expected ACL from the parent.
- `postgres.TestLedgerAppInsertsIntoPartitionCreatedAfterGrant`
  — a partition created *after* the
  role grants were issued is still writable by `ledger_app` through the
  parent table name. The grants name the parent, and a partition attached
  later inherits them; nothing has to re-grant per partition.
- `postgres.TestRoleAttributes` — pins role attributes
  (`LOGIN`, not superuser/createdb/createrole) and the exact grant set each
  role holds (`information_schema.role_table_grants`) on an ordinary table,
  `journal_entries`, and `schema_migrations`.
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
  Since migration `004`, `'' -> 'available'` is additionally refused once the
  classification has journal entries. `available` is the only bucket `Reserve`
  spends from, so promoting a classification that already holds balances turns
  them into spendable, withdrawable funds in one statement — the shipped
  `fee_expense` is debited on every withdrawal fee, so promoting it would hand
  every holder their fee history back as usable balance, through an ordinary
  and correctly signed withdrawal. `'' -> 'pending'` and `'' -> 'locked'` stay
  unrestricted: neither is spendable, so neither can make anyone richer. A
  deployment that genuinely means to promote a classification with history
  does it as `ledger_owner` with the guard dropped, which is deliberately more
  than one statement from an application credential.
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
- `account_policies.status`/`.min_balance`/`.enforce_min_balance`/`.note`/
  `.updated_at` are the only columns `UPSERT AccountPolicy` may change;
  `account_holder`/`currency_id`/`classification_id` (the policy's identifying
  dimension) are immutable. `account_policies` is the only DB-enforced
  freeze/overdraft floor (`postgres/account_policy_enforce.go`), so widening
  its dimension in the same statement that flips `status` to `active` is the
  same class of attack as `balance_role`'s promotion above.

  ⚠️ **Read that list again: the three mutable columns ARE the enforcement
  knobs.** The guard protects which account a policy applies to, not what the
  policy says. `UpsertAccountPolicy` writes `status`, `min_balance` and
  `enforce_min_balance`, so the whitelist has to permit them, so a credential
  with raw SQL can unfreeze an account and move its overdraft floor to
  -1,000,000 in one statement -- measured, as `ledger_app`, and it succeeds
  both before and after 2026-09-02. That is not a defect in the guard; it is
  the limit of what a column whitelist can do for a table whose contents are
  the control.

  What changed on 2026-09-02 (migration 020, D-M3) is the second layer: that
  statement now lands a row in `config_table_changes` carrying the full
  before/after and the authenticated role. Before, `account_policies` was the
  one table in this family with a whitelist that passed the attack AND no
  audit trigger -- excluded from the audit set on the grounds that it had an
  application-level trail in `account_policy_changes`, which the application
  writes and an attacker with raw SQL does not. Neither table recorded
  anything. It could not be stopped and could not be seen; now it is seen,
  and I-58 covers the general rule.
- `account_policy_changes` — its own audit trail — is append-only: no
  `UPDATE`, no `DELETE`.
- `bookings.journal_id` is set-once (`NULL -> non-NULL` only), matching the
  "at most one journal-bearing transition" rule `CLAUDE.md` already
  documents but which no trigger enforced before migration 006.
  `account_holder`/`currency_id`/`classification_id`/`amount`/
  `channel_name`/`reservation_id`/`idempotency_key`/`expires_at`/
  `created_at`/`uid` are immutable; `status`/`channel_ref`/`settled_amount`/
  `metadata`/`updated_at` are `UpdateBookingTransition`'s actual mutable set,
  and `settled_amount` may only increase (same reasoning as
  `reservations.settled_amount`).
- `events.journal_id` is set-once; `account_holder`/`currency_id`/
  `classification_code`/`from_status`/`to_status`/`amount`/`settled_amount`/
  `booking_id`/`metadata`/`occurred_at`/`actor_id`/`source`/`uid` — the
  record of what happened — are immutable. `delivery_status`/`attempts`/
  `next_attempt_at`/`delivered_at` (the outbound delivery queue's own state)
  remain mutable.
- `reservation_settlement_legs`, `reservation_operation_receipts` and
  `booking_transition_receipts` — the three idempotency-receipt tables — are
  append-only: no `UPDATE`, no `DELETE`. Forging or rewriting a receipt lets
  an attacker short-circuit the matching Settle/Release/FinalizeSettlement/
  Transition call for that idempotency key: the operation reports success
  without re-applying, while whatever it was supposed to close (a
  reservation, a booking transition) stays open.
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
provenance without touching any protected journal column; repointing
`bookings.journal_id`/`deposit_addresses.account_holder` or a template's
`entry_template_lines` row changes what a *future*, correctly-signed journal
says without touching the signature mechanism at all — the attestation chain
authenticates what the application read, not what actually happened, so a
credential that can rewrite the inputs to a signature doesn't need to forge
the signature itself.

⚠️ **26-08-26 correction**: the original enforcement below was itself
fail-open by construction, not just incomplete. `001_baseline` section 14
derives `ledger_app`'s grants from each table's own trigger — a table
carrying a `ledger_block_mutation()` `BEFORE UPDATE` trigger gets
`SELECT`/`INSERT` only, *every other table* gets `SELECT`/`INSERT`/`UPDATE`.
That default runs the other direction from every other guard in this file:
absence of a trigger reads as "this table may be freely mutated", not as
"nobody has decided yet". Seven tables were in that state and every one of
them decides where money goes or whether tampering can be seen:
`account_policies`, `account_policy_changes`, `bookings`, `events`,
`reservation_settlement_legs`, `reservation_operation_receipts`,
`booking_transition_receipts`. Every `UPDATE` migration 006 now refuses was
run against a real database as `ledger_app`, before migration 006 existed,
and every one succeeded — see its header for the specific statements.
`postgres/grant_coverage_test.go`'s coverage test made this worse rather
than catching it: it re-derives the append-only set from the *same*
`pg_trigger`/`pg_proc` predicate the migration uses, so it could only ever
confirm the migration's grants matched the migration's own trigger scan,
never that the trigger scan itself was the right test to run. Migration 006
closes the seven instances; `grant_coverage_test.go` closes the shape of the
gap by requiring every table in `public` to be explicitly classified
(append-only / update-revoked / reviewed-ordinary) — a table in none of the
three now fails the test loudly instead of defaulting to full access.

**Enforced by**:
- `ledger_classifications_guard()` / `classifications_mutation_guard`
  trigger (`postgres/sql/migrations/001_baseline.up.sql`, tightened by
  `003_config_table_guards.up.sql`).
- `ledger_reservations_guard()` / `reservations_mutation_guard` trigger
  (`001_baseline.up.sql`).
- `period_closes_no_update` / `period_closes_no_delete` triggers, reusing
  `ledger_block_mutation()` (`001_baseline.up.sql`).
- `ledger_journals_block_arbitrary_update()` — a generic `to_jsonb`
  comparison against a mutable-column whitelist (`event_id`'s
  `NULL -> non-NULL` set-once check lives in the function body, not a
  trigger `WHEN` clause) + `journals_event_id_fkey` FK
  (`001_baseline.up.sql`).
- `ledger_account_policies_guard()` / `account_policies_mutation_guard`,
  `ledger_bookings_guard()` / `bookings_mutation_guard`,
  `ledger_events_guard()` / `events_mutation_guard` — the same
  whitelist-comparison shape, added by
  `006_threat_model_guard_coverage.up.sql`.
- `account_policy_changes_no_update`/`_no_delete`,
  `reservation_settlement_legs_no_update`/`_no_delete`,
  `reservation_operation_receipts_no_update`/`_no_delete`,
  `booking_transition_receipts_no_update`/`_no_delete` — blanket
  `ledger_block_mutation()` refusal plus `REVOKE UPDATE`, matching
  `entry_template_lines`' 003 treatment (`006_threat_model_guard_coverage.up.sql`).
- Depends on P1 (the `ledger_app`/`ledger_owner` role separation, I-22):
  without role separation, the same credential that would abuse these
  columns can `DROP TRIGGER` the guard itself.

**Pinned by**:
- `postgres/mutation_guards_test.go` (classifications / reservations /
  period_closes / journals, pre-dating migration 006):
  - `TestClassificationsGuard_NormalSideImmutable`
  - `TestClassificationsGuard_BalanceRoleOnlyUpgradesFromEmpty`
  - `TestReservationsGuard_DimensionColumnsImmutable`
  - `TestReservationsGuard_SettledAmountMustNotDecrease`
  - `TestReservationsGuard_JournalIDSetOnce`
  - `TestReservationsGuard_StatusWhitelist`
  - `TestPeriodClosesGuard_NoUpdateNoDelete`
  - `TestJournalsGuard_EventIDSetOnce`
  - `TestJournalsGuard_FutureColumnsProtectedByDefault`
- `postgres/roles_test.go` (the seven tables migration 006 added, each pin
  runs the attack statement as `ledger_app` and requires it to fail):
  - `TestAccountPoliciesGuard`
  - `TestBookingsAndEventsGuards`
  - `TestIdempotencyReceiptTablesAreAppendOnly`
- `postgres.TestAccountPolicyEnforcementKnobChangeIsAudited` — the one case
  in this list where the attack SUCCEEDS by construction (the guard has to
  permit the three enforcement columns; see above). It runs the unfreeze +
  overdraft-floor statement as `ledger_app`, requires it to succeed, and then
  requires the resulting `config_table_changes` row to carry the before/after
  and the authenticated role. Asserting the refusal here would have been
  asserting something untrue.
- `postgres/grant_coverage_test.go`'s
  `TestGrantCoverage_EveryTableHasExpectedLedgerAppAndLedgerRoGrants` now
  fails on any table absent from its append-only/update-revoked/reviewed
  classification, which is the structural half of this fix.

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
§7.5, board #12/#13; corrected 2026-09-02, `tamper-evident.md` m-3)**: in
**tx mode** -- a store bound via `WithDB`, i.e. any `JournalWriter` call
composed inside `ledger.Service.RunInTx` -- `PostJournal`,
`ExecuteTemplateBatch` and `ReverseJournal`/`ReverseJournalFraction` never
sign, because there is no point in those call chains that is provably
outside a DB transaction the way `financial.md` requires for the Attestor's
signing call.

In **pool mode** those three DO sign under a configured Attestor (board
#15) -- see **I-31**, which is the invariant for exactly that. This note
used to say they "still never sign" without the mode qualifier, which
contradicted I-31 on the same page and, worse, told a reader that a
correctly-signing path was unsigned. `core/auth.go`'s
`AuthStatusUnsignedTxMode` doc comment has the accurate wording. This was ALSO true, before this fix, of every `JournalWriter` call
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

**Disclosed residual limitation 2 (`unsigned_tx_mode` is one-way, and stays
that way this round -- 2026-09-02 audit, C-R1)**: a journal posted through
`RunInTx` without going through `Authorize`/`PostAuthorized` carries
`AuthStatusUnsignedTxMode` forever. Journals are append-only, so it cannot
be signed after the fact, and `VerifiedBalanceReader` treats any dimension
with such a contributing journal as permanently UNDEFINED (I-32) -- which,
with `RequireVerifiedBalance: true`, means withdrawals from that dimension
are refused for good. The previous round made the trap **disclosed** (the
status column, `ledger.go`'s `RunInTx` doc comment, this note) and provided
the `Authorize` → `PostAuthorized` sequence to avoid it. It did not provide
a way out of it, and **this round does not either**. Recorded as a
deliberate decision rather than an oversight, with the options that were
examined:

- **(a) Re-sign in place** -- widen
  `ledger_journals_block_arbitrary_update`'s mutable whitelist to the
  `auth_*` columns for a one-time `unsigned_tx_mode` → `signed` transition.
  **Rejected.** Design doc §8.2 argues specifically against opening that
  guard for marker columns, and A4 is this repository's own precedent for
  a guard that rotted after being opened "just for one column". The
  whitelist is currently `['event_id']` and every reader of I-2 and I-26
  relies on that being the whole list.
- **(b) A side table** -- `journal_reauthorizations(journal_id PK, digest,
  signature, key_id, created_at)`, itself append-only, consulted by
  `VerifyJournalAuth`/`VerifiedBalance` when the main row is
  `unsigned_tx_mode`. Sound, and the recommended shape if this is ever
  built, but it touches the withdrawal gate's hot read path
  (`postgres/verified_balance_store.go`) and adds a second place a
  signature can live -- a change to the verification model, not a bug fix,
  and one that wants Aaron's sign-off rather than a remediation worker's
  judgement.
- **(c) Refuse instead of degrade** -- a `WithStrictSigning()` option
  making `PostJournal`/`ExecuteTemplate`/`ExecuteTemplateBatch` return an
  error inside `RunInTx` rather than silently producing an unsignable
  journal. Prevents new instances, does nothing for existing ones, and
  lands in `ledger.go` + `postgres/ledger_store.go` -- outside this
  round's remediation task boundary.

Recommendation on record: (c) to stop new occurrences, (b) to rescue
existing ones, both as a separate change with an explicit decision. Until
then the honest statement is the one `ledger.go` already makes: permanently
UNDEFINED, with no remediation API.

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
- `service.VerifyLedger`'s **step 3b** -- the read side of "every entry
  covered". The four items above are all about entries the chain SAYS it
  covers; none of them answers "is anything sitting outside coverage",
  which is the shape of a row inserted by direct SQL after the last batch.
  The 2026-09-02 audit (`tamper-evident.md` M-2) found this half of the
  invariant unimplemented: design doc §8.4 step 3 asks for the anti-join,
  `ListUncoveredEntries` existed for the WRITE side, and `VerifyLedger`
  was listed here as the enforcer while executing only the first half.
  Step 3b now probes `UncoveredEntries` (bounded by
  `VerifyConfig.UncoveredProbeLimit`, default 1000, with the cap reported
  when hit) and splits the result by the journal behind each entry: no
  valid authorization -> `TAMPERED`; legitimately signed or
  `unsigned_tx_mode` -> `DRIFT` with the count in
  `VerifyReport.UncoveredEntries`. A non-zero count is never `VERIFIED`:
  this run cannot testify about entries no signed attestation covers.

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
- `service.TestVerifyLedger_FlagsUncoveredUnsignedEntry` /
  `TestVerifyLedger_UncoveredButLegitimateEntriesAreDriftNotVerified`
  (`service/attest_verify_anchor_test.go`) -- step 3b's two outcomes: a
  forged journal's entries inserted after full coverage are `TAMPERED`;
  entries legitimately posted after the last batch are `DRIFT` with a
  count, never `VERIFIED`.

## I-28: The latest external anchor head matches the DB's attestation chain

(design doc §8.3/§8.4.) The external anchor (`core.Anchor`) remembers
only the latest `(seq, root_hash)` pair (design doc: "几十字节"), but
because I-27's hash chain links every later `root_hash` back to every
earlier batch's content, that single remembered value is enough to
detect a rewrite anywhere in the history: `ledger-cli verify` compares
the anchor's head against the DB row at the same `seq` and flags a
mismatch as `TAMPERED`. An anchor that is *behind* the DB's chain (has
published at least one seq and has not yet seen the latest ones) is a
distinct, benign state -- `DRIFT`, not `TAMPERED` -- because nothing
about it indicates the history was rewritten, only that publishing has
not caught up yet.

**An empty anchor is not a behind anchor** (2026-09-02 audit,
`tamper-evident.md` M-3). `core.Anchor.Head`'s contract is "the highest
seq, or 0 if empty", so "nothing was ever published here" and "what was
published has been erased or rolled back" arrive as the same observation.
Reading both as `DRIFT` -- whose own doc comment called it "a benign,
expected inconsistency", and which `ledger-cli` exits 0 on -- made
deleting the anchor a silent way to switch every external check off. One
`rm` on the dev carrier; one `PutObject` with the ledger's own token on
the R2 carrier. The classification is now:

| observation | verdict |
|---|---|
| `anchorSeq == 0`, DB chain non-empty, no higher observation on record | `NOT_RUN` (fail-closed: no external check ran, and this run cannot tell erasure from a first read) |
| `anchorSeq` lower than a recorded prior observation | `TAMPERED` (no benign mechanism moves an anchor backwards) |
| `0 < anchorSeq < maxSeqSeen` | `DRIFT` (finite, self-healing publish backlog) |
| `anchorSeq > maxSeqSeen` | `TAMPERED` (the anchor knows about attestations the DB does not) |

Distinguishing the first two requires remembering what the anchor said
before, and that memory cannot live in the anchor (the thing under
suspicion). It lives in `anchor_observations` (migration 018,
append-only, no UPDATE/DELETE grant), written by
`AttestationService.catchUpAnchor` on every successful `Head` read and
after every successful `Publish` -- see I-55.

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
  at `seq == anchorSeq` against it. Note what that comparison cannot see
  on its own: with `anchorSeq == 0` (or any seq lower than the chain
  reaches) it simply never fires, which is why the empty/regressed cases
  above are classified separately rather than left to it.
- `core.Anchor.Head`'s no-regression contract (I-56) -- "once Head has
  returned seq N, no later call may return a seq lower than N". The DRIFT
  vs TAMPERED split above is only meaningful if the carrier cannot walk
  its own head backwards.
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
- `service.TestVerifyLedger_DriftOnlyWhenAnchorHasPublishedButLags` -- an
  anchor that has published seq 1 while the DB chain reaches seq 2
  classifies as `DRIFT`, not `TAMPERED` or `VERIFIED`.
- `service.TestVerifyLedger_EmptyAnchorWithNonEmptyChainIsNotRun` /
  `TestVerifyLedger_AnchorRollbackToAnOlderSeqIsTampered` /
  `TestVerifyLedger_EmptyAnchorWithNoPriorObservationIsNotRun` /
  `TestVerifyLedger_EmptyAnchorIsNotRunNotDrift` -- the three rows of the
  table above that used to all collapse into `DRIFT`.
- `service.TestVerifyLedger_NotRunWithoutAnchor` /
  `TestVerifyLedger_NotRunWithoutVerifier` /
  `TestVerifyLedger_NotRunWhenAnchorHeadErrors` -- the fail-closed red
  line (`working-agreements` §3, same discipline as P0's
  `Complete`/`FullCoverage`): a missing public key, missing anchor, or
  an anchor that errors on `Head` all produce `NOT_RUN`, never a
  folded-in `VERIFIED`.
- `service.TestWorker_StartupLogNamesTheAnchorType` -- a dev file anchor and
  a production-shaped one must not look identical at startup:
  `StartupReport.AttestationAnchorType` names the type and the dev one
  additionally warns. The compile-time half of the same guard is
  `anchordev.NewLocalFileAnchorForDevelopment`'s name, which a composition
  root cannot write without saying what it is wiring.
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

## I-32: Withdrawal-time verified balance is fail-closed on any unauthorized contributing journal

**Rule**: `core.VerifiedBalanceReader.VerifiedBalance(holder, currency, classification)`
recomputes the dimension's balance directly from `journal_entries` (like
`CheckpointIntegrityStore.RecomputeBalance`) and additionally requires
*every* journal that contributed an entry to that dimension to carry a
valid P5 authorization (I-26). If even one contributing journal fails
`core.VerifyJournalAuth`, the result is UNDEFINED — surfaced as a non-nil
error wrapping `core.ErrUnauthorizedJournal`, never a number computed by
excluding the failing journal. A dimension with zero contributing journals
returns a DEFINED zero (vacuously: no journal touched it, so none can be
unauthorized).

This is a library-provided mechanism, not an imposed policy: nothing in
this library calls `VerifiedBalance` automatically. `core.Reserver`'s
`ReserveInput.RequireVerifiedBalance` is an opt-in gate a caller sets per
call — off by default, no threshold, no consumer-side choice made on the
library's behalf (`docs/plans/2026-08-21-integrity-hardening-contracts.md`
§W2-3). When that gate is set, the amount it authorizes is the recomputed
one, not `balance_checkpoints` + delta — that is I-49, and it is a
separate rule because it was originally, and wrongly, disclaimed here.

`VerifiedBalance` must be called outside any open transaction: an
`AuthVerifier` may run off-host and `financial.md` forbids external calls
inside a transaction, so `postgres.VerifiedBalanceStore` fails closed
(`core.ErrInvalidInput`) when bound to one — the same guard
`LedgerStore.Authorize` and the `Reserve` gate carry at their own entry
points.

**Why**: The naive alternative — exclude unauthorized journals and sum the
rest — is wrong and dangerous. A journal's net contribution to a dimension
can be negative (a reversal), so excluding an unauthorized one can report
a balance *higher* than the true one: an attacker who forges an unsigned
reversal of a genuine deposit would, under the naive definition, see the
reversal silently dropped and the original deposit's full amount still
counted as available — the opposite of what the check is supposed to
catch. Refusing to answer (UNDEFINED) is the only definition that can
never overstate what is safe to pay out.

**Enforced by**: `postgres.VerifiedBalanceStore.VerifiedBalance` (naive
reference implementation: individually verifies every contributing
journal via `core.VerifyJournalAuth`, then trusts
`CheckpointIntegrityStore.RecomputeBalance`'s entries-only sum once all
pass; refuses outright on a transaction-bound clone);
`postgres.ReserverStore.requireVerifiedAvailableBalance` (the
opt-in `Reserve` gate, run strictly before any transaction is opened —
same placement rule as `Authorize`, since an `AuthVerifier` is permitted
to be a remote call); `service.FullReconciliationService`'s
`unauthorized_journals` check (fleet-wide, samples the NEWEST page of
journals that claim a signature and confirms it still verifies; skips
journals that were never signed at all — that is a coverage gap, not tamper
evidence).

Two corrections to that check from the 2026-09-02 audit:

- **When every journal it saw was skipped, the check records
  `Complete=false`** (C-M8). It used to report `Passed=true, Complete=true`
  in that case, which made `ReconcileCheckResult(name, Passed && Complete)`
  emit green — so "the entire ledger is unverifiable" and "the entire
  ledger verified clean" were the same machine-readable signal. `Passed`
  deliberately stays true: finding no violation IS finding no violation.
- **With no `core.AuthVerifier` wired, the check is not run at all** and
  its name goes into `core.ReconcileReport.SkippedChecks` (C-m4). The
  `Complete=false` placeholder it used to return made `FullCoverage`
  permanently false for every deployment that never called
  `ledger.WithAttestor` — the same dead vote that got check #8 deleted. It
  also **scans the newest page, not the oldest**: it has no resume cursor,
  so the single page it looks at must be the one where a row forged today
  lands (the same shape as C-M1's sampling-direction bug in
  `VerifyLedger`).

**Pinned by**:
- `postgres.TestVerifiedBalance_ZeroContributingJournalsIsDefinedZero` —
  the vacuous-truth case: no journal ever touched the dimension, so the
  result is a defined zero even with no `core.AuthVerifier` configured at
  all.
- `postgres.TestVerifiedBalance_AllAuthorizedMatchesRecompute` — the
  positive path: when every contributing journal is genuinely signed,
  `VerifiedBalance` matches `CheckpointIntegrityStore.RecomputeBalance`
  exactly.
- `postgres.TestVerifiedBalance_UnauthorizedContributingJournalIsUndefined` —
  a single forged, unsigned contributing journal makes the whole balance
  UNDEFINED (a non-nil error, `balance.IsZero()`), not a smaller-but-real
  number.
- `postgres.TestVerifiedBalance_UnauthorizedReversalNeverInflatesBalance` —
  the exact scenario the Why section names: a genuine signed deposit
  reversed by a forged, unsigned reversal must never surface as the
  deposit's full un-reversed amount.
- `postgres.TestReserve_RequireVerifiedBalance_RejectsWhenUnauthorizedJournalExists` —
  the `Reserve` gate: refuses a reservation backed by a forged deposit
  even though the ordinary checkpoint-based available-balance check alone
  (a baseline `Reserve` call in the same test, `RequireVerifiedBalance`
  unset) would approve it.
- `postgres.TestReserve_RequireVerifiedBalance_AllowsWhenEverythingSigned` —
  the gate does not reject a perfectly ordinary, fully signed account.
- `postgres.TestVerifiedBalance_TxBoundStoreFailsClosed` — a
  transaction-bound clone refuses with `core.ErrInvalidInput` and the
  counting `AuthVerifier` records zero calls, so the guard is proven to
  fire *before* the external call, not after it.
- `service.TestFullReconciliation_WithoutAuthVerifier_StillReportsFullCoverage` /
  `TestFullReconciliation_UnauthorizedJournals_ZeroSignedIsIncomplete` /
  `TestFullReconciliation_UnauthorizedJournals_ScansTheNewestPage` /
  `TestFullReconciliation_UnauthorizedJournals_PassesWhenAllSignedJournalsAreValid` /
  `TestFullReconciliation_UnauthorizedJournals_FlagsForgedSignature` /
  `TestFullReconciliation_UnauthorizedJournals_SkipsNeverSignedJournal` /
  `TestFullReconciliation_UnauthorizedJournals_ReportsIncompleteWhenPageLimitHit` —
  the fleet-wide check's skip / pass / flag / coverage-gap-vs-tamper-
  evidence / page-limit-honesty behavior. (A signed-but-unrecognized-key
  journal — e.g. one signed before a key rotation — is also flagged, never
  silently skipped like a never-signed journal, but with wording distinct
  from tamper evidence: see I-45's
  `TestFullReconciliation_UnauthorizedJournals_FlagsUnknownKeyAsDistinctFromForgery`.)

---

## I-33: A cached attestation-time authorization verdict is trusted only in the failing direction; a passing one never excuses the live check

**Rule**: T4 (design doc §8 extended, contracts §W3-B) lets
`service.AttestationService.RunAttestBatch` run `core.VerifyJournalAuth`
once per distinct journal contributing to a batch (instead of once per
`VerifiedBalance` call, per contributing journal, forever — I-32's naive
reference implementation), and persist the result as a
`core.JournalAuthVerdict` on `entry_attestations.auth_verdict`, bound into
the batch's own signed content (`core.AuthVerdictDigest` →
`core.AttestationRootHashV3`, separator `0x12`). `postgres.VerifiedBalanceStore`
then reads that cached verdict instead of re-deriving it:

- `core.JournalAuthVerdictAuthorized` → **not** trusted as a substitute for
  verification. It still gets a live `core.VerifyJournalAuth`.

  This is the correction. T4 originally skipped the live check here, and the
  invariant this section replaces claimed a cached verdict was "at least as
  strict as a live check". It is not, and the gap is not subtle: the verdict
  answers *was this journal authorized when it was attested*, while the
  withdrawal gate needs *is it authorized now*. `core.CanonicalJournalDigest`
  covers every entry's `Amount`, so editing an amount after attestation leaves
  the cached verdict reading `Authorized` while a live check would fail — and
  the fast path skipped exactly the check that would have failed. The batch's
  signed content protects the verdict from being altered; it does not protect
  the entries the verdict was about.

  `entry_attestations.leaf_hash` does protect those, and comparing it is what
  the asynchronous `VerifyLedger` sweep does (I-29/I-30). That is detection.
  This gate is prevention, and it cannot defer to a sweep that runs later.
- `core.JournalAuthVerdictUnauthorized` → the whole balance is UNDEFINED
  immediately (I-32's rule still applies — this is I-32's mechanism
  amortized, not weakened).
- `core.JournalAuthVerdictUnknown` (the sentinel — an uncovered/tail entry,
  or an `entry_attestations` row that predates migration 054, or an
  `AttestationService` with no `core.AuthVerifier` configured) → MUST fall
  back to a live `core.VerifyJournalAuth`, exactly the pre-T4 behavior. It
  is never treated as a passing verdict (would silently widen what counts
  as authorized) nor as a failing one (would make every pre-T4 account
  permanently UNDEFINED the moment T4 ships).

A journal that contributes some cached-`Authorized` entries and some
cached-`Unknown` entries (its entries straddled an attestation batch
boundary, the same ordering hazard design doc §8.2 documents for P6) is
trusted on the strength of the `Authorized` verdict alone — safe because
`core.VerifyJournalAuth` at attestation time reconstructs the journal's
**complete** entry set (`postgres.AttestationStore.JournalAuthMaterial`
batch-fetches ALL of a journal's entries, not just the ones in the current
attestation batch), so the verdict is already a statement about the whole
journal, not just the entries that happened to be covered first.

A cached verdict is not a standing trust anchor with no expiry: P6's
periodic full verify (`service.VerifyLedger`) re-derives `AuthVerdictDigest`
from a **live** `core.VerifyJournalAuth` pass and compares it against the
stored, signed value — the same class of drift detection I-27/I-28 already
provide for `batch_digest`/`merkle_root`. A journal's stored auth columns
being edited without a valid re-sign (an owner-role bypass of the
no-arbitrary-update trigger — this wave's standing threat model) surfaces
as `TAMPERED`, not a silently-stale `VERIFIED`.

**Why**: I-32 established that a withdrawal-time balance check must be
fail-closed on any unauthorized contributing journal. `.local/bench-verify-2026-08-23.md`
measured what enforcing that literally costs: ~216–240µs per contributing
journal, ~84% of which is a single `journals JOIN journal_entries` round
trip (only ~36µs is the actual cryptographic work) — a naive account with
just 10–12 contributing journals already costs more than one full
`PostJournal` write, and the cost grows **linearly and unbounded** for the
lifetime of any account that keeps transacting. Re-deriving the same
answer on every call is not a correctness requirement — I-32 only requires
that an unauthorized journal is never silently excluded from the sum, not
that the check be redone from scratch every time. Caching the verdict in
content an external anchor already protects (I-28) turns an O(number of
historical contributing journals) cost, paid on every withdrawal, into an
O(number of NOT-yet-attested journals) cost — bounded by the attestation
interval, not by the account's lifetime.

**Enforced by**: `service.AttestationService.RunAttestBatch` /
`computeAuthVerdicts` / `verdictsForJournals` (verdict computation at
attestation time, batched via `postgres.AttestationStore.JournalAuthMaterial`
— one round trip for journal metadata, one for entries, regardless of how
many distinct journals are in the batch); `postgres.VerifiedBalanceStore.VerifiedBalance`
(the fast read path: partitions contributing entries by cached verdict,
falls back to the pre-T4 naive per-journal check — `verifyJournalsNaively`
— only for the `Unknown` set); `service.VerifyLedger`'s `isV3` branch (live
drift recompute of `AuthVerdictDigest`, and `core.AttestationRootHashV3`
self-consistency, alongside the existing v1/v2 checks — never gating on
whether a row happens to be v3, so a v1/v2 row's original semantics are
never touched, per `deployment.md`'s "an already-signed value cannot be
silently re-derived").

**Pinned by**:
- `postgres.TestVerifiedBalance_RefusesTamperedEntryAmount` — the headline
  pin, and the case the gate exists for: a journal is attested, both legs of
  an entry are then doubled directly via SQL so the journal still balances,
  and the cached verdict is asserted to still read `Authorized` (or the test
  is not exercising the gap at all). `VerifiedBalance` must refuse. Refusal
  rather than a corrected number, because a corrected number is still
  something a withdrawal could be paid against.
- `postgres.TestVerifiedBalance_CachedAuthorizedVerdictDoesNotSkipTheLiveCheck` —
  the same shape with a corrupted signature instead of a corrupted amount,
  and it carries its own falsification evidence: it verifies directly that a
  live re-check now fails before asserting `VerifiedBalance` refuses. This
  test previously asserted the opposite and was correct about the design of
  the time; the assertion is inverted, not deleted, so the history of what
  changed stays legible.
- `postgres.TestVerifiedBalance_CachedUnauthorizedVerdictIsUndefinedWithoutLiveVerifier` —
  a cached `Unauthorized` verdict rejects the balance even when the reading
  `VerifiedBalanceStore` has no `core.AuthVerifier` at all, proving the
  rejection came from the cache, not the separate nil-verifier bailout.
- `postgres.TestVerifiedBalance_UnattestedForgedTailJournalStillCaughtAlongsideCachedAuthorized` —
  a cached-`Authorized` verdict for one journal must never mask an
  unattested, unauthorized journal contributing to the same dimension.
- `service.TestAttestationService_ComputesAndCachesAuthorizedVerdict` —
  the write side: a genuinely-signed journal's entries are cached
  `Authorized`, and the resulting attestation row is v3 (non-empty
  `auth_verdict_digest`, signed under `core.AttestationRootHashV3`).
- `service.TestAttestationService_CachesUnauthorizedVerdictForForgedJournal` —
  a forged journal is cached `Unauthorized`, not silently coverage-only;
  P6's DELETE-detection coverage still happens regardless (orthogonal
  check).
- `service.TestAttestationService_NoVerifierConfiguredStaysV2` — T4 is
  additive/opt-in: an `AttestationService` with no `core.AuthVerifier`
  produces the exact v2 shape it would have before T4 existed.
- `service.TestVerifyLedger_DetectsAuthVerdictDrift` — the periodic-verify
  drift check: a journal's stored signature corrupted after attestation
  (leaving every `journal_entries` row and `batch_digest`/`merkle_root`
  untouched, isolating this specific check) surfaces as `TAMPERED` via an
  `auth_verdict_digest` mismatch finding.
- `core.TestAuthVerdictDigest_*` / `core.TestAttestationRootHashV3_*` —
  golden vectors (independently computed, cross-checked against this
  file's pre-existing v1 `CanonicalBatchDigest`/`AttestationRootHash`
  pins) and structural properties (length validation, domain separation
  from v2, sensitivity to a changed verdict).

## I-34: Deposit-review resolution requires a capability no Scope implies, and a persistently unreachable second source escalates instead of hanging forever

(`docs/plans/2026-08-21-integrity-hardening-contracts.md`, "Wave 3 契约层"
W3-A; `docs/bugs/2026-07-11-m3-security-review.md` mi2/mi5.)

**Rule** (two independent properties, one task):

1. `POST /deposits/{uid}/review/approve` and `/reject` are gated on
   `server.CapabilityDepositReview` — a privilege bit orthogonal to the
   `read < write < admin` Scope ladder. No Scope level, including
   `ScopeAdmin`, implies it; a key must be deliberately configured with it
   (`name:scope+deposit_review:secret` in `API_KEYS`, or
   `server.APIKey.Capabilities` when constructing a `server.Config`
   directly). The library does not decide that an ingester key and a
   reviewer key must be different keys — it only makes that separation
   possible and makes the default (no capability granted) the safe side.
2. `service.Onchain.reviewGate` tracks consecutive
   `core.DepositConfirmer.ConfirmDeposit` errors per booking and, once a
   token's `core.TokenConfig.ReconcileFailureLimit` is reached, routes the
   booking to `review` (reason `reconcile_unavailable`) instead of
   returning an error forever. Whenever the reconciliation gate is active
   for a token (`OnchainDeps.DepositConfirmer` configured and
   `ReconcileCeiling` positive), `ReconcileFailureLimit` must be a
   deliberate positive integer — `service.Onchain.Run` and
   `ValidateReconcileFailureLimits` refuse to start otherwise, mirroring
   I-26's sibling `AutoCreditCeiling` fence (MJ1) but for availability, not
   mint-safety, so there is no "explicitly unbounded" sentinel to accept.

**Why**: Before (1), `server/routes.go`'s `ScopeWrite` group contained
`POST /bookings`, `POST /bookings/{uid}/transition`, AND
`POST /deposits/{uid}/review/approve` together — a single ScopeWrite key
could forge an over-ceiling deposit sighting and then approve its own
review, making the M3 review gate (`docs/plans/2026-07-11-crypto-deposit-sweep-design.md`
§9.2) a check in name only: the one control that exists specifically to
require a second party never actually required one. Before (2), a
persistently unreachable second source left a legitimate deposit silently
stuck in `confirming` forever — safe (never auto-credited) but invisible,
exactly the "nothing happened and it's done are indistinguishable" failure
mode `~/.claude/rules/working-agreements.md` §3 names.

**Enforced by**: `server.Capability` / `server.CapabilityDepositReview` /
`server.requireCapability` (`server/middleware_auth.go`); the
`deposits/{uid}/review/*` route group in `server/routes.go`, deliberately
split out of the `ScopeWrite` group; `parseScopeAndCapabilities` (API_KEYS
`+`-joined capability parsing). `service.Onchain.reviewGate`'s
`recordReconcileFailure`/`clearReconcileFailure` and the
`reviewReasonReconcileUnavailable` branch; `core.TokenConfig.ReconcileFailureLimit`;
`service.Onchain.validateReconcileFailureLimits` /
`ValidateReconcileFailureLimits`, called from `Run()` and
`ledger.Service.EnableOnchain` (same two call sites as I-26's
`AutoCreditCeiling` fence, same rationale: a push-only/webhook-only
consumer that never calls `Run()` must not skip the check).

**Pinned by** (F-P34, 2026-09-02 audit: these four tests genuinely cover
both properties above, but were only cited under I-38's Pinned by list --
deleting this whole section previously left nothing that would go red,
because nothing here was actually checking that it was pinned):
- `server.TestCapabilityIndependentOfScope` — property 1's capability-vs-scope
  independence (also cited under I-38, where the same test additionally
  supports the openapi/write-scope contract).
- `server.TestDepositReview_SelfMintSelfApprove_MI2` — property 1's
  end-to-end claim: a `ScopeWrite`-only key cannot approve its own review
  (also cited under I-38).
- `service.TestOnchain_IngestDeposit_ReconcileError_EscalatesToReviewAfterFailureLimit` —
  property 2: consecutive reconcile failures past `ReconcileFailureLimit`
  route to `review`, not an infinite error loop.
- `service.TestOnchain_IngestDeposit_ReconcileError_FailsClosedStaysConfirming` —
  property 2's complement: below the limit, the booking stays `confirming`
  (fails closed) rather than either auto-crediting or escalating early.

## I-35: Partition maintenance never requires the serving credential to hold DDL or table ownership

(`docs/audits/2026-08-25-financial-engineering/threat-model.md`, "分区维护路径要求应用持有 owner 权限"; `docs/plans/2026-08-26-audit-remediation-contracts.md` §4 item 2.)

**Rule**: `postgres/partition_store.go` creates monthly partitions and
rebalances the default partition by calling two `SECURITY DEFINER`
functions — `ledger_create_monthly_partition` and
`ledger_rebalance_default_partition` — rather than issuing
`CREATE TABLE ... PARTITION OF` / `ALTER TABLE ... DETACH/ATTACH PARTITION` /
`TRUNCATE` directly. Both functions are owned by `ledger_owner` and run with
its privileges regardless of the caller, so `ledger_app`'s grant is `EXECUTE`
on exactly these two functions — nothing that looks like DDL.

⚠️ **That ownership sentence was false from migration 007 until 019/021**
(2026-09-02, D-M1). 001_baseline transfers ownership with a catalogue sweep at
the bottom of its own file, so nothing built by a later migration was ever
swept: measured on a clean install of 001–015, both of these functions came
back owned by the credential that ran the migration — a superuser in the
common install — and `ledger_app` holds `EXECUTE` on both. The privilege this
invariant says is `ledger_owner`'s was whatever the bootstrap credential had.
007's header argues the blast radius shrinks *because* these run as
`ledger_owner`; that premise did not hold in any deployment. Migration 019
extracts the sweep into a callable `ledger_resweep_ownership()`, 021 runs it,
and I-57 makes it a gate rather than a sentence.

**Why**: all four statements the old direct-DDL version issued are
owner-gated; `ledger_app` holds none of them (confirmed: `CREATE TABLE
... PARTITION OF` → `permission denied for schema public`, `DETACH
PARTITION` → `must be owner of table`, `TRUNCATE` → `permission denied`, run
against a real database before migration 007). The only way the previous
`PartitionStore` could ever have worked in a deployment that actually
enforces P1 role separation was a serving pool connected as `ledger_owner`
— and that pool's `TRUNCATE journal_entries_default` walks straight past
`journal_entries`' no-DELETE trigger: `TRUNCATE` does not fire row-level
triggers at all, confirmed by inserting two real, balanced journal entries
into the default partition, connecting as `ledger_owner`, and watching
`TRUNCATE` silently remove both with no trigger firing. `journal_entries`'
append-only guarantee (I-2) is only as strong as the weakest way to bypass
it, and giving the app pool ownership to make partitioning work was that
weakest way. `SECURITY DEFINER` closes both problems in one move: the
serving pool never needs elevation, and the only owner-privileged code path
reachable from `ledger_app` is one that always copies rows into their
permanent partition inside the same statement before truncating what is now
guaranteed to be an exact duplicate — the same move-then-truncate order the
Go code always used, now the only path available rather than a convention a
caller happens to follow.

**Enforced by**: `ledger_create_monthly_partition` / `ledger_rebalance_default_partition`
(`postgres/sql/migrations/007_role_hardening_and_partition_security_definer.up.sql`,
both `SECURITY DEFINER` with `SET search_path = public` to remove
schema-shadowing as a privilege-escalation vector); `GRANT EXECUTE ... TO
ledger_app` on both, and nothing else; `postgres/partition_store.go` calling
only these two functions, never raw DDL. `ledger_create_monthly_partition`
additionally validates its name argument against
`^journal_entries_y[0-9]{4}m[0-9]{2}$` before using it in `format(%I)`,
since `EXECUTE` on the function is itself a `ledger_app`-reachable
capability and must not accept an arbitrary identifier.
`ledger_rebalance_default_partition` validates its date range the same way and
for the same reason (migration 021, D-m1): month-aligned, non-inverted, and at
most 120 months. Migration 013 wrote that argument down about the sibling
function and did not carry it across, so until 021 a single call could create
three centuries of partitions — measured as `ledger_app`, a two-year range
produced 24 partition tables and 96 dependent relations that `ledger_app`
cannot drop, each call holding `ACCESS EXCLUSIVE` on `journal_entries`. The
cap is on the caller's arguments only; the widening the function then performs
to cover rows already sitting in the default partition stays unbounded, or a
lapsed horizon would be unrecoverable.

**Pinned by**:
- `postgres.TestPartitionFunctions_OwnedByLedgerOwner` — the assertion this
  invariant stated as fact and none of its pins checked. The three that
  existed verified that the functions can be called, that the name argument is
  validated, and that `search_path` includes `pg_temp`; "owned by whom" was
  the one property that mattered for the `SECURITY DEFINER` claim and the one
  nobody looked at.
- `postgres.TestLedgerRebalanceDefaultPartition_RejectsUnboundedRange` — runs
  the measured attack (a three-century range) plus the inverted and
  non-month-aligned cases as `ledger_app`, and requires the legitimate
  caller's own range shape to still succeed so the refusals are not vacuous.
- `postgres.TestLedgerAppInsertsIntoPartitionCreatedAfterGrant` — rewritten
  for this migration: it now uses a single `ledger_app` pool for both
  creating the partition and inserting into it (previously required a
  separate `ledger_owner` pool `PartitionStore` in production has no way to
  construct — a green result there proved nothing about what `ledgerd`
  could actually do).
- `postgres.TestPartitionMaintenanceRejectsUnshapedPartitionNames` — the
  name-shape validation, including a statement-terminator injection
  attempt.
- `postgres.TestLedgerAppIsLeastPrivilege` (unchanged) still pins that
  `ledger_app` cannot `TRUNCATE`/`ALTER TABLE`/`CREATE TABLE` directly —
  this invariant is about the one narrow path around that, not a
  relaxation of it.

---

## I-36: A read-only role's grant never exposes a write-path secret, and a legitimate change to a config table's guarded columns is never invisible

(`docs/audits/2026-08-25-financial-engineering/threat-model.md`, "`ledger_ro`
能读出站 webhook HMAC 密钥"; `docs/audits/.../TODO.md` §9 "共同盲区".)

**Rule** (two properties under one invariant — both are about what a
narrowly-scoped, otherwise-legitimate grant can quietly do):

1. `ledger_ro` holds a column-level `SELECT` on `webhook_subscribers`
   covering every column except `secret` — the HMAC key each outbound event
   delivery signs with (`service/delivery/webhook.go`). It has no
   table-level grant on `webhook_subscribers` at all; `SELECT *` and
   `SELECT secret` both fail.
2. `currencies`/`classifications`/`journal_types`/`entry_templates` (the
   config tables I-25 guards) get an `AFTER UPDATE` trigger that logs every
   *committed* change — old row, new row, `current_user`, timestamp — to
   `config_table_changes`. `reconcile_scan_cursors` (outside I-25's scope
   since it does not participate in balance computation, but still
   decides whether the checkpoint-drift detector scans anything) gets the
   same treatment via `reconcile_scan_cursor_changes`. Both audit tables are
   themselves append-only.

**Why**: (1) — a blanket `GRANT SELECT ON ALL TABLES IN SCHEMA public TO
ledger_ro` was written under the framing "broader than ideal, but only a
confidentiality concern" (design docs' non-goal 1). Reading
`webhook_subscribers.secret` is not that: it is the key `ledger_ro`'s own
holder uses to authenticate every event this ledger sends outbound, so a
read-only credential that can read it can forge signed deliveries to any
subscriber — an integrity capability smuggled through a confidentiality
grant, confirmed by connecting as `ledger_ro` and selecting `url, secret`
straight off the table before this migration.

(2) — I-25's guards (003's whitelist, 006's additions) stop illegitimate
writes to these tables, but a legitimate one the guard allows — an
`is_active` toggle, a `display_label` edit, the `''  -> available`
`balance_role` upgrade — previously left no record of who changed it or
when. "看不出改过" per the audit's §9: C2 (I-25) *prevents* the attack the
threat model describes; it does not make an attempt to try it, successful
or not, visible after the fact for the writes it does allow through.
`current_user` is always `ledger_app` in every deployment — that is
precisely the credential this whole guard system defends against — so this
does not attribute a change to a business actor; it answers "this row
changed from A to B at time T", which is what was missing. A rejected write
(the guard raises, the statement never commits) still leaves nothing behind
in `config_table_changes` — the `AFTER` trigger never runs on a rolled-back
transaction — which is a residual gap this invariant does not close, only
names: a rejected attack attempt and a normal day still look identical from
inside the database, only successful narrow mutations are now attributable.

**Enforced by**: `REVOKE SELECT ON public.webhook_subscribers FROM
ledger_ro` + column-level `GRANT SELECT (...) ... TO ledger_ro` naming every
column except `secret`
(`postgres/sql/migrations/007_role_hardening_and_partition_security_definer.up.sql`).
`ledger_log_config_table_change()` / `currencies_audit` /
`classifications_audit` / `journal_types_audit` / `entry_templates_audit`
(`WHEN (to_jsonb(OLD) IS DISTINCT FROM to_jsonb(NEW))` so a true no-op
UPDATE — e.g. `deposit_addresses`' idempotent re-registration upsert
pattern, were it applied to one of these tables — is not logged);
`ledger_log_reconcile_scan_cursor_change()` / `reconcile_scan_cursors_audit`
(same `WHEN` shape); `config_table_changes` and
`reconcile_scan_cursor_changes` both carry `ledger_block_mutation()` on
`UPDATE`/`DELETE`, so the audit trail cannot itself be edited or erased by
the same credential whose writes it records
(`postgres/sql/migrations/006_threat_model_guard_coverage.up.sql`).

**Pinned by**:
- `postgres.TestLedgerRoCannotReadWebhookSecret` — direct `secret` select,
  `SELECT *`, and every other column, all against a real `ledger_ro`
  connection.
- `postgres.TestConfigTableChangesAudited` — a legitimate `is_active`
  toggle produces exactly one audit row with the right before/after values
  and `changed_by = 'ledger_app'`; a rejected `exponent` change (003's
  guard) produces none; the audit table itself refuses `UPDATE`/`DELETE`.
- `postgres.TestReconcileScanCursorChangesAudited` — the exact §4-3 attack
  (parking the cursor at `INT64_MAX` with `lap_dirty = false` to fake a
  fully-scanned lap) is confirmed to still succeed at the DB layer (this
  invariant does not claim to block it — `SetScanCursor` legitimately writes
  arbitrary values, so a whitelist guard cannot distinguish the attack from
  a legitimate lap reset) while producing an audit row that shows exactly
  what changed; refusing to trust a zero-row scan as `Complete=true` is
  `service/reconcile.go`'s half of this seam, not this invariant's.
## I-37: Solvency liability counts only role-bearing user-side balances

(`docs/plans/2026-08-26-audit-remediation-contracts.md`, D-money;
`docs/audits/2026-08-25-financial-engineering/financial-correctness.md`
Major #1, "偿付能力把 user-side debit-normal 费用账当成负债".)

**Rule**: `SolvencyReport.Liability` (and `GetTotalLiabilityByAsset`) is the
sum of user-side (`account_holder > 0`) balances for classifications with a
**non-empty `balance_role`** (`available` / `pending` / `locked`) only — the
same basis `BalanceReader.GetBalanceBreakdown` uses for a holder's
spendable-money view (I-11). Role-less user-side classifications (e.g.
`fee_expense`, a debit-normal cost/memo account booked to the user's own
holder id purely for per-user fee reporting) are excluded.

**Why**: `fee_expense` is money the user already paid, not money the platform
owes back — it has no `balance_role` for exactly that reason (I-11: role-less
means "not part of the holder's spendable-money view"). Summing it into the
liability figure anyway turned every dollar of cumulative fee revenue into a
phantom dollar of insolvency: the standard preset flow (deposit 500 → lock
105 → withdraw_fee 5 → withdraw_confirm 100, leaving
`main_wallet=395, locked=0, fee_expense=5, custodial=395`) reported
`Liability=400` against `Custodial=395` — `Margin=-5, Solvent=false` — for a
platform that was, in fact, fully solvent (custodial covered every reservable
user balance exactly). Fee revenue is the platform's *income*, not a growing
hole in its books; a solvency check that manufactures a deficit proportional
to it is worse than no check, because it also **hides** a real deficit of the
same magnitude behind the same-looking number (`Margin=-5` from fees alone is
indistinguishable from `Margin=-5` from an actual unbacked issuance).

**Enforced by**:
- `postgres/sql/queries/platform_balances.sql`'s `GetTotalUserSideBalance`
  joins `classifications` and filters `c.balance_role <> ''` before summing.
- `postgres.PlatformBalanceStore.SolvencyCheck` /
  `GetTotalLiabilityByAsset` consume that query unchanged — the fix is
  entirely in the query's `active` CTE.

**Pinned by**:
- `postgres.TestSolvencyCheck_WithdrawFee_DoesNotManufactureDeficit` — the
  exact repro above: before the fix this test's own numbers show
  `Liability=400, Margin=-5, Solvent=false`; after, `Liability=395, Margin=0,
  Solvent=true`, and `GetTotalLiabilityByAsset` agrees with `SolvencyCheck`.

> **Addendum (M-4 fix, `.local/independent-review-2026-08-26.md`,
> docs/plans/2026-08-26-audit-remediation-contracts.md follow-on
> fix-backend-1 batch, board #43).** The rule above is correct GIVEN that
> every real liability classification is actually tagged with a
> `balance_role` — nothing in `ClassificationInput` enforced that. A
> classification created without one (the exact shape the independent review
> found: commit `6c83236`'s own message reported fixing three pre-existing
> test fixtures that built "liability" classifications with no
> `balance_role`) is silently excluded from `Liability` by the rule above,
> understating what the platform owes — the "false surplus" direction, the
> most dangerous one for a solvency check to be wrong in.
>
> Two structural changes close this, both required:
>
> 1. `core.ClassificationInput.Validate` now refuses `balance_role = ""` on
>    any NEW non-system classification. A new `BalanceRole` value,
>    `BalanceRoleMemo` (migration 011 widens the `balance_role` CHECK
>    constraint to allow it), lets a classification declare "deliberately
>    NOT a liability" explicitly instead of leaving the field blank —
>    `BalanceRoleNone` ("") no longer has to carry both that intent AND
>    "nobody tagged this yet" at once. `presets.DefaultTemplateClassifications`'
>    `fee_expense` now declares `BalanceRoleMemo` explicitly (previously
>    blank); `GetTotalUserSideBalance`'s filter changed from
>    `balance_role <> ''` to `balance_role NOT IN ('', 'memo')` so a
>    memo-tagged classification is excluded from `Liability` the same way an
>    untagged one always was (folding 'memo' into "any non-empty role means
>    liability" would have reproduced the exact phantom-insolvency bug this
>    invariant's main rule already fixed, via a different route).
> 2. A new reconcile check, `role_less_liability`, closes the visibility gap
>    for classifications that predate this rule or were written directly
>    (detection, not prevention — it does not change what
>    `SolvencyReport.Liability` counts): it flags any user-side, non-system
>    classification with a nonzero balance and `balance_role = ''`,
>    independent of the query `GetTotalUserSideBalance` itself uses.
>
> **Correction (same batch, after Team Lead review).** The first version of
> this addendum's reconcile check filtered `c.normal_side = 'credit'`,
> reasoning "liability-shaped by construction". That reasoning is false in
> THIS library's own convention: `main_wallet`, the canonical real
> liability, is DEBIT-normal (DR increases what the platform owes the
> holder) — a classification built by copying `main_wallet`'s shape without
> its `balance_role` is a role-less DEBIT-normal classification, exactly the
> shape the credit-only filter let through uncaught. Team Lead reproduced
> this end-to-end on the pre-correction code: a `main_wallet`-shaped
> classification with a real, nonzero balance and no `balance_role` produced
> `custodial=600 liability=500 margin=100 solvent=true` — identical to the
> unfixed behavior. `balance_role`, not `normal_side`, is the only signal
> that ever distinguished a real liability from a legitimate memo account;
> filtering on `normal_side` filtered on the wrong axis. The query no longer
> filters on `normal_side` at all — see change 1 above (`BalanceRoleMemo`)
> for how false positives on legitimate memo accounts are avoided instead.
>
> **Enforced by (addendum)**: `core.ClassificationInput.Validate`
> (`core/interfaces.go`); `core.BalanceRoleMemo`
> (`core/types.go`); `postgres/sql/migrations/011_balance_role_memo.up.sql`;
> `presets.DefaultTemplateClassifications`'s `fee_expense` entry
> (`presets/templates.go`); `postgres/sql/queries/platform_balances.sql`'s
> `GetTotalUserSideBalance`; `postgres/sql/queries/reconcile.sql`'s
> `ReconcileRoleLessLiabilities`; `service.FullReconciliationService.runCheckRoleLessLiability`
> (`service/reconcile.go`), wired into the check suite as
> `role_less_liability`.
>
> **Pinned by (addendum)**:
> `service.TestRoleLessLiability_Violation` /
> `TestRoleLessLiability_Clean` /
> `TestFullReconciliation_RoleLessLiability_ExplicitMemoIsNotFlagged`
> (an explicitly memo-tagged debit-normal classification must never be
> flagged) /
> `TestFullReconciliation_RoleLessLiability_UntaggedDebitNormalIsFlagged` —
> the correction's own pin: a role-less DEBIT-normal classification (the
> exact shape the credit-only filter missed) with a real balance is flagged,
> and `SolvencyCheck.Liability` is confirmed blind to it directly /
> `TestFullReconciliation_RoleLessLiability_DetectsMistaggedClassification`
> — the original DB-backed pin (credit-normal mistagged shape), still
> correctly caught by the corrected (broader) query /
> `postgres.TestInstallPresets_BalanceRoleUpgradeAndConflict` — `fee_expense`
> upgrades to `BalanceRoleMemo` on install, not `BalanceRoleNone` /
> `postgres.TestSolvencyCheck_WithdrawFee_DoesNotManufactureDeficit` — this
> invariant's original pin, re-verified against the corrected
> `GetTotalUserSideBalance` filter (a memo-tagged `fee_expense` must still be
> excluded from `Liability`, not just an untagged one).

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

## I-38: The HTTP wire contract is machine-checked against docs/openapi.yaml, and a write-scope key cannot post a journal touching a system classification through either journal endpoint

(`docs/audits/2026-08-25-financial-engineering/structure.md`'s two Majors;
`docs/plans/2026-08-26-audit-remediation-contracts.md` Wave 2, D-contract.)

**Rule** (two independent properties, one task):

1. Every cursor-paginated list response's `next_cursor` field is a JSON
   `null` when exhausted -- never an omitted key (`undefined` in JS) or an
   empty string, both of which used to be live, inconsistent spellings of
   "no more pages" across different handlers. `server.PagedResponse` carries
   `NextCursor` as `*string`; `cursorPtr("")` (exhausted) is `nil`, which
   `encoding/json` renders as `null`. Every `docs/openapi.yaml` schema with a
   `next_cursor` property types it to admit `"null"`
   (`type: [string, "null"]` in OpenAPI 3.1), and every requestBody /
   response schema **registered in `server/openapi_contract_test.go`'s
   `requestBodySchemaCases` / `responseEnvelopeCases` / `listEnvelopeCases`**
   agrees on field names with that handler's Go wire struct -- both
   mechanically, not by convention: a `go test ./server/...` run checks the
   openapi.yaml source against `server` package reflection on every push and
   PR (unlike the pre-existing `ledger-react.yml` gate, this one is not
   path-filtered and needs no running server).
   **Revision note (M-8, 2026-08-26 independent review, second pass):** the
   bolded qualifier above is the fix. The first revision's three registries
   were hand-maintained lists with nothing asserting they covered every
   schema `paths` actually references -- a new endpoint with a new named
   schema, or an existing endpoint whose schema silently drifted from the
   registries above, passed with zero signal, exactly the class of gap this
   guarantee exists to close. Two completeness tests
   (`TestOpenAPIContract_EveryRequestBodySchemaIsRegistered` /
   `TestOpenAPIContract_EveryResponseEnvelopeSchemaIsRegistered`) now
   enumerate every schema `docs/openapi.yaml`'s `paths` references (requestBody,
   and 2xx responses respectively) and `Fatalf` on anything not present in
   one of the three registries -- mirroring
   `postgres/grant_coverage_test.go`'s "a new table defaults to nothing, not
   to full access" shape. Turning these two tests on immediately surfaced
   three real, pre-existing drifts the un-gated registries had missed (fixed
   in the same pass, not left for a future one): `GET
   /balances/{holder}/{currency}` was documented as `BalancesEnvelope`
   (`{list, next_cursor}` of `Balance`) but its handler
   (`handleGetBalanceByCurrency`) actually returns `{total, classifications}`
   -- now its own `BalanceByCurrencyEnvelope` schema;
   `JournalWithEntriesEnvelope` documented `data: {journal: {...}, entries:
   [...]}` but `handleGetJournal` returns a flat `journalResponse` with
   `entries` as a sibling field, not nested under a `journal` key -- fixed to
   `allOf: [Journal, {entries: [...]}]`; `POST /reconcile` and `POST
   /reconcile/full` referenced the bare `ReconcileResult` / `ReconcileReport`
   schemas directly, with no `Envelope` wrapping at all, even though both
   handlers go through `httpx.OK` like every other endpoint and therefore do
   envelope their response at runtime -- now `ReconcileEnvelope` /
   `ReconcileReportEnvelope`. `POST /reconcile/account` was also missing a
   response schema entirely (fixed to `ReconcileEnvelope`, matching its
   handler). None of the three represent a behavior change -- the Go
   handlers were already correct; only the documentation the completeness
   gate exists to keep honest was wrong.
2. `POST /journals/template` refuses (403), no matter what scope the
   caller's API key holds, any `template_code` whose template has a leg on a
   classification flagged `is_system` -- derived from the template's own rows
   at request time, so it holds for a template this library ships and for one
   a deployment defined itself, and a template that cannot be resolved fails
   closed (the error propagates; `ExecuteTemplate` is not reached). On top of
   that structural rule it also refuses any code in the effective protected
   name set: `presets.ProtectedTemplateCodes()` (the five codes named below)
   PLUS `server.Config.ProtectedTemplateCodes` (a deployment's own additional
   system-only codes). `Config.AllowGenericTemplatePost` is the single,
   per-code, explicit opt-out from BOTH layers. A deployment does not need to
   enumerate anything to get this protection.
   The same two-layer check applies to `POST /journals/deposit-tolerance`,
   which takes no `template_code` but turns caller-supplied expected/actual
   amounts into executions of those very codes: every step the plan would
   execute passes the same gate, and the route sits in the admin group rather
   than the write group (contract §7.11). Under default configuration that
   endpoint therefore answers 403; a plan with no steps executes nothing and
   is unaffected.
   **Revision note (M-2, 2026-08-26 independent review, second pass):** the
   first revision of this guarantee left `ProtectedTemplateCodes` empty by
   default, on the theory that "this library does not know which of a
   deployment's own template codes are meant to be system-only." That is
   true of a deployment's OWN codes, but not of the library's own
   deposit-confirmation codes, which it ships and therefore already knows
   are dangerous to expose here -- so every deployment that installed a
   deposit preset and did not separately remember to configure this field
   stayed exposed to the exact finding this guarantee exists to close. The
   default now includes the library's own codes; `AllowGenericTemplatePost`
   is the new opt-out for a deployment with a reviewed reason to post one of
   them through this endpoint anyway.
   **Revision note (D-C1, 2026-09-02 deep audit, Critical):** the name list
   was still the whole of this endpoint's guard, and `dev_credit` -- the one
   template in this library whose doc comment says it mints spendable holder
   balance with nothing behind it -- was never on it. A `write`-scope key
   could post it in any ENV, with `POST /dev/credits` correctly answering
   `FeatureNotEnabled` the whole time, and the same held for every other
   preset touching a system account (`capital_injection`, `fee_charge`,
   `checkout_settlement_*`, `fx_*`, `transfer_*`, ...) plus every
   deployment-defined one. The two endpoints' defenses had different shapes:
   (3) below was already structural, this one was a hand-kept list of four.
   The structural rule above is now the primary guard and the list is
   belt-and-braces (it answers before the template table is read, so it also
   covers a code whose rows do not exist yet); `dev_credit` was added to it
   as well. This tightening is a wire-behavior break -- see the 2026-09-02
   audit TODO's breaking-change list for the codes that changed from 201 to
   403 and what a deployment does about it.
   **Revision note (§7.11, 2026-09-02, found while sibling-scanning D-C1):**
   closing the named-template endpoint left a second spelling of the same
   mint wide open. `POST /journals/deposit-tolerance` sat in the `write`
   scope group and executed `deposit_confirm_pending` / `deposit_confirm` /
   `deposit_release_pending` / `deposit_record_overage` from
   `expected_amount` / `actual_amount` the caller supplies -- so
   `expected == actual == 1000000` posted a full deposit confirmation for a
   million, by a plain write-scope key, while the identical code was refused
   by name one route above. Every planned step now goes through the same
   `refuseProtectedTemplate`, and the route moved to the admin group next to
   `POST /dev/credits`. This is the guarantee's real shape: the rule is about
   what gets executed, not about which field named it.

3. `POST /journals` (the handwritten-entry endpoint) refuses, by default, any
   entry that touches a classification flagged `is_system` (custodial,
   suspense, equity, …). This is the handwritten-path counterpart to (2),
   added in the 2026-08-29 review (M-2): without it a `write`-scope key could
   reproduce a deposit confirmation's exact double entry
   (`DR main_wallet(user) / CR custodial(system)`) by hand and mint spendable
   user balance, straight past the template blacklist. The guard reads the
   live `is_system` flag from the classification store (not a hand-maintained
   list), so a new system classification is covered the moment it exists. The
   library's own system-side flows (deposit/transfer/fx/capital) go through
   templates or server-side orchestration, not this endpoint, so the default
   does not constrain them. `Config.AllowSystemClassificationPost` opts a
   deployment out after a reviewed decision (and logs a startup warning when
   set), for the case where an operator legitimately hand-posts system-side
   journals over HTTP.

   Together (2) and (3) mean a leaked or over-scoped `write`-scope key can no
   longer post accounting that touches a system classification -- deposit-shaped
   or otherwise -- through *either* the template or the handwritten endpoint
   under default configuration — the guarantee this
   invariant claims. Enforced by `handlePostJournal`'s
   `rejectSystemClassificationEntries` (`server/handler_journals.go`), pinned by
   `TestPostJournal_RejectsSystemClassificationByDefault`.

**Why**: Before (1), a client following `docs/openapi.yaml` literally (e.g.
checking `data.next_cursor === null` to know when to stop paging, or sending
the documented `expires_in` / `holders` / omitted `source` /
`effective_at` fields) would get silently wrong behavior with no test or CI
signal, because nothing ever compared the spec to a running server or the Go
source -- `ledger-react.yml`'s `codegen:check` only compares the generated
TS to the same YAML file, a self-consistency check that cannot detect the
YAML itself being wrong. Before (2), `presets.DepositConfirmTemplateCode`
and its siblings (`deposit_confirm_pending`, `deposit_release_pending`,
`deposit_record_overage`) were reachable by name through the same endpoint
any other template goes through, with no gate at all -- a leaked or
over-scoped write-scope key could mint accounting indistinguishable from a
verified on-chain deposit. The empty-by-default revision closed that only
for a deployment that remembered to opt in; the current revision closes it
by default.

**Enforced by**: `server.PagedResponse` / `cursorPtr` (`server/response.go`);
`refuseProtectedTemplate` (`server/handler_journals.go`) -- the single
function both `handlePostTemplate` and `handlePostDepositTolerance` call,
combining `rejectSystemClassificationTemplate` (which shares
`systemClassificationUIDs` with the handwritten path's
`rejectSystemClassificationEntries`) with
`presets.ProtectedTemplateCodes()` (`presets/protected_templates.go`) merged
with `server.Config.ProtectedTemplateCodes` and reduced by
`Config.AllowGenericTemplatePost` into `server.Server.protectedTemplateCodes`
(`server/server.go`), both checked in `handlePostTemplate`
(`server/handler_journals.go`); `server/openapi_contract_test.go`'s
`TestOpenAPIContract_RequestBodiesMatchGoStructs` /
`TestOpenAPIContract_ResponseEnvelopesMatchGoStructs` /
`TestOpenAPIContract_ListEnvelopeItemsMatchGoStructs` /
`TestOpenAPIContract_NextCursorIsNullable` /
`TestOpenAPIContract_EveryRequestBodySchemaIsRegistered` /
`TestOpenAPIContract_EveryResponseEnvelopeSchemaIsRegistered`, run
unconditionally by `ci.yml`'s `test` job; `.github/workflows/ledger-react-publish.yml`'s
`verify` job now also runs `codegen:check` before publishing (previously only
`ledger-react.yml` did, and only on PRs touching `web/**` or `docs/openapi.yaml`).

**Pinned by**:
- `server.TestDepositReview_SelfMintSelfApprove_MI2` — the end-to-end mi2
  exploit chain with one ScopeWrite-only key: create a booking (`POST
  /bookings`) and drive it to `review` (`POST /bookings/{uid}/transition`)
  both succeed (unaffected by this fix), but approving it with the SAME key
  is rejected with 403; a second sub-test pins that `ScopeAdmin` does not
  imply the capability either; a third pins that a key holding only the
  capability (no `ScopeWrite` at all) can still approve.
- `server.TestParseAPIKeys` — its capability subtests cover the `API_KEYS`
  wire-format parsing for the new `+capability` suffix (e.g.
  `write+deposit_review`), an unknown capability being rejected, and the
  safe default: no suffix grants no capability, even for `admin`.
- `server.TestCapabilityIndependentOfScope` — `Capability.has` behaves as a
  bitmask independent of any `Scope` value.
- `service.TestOnchain_IngestDeposit_ReconcileError_EscalatesToReviewAfterFailureLimit` —
  failures below `ReconcileFailureLimit` behave exactly as I-21/mi4 already
  pin (fail closed, stays `confirming`, no error path result changed); the
  failure that reaches the limit escalates to `review`
  (`reconcile_unavailable`) instead, still with no journal.
- `service.TestOnchain_IngestDeposit_ReconcileError_FailsClosedStaysConfirming` —
  unchanged (mi4): re-run after this fix to confirm a single failure with
  `ReconcileFailureLimit` left at its test-harness zero still fails closed
  rather than escalating immediately (the zero-limit-means-unconfigured
  guard in `recordReconcileFailure`).
- `service.TestOnchain_Run_RejectsUnconfiguredReconcileFailureLimit` /
  `TestOnchain_Run_AllowsReconcileGateDisabled` — the startup fence: active
  reconciliation gate with no `ReconcileFailureLimit` refuses to start;
  reconciliation gate not activated at all is unaffected.
- `server.TestPostTemplate_ProtectsEveryInstalledTemplateWithASystemLeg` —
  D-C1's own pin, and the structural one: it installs every preset bundle
  this library ships (including `DevCreditBundle`, the one
  `InstallExtendedPresets` deliberately excludes) into in-memory config
  stores, enumerates the resulting template table, and requires a 403 on
  every template with an `is_system` leg. The verdict comes from the
  installed rows, never from the guard's own idea of what is dangerous —
  which is why the earlier pin could not notice `dev_credit` was missing
  (D-m9). Verified red 2026-09-02: 15 of the 19 system-leg templates
  answered 201 with `ExecuteTemplate` reached.
- `server.TestPostTemplate_AllowsInstalledTemplatesWithoutASystemLeg` — the
  control for the above: installed templates whose legs are all holder-side
  (`lock_funds`, `unlock_funds`) still execute, so the rule is not a blanket
  deny of the endpoint.
- `server.TestPostTemplate_RefusesTheAuditedMintingCodes` — the three codes
  the 2026-09-02 audit's own httptest reproduction posted successfully,
  named as literals: `dev_credit`, `capital_injection`, `fee_charge`.
- `server.TestPostTemplate_HardcodedListStandsWithoutTheTemplateTable` /
  `TestPostTemplate_UnknownTemplateCodeNeverReachesExecuteTemplate` — the two
  layers are independent and the order is load-bearing: a protected code is
  refused even when the template table answers `ErrNotFound` for everything,
  and an unknown code fails closed at the guard rather than reaching
  `ExecuteTemplate`.
- `server.TestPostTemplate_DefaultProtectsDepositCodes` — M-2's own pin:
  an unconfigured `Config.ProtectedTemplateCodes` still refuses every code in
  the library's hardcoded set; verified red before that revision (all four
  posted 201, not 403). Written as literals rather than ranged over
  `presets.ProtectedTemplateCodes()` (D-m9): a table driven by the
  implementation's own return value cannot fail because a dangerous code is
  absent from that return value.
- `server.TestPostTemplate_DefaultDoesNotProtectUnrelatedCodes` — the
  name-list layer is not a blanket deny: a code outside the library's own set
  and outside `Config.ProtectedTemplateCodes` is not refused by it.
- `server.TestPostTemplate_AllowGenericTemplatePostIsTheOnlyWayPastTheSystemLegRule`
  — the structural rule's single opt-out, per-code and explicit.
- `server.TestPostDepositTolerance_RefusesProtectedTemplatesByDefault` /
  `TestPostDepositTolerance_RequiresAdminScope` — §7.11's own pins: all five
  outcomes that would execute a step are refused under default config
  (verified red 2026-09-02: all five posted 201 with `ExecuteTemplate`
  reached), and a `write`-scope key is refused by the route's scope even with
  the step codes opted in (verified red before the route moved).
- `server.TestPostDepositTolerance_PlanWithNoStepsIsUnaffected` /
  `TestPostDepositTolerance_AllowGenericTemplatePostOptsItBackIn` — the
  controls: the gate refuses executions, not the endpoint, and the same
  single escape hatch opts a deployment back in.
- `server.TestReverseJournal_RejectsClientSuppliedIdempotencyKey` /
  `TestReverseJournal_RejectsIdempotencyKeyHeader` /
  `TestReverseJournal_WithoutIdempotencyKeyPostsTheReversal` — H-M3's Go
  half: `POST /journals/{uid}/reverse` derives its idempotency key
  server-side, so a caller-supplied one (body field, or lifted from the
  `Idempotency-Key` header) is now refused (400) instead of parsed away in
  silence.
- `server.TestPostTemplate_AllowGenericTemplatePostOptsCodeBackIn` /
  `TestPostTemplate_AllowGenericTemplatePostIsScopedToOneCode` — the escape
  hatch opts a specific default-protected code back in without opening the
  others, answering whether defaulting protection on could brick a
  deployment with a reviewed reason to post one of these codes generically.
- `server.TestOpenAPIContract_EveryRequestBodySchemaIsRegistered` /
  `TestOpenAPIContract_EveryResponseEnvelopeSchemaIsRegistered` — M-8's own
  pins: verified red by construction (delete any single entry from
  `requestBodySchemaCases` / `responseEnvelopeCases` / `listEnvelopeCases`
  and either test fails, naming exactly the schema that was removed);
  restoring the entry passes again. Registering `BalanceByCurrencyEnvelope`,
  `JournalWithEntriesEnvelope`, `ReconcileEnvelope`, and
  `ReconcileReportEnvelope` to make these two tests pass is what surfaced
  the three real drifts the revision note above describes.
## I-39: Advisory-lock coordination is structurally safe, not conventionally safe

(`docs/audits/2026-08-25-financial-engineering/concurrency.md`, board #30/#24,
D-lock.)

**Rule** (six independent guarantees under one number — see this task's bus
checkpoint for why they were not split further):

1. **Disjoint lock namespaces.** `AcquireBalanceLock` and
   `AcquireIdempotencyLock` (`postgres/sql/queries/journals.sql`) hash a
   literal per-query string prefix concatenated onto the caller's key
   (`'bal:' || key` / `'idem:' || key`) through
   `hashtextextended(text, 0)`, then take a single-key
   `pg_advisory_xact_lock(bigint)` on the full 64-bit result. A caller
   fully controls `idempotency_key` end to end (`server/handler_journals.go`
   accepts it with no format restriction) and could otherwise pick a string
   like `"balance:1:1"` to alias a real balance-lock key, constructing an
   ABBA deadlock between two ordinary `POST /journals` calls with no
   malicious intent required on the amounts or accounts themselves; because
   the two prefixes differ in their first byte, the set of strings each
   query can hash is disjoint by construction, so no caller-supplied key can
   land the two namespaces on the same lock. This is structurally
   impossible, not merely discouraged by convention.
   **Revision note (M-6, 2026-08-26 independent review, second pass):** an
   intermediate revision of this guarantee used PostgreSQL's two-key form,
   `pg_advisory_xact_lock(int4, int4)`, with fixed namespace literals (`1`
   for balance, `2` for idempotency) as the first argument, relying on the
   two-key API being a genuinely separate lock space from the single-key API
   regardless of the values passed (a 4th field in PostgreSQL's internal
   `LOCKTAG` distinguishes them). That closed the ABBA above, but it also
   narrowed the *hashed* value from `hashtextextended`'s 64 bits to
   `hashtext`'s 32 bits — and a 32-bit collision between two DIFFERENT
   `(holder, currency_id)` pairs is reachable at realistic account
   cardinalities (found by an offline birthday-attack search in under a
   second; see `postgres.TestAcquireBalanceLocks_HashCollisionCrossBatchDeadlock_Fixed`).
   Guarantee 2 below (whole-batch lock order) sorts each transaction's OWN
   pairs by `(holder, currency_id)`, not by the hash the lock is actually
   taken on, so it does not prevent two transactions with disjoint holder
   sets from interleaving into a real ABBA when their pairs alias the same
   32-bit hash — reintroducing the exact deadlock shape this guarantee
   exists to close. The string-prefix approach above restores the full
   64-bit range while keeping the same "disjoint by construction" property
   the two-key form had, without depending on PostgreSQL's internal
   two-key/single-key LOCKTAG distinction.
   **Residual, accepted risk**: this single-key form shares its 64-bit
   space with every other single-key `pg_advisory_lock`/
   `pg_try_advisory_lock` caller in the codebase — currently
   `service.advisoryLockKey` (an FNV-64a hash of a small fixed set of job
   names, used by `LockedJob` and `SnapshotService`), which the two-key form
   was also disjoint from. A job-name hash landing on the same 64-bit value
   as a live balance/idempotency key is not attacker-influenceable (job
   names are fixed constants, not caller input), and every such caller uses
   the non-blocking `pg_try_advisory_lock`, so the only possible effect is a
   single skipped/delayed lock-wait, never a deadlock (a non-blocking
   acquire cannot participate in a wait-for cycle) and never an incorrect
   result.
2. **Whole-batch lock order.** `ExecuteTemplateBatch`'s pool-mode branch
   (`postgres/ledger_store.go`) unions every journal's `(holder,
   currency_id)` pairs across the WHOLE batch, sorts once
   (`sortedUniquePairs`), and acquires that union before posting any
   journal in the batch — instead of letting each journal's
   `postJournalWithQueries` call `acquireBalanceLocks` independently
   (correct within one journal, uncoordinated across the N journals one
   batch call posts in a single transaction). Two ordinary batches whose
   journals list the same two holders in opposite order (e.g. two batch
   settlements) no longer deadlock.
3. **No external call from a transaction-bound store.**
   `ReserverStore.Reserve`'s `RequireVerifiedBalance` gate
   (`postgres/reserver_store.go`) may invoke a `core.AuthVerifier`, which
   `core.AuthVerifier`'s own doc comment permits to be a remote call —
   `financial.md`'s "no external calls inside a DB transaction" applies to
   it exactly as it does to `Attestor.Sign`. The gate now refuses
   (`core.ErrInvalidInput`) when called on a transaction-bound store
   (`WithDB`, reachable from inside `ledger.Service.RunInTx`'s callback),
   mirroring `LedgerStore.Authorize`'s identical guard — before this fix it
   copied Authorize's placement comment but not its guard, and would
   silently run the gate (and any remote call it makes) from inside the
   caller's open transaction.
4. **Every background job is leader-elected.** The `expiration` job
   (`service/worker.go`) is now wrapped in `service.NewLockedJob`, like its
   five siblings (`reconcile`, `system_rollup`, `full_reconcile`,
   `partition`, `attestation`). It was previously the one job that ran
   unconditionally on every replica: `K` replicas racing
   `GetExpiredReservations`/`ListExpiredBookings` on the same tick all read
   the same expired batch and each call `Release`/`Transition` on it — the
   row lock inside those calls serializes the writes so nothing corrupts,
   but `K-1` of every `K` calls fail with `ErrInvalidTransition` and get
   logged as errors, drowning out genuine failures in noise.
5. **A permanently-excluded rollup item cannot block its own remedy
   forever.** `CheckpointIntegrityStore.RebuildCheckpoint`'s precondition
   query (`CountPendingRollupForDimension`,
   `postgres/sql/queries/integrity_checkpoint.sql`) now excludes
   `rollup_queue` rows with `failed_attempts >= 10` — the same threshold
   `DequeueRollupBatch` (`checkpoints.sql`) uses to permanently stop
   retrying an item. An item past that threshold can never be dequeued and
   processed again, so it can never race a rebuild the way an in-flight
   item could; before this fix its `processed_at` stayed `NULL` forever, so
   `RebuildCheckpoint` refused with `core.ErrRollupPending` indefinitely —
   blocked by the exact class of failure (a repeatedly-failing rollup) it
   exists to repair.
6. **Registration-rescan progress has a claim-token guard, like its two
   siblings.** `AdvanceRegistrationRescan` and `RetryRegistrationRescan`
   (`postgres/registration_rescan_store.go`) now only apply when the row's
   `attempts` still equals the `expectedAttempts` the caller observed when
   it claimed the job (`core.RegistrationRescan.Attempts`, bumped by
   `ClaimRegistrationRescans` on every claim including a re-claim after an
   expired lease) — the same claim-token-guard shape `rollup_queue`'s
   `MarkRollupProcessed` and `events`' `UpdateEventDelivered` already use,
   keyed on `attempts` here rather than a lease timestamp since `Attempts`
   was already threaded through `core.RegistrationRescan` (no new column).
   `registration_rescans` was previously the one claim mechanism among the
   three (`rollup_queue`, `events`, `registration_rescans`) with no guard at
   all: a worker whose lease outlived its own processing (e.g. a slow RPC)
   could write progress after another worker had already re-claimed and
   possibly re-advanced the same row, clearing the second worker's live
   claim (`claimed_until = NULL`) out from under it.
   **Verified, not assumed** (concurrency.md marked "能否造成漏扫" PLAUSIBLE):
   tracing every write path found no way for a missing guard to cause a
   **skipped** block range — `RetryRegistrationRescan` never writes
   `next_block` at all, and `AdvanceRegistrationRescan` always derives its
   written `next_block` from a window that was actually fetched and
   ingested, so the worst pre-fix outcome is a **regression** (a stale
   worker's late write can move `next_block` backward, or re-open a
   `completed` row to `pending`), causing a redundant rescan of an
   already-covered range — safe because `IngestDeposit`'s idempotency key
   (`deposit-{chain_id}-{tx_hash}-{txlog_seq}`) makes re-ingestion a no-op,
   not a duplicate credit. The guard is fixed here regardless (the missing
   mechanism itself was already CONFIRMED, independent of this severity
   finding): it still eliminates real waste (redundant multi-block rescans)
   and a real correctness smell (a live claim being silently cleared by a
   worker that no longer owns it), even though it does not close a
   fund-safety gap the way (1)-(3) above do.

**Why**: advisory locks are the library's only cross-transaction
coordination primitive on the money path (I-4, I-5's load-bearing
prerequisite). A shared key space or an uncoordinated multi-resource
acquisition turns that primitive into a source of deadlocks and
availability incidents (SQLSTATE 40P01 / stuck workers) that scale with
caller behavior no one has to intend maliciously — see (1) and (2) above.
Guarantee (3) is `financial.md`'s core red line applied to a gate that
copied its sibling's comment without its enforcement. Guarantees (4) and
(5) are instances of `working-agreements.md` §5 ("能被结构强制的规则不应该靠
人记忆/约定"): a job that "should" be leader-elected because its five
siblings are is not actually leader-elected until it is wrapped the same
way, and a remedy that "should" always be available is not actually always
available until its own precondition excludes the failure state it exists
to fix.

**Enforced by**:
- `postgres/sql/queries/journals.sql`'s `AcquireBalanceLock`
  (`hashtextextended('bal:' || key, 0)`) and `AcquireIdempotencyLock`
  (`hashtextextended('idem:' || key, 0)`), both single-key
  `pg_advisory_xact_lock(bigint)`.
- `postgres/ledger_store.go`'s `sortedUniquePairs` (shared dedupe+sort,
  extracted from `balancePairsFromEntries`) and `ExecuteTemplateBatch`'s
  pool-mode pre-lock step, before its per-journal posting loop.
- `postgres/reserver_store.go`'s `s.pool == nil` guard inside `Reserve`'s
  `RequireVerifiedBalance` branch.
- `service/worker.go`'s `expirationJob := service.NewLockedJob("expiration",
  ...)`, mirroring `reconcileJob` / `sysRollupJob` / `fullReconcileJob` /
  `partitionJob` / `attestJob`.
- `postgres/sql/queries/integrity_checkpoint.sql`'s
  `CountPendingRollupForDimension`, `AND failed_attempts < 10`.
- `postgres/registration_rescan_store.go`'s `AdvanceRegistrationRescan` /
  `RetryRegistrationRescan`, both `WHERE uid = $1::uuid AND attempts = $N`;
  `core.RegistrationRescanStore`'s interface doc comment
  (`core/onchain.go`); the 3 call sites in `service/onchain.go` that thread
  `job.Attempts` through as `expectedAttempts`.
- `postgres/errors.go`'s `normalizeStoreError`, SQLSTATE `40001`
  (`serialization_failure`) / `40P01` (`deadlock_detected`) wrapped into
  `core.ErrTransient` (bus #24) — called from `acquireBalanceLocks` and
  `acquireIdempotencyLock` themselves (not only the generic
  `wrapStoreError` call sites), so a real deadlock surfacing at the
  advisory-lock statement itself — exactly where guarantees (1) and (2)
  above are enforced — is classified correctly rather than falling through
  to `core.IsRetryable`'s `default: true` catch-all, indistinguishable from
  a permanent bug.

**Pinned by**:
- `postgres.TestAcquireIdempotencyLock_NeverCollidesWithBalanceLock` —
  drives a real two-transaction deadlock scenario that a shared namespace
  (either the very first revision, which hashed the caller's key directly
  with no prefix, or a hypothetical un-prefixed single-key form) reproduces
  and the `'bal:'`/`'idem:'` prefix separation eliminates outright.
- `postgres.TestAcquireBalanceLocks_HashCollisionCrossBatchDeadlock_Fixed` —
  M-6's own pin: two real transactions, sorted and locked through the exact
  `sortedUniquePairs`+`acquireBalanceLocks` primitives `ExecuteTemplateBatch`
  uses, over four `(holder, currency_id)` pairs that are real `hashtext()`
  32-bit collisions (found by an offline birthday-attack search, verified
  against the live DB at test time rather than assumed stable). Verified red
  against the intermediate 32-bit two-key revision (a real SQLSTATE 40P01,
  confirmed by running this test before applying the 64-bit fix); green
  against the current `hashtextextended` revision.
- `postgres.TestExecuteTemplateBatch_GlobalLockOrder_PreventsCrossJournalDeadlock` —
  two real transactions modeling "two batches whose journals touch the same
  two holders in reverse order," using the exact
  `sortedUniquePairs`+`acquireBalanceLocks` primitives `ExecuteTemplateBatch`
  now calls.
- `postgres.TestAcquireBalanceLocks_RealDeadlock_WrapsErrTransient` — the
  per-journal-only pattern's real ABBA deadlock (the shape guarantee (2)
  eliminates for `ExecuteTemplateBatch` specifically), doubling as bus #24's
  real-trigger pin: asserts the losing side's error satisfies
  `errors.Is(err, core.ErrTransient)`, not merely `core.IsRetryable`'s
  default.
- `postgres.TestReserve_RequireVerifiedBalance_RejectsOnTransactionBoundStore` —
  contrasts pool mode (gate runs, returns `core.ErrUnauthorizedJournal` with
  no `AuthVerifier` configured) against tx mode (guard refuses with
  `core.ErrInvalidInput` before the gate runs at all); verified red before
  the fix (reverting the guard makes tx mode return
  `ErrUnauthorizedJournal` too, proving it reached the gate from inside the
  open transaction).
- `service.TestWorker_Expiration_SkipsWhenAnotherReplicaHoldsTheLock` /
  `TestWorker_Expiration_RunsWhenLockIsFree` — holds the real
  `job:expiration` advisory lock on a separate session and asserts the
  finder is never called while held, and is called once the lock is free;
  verified red before the fix (14 calls leaked through while the lock was
  held).
- `postgres.TestCheckpointIntegrity_RebuildCheckpoint_NotBlockedByPermanentlyFailedRollupItem` —
  poisons a checkpoint, exhausts a `rollup_queue` item's `failed_attempts`
  to 10, confirms the item is genuinely unreachable via
  `DequeueRollupBatch`, and asserts `RebuildCheckpoint` still succeeds;
  verified red before the fix (`core.ErrRollupPending` forever).
- `postgres.TestNormalizeStoreError` — table-driven unit coverage for the
  `40001`/`40P01` → `core.ErrTransient` classification itself.
- `postgres.TestRegistrationRescanStore_AdvanceRejectsStaleClaim` /
  `TestRegistrationRescanStore_RetryRejectsStaleClaim` — construct the real
  timing sequence (worker A claims with a short lease, the lease genuinely
  expires, worker B re-claims and bumps `attempts`, A then tries to write
  using its stale `attempts`) rather than asserting on the guard's SQL in
  isolation; assert both that A's write is rejected and that B's live claim
  and the row's real `next_block` are untouched by it. Verified red before
  the fix by temporarily dropping `AND attempts = $N` from each query: A's
  write silently succeeds instead of erroring.

**Known residual gap** (documented, not silently left): `ExecuteTemplateBatch`'s
tx-mode counterpart, `executeTemplateBatchWithQueries`, has the identical
per-journal-only lock-order defect guarantee (2) above fixes in pool mode —
concurrency.md notes both branches explicitly. That function's lines sit
inside the tx-mode `AuthStatusUnsignedTxMode` region assigned to a different
task in this wave (board #30/#31's file-ownership seam,
`docs/plans/2026-08-26-audit-remediation-contracts.md` §8), so it is left
unfixed here rather than crossing that boundary; flagged to Team Lead for a
follow-up applying the same `sortedUniquePairs` pre-lock pattern there.

## I-40: A transaction-bound clone never silently drops or escapes what the top-level Service promised

(2026-08-25 audit, consumer-surface territory: `Worker.SetAttestor` had zero
production call sites; §3/§5 facade findings on `RunInTx`.)

**Rule** (four independent properties, one task):

1. `(*ledger.Service).Worker` wires the P6 batch attestation job
   (`Worker.SetAttestor`) automatically whenever the Service was constructed
   `WithAttestor` — a consumer no longer has to remember a second, separate
   call. `AttestInterval`/`AttestBatchSize` fill from
   `service.DefaultWorkerConfig` on a zero-valued `WorkerConfig`, same as
   every other job's interval/batch-size field.
2. `PendingStore.ConfirmPending`/`CancelPending` sign their journal in pool
   mode whenever an Attestor is configured, exactly like every other
   pool-mode write path (`PostJournal`, `ExecuteTemplateBatch`,
   `ReverseJournal`). Before this fix they always posted
   `core.AuthStatusUnsignedTxMode`, regardless of pool/tx mode, because their
   own transaction wrapping made the inner `LedgerStore` look tx-bound.
3. Calling `RunInTx` (or `RunInTxWithOptions`) again on the `*Service` handed
   to an outer `RunInTx` callback returns an error instead of silently
   opening a second, pool-sourced, independent transaction.
4. `AttestationService`, `VerifyLedger`, and `EnableOnchain` are refused (an
   error, or `VerifyStatusNotRun` for `VerifyLedger`, which returns a value
   not an error) when called on the transaction-bound clone `RunInTx` hands
   to its callback — each of them either reads/writes through the pool
   directly (bypassing the caller's transaction) or, for `EnableOnchain`,
   would set state on a short-lived clone `RunInTx` discards when the
   callback returns while reporting success.

**Why**: (1)+(2) are the same failure shape C1 named at the top level: a
mechanism the library implements correctly, with no shipped entry point that
turns it on. `DefaultWorkerConfig` configuring `AttestInterval: 60s` made a
WithAttestor deployment's P6 chain look like it was running when nothing
ever called the one method that starts it. (3) and (4) are TOCTOU-adjacent:
a `*Service` value looks identical whether it is the top-level Service or a
transaction-bound clone, so nothing at the call site stops a caller from
calling a top-level-only method from inside a callback and getting an
answer that is either silently wrong (mixed pool/tx views for
`VerifyLedger`) or silently discarded (`EnableOnchain` configuring a value
nobody keeps) instead of a clear rejection.

**Enforced by**: `(*ledger.Service).Worker`'s `s.attestor != nil` branch and
`mergeWorkerConfig`'s `AttestInterval`/`AttestBatchSize` cases (`ledger.go`);
`PendingStore.checkPendingBalanceAndPost`'s pool-mode `Authorize` +
`PostAuthorized` sequencing (`postgres/pending_store.go`);
`RunInTxWithOptions`'s `s.tx != nil` guard; `AttestationService` /
`VerifyLedger` / `EnableOnchain`'s own `s.tx != nil` guards (`ledger.go`).
`withTx` also now carries `attestor`/`authVerifier` onto the clone (they
were previously dropped, which happened to make some of the guards above
pass for the wrong reason before this fix — see the pinned tests' own doc
comments for how each isolates the guard it protects from that adjacent
bug).

**Pinned by** (the four below are declared in the root package, cited
without a qualifier per this doc's bare-citation convention -- see I-13):
- `TestServiceWorker_AttestsAutomaticallyWhenAttestorConfigured` —
  goes through `ledger.New` + `WithAttestor` + `svc.Worker(cfg).Run`, no
  manual `SetAttestor` call, and polls `ledger_attestations` directly for
  proof the chain advanced; deleting the auto-wiring in `Worker()` turns
  this red without touching the test.
- `postgres.TestPendingStore_ConfirmPending_SignsWhenAttestorConfigured` /
  `TestPendingStore_CancelPending_SignsWhenAttestorConfigured` — both build a
  `LedgerStore.WithAuth` in pool mode and assert `core.AuthStatusSigned` (and,
  for Confirm, non-empty stored digest/signature/key id).
- `TestService_RunInTx_NestedCallIsRejected` — a `RunInTx`
  callback that calls `tx.RunInTx` again gets an error and the inner
  callback never runs.
- `TestService_AttestationService_RefusedOnTxBoundClone` /
  `TestService_VerifyLedger_NotRunOnTxBoundClone` /
  `TestService_EnableOnchain_RefusedOnTxBoundClone` — each first proves, on
  the top-level Service, that its configuration would otherwise succeed
  (VERIFIED / a non-nil `*service.Onchain`), then proves the identical call
  is refused from inside `RunInTx` — isolating the guard from the
  attestor/authVerifier-on-clone fix and from `EnableOnchain`'s own
  `AutoCreditCeiling` fence.
- `TestService_Worker_ConcurrentCallsDoNotRaceOnEventStore` — `go
  test -race`-only: N goroutines each calling `svc.Worker(cfg)` with a
  different `EventClaimLease` no longer race on a shared
  `*postgres.EventStore`, because each `Worker()` call now builds its own.

## I-41: Reconciliation coverage and onchain money-path signals fail closed on ambiguity rather than folding it into a clean bill of health

(2026-08-25 financial-engineering audit, `operability.md` + `onchain-money-path.md`
+ `threat-model.md` §4-3; contract `docs/plans/2026-08-26-audit-remediation-contracts.md`
Wave 2 D-ops.)

**Rule** (five independent properties, one task):

1. `FullReconciliationService`'s check suite contains no check that can
   structurally never run. A check whose `Complete` field is permanently
   `false` for every possible input carries no information and poisons
   `ReconcileReport.FullCoverage` to false forever — it is removed from the
   suite rather than left in as a permanently-red placeholder.
2. `checkpoint_balance` (check #2)'s `Complete` signal additionally
   requires that either this run's scan started from the fresh-lap
   sentinel cursor, or it actually examined at least one (holder, currency)
   pair. A run that resumes from a non-fresh persisted cursor and finds
   zero pairs on its very first page reports `Complete=false` — that shape
   is indistinguishable, on the wire, from a cursor advanced by something
   other than this scan loop genuinely walking the data (`reconcile_scan_cursors`
   has no DB-level mutation guard against `ledger_app`).
3. `core.Metrics.BalanceDrift` is fed the drift-from-the-zero-floor
   (0 when a dimension is healthy, the shortfall's magnitude when it is
   not), not the raw balance, and is re-emitted on every rollup for that
   dimension — including the healthy ones — so the gauge can return to
   zero instead of staying pinned at the last violation it ever observed.
4. `chains/evm.Scanner.ScanBalances` fails closed uniformly on an
   unreadable balance (a reverted call, or a malformed return length),
   regardless of which internal path (Multicall3 aggregate3, or the
   concurrent single-call fallback) a chain happens to support at scan
   time. Treating an unreadable balance as a genuine zero silently drops
   that address from the sweep-eligible set with no error, no log, no
   metric.
5. `chains/evm.Sweeper.BatchSweep`'s gas-bump replacement fee floor
   prefers the actual fee of the transaction sitting at the caller-supplied
   `priorTxHash`, read from the chain via `TransactionByHash`, over this
   process's own in-memory record of the last fee it used — the in-memory
   record does not survive a process restart, while a hash sourced from a
   booking's durably persisted `ChannelRef` does. `Sweeper.GasPrice`
   reports the identical fee-cap basis `BatchSweep`'s non-retry path
   actually pays, so `core.SweepPolicy.GasCeiling` bounds what will really
   be paid rather than a different, lower-tending RPC estimate.

**Why**: (1) `full_coverage` existed specifically so a run that only
partially covered the fleet could not be read as a clean bill of health
(P0 of the 2026-08-21 tamper-evident-ledger wave) — a permanently-poisoned
version of that exact signal defeats its own purpose and trains operators
to ignore it (`~/.claude/rules/working-agreements.md` §3: an unusable
signal is equivalent to no signal). (2) Without this, an attacker who
resets `reconcile_scan_cursors` before every scheduled reconcile run keeps
`checkpoint_balance` — the only detector for checkpoint tampering — reading
green throughout an entire poisoning window. (3) A gauge that can only ever
increase from zero and never return to it is, functionally, a one-shot
alarm masquerading as a live signal; worse, it shared an alert rule with
`reconcile_gap_units` (the actual checkpoint-tamper detector), so silencing
the never-clearing false alarm would have silenced the real one too. (4)
"unreadable" and "zero" have different remediations (retry vs. genuinely
nothing to sweep) and different urgency (a persistently unreadable balance
means funds are invisible to the sweep job, not that they don't exist).
(5) Before this fix, a gas-bumped sweep transaction that got stuck (e.g. in
a gas spike) and then hit a process restart had no way to reconstruct a
replacement fee high enough to beat whatever was genuinely still pending —
every subsequent bump attempt inherited the same blind spot, permanently
stalling that chain's fund collection with no self-healing path
(onchain-money-path.md's headline finding).

**Enforced by**: `service.FullReconciliationService.RunFullReconciliation`
(check suite composition, `service/reconcile.go`); `runCheck2GlobalBalance`'s
`resumedLap` tracking and the `scanned == 0 && resumedLap` branch (same
file); `service.RollupService.processItem`'s `BalanceDrift` call (`service/rollup.go`);
`chains/evm.multicallResultsToBalances` / `chains/evm.decodeERC20BalanceOf`
(`chains/evm/scanner.go`) excluding an unreadable address from `balances`
(see the m-10 correction below for the per-address, not per-batch, grain
this operates at) instead of defaulting it to zero;
`chains/evm.Sweeper.priorFeeFloor` / `chains/evm.Sweeper.GasPrice`
/ `chains/evm.feeCapBasis` (`chains/evm/sweeper.go`); `core.Sweeper.BatchSweep`'s
`priorTxHash` parameter (`core/interfaces.go`).

**Pinned by**:
- `service.TestFullReconciliation_AllPass` / `TestFullReconciliation_FullCoverageCanBeTrue` —
  the suite now runs exactly 13 checks, and `FullCoverage` can actually read
  `true` when everything is wired and nothing is capped or skipped (the DB-backed
  half is `service.TestFullReconciliation_UnauthorizedJournals_PassesWhenAllSignedJournalsAreValid`'s
  added `FullCoverage` assertion).
- `service.TestCheck2GlobalBalance_ResumedCursorZeroPairsIsIncomplete` /
  `TestCheck2GlobalBalance_FreshCursorZeroPairsStillComplete` — the
  resumed-vs-fresh boundary, both directions; `service.TestFullReconciliation_Check2ResumesAcrossRuns`'s
  fourth run is the DB-backed pin for the same boundary.
- `service.TestRollupService_DriftDetection` (asserts the reported value is
  the positive magnitude 85, not the raw balance -85) /
  `TestRollupService_DriftDetection_ClearsOnceHealthy`.
- `chains/evm.TestMulticallResultsToBalances_FailsClosedPerAddress` /
  `TestDecodeERC20BalanceOf_FailsClosedOnMalformedReturn`;
  `service.TestOnchain_Sweep_UnreadableAddressDoesNotBlockReadableAddresses`
  (correction below).
- `chains/evm.TestSweeper_QuoteFee_PrefersChainTruthOverMemory` /
  `TestSweeper_QuoteFee_FallsBackToMemoryWhenPriorHashUnknown` /
  `TestSweeper_QuoteFee_NoPriorMeansNoBump`.
- `service.TestOnchain_Sweep_GasBumpCarriesPriorTxHash` /
  `TestOnchain_Sweep_GasBumpFallsBackToChannelRefAfterRestart` — point 5's
  SERVICE half, which had no pin at all until the 2026-09-02 remediation
  (G-M10): the three `chains/evm` tests above only cover the pure fee
  arithmetic, so reverting `service/onchain.go`'s gas-bump call site to pass
  `priorTxHash: ""` — i.e. the whole of point 5, from the caller's side —
  left the suite green. Both existing revival tests configured
  `MaxSweepBumps(0)`, so the gas-bump branch was never executed by anything.
  The second test covers the restart case specifically: with the in-memory
  tracking gone, the bump must fall back to the booking's persisted
  `ChannelRef`, which is the durable hash point 5 is about.
- `chains/evm.TestSweeper_ReplacementGasPrice_QuotesTheEscalatedBidInGwei` /
  `TestSweeper_ReplacementGasPrice_FallsBackToMarketOnFirstDispatch` and
  `service.TestOnchain_Sweep_GasBumpRespectsGasCeiling` — point 5's final
  clause ("`GasCeiling` bounds what will really be paid") held only for a
  FIRST dispatch. A replacement bids `max(market basis, prior fee x 1.125)`,
  which the market basis does not bound, so on the retry path — the only
  path where the fee escalates — the ceiling was being compared against a
  quantity that could be arbitrarily smaller than the bid (G-M4). The gate
  now reads `core.Sweeper.ReplacementGasPrice` for the same
  `(signerNonce, priorTxHash)` pair the ensuing `BatchSweep` is called with.
- `service.TestOnchain_Sweep_RevivedDispatchRespectsGasCeiling` — the same
  gap's sibling, at the other call site: `advanceSweep`'s dispatch branch is
  also how a REVIVED sweep booking goes out, and a revival keeps the failed
  booking's nonce, so the adapter still bids over the exhausted bump
  ladder's floor while the only gate that ran (`sweepTick`'s) had compared
  the market price, before the nonce was even known. Both dispatch and bump
  now gate on the bid for their own nonce.

> **Correction (m-10 fix on point 4, `.local/independent-review-2026-08-26.md`,
> third-pass independent review).** Point 4 as originally written and its
> first implementation failed the ENTIRE `ScanBalances` call closed the
> moment any single address in the batch was unreadable —
> `multicallResultsToBalances` / `scanConcurrently`'s per-address failure
> returned an error that aborted the whole batch (on the concurrent
> fallback path, via `errgroup` cancelling every other still-in-flight
> address's context the instant one goroutine returned non-nil). That
> correctly avoided the original fail-open bug (unreadable treated as zero)
> but introduced a different cost in the fail-closed direction: one broken
> or slow-to-respond address — a single flaky RPC response, a
> broken/malicious token contract at one specific address — meant *zero*
> addresses got swept that round, no matter how many others were perfectly
> readable. `sweepTick` (`service/onchain.go`) propagated that single error
> straight out, so `service.Onchain`'s entire sweep tick for that
> `(chain, token)` was a no-op.
>
> Corrected rule: `ChainScanner.ScanBalances` (`core/interfaces.go`) returns
> `(balances, unreadable, err)`. Per-address read failures land in
> `unreadable` and are excluded from `balances` — never defaulted to zero,
> preserving point 4's original guarantee — but do NOT abort the batch;
> every other address's balance is still returned. `err` is now reserved
> for failures that invalidate the whole round (chain/token not configured,
> the RPC client itself unreachable, a malformed `aggregate3` response —
> things that would have failed identically for every address anyway).
> Callers must surface `unreadable` (this is still fail-closed, not
> fail-open: an unreadable address is excluded from this round's
> sweep-eligible set, not swept as if its balance were 0) — `sweepTick`
> logs a `Warn` and calls the new `core.Metrics.SweepAddressUnreadable`
> counter, then proceeds with whatever addresses WERE readable.
>
> **Enforced by (correction)**: `core.ChainScanner.ScanBalances`'s
> three-return signature (`core/interfaces.go`);
> `chains/evm.multicallResultsToBalances` / `chains/evm.Scanner.scanConcurrently`
> (`chains/evm/scanner.go`) collecting per-address failures into `unreadable`
> instead of returning an error for the batch; `service.Onchain.sweepTick`'s
> `unreadable` handling (`service/onchain.go`); `core.Metrics.SweepAddressUnreadable`
> / `observability.PrometheusMetrics.SweepAddressUnreadable`.
>
> **Pinned by (correction)**:
> `chains/evm.TestMulticallResultsToBalances_FailsClosedPerAddress` — the
> renamed/rewritten form of the original pin, now asserting the OTHER,
> readable address in the same batch still comes through instead of
> asserting the whole batch returns nil. `service.TestOnchain_Sweep_UnreadableAddressDoesNotBlockReadableAddresses`
> — an end-to-end `sweepTick` proof: one unreadable address alongside one
> readable, above-threshold address still produces a sweep booking for the
> readable one, and `core.Metrics.SweepAddressUnreadable` is called with the
> unreadable count.

> **Correction (M-1 fix on point 2, `.local/independent-review-2026-08-26.md`,
> docs/plans/2026-08-26-audit-remediation-contracts.md follow-on
> fix-backend-1 batch, board #43).** Point 2 as originally written only
> distrusted a resumed run that found **literally zero** pairs on its first
> page. A cursor tampered to leave exactly one (or any small number of, up to
> the page size) pairs unscanned sailed straight through it: the run scanned
> its few remaining pairs, found nothing wrong, reached the natural end of
> the data, and reported `Complete=true` — resetting the cursor and
> discarding every pair the tampering skipped. Corrected rule: a resumed run
> reaching the natural end of the data may only report `Complete=true` when
> its **cumulative pairs verified this lap** (`lap_scanned`, persisted
> alongside the resume cursor, reset to 0 exactly when the cursor resets to
> the fresh-lap sentinel) reaches the number of distinct (holder, currency)
> pairs the checkpoint fleet currently has (`CountCheckpointAccountPairs`) —
> not merely "the next page query came back short", which a resumed cursor's
> own (attacker- or otherwise-reachable) starting position can produce at any
> pair count, not just zero. A resumed run finding literally zero pairs keeps
> the original (unchanged) treatment as a stricter special case. This does
> not weaken point 2's guarantee for legitimate multi-run laps (the
> resumability C4b exists for): `lap_scanned` accumulates across every
> capped/timed-out run of the same lap, so a lap that genuinely finishes
> across several runs still reaches the required total and reports
> `Complete=true` on the run that completes it.
>
> **Enforced by (correction)**: `service/reconcile.go`'s `runCheck2GlobalBalance`,
> the `resumedLap` branch requiring `lapScanned >= total` (queried via
> `ReconcileQuerier.CountCheckpointAccountPairs`); `postgres/sql/migrations/010_reconcile_scan_lap_coverage.up.sql`
> (`reconcile_scan_cursors.lap_scanned` column + extended audit trigger);
> `postgres/sql/queries/reconcile.sql`'s `UpsertReconcileScanCursor` /
> `GetReconcileScanCursor` / `ReconcileCountCheckpointAccountPairs`.
>
> **Pinned by (correction)**:
> `service.TestCheck2GlobalBalance_ResumedLapUndercountedIsIncomplete` — the
> exact repro (a cursor tampered to sit one pair before the true end, with no
> genuine prior progress recorded, is no longer trusted as `Complete=true`) /
> `TestCheck2GlobalBalance_ResumedLapReachesFullCoverageAcrossCappedRuns` —
> the non-regression companion: a legitimate multi-run lap whose cumulative
> `lap_scanned` genuinely reaches the fleet's pair count still completes /
> `TestCheck2GlobalBalance_ResumesFromPersistedCursor` (updated) —
> the original resumed-with-real-prior-progress scenario, still trustworthy /
> `TestCheck2GlobalBalance_CountCheckpointAccountPairsErrorIsReported`.

> **Correction (M-3 fix on point 3, same batch).** Point 3 as originally
> written re-emits `BalanceDrift` on every rollup for a dimension, healthy or
> not, so the gauge can return to zero. That Gauge is labelled `(class,
> currency)` WITHOUT holder (deliberately, to keep cardinality bounded), so a
> healthy item for a DIFFERENT holder sharing that label can legitimately
> re-`Set` the same series back to zero immediately after a genuine violation
> was reported for the FIRST holder — making a real, still-open violation
> invisible to anything alerting on the Gauge the moment any other holder in
> the same bucket is next processed. The Gauge's per-item self-clearing
> behavior described in point 3 is unchanged and still correct for what it
> is (a coarse, most-recent-reading-per-label indicator) — what changed is
> that alerting no longer relies on it alone. A new monotonic Counter,
> `core.Metrics.NegativeBalanceDetected` (Prometheus
> `negative_balance_detected_total`, same `(class, currency)` labels),
> increments exactly when a violation is detected and — being monotonic —
> cannot be reset by an unrelated holder's healthy item the way the Gauge
> can. Alerting should key off `increase(negative_balance_detected_total[window]) > 0`.
>
> **Enforced by (correction)**: `core.Metrics.NegativeBalanceDetected`
> (`core/metrics.go`); `observability.PrometheusMetrics.NegativeBalanceDetected`
> (`observability/prometheus.go`); `service.RollupService.processItem`'s call
> alongside the existing `BalanceDrift` call, on the same violation branch
> (`service/rollup.go`).
>
> **Pinned by (correction)**:
> `service.TestRollupService_NegativeBalanceDetected_SurvivesUnrelatedHealthyItem`
> — reproduces the exact cross-holder clobbering shape (holder A violates,
> holder B is healthy and shares A's label) and asserts the Gauge's second
> reading really is zero (the precondition, not the bug) while the Counter
> still shows exactly one detection.

## I-42: `journal_entries.id` is sourced from the sequence alone, never an explicit value

(board #37 / `docs/audits/2026-08-25-financial-engineering/financial-correctness.md`'s
Minor -- "`journal_entries.id` 单独不唯一, I-5 的单调性完全依赖序列" -- schema-fact
CONFIRMED, consequence PLAUSIBLE at audit time, CONFIRMED by the pin below;
`docs/plans/2026-08-26-audit-remediation-contracts.md` §9, W3-id.)

**Rule**: `ledger_app` cannot INSERT into `journal_entries` (the parent or any
of its partitions) with a statement that names the `id` column explicitly.
The only path by which a row's `id` is ever assigned is the column's
`DEFAULT` -- `nextval` on `journal_entries_id_seq`, one sequence shared by
the parent and every partition.

**Why**: `journal_entries` is partitioned monthly by `created_at`, and a
partitioned table's primary key must include the partition key, so the table
is keyed on `(id, created_at)`, not `id` alone. `001_baseline.up.sql`'s own
comment calls this "a uniqueness backstop beyond trusting the sequence" --
it is not one, and that comment is left as-written (already-applied
migrations are never edited). The composite key only forbids the exact same
`id` repeating inside the *same* partition; nothing at the schema level
stopped the same `id` from appearing once per partition before this
invariant closed the gap. Every per-account balance path filters strictly on
`id > checkpoint.last_entry_id` (I-5), so a row whose `id` duplicates one
already at or below the watermark would be permanently invisible to
`GetBalance`, while `SumGlobalDebitCreditByCurrency`/`reconcile.sql` (which
sum every row, unfiltered by `id`) would count it regardless -- and because
such a forged pair is itself internally balanced, the global debit==credit
sanity check stays green throughout, so nothing else in the system flags the
divergence. The pre-migration-008 form of the pin below (see git history for
its prior name and body) confirmed this consequence directly rather than
leaving it PLAUSIBLE. The realistic trigger is not "an
attacker guesses a free `id`" -- `ledger_app` already holds INSERT on
`journal_entries` (I-22 classifies it append-only: SELECT/INSERT, no
UPDATE), and until migration 008 that grant was table-level, covering every
column including `id`. A raw INSERT under a leaked `ledger_app` credential
lands here.

> **Correction (2026-08-26).** Migration 008's own comment, and an earlier
> draft of this paragraph, also named "a sequence that regresses after a PITR
> restore" as a trigger. **That claim is false**, and the first DR drill
> (`docs/DR.md`, same date) falsified it directly rather than by argument:
> PostgreSQL WAL-logs sequence advancement *ahead* of `nextval()` consumption
> — crash-safety by design — so a restored sequence comes back at or ahead of
> the highest id in the table, never behind. Measured on the restored
> instance: `last_value` 106 against `max(id)` 80. `pg_dump` / `pg_restore`
> preserves exact state through its own `setval()` (120 to 120). The
> direction of error is toward **gaps** in the id space, never duplicates.
>
> The migration's comment is left as written, because a migration that has
> landed is never edited (`deployment.md`); this paragraph is the correction
> of record. What 008 actually defends against is the leaked-credential
> path — which is real, and was demonstrated: with 008 neutralised, an
> explicit-id INSERT as `ledger_app` succeeds.

**Enforced by**: `postgres/sql/migrations/008_journal_entries_id_sequence_only.up.sql`
-- a `pg_partition_tree('journal_entries')`-driven loop (so it reaches
whichever partitions a given deployment has actually accumulated, not a
hardcoded name list) that `REVOKE`s `ledger_app`'s table-level INSERT on the
parent and every existing partition and replaces it with a column-level
INSERT naming every column except `id`. Partitions created after this
migration (`ledger_create_monthly_partition` /
`ledger_rebalance_default_partition`, both migration 007, both SECURITY
DEFINER) never receive a per-partition grant at all -- the only way
`ledger_app` reaches them is tuple-routing through the parent's own name,
which Postgres checks against the parent's ACL (the one this migration
restricts), not the partition the row physically lands in --
`postgres.TestLedgerAppInsertsIntoPartitionCreatedAfterGrant` (I-35) already
pins that routing behavior. `postgres/sql/queries/journals.sql`'s
`InsertJournalEntry` is the sole production write path into this table and
never lists `id` in its column list, so it is unaffected.

**Pinned by**:
- `postgres.TestJournalEntries_DuplicateIDAcrossPartitions_Rejected` --
  the same forged-id attack the pre-fix pin demonstrated, now run against a
  real `ledger_app` credential (`newAppPool`) and asserted refused
  (`SQLSTATE 42501`) rather than silently splitting the ledger's two views
  of the same balance.
- `postgres.TestRoleAttributes` -- static ACL shape: `journal_entries` keeps
  table-level `SELECT` for `ledger_app`, no table-level `INSERT`, a
  column-level `INSERT` on `journal_id` (representative of the
  non-`id` column set), and no column-level `INSERT` on `id`.
- `postgres.TestGrantCoverage_EveryTableHasExpectedLedgerAppAndLedgerRoGrants/journal_entries` --
  the same shape, enumerated structurally alongside every other table's grant
  policy rather than hardcoded to this one table.

> **Correction (M-5 fix, `.local/independent-review-2026-08-26.md`,
> docs/plans/2026-08-26-audit-remediation-contracts.md follow-on
> fix-backend-1 batch, board #43).** `docs/RUNBOOK.md` §9's emergency-freeze
> "restore privileges" step used to be a single blanket
> `GRANT INSERT ON journals, journal_entries, events, bookings TO ledger_app`.
> Following that step after any emergency freeze silently undid this
> invariant's entire protection: it hands `ledger_app` back its table-level
> INSERT on `journal_entries`, `id` column included, regardless of what this
> migration's column-level grant says. Corrected: `journal_entries` is
> excluded from the blanket `GRANT` and restored instead by re-running this
> migration's own `pg_partition_tree`-driven DO loop, which is safe to
> re-run at any time (it derives every partition from the catalog, not a
> hardcoded list). RUNBOOK.md now also names the same trap in 001_baseline's
> own ACL-derivation loop comment (section 14), which issues the identical
> table-level `GRANT SELECT, INSERT` and would silently undo this migration
> the same way if ever re-run against `journal_entries` by name.
>
> **Enforced by (correction)**: `docs/RUNBOOK.md` §9 step 4 (corrected text,
> re-running this migration's DO loop verbatim).
>
> **Pinned by (correction)**: no compiled code executes a markdown file, so
> this is pinned directly against the two SQL fragments (old vs. corrected)
> rather than against the doc: `postgres.TestRunbookEmergencyRecovery_NaiveGrantReopensIDColumn`
> (the OLD step 4 text really does reopen explicit-id INSERT under
> `ledger_app` -- proves the vulnerability RUNBOOK.md used to hand every
> on-call engineer) / `postgres.TestRunbookEmergencyRecovery_CorrectedGrantKeepsIDColumnClosed`
> (the corrected step 4 keeps `id` closed AND leaves ordinary id-omitting
> inserts unaffected).
## I-43: A classification's normal_side is interpreted in exactly one place, on each side of the language boundary, and an unrecognized value is refused rather than defaulted

(2026-08-25 financial-engineering audit, `financial-correctness.md` Minor
"同一个符号语义有 17 处独立实现"; contract
`docs/plans/2026-08-26-audit-remediation-contracts.md` §9 W3-sign.)

**Rule**: every computation that turns a `(normal_side, entry_type)` pair (or
a classification's pre-bucketed debit/credit sums) into a signed balance
contribution routes through exactly one function on each side of the
Go/SQL boundary:

- **Go**: `core.Sign(normalSide, entryType)` is the sole authority. Its two
  wrappers, `core.SignedAmount` (single entry) and `core.Delta` (bucketed
  debit/credit sums), and `core.EntryDirection` (account-policy enforcement's
  increase/decrease classification) are all defined in terms of it. All
  three refuse — return an error — for any `normalSide`/`entryType` outside
  `{debit, credit}`, rather than defaulting.
- **SQL**: `ledger_signed_amount(normal_side, entry_type, amount)` (per-row)
  and `ledger_signed_delta(normal_side, debit_sum, credit_sum)` (bucketed),
  installed by migration 009. Both refuse an unrecognized `normal_side` via
  `ledger_reject_unknown_normal_side` (`RAISE EXCEPTION`,
  `ERRCODE = invalid_parameter_value`), except that a `NULL entry_type`
  (the LEFT JOIN placeholder row Postgres produces for "this classification
  has zero matching journal_entries", not a real row with an unrecognized
  value) yields `NULL`, not an error — `SUM()` ignoring that `NULL` is what
  lets `COALESCE(SUM(...), 0)` still report a genuine zero-entries balance
  as 0, matching what the `ELSE 0` shape below used to do for that one case.

Before this collapse, 17 independent implementations existed — 7 in Go
(`core/account_policy.go`'s `EntryDirection`, `service/rollup.go`,
`service/reconcile.go` ×3, `postgres/ledger_store.go`,
`postgres/trial_balance_store.go`, `postgres/reconcile_queries.go`) and 10
SQL expressions in 3 shapes across `checkpoints.sql`,
`integrity_checkpoint.sql`, `platform_balances.sql` (×3),
`holder.sql`, and `reconcile.sql` — all agreeing on the four
`(normal_side, entry_type) → sign` combinations that
`normal_side ∈ {debit, credit}` actually reaches, but disagreeing on what to
do with a value outside that set: `service/rollup.go` and one
`service/reconcile.go` site errored; `postgres/ledger_store.go` and
`postgres/trial_balance_store.go` silently defaulted to debit-normal;
`postgres/reconcile_queries.go` and two of the three SQL fallback shapes
silently defaulted to credit-normal; and `checkpoints.sql`'s
`ListComputedBalancesForHolders` (`ELSE 0`) silently excluded the entry from
the balance entirely.

**Why**: `normal_side ∈ {debit, credit}` is enforced today by a `CHECK`
constraint on both `classifications.normal_side` and
`journal_entries.entry_type` (`001_baseline.up.sql` :169/:220/:331) plus an
immutability trigger on `normal_side` once set — so a third value is
unreachable in a healthy deployment, and the pre-existing 17-way disagreement
was drift risk rather than a live bug (`docs/audits/2026-08-25-financial-engineering/financial-correctness.md`
records it CONFIRMED-but-unreachable-today). Two properties still made it
worth collapsing rather than leaving alone: (1) 17 independent copies means
changing the sign rule — or fixing a bug in it — requires editing 17 places
correctly, a `working-agreements.md` §5 "can this be made a machine check"
smell in its own right; (2) the `ELSE 0` shape is qualitatively worse than
the others — an entry that fails to classify does not error, does not log,
and does not miscount at the wrong sign, it simply vanishes from the `SUM`,
which is the one failure mode `financial.md` and `working-agreements.md` §3
single out as unacceptable regardless of reachability: a ledger must fail
closed, not silently drop money, even in a branch schema constraints make
practically unreachable today.

**Enforced by**: `core.Sign` / `core.SignedAmount` / `core.Delta` /
`core.EntryDirection` (`core/account_policy.go`) — every call site in
`service/rollup.go`, `service/reconcile.go`, `postgres/ledger_store.go`,
`postgres/trial_balance_store.go`, `postgres/reconcile_queries.go`, and
`postgres/account_policy_enforce.go` routes through one of these instead of
reimplementing the comparison. `ledger_signed_amount` /
`ledger_signed_delta` / `ledger_reject_unknown_normal_side`
(migration `009_normal_side_sign_convergence`) — every sign expression in
`postgres/sql/queries/*.sql` calls one of these instead of reimplementing
the `CASE`. **That list is no longer maintained by hand here.** It was, and
the hand-maintained version silently omitted `balance_trends.sql` through two
separate enumerations — see I-50, which makes the enumeration mechanical and
is what this claim now rests on. `ledger_signed_amount` is deliberately not
`STRICT` (an explicit `WHEN p_entry_type IS NULL THEN NULL` branch handles
the LEFT JOIN placeholder case instead) because a `STRICT` `LANGUAGE sql`
function is never inlined by the planner — see the migration's comment and
`postgres.BenchmarkListComputedBalancesForHolders` in
`postgres/benchmarks_test.go`, which measured ~1.4ms/op for this
(non-`STRICT`, inlined) design against ~1.37ms/op for the pre-migration raw
`CASE` (statistically indistinguishable) and ~3.0ms/op for an otherwise
identical `STRICT` variant (~2.1x slower) — the `STRICT` measurement was
taken with a throwaway benchmark during this task and is not preserved in
the repository.

**Pinned by**:
- `core.TestSign_RejectsUnknownNormalSideAndEntryType` / `TestSign_Table` —
  the "third value" scenario the audit's failure analysis describes, and the
  four valid combinations every wrapper must agree on.
- `core.TestSignedAmount_RejectsUnknownNormalSide` /
  `TestDelta_RejectsUnknownNormalSide` /
  `TestEntryDirection_RejectsUnknownNormalSide` — each of the three
  documented pre-collapse fates (rollup.go's error, ledger_store.go's
  debit-normal default, reconcile_queries.go's credit-normal default, and
  EntryDirection's silent decrease-classification) now agree: all reject.
- `core.TestDelta_AgreesWithSignedAmount` — pins the linearity
  `Delta(ns, debitSum, creditSum) == SignedAmount(ns, debit, debitSum) +
  SignedAmount(ns, credit, creditSum)`, so `Delta` cannot drift from `Sign`
  as a second, independently-maintained copy.
- `postgres.TestLedgerSignedAmount_AgreesWithCoreSignedAmount` /
  `TestLedgerSignedDelta_AgreesWithCoreDelta` — the SQL functions match
  `core.SignedAmount` / `core.Delta` bit-for-bit across all four valid
  combinations.
- `postgres.TestLedgerSignedAmount_RejectsUnknownNormalSide` /
  `TestLedgerSignedDelta_RejectsUnknownNormalSide` — the SQL-side reject,
  asserted against the exact Postgres error code
  (`22023`/`invalid_parameter_value`).
- `postgres.TestLedgerSignedAmount_NullEntryTypeIsNoContribution` — the LEFT
  JOIN placeholder case is `NULL` (no contribution), not an error; this is
  the regression the first cut of this function actually hit
  (`TestVerifiedBalance_ZeroContributingJournalsIsDefinedZero` went red
  against it before the `WHEN p_entry_type IS NULL` branch was added).
- `postgres.TestNormalSideDomain_StructurallyClosedByCheckConstraints` —
  the `CHECK` constraints that make the `ELSE` branch practically
  unreachable are themselves verified against a real INSERT, for both
  `classifications.normal_side` and `journal_entries.entry_type`.

## I-44: The holder transaction view's `kind` is drawn from a small, deployment-stable product vocabulary — never an internal accounting identifier, and never a value that conflates "untagged" with a declared meaning

(M-7, `.local/independent-review-2026-08-26.md`;
`docs/plans/2026-08-26-audit-remediation-contracts.md` follow-on fix-m7-kind
batch, board #49, Aaron 2026-08-27 route ③.)

**Rule**: `HolderTransaction.Kind` (`core/holder.go`) is one of
`core.HolderTxKind`'s seven values —
`""`/`deposit`/`withdrawal`/`transfer`/`fee`/`adjustment`/`other` as stored
on `journal_types.holder_kind`, but **never `""` on the wire**: the read
path (`ListHolderTransactionRows`, `postgres/sql/queries/holder.sql`)
`COALESCE(NULLIF(jt.holder_kind, ''), 'other')`s every row, so an untagged
journal type reads as the disclosed `other` bucket rather than leaking the
internal "nobody has tagged this yet" state onto the wire as an empty
string that is not itself a member of the enum a consumer switches on.

This is the third shape this field has had:

1. `journal_types.code` (e.g. `"deposit_confirm"`) — an internal
   accounting-engine identifier an operator names when configuring their own
   journal types, narrating *how the ledger produced the balance*, which
   `~/.claude/rules/user-facing-surfaces.md` forbids on a holder-facing
   surface.
2. `journal_types.uid` — compliant (opaque, reveals nothing about the
   engine's internal taxonomy), but per-deployment-random: unwriteable as a
   literal, so `@azex/ledger-react`'s `kindLabels` prop (keyed by a stable
   string a host app hardcodes, e.g. `{ deposit: "Top up" }`) could never
   match it and silently fell back to `kind_label` on every deployment.
3. `core.HolderTxKind` (this invariant) — small, coarse, and
   **deployment-stable**: every deployment's transactions bucket into the
   same six named values (plus the untagged-reads-as-`other` fallback
   above), so a host app's `kindLabels` map, keyed by these literals, works
   identically across every deployment. `HolderTxKindOther` is the explicit
   "a journal type author considered the vocabulary and this genuinely
   doesn't fit" declaration; it is a different thing from `""` ("nobody has
   looked at this yet") even though both currently read as `other` on the
   wire — see the next paragraph for why collapsing them on the *wire* does
   not collapse them in the *store*.

**Why this is not the same shape as the M-4 bug it deliberately does not
copy**: M-4 (I-37's addendum) found that letting `""` on
`classifications.balance_role` mean both "a deliberate memo account" and
"nobody tagged this yet" was dangerous because
`core.ClassificationInput.Validate` and `SolvencyReport.Liability` could not
tell the two apart — a real liability that fell through the cracks was
**silently miscounted**, a financial-correctness failure. `holder_kind` has
no such consumer: nothing computes a number from it, and its only downstream
use is display. `core.JournalTypeInput.Validate` is therefore **not**
required to refuse `HolderTxKindNone` at creation the way
`ClassificationInput.Validate` refuses `BalanceRoleNone` for a non-system
classification — doing so would have required hardening `CreateJournalType`
against every caller across this repository's test scaffolding, the
`examples/` package, and the `POST /journal-types` HTTP handler, none of
which touch a financial computation, for a field whose worst-case failure
mode is an honest, disclosed generic label instead of a silently wrong
balance sheet. The distinction this invariant actually enforces is narrower
and cheaper: the *store* keeps `""` (untagged) and `other` (explicitly "none
of the above") as different values — a deployer can always find and retag
the former via `SetHolderKind` — while the *wire* never emits the internal
`""` state as literal text a client would have to special-case alongside
the six real vocabulary values.

**Enforced by**: `core.HolderTxKind` + `.IsValid()` (`core/types.go`) —
accepts the seven values above, rejects everything else, including the two
rejected prior shapes (a journal-type code or a uid string).
`core.JournalTypeInput.HolderKind` / `core.JournalTypeStore.SetHolderKind`
(`core/interfaces.go`). `postgres.ClassificationStore.CreateJournalType` /
`.SetHolderKind` (`postgres/classification_store.go`) both call
`HolderTxKind.IsValid()` before writing. `journal_types.holder_kind`
(migration `012_journal_type_holder_kind`) is `NOT NULL DEFAULT ''` with a
`CHECK` constraint enumerating the same seven values as the Go-side
`IsValid()` — the DB layer cannot accept a value the Go layer would refuse,
or vice versa. `postgres/sql/queries/holder.sql`'s
`ListHolderTransactionRows` is the sole place the `COALESCE(NULLIF(...),
'other')` read-time fallback is applied. `presets.JournalTypePreset.HolderKind`
(`presets/templates.go` and every other `presets/*.go` bundle file) —
every preset journal type this package installs declares one explicitly;
`presets.ensureJournalTypePreset`'s expand-safe upgrade (mirroring
`ensureClassificationPreset`'s `balance_role` upgrade) retags a
pre-migration-012 row in place on re-install and rejects a conflicting
pre-existing non-empty value rather than silently overwriting it.

**Pinned by**:
- `core.TestHolderTxKind_IsValid` — the closed vocabulary, including the two
  explicitly rejected prior shapes.
- `postgres.TestJournalTypeStore_HolderKind` — untagged creation accepted,
  a recognized value round-trips, a garbage string refused at both creation
  and `SetHolderKind`, and `SetHolderKind` can move a row in either
  direction (not upgrade-only, unlike `SetBalanceRole`).
- `postgres.TestHolderTransactionsProjection` — the untagged-row wire
  behavior: an untagged journal type's transactions read `kind: "other"`,
  never `""` and never the journal type's internal uid.
- `postgres.TestHolderTransactionsKindIsExplicitVocabulary` — the other
  half: an explicitly tagged journal type's `kind` passes through unchanged,
  proving the `other` fallback is specific to untagged rows.
- `postgres.TestInstallPresets_HolderKindUpgradeAndConflict` /
  `TestInstallPresets_HolderKindConflictAtCreation` — the preset
  expand-safe-upgrade-or-reject-on-conflict behavior, mirroring
  `TestInstallPresets_BalanceRoleUpgradeAndConflict` /
  `_BalanceRoleConflictAtCreation` for `balance_role`.

> **Addendum (M-7 follow-up, Team Lead review, same batch).** The rule above
> is correct as far as it goes — an untagged journal type never leaks `""`
> onto the wire — but leaves the "untagged" state itself with no path to
> discovery: a deployer who forgets to call `JournalTypeStore.SetHolderKind`
> on their own journal type has no way to learn that fact other than a user
> noticing `"other"` where a specific label was expected. This is the same
> `working-agreements.md` §3 shape the M-4 addendum above closes for
> `balance_role`, at a lower severity (display, not solvency) — and the fix
> follows the same pattern: a reconcile check, detection only, that does not
> change what `kind` resolves to on the wire.
>
> A new reconcile check, `untagged_holder_kind`, flags any journal type that
> appears in the holder transaction view's population (a journal entry
> posted for a user holder against a role-bearing classification —
> `postgres/sql/queries/holder.sql`'s own `WHERE` clause) but carries
> `holder_kind = ''`. A journal type that never touches a role-bearing
> classification never surfaces in the holder view at all, so it is never
> flagged regardless of its `holder_kind` — matching the population the
> fallback in the main rule above actually applies to, not a blanket "every
> untagged row" scan.
>
> **Enforced by (addendum)**: `postgres/sql/queries/reconcile.sql`'s
> `ReconcileUntaggedHolderKindJournalTypes`;
> `service.FullReconciliationService.runCheckUntaggedHolderKind`
> (`service/reconcile.go`), wired into the check suite as
> `untagged_holder_kind`.
>
> **Pinned by (addendum)**: `service.TestUntaggedHolderKind_Clean` /
> `_Violation` / `_QueryError` (mock-backed, mirroring the
> `role_less_liability` unit tests) /
> `service.TestFullReconciliation_UntaggedHolderKind_DetectsAndDoesNotFalsePositive`
> — the DB-backed pin, which also asserts the actual consequence directly:
> the untagged journal type's holder transactions really do read
> `kind: "other"` on the wire, and both b-directions (an explicitly tagged
> journal type; one that only ever touches a role-less classification) are
> confirmed never flagged.

---

## I-45: An AuthVerifier distinguishes "I don't hold this key" from "this signature is invalid", and the unauthorized_journals check reports them as different findings

(Team Lead, 2026-08-27, traced from a CI red: `service`'s `-race` full-package
run non-deterministically flagged genuinely signed journals across
concurrent tests as tampered — because every test in the package generates
its own ed25519 keypair, and `runCheckUnauthorizedJournals` could not tell
"I don't have this journal's key" apart from "I have this journal's key and
the signature is wrong".)

**Rule**: `core.AuthVerifier.Verify` MUST return an error wrapping
`core.ErrUnknownAuthKey` when it does not hold the given `keyID` at all —
distinct from a `keyID` it does hold whose signature simply fails to
verify, which must NOT wrap `ErrUnknownAuthKey`. `core.VerifyJournalAuth`
propagates this through Go's multi-`%w` wrapping (`fmt.Errorf("...: %w: %w",
err, ErrUnauthorizedJournal)`), so `errors.Is(err, core.ErrUnauthorizedJournal)`
still succeeds for both kinds — every existing caller that only checks the
coarse sentinel (`VerifiedBalanceReader`, I-32's fail-closed balance gate)
keeps its current behavior with zero code change. A caller that needs the
finer distinction checks `errors.Is(err, core.ErrUnknownAuthKey)`
specifically.

`service.FullReconciliationService`'s `unauthorized_journals` check
(I-32) is that caller. A signed journal (non-empty `auth_key_id`) that
fails `core.VerifyJournalAuth` produces one of two Finding shapes:

- `errors.Is(err, core.ErrUnknownAuthKey)`: reported as "signed with a key
  id this verifier does not recognize ... register the key ... not by
  itself evidence of tampering". This is the expected, benign state of
  every journal signed before a legitimate key rotation retired its old
  key. It still sets `Passed=false` and is never silently dropped — see
  Why below for why a quiet skip here would be dangerous, not merely
  imprecise.

  **"Register the key" is executable as of 2026-09-02** (`tamper-evident.md`
  M-5). It was not before: `authdev.NewLocalVerifier` took exactly ONE
  public key, `LocalVerifier.keys` is unexported, and there was no
  `Register`/setter — so the action this Finding tells an operator to take
  had no API behind it in the verifier the library ships, and a rotation
  made `VerifiedBalance` UNDEFINED (I-32) for every holder with history,
  i.e. refused withdrawals fleet-wide. `authdev.NewLocalVerifierSet(map[
  string]ed25519.PublicKey)` now holds every generation at once; the set is
  copied and immutable, so `Verify` needs no lock and nothing can widen the
  trusted set at runtime. `docs/RUNBOOK.md`'s "P5 signing key rotation"
  section is the procedure, including the part rotation does NOT fix: this
  verifier has no `NotAfter`, so a retired-but-registered key (which point
  1 above requires it to stay) can still sign a newly forged journal. A
  leaked signing key is an incident, not a rotation.
- anything else: reported exactly as before — "claims a signature but
  fails authorization verification" — real tamper evidence.

Both are distinct Finding descriptions in the same `CheckResult`, never
conflated into one alarming or one silent message.

**Why**: Before this fix, `runCheckUnauthorizedJournals` treated any
non-nil `verifier.Verify` error identically — reporting "claims a
signature but fails authorization verification" (tamper language)
regardless of cause. Two real deployments hit this:

1. **Key rotation.** A fleet that rotates its P5 signing key leaves every
   journal signed before the rotation permanently unverifiable by a
   verifier that only holds the current key. Every one of those journals
   was reported as tampered, forever, on every reconcile run. A check that
   is always red after routine, expected maintenance gets muted by
   operators — which is exactly how the *real* signal this check exists
   for (I-32: actual forged/corrupted journals) dies unnoticed
   (`working-agreements.md` §3: "a check that screams at everything gets
   ignored").
2. **Shared test key material.** `service`'s test package has many tests
   that each call `ed25519KeyPair(t, someKeyID)` and post journals signed
   under their own local keypair; when the full package runs under
   `-race`, `runCheckUnauthorizedJournals` in one test could observe
   journals another concurrent test posted under a key its own verifier
   never held — the same "unknown key" shape as production key rotation,
   misreported as tampering, and non-deterministically failing CI (`go
   test -race ./service/...` red; any single test run in isolation green
   — the exact "single test green, full package red" divergence that
   originally surfaced this bug).

The tempting one-line fix — treat an unknown key like a never-signed
journal and skip it silently — trades this false positive for a worse
false negative: an attacker who forges a journal with a fabricated
`auth_key_id` (one that was never a real key at all, not even a
rotated-out one) would look identical to a rotated-out key under that
fix and evade `unauthorized_journals` entirely — the exact "误报换成漏报"
shape `~/.claude/rules/working-agreements.md` §2 requires catching before
adopting an evaluator's finding. The fix in this invariant keeps every
unknown-key journal visible in `Findings` (so nothing evades the check)
while giving it wording an operator acts on correctly (register the key)
instead of wording that sends them chasing a forgery that was never
there.

**Enforced by**: `core.ErrUnknownAuthKey` (`core/errors.go`);
`core.AuthVerifier`'s interface doc contract (`core/interfaces.go`);
`authdev.LocalVerifier.Verify` (`authdev/ed25519.go`) — the reference
implementation, wraps `ErrUnknownAuthKey` on an unrecognized `keyID`, does
not wrap it when a known key's signature fails `ed25519.Verify`;
`service.FullReconciliationService.runCheckUnauthorizedJournals`
(`service/reconcile.go`) — branches on `errors.Is(err,
core.ErrUnknownAuthKey)` to choose the Finding wording, in both cases
still setting `Passed=false` (never a silent skip, unlike the
never-signed-at-all case this same function already handles per I-32);
`authdev.NewLocalVerifierSet` (`authdev/ed25519.go`) — the multi-generation
constructor that makes "register the retired key" a thing an operator can
actually do, plus `LocalVerifier.KeyIDs` for a startup log or health
endpoint answering "which generations can this process verify?".

**Pinned by**:
- `authdev.TestNewLocalAttestor_RejectsUnknownKeyID` — an unrecognized
  `keyID` wraps `core.ErrUnknownAuthKey`.
- `authdev.TestNewLocalAttestor_RejectsTamperedDigest` — a KNOWN key's
  failed verification does NOT wrap `core.ErrUnknownAuthKey` (the negative
  half of the same contract).
- `service.TestFullReconciliation_UnauthorizedJournals_FlagsUnknownKeyAsDistinctFromForgery`
  — a journal genuinely signed under a key the check's verifier does not
  hold is flagged (never silently skipped), with wording distinct from,
  and never overlapping, the forged-signature tamper wording
  `TestFullReconciliation_UnauthorizedJournals_FlagsForgedSignature`
  already pins.
- `authdev.TestLocalVerifier_MultiKeyVerifiesBothGenerations` — both
  generations verify while both are registered; dropping the retired one
  reports `ErrUnknownAuthKey` (a coverage gap, not tamper evidence).
- `authdev.TestNewLocalVerifierSet_RejectsUnusableKeyMaterial` — an empty
  set, an empty key id, or a wrong-length public key fails at construction
  rather than later, inside the ledger, as "unauthorized journal".
- `TestService_KeyRotation_OldGenerationMustStayRegistered` (root package,
  cited bare per this doc's convention — see I-13) — the
  end-to-end cost of getting it wrong, through `ledger.New` /
  `ledger.WithAttestor`: with only the new key registered, a dimension
  carrying pre-rotation history reports `ErrUnauthorizedJournal` and
  `Reserve(RequireVerifiedBalance: true)` is refused; with both
  registered, the balance resolves and the gate passes.
## I-46: A journal/attestation digest depends only on the `effective_at` instant a `TIMESTAMPTZ` column can actually store, never on sub-microsecond digits a signer's clock happened to produce

(Board #51; digest-precision bug Team Lead reproduced locally 2026-08-27. CI
had been red on `a genuinely signed journal must pass VerifyJournalAuth`
since 2026-08-25 -- at least two days before anyone traced it to this.)

**Rule**: `core.CanonicalJournalDigest` (P5, I-26) and
`core.CanonicalBatchDigest`/`core.encodeAttestedEntry` (P6, I-27) both run
every `effective_at` timestamp through `core.canonicalTimestamp` (UTC,
floored -- never rounded -- to microsecond resolution) before formatting it
into the byte layout that gets hashed. Every verifier reconstructs
`effective_at` by reading it back from a `TIMESTAMPTZ` column, which can
never store more than microsecond precision (pgx's binary `Timestamptz`
encoder performs the identical floor, `ts.Time.Nanosecond()/1000`, when
writing a Go `time.Time` to Postgres) -- so a digest computed over a
caller's raw in-memory `time.Time`, before that floor has ever been
applied, is not reproducible by any verifier, on any platform whose clock
actually has sub-microsecond resolution.

**Why**: macOS's `time.Now()` happens to already return microsecond-aligned
values, so the bug was invisible in local dev. Linux (production) has
genuine nanosecond clock resolution, so every journal signed there had its
digest computed over an instant strictly finer than what `postJournalWithQueries`
ever persisted -- `VerifyJournalAuth` (via `postgres.fetchJournalAuthMaterial`,
`service.VerifyLedger`, and `RunAttestBatch`'s T4 auth-verdict cache, which
this bug would have poisoned with false `JournalAuthVerdictUnauthorized`
entries for every Linux-signed journal in a batch) recomputed a different
digest from the DB-read-back value and rejected every one. This was live
and already broken on any Linux deployment before this fix landed --
`working-agreements.md` §3's "在被测试的地方能过、在真正运行的地方不能" is
this bug's exact shape: `time.Now()`-based tests passed on the author's
Mac and only failed in Linux CI.

**Enforced by**: `core.canonicalTimestamp` (`core/auth.go`), called from
`core.CanonicalJournalDigest` and `core.encodeAttestedEntry`
(`core/attestation.go`) before every `.Format(time.RFC3339Nano)` call that
feeds a signed digest. No new domain separator accompanies this fix --
`canonicalTimestamp`'s doc comment lays out why: the floor is a no-op for
any `effective_at` that was already microsecond-aligned (every instant that
round-tripped a `TIMESTAMPTZ` column, and every instant macOS's `time.Now()`
ever produced), so every signature that ever verified successfully keeps
verifying byte-for-byte; the only case this fix changes was, by I-26's own
requirement, already failing `VerifyJournalAuth` before this change existed.

**Pinned by**: `core.TestCanonicalJournalDigest_MicrosecondPrecisionOnly`,
`core.TestVerifyJournalAuth_SurvivesTimestamptzRoundTrip`,
`core.TestCanonicalBatchDigest_MicrosecondPrecisionOnly` -- all three
construct `effective_at` with an explicit non-zero nanosecond remainder
(`time.Date(..., 123456789, time.UTC)`), never `time.Now()`, so they fail
on every platform -- including macOS -- before `canonicalTimestamp` exists,
not just in Linux CI.
`postgres.TestPostJournal_SignedAtSubMicrosecondEffectiveAtStillVerifies`
(C-R2, 2026-09-02 audit: the core-package pin above simulates the
`TIMESTAMPTZ` floor with `.Truncate(time.Microsecond)` in Go; this one signs
a real `PostJournal` call with a sub-microsecond `EffectiveAt`, reads the
stored digest/timestamp back through an actual `TIMESTAMPTZ` column via
`AttestationStore.JournalAuthMaterial`, and requires `core.VerifyJournalAuth`
to still pass -- a platform-independent DB round trip, not a clock-dependent
one).

## I-47: `Migrate()` serializes against every other `Migrate()` call on the same Postgres cluster, not just against callers targeting the same database

(Board #52; CI's `test` job failed 17 tests at once, all tracing to
`internal/postgrestest/postgrestest.go:128`'s `Migrate` call.)

**Rule**: `postgres.Migrate` takes a session-level advisory lock on the
cluster's `postgres` maintenance database, before opening golang-migrate's
own connection to the target database, and holds it for the entire `Up()`
run. `acquireClusterLock` (`postgres/migrate.go`) is the sole place this
happens; no SQL migration file changes.

Since 2026-09-02 (`concurrency.md` B-m4) the acquisition is a polled
`pg_try_advisory_lock` with a bounded budget
(`WithMigrateLockBudget`, default five minutes), a per-attempt `Info` line
naming the key, and a context (`MigrateContext`). It was a bare blocking
`pg_advisory_lock` on `context.Background()`: when the holder's process was
SIGKILLed and its connection went half-open, every `Migrate` on the cluster
— which is precisely the population this invariant exists to serialize —
blocked forever with no log, no timeout and no way to cancel. Bounded
failure beats an unbounded silent wait. As a side effect there is now no
blocking session-level advisory lock anywhere in the repository, which
`postgres.TestNoBlockingSessionAdvisoryLocks` keeps true and which the
residual-risk note on `AcquireBalanceLock`
(`postgres/sql/queries/journals.sql`) leans on as its *secondary* argument —
its primary argument is the per-database scoping described below.

**Why**: `001_baseline` and
`007_role_hardening_and_partition_security_definer` between them issue 8
statements against `ledger_owner`/`ledger_app`/`ledger_ro` — 3×
`CREATE ROLE IF NOT EXISTS`, `GRANT ledger_owner TO %I WITH INHERIT TRUE` +
`REVOKE ledger_owner FROM %I` (both in `001_baseline`), and 3× `ALTER ROLE`
(`007`). Every one of those writes a row in a cluster-wide shared system
catalog — `pg_authid` for the role itself, `pg_auth_members` for role
membership — that exists once per *cluster*, not once per *database*.
`013_partition_function_hardening` and every other migration were checked
and carry no equivalent statement; their `GRANT`/`REVOKE` target tables and
functions, whose ACLs (`pg_class.relacl`, `pg_proc.proacl`) are ordinary
per-database catalog rows and need no lock.

Two `Migrate()` calls installing into two *different* databases on the same
cluster used to run those 8 statements concurrently. PostgreSQL does not
block the loser and let it proceed once the winner commits — it raises
`tuple concurrently updated` (the `ALTER ROLE` shape) or a
`pg_authid_rolname_index` unique-constraint violation (the `CREATE ROLE`
shape, when the role does not exist yet on either side of the race) and
aborts that caller's whole migration transaction. This is invisible in
local dev (`internal/postgrestest.SetupDB` starts one Postgres container per
Go test *package*, so nothing shares a cluster) and was invisible in CI
before board #52 introduced `DATABASE_URL`, because that env var is what
first made every test package in the `test` job share one Postgres service
container.

golang-migrate's own advisory lock
(`github.com/golang-migrate/migrate/v4/database/pgx/v5`, `Postgres.Lock`)
does not close this gap: its key is `GenerateAdvisoryLockId(databaseName,
schema, table)`, and PostgreSQL's `pg_advisory_lock` is itself scoped to
the database of the connection that took it — two sessions connected to
two different databases on one cluster never contend for the same
advisory-lock key, confirmed empirically (session B acquires a key
instantly while session A, connected to a different database, still holds
it). No lock taken against the database being migrated — golang-migrate's
or a naively-added one — can ever serialize two callers targeting two
different databases. The lock has to come from a database every caller can
reach regardless of which database it is about to migrate: `postgres`, the
maintenance database every cluster creates at `initdb` time. This is an
additional install prerequisite (`CONNECT` on `postgres`, granted to
`PUBLIC` by default) — see `docs/RUNBOOK.md`'s "Database roles" operational
notes.

Serializing CI back onto one goroutine (`go test -p 1`) was rejected as a
fix: that changes when CI happens to run tests relative to each other, not
whether two `Migrate()` callers on one real cluster can collide — the exact
shape Aaron's own local dev setup uses (`infra.md`'s shared `dev-postgres`,
one cluster, one database per project) and any multi-replica deployment
that runs its migration job from more than one pod at once.

**Enforced by**: `postgres.acquireClusterLock` and
`postgres.maintenanceDatabaseURL` (`postgres/migrate.go`), called from
`postgres.Migrate` / `postgres.MigrateContext` before
`migrate.NewWithSourceInstance`.

**Pinned by**:
- `postgres.TestMigrate_ClusterLockHeldElsewhere_FailsWithinBudget` — a
  foreign session holds the key; `Migrate` must fail with an error naming
  the key inside its budget instead of hanging, and must log that it is
  waiting.
- `postgres.TestMigrate_ClusterLockReleased_Succeeds` — the bounded wait is
  still a wait: `Migrate` succeeds once the holder lets go.
- `postgres.TestMigrateContext_CancelledWhileWaiting` — a torn-down boot
  sequence stops waiting.
- `postgres.TestMigrate_ConcurrentAcrossDatabases` — installs
into 8 freshly created databases on one cluster concurrently (after a
sequential warm-up `Migrate()` call so every racer hits `007`'s
unconditional `ALTER ROLE` rather than racing `001`'s
`CREATE ROLE IF NOT EXISTS` instead); fails reliably before this fix
(`tuple concurrently updated` / `pg_authid_rolname_index` violation
depending on timing) and passes reliably after it.

## I-48: Every `core.Anchor` implementation is provably conformant, not just documented as such

(Board #53. `core.Anchor`'s contract — P6's external anchor, design doc
§8.3 — previously lived only in its doc comment's prose; nothing forced a
consumer's production carrier, or a future change to `anchordev`'s dev
implementation, to actually satisfy it.)

**Rule**: `anchortest.RunConformance(t, newAnchor)` (`anchortest/conformance.go`)
is a reusable suite any `core.Anchor` implementation can run against
itself. It checks exactly what `core.Anchor`'s doc comment
(`core/interfaces.go`) promises, in order, as one scenario:

1. `Head` on an anchor nothing has been published to returns `(0, nil,
   nil)` — never an error.
2. `Publish` followed by `Head` reflects what was published, and `Head`
   tracks the *highest* seq across multiple publishes, not merely "a"
   published seq.
3. Re-publishing the same seq with identical bytes succeeds (idempotent
   replay).
4. Re-publishing the same seq with different bytes returns an error, and —
   inferred from what "must return an error" has to mean for the guarantee
   to matter, not spelled out verbatim — does not corrupt the previously
   recorded state.
5. A second, independently constructed client (standing in for a separate
   verifier process against the same carrier) sees what an earlier client
   published — not just because it happens to be the same Go value.

Two things adjacent to the contract are deliberately left unenforced by
this suite, documented in its package comment so the distinction between
"the port requires this" and "one implementation added extra strictness"
stays visible: seq ordering other than an exact replay (`anchordev`'s
`LocalFileAnchor` chooses to reject anything but `Head()+1`; that is its
own added strictness, not something every `core.Anchor` must do), and
concurrency safety of `Publish` (design doc §8.3's intended caller is a
single local retry queue — serialized by construction — so the port makes
no promise about concurrent calls; `LocalFileAnchor` happens to provide it
via an internal mutex anyway, pinned by its own package's tests).

**Why**: a contract that exists only as prose in a doc comment gets out of
sync with implementations silently — a future edit to `LocalFileAnchor`,
or a consumer's first production carrier (an object-lock bucket, a public
chain), has no machine-checkable way to prove it still satisfies what
`core.Anchor` promises, short of re-reading the doc comment and manually
re-deriving every edge case. `RunConformance` turns that prose into a
single reusable suite so both the library's own dev implementation and
every consumer-supplied carrier are held to the identical, explicit bar —
and so a check that could never fail (an unfalsifiable "conformance
suite") is not mistaken for one that actually verifies something (see
`anchortest`'s own tests, next).

**Enforced by**: `anchortest.Check` / `anchortest.RunConformance`
(`anchortest/conformance.go`) — `Check` is the pure, `*testing.T`-free
form; `RunConformance` is a thin adapter that reports the same result as
named `t.Run` subtests.

**Pinned by**:
- `anchortest.TestCheck_CatchesIgnoresByteMismatch` /
  `TestCheck_CatchesHeadAlwaysZero` / `TestCheck_CatchesHeadErrorsOnEmpty` /
  `TestCheck_CatchesInMemoryOnlyFake` (`anchortest/conformance_test.go`) —
  four deliberately broken fake `core.Anchor` implementations, one per
  documented requirement above, each proving `Check` reports the specific
  violation it exists to catch. (These assert on `Check`'s returned
  `[]Violation` value rather than nesting a failing `RunConformance` call
  under `t.Run`: Go's `testing` package marks every ancestor test as
  failed the moment any subtest fails, regardless of what the parent does
  with `t.Run`'s returned bool afterward, so there is no way to make "this
  correctly caught the bug" itself report `PASS` while nesting the failure
  — `Check`'s `*testing.T`-free design exists specifically to make that
  assertion possible.)
- `anchortest.TestCheck_PassesWellBehavedImplementation` /
  `TestRunConformance_PassesWellBehavedImplementation` — a minimal correct
  reference implementation proves the suite is not unconditionally red.
- `anchordev.TestLocalFileAnchor_Conformance`
  (`anchordev/conformance_test.go`) — the library's own dev implementation
  run through the same suite a consumer's production carrier would use,
  passing on the first attempt (a second look for a suite that's too
  lenient, not evidence it is — see the two "deliberately left
  unenforced" items above, which is where that leniency actually is, and
  is documented as intentional rather than an oversight).

## I-49: Under the verified-balance gate, what `Reserve` may lock is computed from entries alone — `balance_checkpoints` cannot raise it

**Rule**: When `core.ReserveInput.RequireVerifiedBalance` is set, the
available base `Reserve` sizes the reservation against is `min(V, E)` minus
that holder's active reservation holds, where both terms sum over every
`balance_role='available'` classification the holder has entries in and
**neither reads `balance_checkpoints`**:

- **V** = Σ `VerifiedBalance(holder, currency, cls)`, computed by the gate
  *before* the transaction opens (I-32 → an entries-only recompute plus an
  authorization check per contributing journal). Authorized, possibly stale.
- **E** = Σ `RecomputeCheckpointFromEntries(holder, currency, cls)`,
  recomputed *inside* the transaction while the `(holder, currency)`
  advisory lock is held, in pure SQL with no external call. Current, but
  with no authorization check.

Insufficiency against that base is `core.ErrInsufficientBalance` — distinct
from the authorization refusal (`core.ErrUnauthorizedJournal`) I-32 defines.
With the flag unset, `Reserve` is unchanged: checkpoint + delta under the
lock, exactly as before.

The set of classifications summed comes from `journal_entries`
(`ListComputedBalancesForHolders`' `populated` CTE is a `DISTINCT` over
entries), so the enumeration is entries-derived too. Only the
classification's `balance_role` is read from config; the `balance` column
that query also returns is deliberately ignored on this path.

**Why**: `balance_checkpoints` is the one balance-bearing table that must
stay `UPDATE`-able — the rollup worker's whole job is writing it — so it
carries no `ledger_block_mutation` trigger, and the standing threat model's
first row (an attacker holding the application's DB credential) can raise a
row in it with one statement. Design doc
`2026-08-21-tamper-evident-ledger-design.md` §0 accordingly classifies the
checkpoint as an **untrusted cache** and requires the withdrawal path to
recompute in full.

The gate did not do that. It verified signatures — correctly, and every
journal in the attack is genuinely signed, so the check passes — and then
threw the recomputed number away; `reserveWithQueries` sized the
reservation off checkpoint + delta. One `UPDATE balance_checkpoints`
therefore bought an arbitrarily large reservation, and then an arbitrarily
large settlement, with the tamper-evidence machinery reporting nothing
wrong, because nothing it watches *was* wrong. The asynchronous
`checkpoint_balance` reconcile check finds the drift on its next run; the
money is gone by then.

I-32's earlier wording made this a feature rather than a bug ("it is an
authorization check, not a stricter amount check", stated in its `Pinned by`
section), which is why no pin was red: the rule itself had legalized the
gap. This invariant is the corrected rule — see
`docs/audits/2026-09-02-deep-audit/tamper-evident.md` C-1 and
`lead-verification.md` C-Critical-1 for the reproduction.

**Why `min` of two figures and not either one alone**: the gate must run
before the transaction opens, because an `AuthVerifier` may be a remote call
and `financial.md` forbids external calls inside a transaction. So **V** is
authorized as of a moment that has already passed, and a journal committed
in that window leaves it wrong in whichever direction that journal moved
the balance. Each figure is blind to exactly what the other sees:

- A **genuine spend** committing in the window leaves V *stale-high*. E sees
  it, `E < V`, and the base is E. Without this term the fix for the
  tampering hole would have opened a TOCTOU hole instead: before it, the
  base was read under the lock (checkpoint-derived, but I-4-safe), and
  moving it outside the lock without an under-lock recheck is precisely the
  over-sell race I-4's advisory lock exists to close.
- A **forged, unsigned credit** committing in that same window raises E — E
  has no authorization check, by design, because it must not make an
  external call while holding the lock. Then `E > V` and the base is V, so
  the forgery buys nothing.

Taking the minimum is safe in both directions and needs no assumption about
which kind of journal landed. It can refuse a reservation that a perfectly
synchronized reader would have allowed; it cannot allow one that reader
would have refused. Verifying under the lock instead is not an option: it
would put the verifier call inside the transaction.

**Enforced by**: `postgres.ReserverStore.requireVerifiedAvailableBalance`
(V, outside the transaction),
`postgres.ReserverStore.sumAvailableFromEntriesWithQueries` (E, under the
advisory lock, on the caller's transaction) and
`postgres.ReserverStore.reserveWithQueries` (takes the minimum as
`availableBase` whenever the gate ran, in place of
`sumBalancesByRoleWithQueries`).

**Pinned by**:
- `postgres.TestReserve_RequireVerifiedBalance_RejectsInflatedCheckpoint` —
  every journal genuinely signed, `balance_checkpoints` inflated by
  1,000,000 via direct SQL: a gated `Reserve` for 500,000 must fail with
  `core.ErrInsufficientBalance`. Two controls make the pin load-bearing:
  a gated reservation for an amount the real balance covers still succeeds
  (the gate is not refusing everything), and the same 500,000 *without* the
  flag succeeds (the tampering is genuinely in effect, and is exactly what
  the pre-fix gated path paid out).
- `postgres.TestReserve_RequireVerifiedBalance_RechecksUnderLock` — the
  `min(V, E)` half, with the window opened for real rather than simulated:
  a second goroutine posts a genuine signed spend of 950 on its own
  transaction (which takes the same `(holder, currency)` advisory lock),
  waits until the gated `Reserve` is observably queued behind that lock in
  `pg_locks`, and only then commits. V is therefore 1000 deterministically
  (the spend was uncommitted when the gate ran) and E is 50, so a 500
  reservation must be refused. Replacing the minimum with
  `*verifiedAvailableBase` reserves 500 against a balance of 50.

---
## I-50: "the sign convention has one implementation" is checked by machine, not by a list somebody maintains

(2026-09-02 audit A-M7 / A-M1 / A-N1 / A-M3 / A-N4,
`docs/audits/2026-09-02-deep-audit/financial-correctness.md`.)

I-43 collapsed seventeen copies of the normal_side sign convention into
`core.Sign` and `ledger_signed_amount`. What it did not do was make the
collapse *stay* collapsed: its "Enforced by" was a hand-written list of five
SQL files, and a hand-written list cannot notice the file nobody thought to
look at.

Two independent enumerations — the 2026-08-25 audit's "10 SQL expressions"
and commit `15d110e`'s "9" — both missed `postgres/sql/queries/balance_trends.sql`.
That file was the eighteenth copy and the only one whose *answer* was wrong:
it decided inflow-versus-outflow from `entry_type` alone, never joining
`classifications`, so for every debit-normal role account (`main_wallet`,
`locked` — the canonical user wallet) the direction was inverted. One JSON
row read `balance=395 inflow=105 outflow=500` for a holder who had deposited
500. `/holders/{h}/trends` served that to every consumer, while
`ListHolderTransactions` — reading the same entries through
`ledger_signed_amount` — reported the same deposit as an inflow. Two
user-facing surfaces of one ledger, disagreeing about which way the money
went, with only one of them protected.

The same failure mode, in a different expression: `balance_role NOT IN ('',
'memo')` is the predicate that answers both "what does the platform owe" and
"what money can the holder see". Four copies existed. The 2026-08-26 M-4 fix
updated one. Retagging `fee_expense` to `'memo'` then pulled it *into* the
three unfixed aggregates (`'memo' <> ''` is true), where `withdraw_fee`'s two
holder-side legs netted to exactly zero and the row was dropped as empty: a
holder's balance fell by 5 with no line anywhere in their statement to
account for it.

**Rule**: Neither the sign convention nor the holder-visible-money predicate
may acquire a second implementation without a human explicitly classifying
it. Concretely, all three of the following are enforced mechanically and fail
the build on anything unclassified:

1. **SQL.** No query in `postgres/sql/queries/*.sql` may derive an amount
   from `entry_type` outside `ledger_signed_amount` / `ledger_signed_delta`,
   unless the `(file, query name)` it appears in carries a written exemption
   AND the exemption's expression count still matches. Every current
   exemption has the same justification: the expression computes
   *debits minus credits*, or splits them into two columns, which is a
   statement about a journal balancing and is independent of any
   classification's `normal_side` by construction.
2. **Go.** No non-test file may branch on `core.NormalSideDebit` /
   `core.NormalSideCredit` outside `core.Sign`, unless the file carries a
   written exemption. Declaring a classification's polarity
   (`NormalSide: core.NormalSideCredit`, all of `presets/`) is the *input* to
   the convention, not a copy of it, and is not a branch.
3. **The predicate.** Every query filtering on `balance_role` to mean "money
   the holder can see / money the platform owes" spells it
   `balance_role NOT IN ('', 'memo')`, character for character, and the set
   of queries doing so is itself pinned. (`ReconcileRoleLessLiabilities`'s
   `balance_role = ''` is the deliberate inverse — a detector for *untagged*
   classifications — and is a different question, not a fork of this one.)

A stale exemption fails too: an exemption whose expression no longer exists
silently pre-approves whatever is written there next.

The list in I-43's "Enforced by" is now this gate's *output*, not its source
of truth. That inversion is the invariant.

**Enforced by**: `postgres/sign_authority_gate_test.go` — `sqlSignExemptions`
and `goSignExemptions` are the classification tables; anything not in them
fails. Runs under plain `go test ./...` with no path filter or build tag, so
it cannot be skipped by a CI job that forgot it.

**Pinned by**:
- `postgres.TestSignAuthorityGate_SQLHasNoUnclassifiedEntryTypeArithmetic` —
  self-proved by reverting `balance_trends.sql`'s two columns to the bare
  `CASE` they shipped with: the gate names the file, line and query.
- `postgres.TestSignAuthorityGate_GoHasNoUnclassifiedNormalSideBranch` —
  self-proved by adding a `NormalSide` comparison to `service/rollup.go` with
  its exemption removed.
- `postgres.TestSignAuthorityGate_HolderVisibleMoneyPredicateHasOneSpelling` —
  the four copies, counted per file, compared literally.
- `postgres.TestBalanceTrends_GapFill` — the direction itself, on the fixture
  that exposed it: a DR of 500 into a debit-normal available wallet is an
  inflow of 500 and an outflow of 0. (Before the fix: inflow 0, outflow 500.)
- `postgres.TestSettlementNettingViolations_ReportsTheSameSignAsGetBalance` —
  the reconciler's Finding and `GetBalance` describe one settlement position
  with the same sign. (Before the fix: `net=-40` against a balance of `40`.)
- `postgres.TestHolderStatement_ExplainsEveryMovementOfTheBalance` — stated
  as a property rather than a row list: the statement's net must equal
  `GetBalanceBreakdown().Total`, so ANY future divergence between the two
  scopes fails here whatever causes it. (Before the fix: total 395 against a
  statement netting 400.)
- `postgres.TestHolderCurrencies_IgnoresMemoOnlyCurrencies` — a currency the
  holder only ever paid a memo-tracked cost in is not one of their
  currencies.
- `postgres.TestPresetSolvency_EveryShippedTemplate` — one row per shipped
  template stating its exact effect on the solvency report. The sign errors
  this gate exists for were all found by *measuring solvency*, and the
  reason they survived was that `solvency_test.go` had a single case
  covering only the withdrawal path.

**Deliberately not enforced**: the gate matches text, not parsed SQL or Go
ASTs. It is tuned to be noisy about anything shaped like the bug and to force
a classification, not to be exact. Someone determined to evade it can, by
writing the expression in a shape it does not recognise — but they cannot do
it *by accident*, which is how all eighteen copies got there.
---

## I-51: A caller-supplied link on a journal is a claim the store verifies, never a label it records

`core.JournalInput` carries two links a caller can set by hand —
`ReversalOfUID` and `EventUID`. Both are read downstream as statements of
fact, so both are validated before anything is written, from every entry
point (library facade and HTTP alike).

### Rules 1–3: `reversal_of`

A journal carrying `reversal_of = J` must actually be a reversal of `J`:

1. **`J` is not itself a reversal.** Reversals are leaves; the chain is one
   level deep, which is what makes "everything that reverses `J`" a query
   rather than a traversal.
2. **Same-dimension inversion.** Every entry must invert an entry `J` actually
   has: same `(account_holder, currency, classification)`, opposite
   `entry_type`. An entry on a dimension `J` never touched — including one on
   a dimension `J` has but on the same side rather than the opposite — is
   refused (`ErrInvalidInput`).
3. **Within what is left.** Per dimension, `already reversed + this journal's
   amount ≤ J's original amount` (`ErrConflict`), aggregated at exactly the
   grain `cumulativeReversedByDimension` reads.

This holds no matter which API posts the reversal: `ReverseJournal`,
`ReverseJournalFraction`, or a `PostJournal` whose caller set
`core.JournalInput.ReversalOfUID` itself.

**Why**: `reversal_of` is not a decoration, it is an input to arithmetic.
`cumulativeReversedByDimension` treats every journal linked to `J` as
reversal history worth its own amounts, and `ReverseJournalFraction(J, 1, 1)`
— "reverse everything remaining" — computes `original − already reversed`
from it. So one unvalidated link is enough to make a full reversal reverse
less than everything and return `nil`:

```
post 100                                   balance = 100
PostJournal{reversal_of: J, four net-zero legs}   balance = 100   err = <nil>
ReverseJournalFraction(J, 1, 1)                                   err = <nil>
  -> reversal journal posted: debit 50 / credit 50
after "fully reversed"                     balance = 50   <= expected 0
CheckAccountingEquation  balanced=true gap=0
ReconcileAccount         balanced=true gap=0
```

Every defense in the ledger stays green there, because nothing about double
entry is broken: the middle journal is per-currency balanced, moves no money,
and its legs are all on dimensions `J` genuinely touches — only the
*directions* make it not a reversal. What is broken is the reversal chain, and
I-2's cumulative rule cannot see it: the total reversed is exactly 100, the
upper bound is respected, and 50 stays on the books. The caller is told the
journal was fully reversed.

The field is reachable from library mode (`svc.JournalWriter().PostJournal`),
which `CLAUDE.md` names as the preferred consumption mode; the HTTP surface
never accepted it. No attacker is needed — a consumer that runs its own
correction flow and tags the correcting journal with `reversal_of` for
auditability, which is what the field is for, lands on this.

### Rule 4: `event_uid`

A journal carrying `event_uid = E` must be about what `E` happened to:

4. **The event's dimension is present, and the link is free.** `E`'s booking's
   `(account_holder, currency)` must appear among this journal's entries, and
   `events.journal_id` must not already be set (`ErrInvalidInput` /
   `ErrConflict` respectively). Amounts and classifications are deliberately
   *not* constrained — fees, spreads and multi-leg settlements legitimately
   post more than the booking's own amount.

Same failure shape, wider reach: `event_uid` is a field on `POST /journals`,
so this one needs no library-mode consumer at all. Posting fills
`events.journal_id` and through it the booking's **set-once** `journal_id`
(see CLAUDE.md, "Event-Journal atomicity"), so before this rule any journal
could claim any stranger's event — after which the booking's real settling
transition fails with `ErrConflict` **permanently**, and an unrelated journal
stands as that booking's accounting record. Nothing detects it: the claiming
journal is balanced, and the booking simply never settles.

**Enforced by**:
- `validateReversalOfInput` (`postgres/reversal_fraction_store.go`), called
  from `postJournalWithQueries` (`postgres/ledger_store.go`) — the single
  choke point every journal insert passes through, so the reversal APIs
  re-validate their own derived entries there too.
- The `event_uid` branch of `postJournalWithQueries`, which resolves the
  event, refuses an already-linked one, and requires the booking's
  `(holder, currency)` pair among `balancePairsFromEntries(resolved)` —
  all before the first INSERT, so a refused claim leaves both `journal_id`
  columns untouched.
- `GetJournalForUpdateByUID` on the referenced journal, taken while resolving
  `reversal_of` and **before** the balance advisory locks, so the entries and
  reversal history the rules above read cannot change before this journal
  commits — the same row lock `ReverseJournal` and `ReverseJournalFraction`
  take, in the same order relative to the balance locks.

**Pinned by**:
- `postgres.TestPostJournal_ReversalOfUID_RejectsNonReversingEntries` — the
  repro above, asserted at the holder's balance (0 after "reverse everything
  remaining"), not at any internal shape.
- `postgres.TestPostJournal_ReversalOfUID_RejectsReversalOfAReversal`
- `postgres.TestPostJournal_ReversalOfUID_RejectsAmountBeyondRemaining` —
  including the positive case: a correctly shaped hand-written reversal within
  the remaining amount must still post.
- `postgres.TestPostJournal_EventUID_RejectsUnrelatedJournal` (rule 4) — a
  stranger's journal is refused, and then the booking's OWN journal still
  posts against the same event, proving the refusal consumed neither
  `journal_id`; a second claim on the now-linked event is refused too.
## I-52: The forward-scan cursor never outruns ingestion

`chain_cursors.last_scanned_block` advances for a window `[from, to]` only
after **every** deposit sighting `evm.Reader.FetchDeposits` returned for that
window has either been ingested successfully or been written to
`ingest_dead_letters`. A sighting whose ingestion failed in a way a retry
could fix leaves the cursor where it is, so the whole window is re-scanned on
the next tick (`IngestDeposit` is idempotent, so re-scanning has no other
effect). The cursor is also monotonic in storage: `SetChainCursor` only
applies a strictly greater block, so no replica can drag it backwards.

**Why**: below the cursor, nothing ever looks again. The forward scan starts
at `cursor + 1`; the pending/confirming recheck loop only revisits bookings
that already exist; a registration rescan only covers newly registered
addresses. So a sighting dropped while the cursor advanced past it is a real,
on-chain deposit that the ledger will never see again — the user's money is
on chain and absent from the books. Before this invariant (G-C2's sibling
G-C1, 2026-09-02 audit) an ingest failure produced one log line — into
`core.NopLogger()` by default — and the cursor advanced regardless, while
`Metrics.ChainCursorLag` kept reporting a healthy zero *because* the cursor
kept moving. The same logical action next door (`processRegistrationRescan`)
had had the correct semantics all along, which is what made this a
same-shape sibling rather than a novel bug.

The deliberate asymmetry: a sighting whose rejection is **deterministic**
(`core.IsRetryable` false — a payload conflict on an existing key, a currency
that was never registered, an amount finer than the currency's exponent) is
dead-lettered and then skipped. Holding the cursor for it would convert one
unbookable deposit into "this chain ingests nothing, ever again", and the
dead-letter row means it is recorded rather than lost. Everything else,
including an unclassified error, holds the cursor.

**Enforced by**:
- `service.Onchain.scanChainOnce` (`service/onchain.go`) — collects blocking
  failures and returns without calling `SetCursor`; classifies via
  `permanentIngestFailure` (`core.IsRetryable`); records deterministic
  rejections through `DeadLetterRecorder`. `Metrics.ChainCursorLag` is
  reported against the block actually scanned, so a held cursor shows up as
  a growing lag.
- `service.Onchain.escalateWatcherStall` — after
  `WithWatcherStallAlertAfter` consecutive failed ticks on one chain, every
  still-blocking sighting is dead-lettered and a wedged-watcher error is
  logged; the cursor still does not move.
- `service.Onchain.processRegistrationRescan` — same classification, same
  fail-closed advance semantics for the historical rescan path.
- `postgres.SetChainCursor` (`postgres/sql/queries/chain_cursors.sql`) —
  `WHERE chain_cursors.last_scanned_block < EXCLUDED.last_scanned_block`.
- `service.newWatchLockedJob` — the per-chain watch loop runs under
  `advisoryLockKey("job:onchain_watch:<chainID>")`, so concurrent replicas
  cannot undo a deliberately held cursor.

**Pinned by**:
- `service.TestOnchain_Watch_HoldsCursorWhenIngestFails` (a transient
  ingest failure leaves the cursor unset across two ticks, dead-letters the
  blocking sighting on reaching the escalation threshold, and books the
  deposit exactly once when ingestion finally succeeds)
- `service.TestOnchain_Watch_DeadLettersPermanentRejectionAndAdvances` (a
  deterministically unbookable sighting is recorded and only then skipped)
- `postgres.TestChainCursorStore_SetCursor_IsMonotonic`
- `service.TestOnchain_Watch_SkipsWhenAnotherReplicaHoldsTheLock` /
  `TestOnchain_Watch_RunsWhenLockIsFree`

---

## I-53: The forward scan stays behind the reorg-mutable tip

The watcher never scans (and therefore never marks scanned) a block newer
than `latest - Confirmations + 1`, where `Confirmations` is the chain's
configured confirmation threshold (`core.ChainConfig.Confirmations`, floored
at 1). The registration rescan path uses the same bound.

**Why**: `Confirmations` is the consumer's own statement of how deep a reorg
they expect to be surprised by. Scanning to the head marked blocks scanned
that a reorg can still replace, and the cursor never goes back (I-52): a
transfer that exists only in the replacement block — reordered in, or
included from the mempool by the replacement — had never been scanned and
never would be. One consequence for alerting: `Metrics.ChainCursorLag`'s
healthy baseline is `Confirmations`, not 0.

**Enforced by**:
- `service.Onchain.scanChainOnce` / `processRegistrationRescan` via
  `confirmationDepth(cfg)` (`service/onchain.go`).

**Pinned by**:
- `service.TestOnchain_Watch_NeverScansPastConfirmationDepth`
  (`LatestBlock` 1000 with `Confirmations` 12: the window handed to
  `FetchDeposits` ends at 989 at the latest, and the cursor never claims
  more)

## How to add a new invariant

---


## I-54: A job that is not running, a degraded mode that is in force, and a clone that has escaped its transaction are all observable without a logger

(2026-09-02 second-round audit: `consumer-surface.md` E-M1/E-M3/E-M4/E-M5,
`operability.md` I-11, `tamper-evident.md` R-3, `test-credibility.md` F-M1/F-M2,
`concurrency.md` B-M6/B-m1. Contract
`docs/plans/2026-09-02-remediation-contracts.md` §7.4, Wave 1 W1-facade.)

**Rule** (five properties, one task):

1. `service.Worker.Run` returns an error, having started nothing, when its
   logger is `core.NopLogger` — the default `ledger.New` installs — unless
   the consumer opted into silence with `ledger.WithSilentWorker()` /
   `Worker.AllowSilent()`. Every signal a worker produces (which optional
   jobs are on, whether the attestation chain has an external anchor, each
   job's per-tick failure) travels over `core.Logger` and nowhere else, so a
   worker booted under the silent default is indistinguishable from one that
   never booted.
2. What that startup log says is also available as data:
   `Worker.StartupReport()` returns the same facts (including
   `AttestationAnchor` and `LeaderElection`) plus a `Warnings` list, readable
   before `Run` and with no logger involved. A degraded-but-permitted mode —
   attestation with a nil anchor, no advisory-lock pool — is never reported
   only by a log line.
3. Violating a documented ordering constraint returns an error rather than
   logging one: `Worker.Subscribe` after `Run` is refused, because the
   `event_callback` loop's existence was decided at startup and the handler
   would never be invoked.
4. `(*ledger.Service).Worker` wires every job the library can wire on the
   consumer's behalf — partition management, the advisory-lock pool, the
   local event poller, the full reconciliation suite, and (when
   `WithAttestor` was used) batch attestation — and both `EventStore`
   instances it and `ledger.New` build route their claim-lost warnings
   through the injected `core.Logger`. `service.WorkerConfig`'s defaults are
   filled field-driven, not by a hand-written list, so a new field cannot
   silently leave its job disabled.
5. Every exported `*Service` method that reaches past a `RunInTx` clone —
   reads `s.pool`, or writes a `Service` field the clone discards — either
   branches on `s.tx` or declares its clone behaviour with a `clone-safe:`
   note in its doc comment. This replaces I-40 property 4's enumeration of
   three methods with a universal claim, checked mechanically against
   `ledger.go`'s AST. `Worker` and `RegisterChannel` are refused on a clone;
   `Pool` and `Ping` are declared (`Ping` now probes through `DBTX()`, so a
   clone retained past its callback reports unhealthy rather than
   contradicting its own data plane); `Onchain()` is readable on the clone
   and returns the top-level instance.

**Why**: the previous round's remedy for "`svc.Worker` silently disables
three jobs" was a startup log line — delivered over a channel that is off by
default, so the fix was as invisible as the bug
(`working-agreements.md` §3:未运行 ≠ 通过；降级必须落痕). The same shape
recurred three times independently in one round: `EventStore.SetLogger` was
added to route warnings to the consumer's logger and then never called from
the composition root; `SetFullReconciler` was never auto-wired even though
`DefaultWorkerConfig` has always advertised a `FullReconcileInterval`, so
sixteen reconciliation checks — `unauthorized_journals` among them — were off
for every library consumer; and I-40's clone list, being an enumeration,
never grew to include the fourth, fifth and sixth methods with the identical
defect. Property 5 exists because enumerated lists in this repository have
failed twice before by omission (README's API table, the sentinel-mapping
table) and were fixed both times by deriving them mechanically.

**Enforced by**: `service.Worker.Run`'s `core.IsNopLogger` refusal and
`Worker.StartupReport` (`service/worker.go`); `core.IsNopLogger`
(`core/logger.go`); `Worker.Subscribe`'s `running` check returning an error
under the mutex that now guards `localDeliverer`; `(*Service).Worker`'s
`s.tx` guard, its `SetFullReconciler` / `SetPartitionService` / `SetPool` /
`SetLocalPoller` / `SetLogger` wiring, and `ledger.New`'s
`eventStore.SetLogger`; `mergeWorkerConfig`'s field-driven fill;
`RegisterChannel`'s `s.tx` guard; `withTx` carrying `onchain`; `Ping` routing
through `DBTX()` (all `ledger.go`).

**Pinned by** (root package, cited bare per this doc's convention — see I-13):
- `TestServiceWorker_RefusesToRunUnderTheDefaultSilentLogger` — README's
  Quick Start wiring verbatim (`ledger.New(pool)` → `svc.Worker(...)` →
  `Run`) must return an error naming both `ledger.WithLogger` and
  `WithSilentWorker`; `TestServiceWorker_StartsAndReportsWhenALoggerIsInjected`
  is its control and additionally asserts the startup report's content.
- `TestServiceWorker_StartupReportIsReadableWithoutALogger` — a
  `WithAttestor` Service's Worker reports `Attestation=true`,
  `AttestationAnchor=false` and the anchorless warning as data.
- `TestServiceWorker_WiresTheAdvisoryLockPool` /
  `TestServiceWorker_ExtendsThePartitionHorizon` /
  `TestServiceWorker_WiresTheFullReconciler` — the three `Worker()` wiring
  lines that had no pin; the last asserts the suite's own per-check metric,
  not a wiring flag. Deleting any one line turns exactly one of them red.
- `TestServiceWorker_SubscribeAfterRunIsAnError`.
- `TestService_EventStoreClaimLostWarningsReachTheInjectedLogger` /
  `TestServiceWorker_EventPollerClaimLostWarningsReachTheInjectedLogger` —
  both `EventStore` instances, the second end to end through a blocked
  handler whose lease is stolen mid-flight.
- `TestService_WithAttestor_WiresTheWithdrawalGateVerifier` /
  `TestService_WithAttestor_GateRejectsAnUnverifiableJournal` — the
  `ledger.WithAttestor` → withdrawal-gate verifier wiring, reached only
  through `ledger.New` (`postgres.NewVerifiedBalanceStore` is never named);
  also listed under I-32 / I-33.
- `TestMergeWorkerConfig_FillsEveryField` /
  `TestMergeWorkerConfig_KeepsCallerValues` (internal test) — every
  `WorkerConfig` field ends non-zero and equal to its default, and explicit
  caller values survive.
- `TestCloneEscapeSurfaceIsDeclaredOrGuarded` — the AST gate for property 5,
  with `TestCloneEscapeScanner_CatchesAnUnguardedUndeclaredMethod` proving
  the scanner is falsifiable rather than vacuous (I-48's lesson).
- `TestService_RegisterChannel_RefusedOnTxBoundClone` /
  `TestService_Worker_RefusedOnTxBoundClone` /
  `TestService_Onchain_VisibleOnTxBoundClone` /
  `TestService_Ping_FollowsTheCloneTransaction` — each establishes a control
  on the top-level Service first, so the assertion isolates the guard.

## I-55: What the anchor said before is remembered, so an erased or rolled-back anchor is not read as a benign backlog

(2026-09-02 deep audit, `tamper-evident.md` M-3; remediation contract §7.7.)
`core.Anchor.Head` answers `(0, nil, nil)` both for "nothing was ever
published here" and for "what was published has been erased". Nothing in
either answer distinguishes them, so the distinction has to come from
somewhere else: `anchor_observations` (migration 018) is an append-only
record of every `(seq, head)` this deployment has observed the anchor
reporting, written by `service.AttestationService.catchUpAnchor` on each
successful `Head` read and after each successful `Publish`.
`service.VerifyLedger` compares the live head against `MAX(observed_seq)`:
lower than something we recorded seeing is `TAMPERED`, not `DRIFT`. See
I-28's table for the full classification.

**Why**: `DRIFT`'s own doc comment defined it as "a benign, expected
inconsistency" and `ledger-cli verify` exits 0 on it. So the cheapest
possible attack on P6 was not to forge anything -- it was to remove the
witness: `rm anchor.txt` on the dev carrier, or one `PutObject` with the
ledger's own token on the R2 carrier, after which the DB chain verified
against nothing and reported a benign catch-up. `working-agreements.md` §3
names this shape exactly: "未运行 ≠ 通过". An external check that did not
run must never resolve to the same verdict as one that ran and passed.

The memory is deliberately modest about what it proves. A party who can
rewrite `anchor_observations` can also erase the memory -- but that party
is the database-credential holder the external anchor exists to defend
against, the anchor still holds its own side of the evidence, and the
append-only trigger plus the missing UPDATE/DELETE grant mean the
application credential cannot do it at all. What this closes is the
asymmetry where the attacker had to compromise **nothing** in the database
to make an erased anchor look healthy.

**Enforced by**:
- Migration 018's `anchor_observations` -- `BIGSERIAL` + `uid`, two
  `ledger_block_mutation()` triggers (no UPDATE, no DELETE), and an ACL
  that grants `ledger_app` only `SELECT, INSERT` and `ledger_ro` only
  `SELECT`. Owned by `ledger_owner`, transferred inside the same temporary
  membership window 001 §14 uses.
- `service.AttestationService.catchUpAnchor` -- records the observation
  BEFORE acting on it, so a run that dies mid-catch-up still leaves the
  observation behind.
- `service.AttestationService.RunAttestBatch` -- records after a successful
  `Publish` too. Without that, the memory always lagged one batch behind
  and an anchor erased right after its first publish had nothing to
  contradict it.
- `service.VerifyLedger` -- reads `HighestObservedAnchorSeq` and fails
  closed (`NOT_RUN`) if that read itself fails: with the memory unreadable,
  this run cannot tell a rollback from a first read.
- `postgres.AttestationStore.RecordAnchorObservation` /
  `HighestObservedAnchorSeq`.

**Pinned by** (`service/attest_verify_anchor_test.go`):
- `TestVerifyLedger_AnchorRollbackToAnOlderSeqIsTampered` -- three
  attestations published, then the carrier rewritten out of band to seq 1.
  Pre-fix verdict: `DRIFT` ("anchor is behind the DB chain by 2
  attestation(s) (catch-up pending)").
- `TestVerifyLedger_EmptyAnchorWithNonEmptyChainIsNotRun` -- the anchor
  deleted after publishing seq 2. Pre-fix verdict: `VERIFIED`.
- `TestVerifyLedger_EmptyAnchorWithNoPriorObservationIsNotRun` -- the
  honest ambiguity: no observation on record, so `NOT_RUN`, not `TAMPERED`.
- `TestVerifyLedger_DriftOnlyWhenAnchorHasPublishedButLags` -- the control:
  `DRIFT` still exists and still means what it says.

## I-56: An anchor's head never regresses, and that is a machine-checked property of every implementation

(2026-09-02 deep audit, `tamper-evident.md` M-4 / m-2; extends I-48.)
`core.Anchor.Head`'s contract now includes: once `Head` has returned seq
N, no later call may return a seq lower than N. "The highest seq ever
published" is the contract; "the value of the last write" is not.
`anchortest` checks both halves -- `HeadNeverRegressesOnAnOlderPublish`
through the interface, and `HeadNeverRegressesAfterAnOutOfBandOlderWrite`
using the implementation's own client via `anchortest.WithOutOfBandWrite`,
because the attack does not go through `Publish`.

**Why**: I-28's DRIFT-vs-TAMPERED split, and therefore I-55, are only
meaningful if the carrier cannot walk its own head backwards. `anchortest`
previously declared seq ordering explicitly out of scope ("this suite
takes no position on that choice and does not test it") -- and under that
licence this library's own production carrier shipped with a `Head` that
read a single mutable object's CURRENT version. The object's past versions
were protected by Object Lock and read by nothing, so one out-of-band
`PutObject` of an older seq rolled the trusted head back. The property the
library depends on was the property its conformance suite had promised not
to check.

`anchors/r2` now stores one immutable object per seq
(`<Key>/seq-<20-digit>.json`, conditional create with
`If-None-Match: "*"`, plus a read-back comparison so a server that ignores
the header cannot turn a mismatched replay into a silent overwrite), and
`Head` lists the prefix and resolves `MAX(seq)`. Two honest boundaries,
both documented in that package and in `docs/RUNBOOK.md`:

- **Forward injection stays possible and stays loud.** Writing
  `seq-99999999.json` with garbage moves `Head` forward, and
  `VerifyLedger` reports "anchor knows about seq X but the DB chain only
  reaches Y" as `TAMPERED`. A forward jump cannot hide anything; only a
  backward move can.
- **Object Lock does not stop a delete marker.** Retention prevents
  deleting a specific version, not a plain `DELETE` that writes a marker
  and hides the object from a listing. The ledger-side credential
  therefore must not carry `DeleteObject` -- and if it somehow does, I-55's
  recorded observation still turns the resulting regression into
  `TAMPERED`.

**Enforced by**:
- `core.Anchor.Head`'s doc comment (the contract text itself).
- `anchortest.Check` / `anchortest.RunConformance` run the
  `HeadNeverRegressesOnAnOlderPublish` and
  `HeadNeverRegressesAfterAnOutOfBandOlderWrite` phases; the latter is
  SKIPPED (and reported as skipped, never as passed, via
  `anchortest.Skipped`) when the caller supplies no out-of-band writer.
- `anchors/r2.New` (with `anchors/r2.Config`) builds the `Anchor` whose
  one-object-per-seq layout and `ListObjectsV2`-based `Head` make a
  regression impossible to publish; `Head` also fails closed on a foreign
  object under its prefix rather than skipping it.
- `anchordev.LocalFileAnchor`'s existing refusal of any seq that is not
  `Head()+1` or an exact replay.

**Pinned by**:
- `anchortest.TestCheck_CatchesHeadRegression` /
  `TestCheck_CatchesOutOfBandHeadRegression` -- deliberately broken fakes
  prove both phases can fail (I-48's lesson: a check that cannot fail is
  not a check).
- `anchortest.TestCheck_OutOfBandPhaseIsSkippedNotPassedWithoutTheHook` --
  the phase must report as skipped, not silently pass.
- `anchors/r2.TestAnchor_Conformance` -- runs the full suite, out-of-band
  hook supplied, against MinIO with Object Lock AND a COMPLIANCE default
  retention (before this round the test bucket set no retention at all, so
  it ran plain S3 semantics while the code discussed WORM).
- `anchors/r2.TestAnchor_ObjectLockRefusesDeletingAPublishedVersion` --
  the retention is real and observable.
- `anchors/r2.TestAnchor_PublishIsCreateOnlyPerSeq` /
  `TestAnchor_HeadFailsClosedOnAForeignObject`.
## I-57: Every object in the schema belongs to `ledger_owner`, and a migration that creates one cannot forget to say so

(`docs/audits/2026-09-02-deep-audit/threat-model.md`, "007 的两个 SECURITY DEFINER 分区函数不属于 ledger_owner"; `docs/plans/2026-09-02-remediation-contracts.md` §4, D-M1.)

**Rule**: every relation (`r`, `p`, `S`, `v`, `m`) and every routine in schema
`public` is owned by `ledger_owner`. Not "should be", and not "at install
time": at every point after `postgres.Migrate` returns, checked by enumerating
`pg_class` and `pg_proc` rather than by trusting any migration to have done it.

**Why**: I-22 explains what ownership confers that `GRANT` cannot — `ALTER`,
`DROP`, `TRUNCATE` and trigger management are owner-gated and cannot be
granted. Two consequences follow, and 001_baseline's own header states both
before the schema goes on to violate them:

1. **A function's owner can rewrite its body.** Replacing
   `ledger_block_mutation`'s body with `BEGIN RETURN NEW; END` turns every
   append-only guarantee in this schema off, silently, while leaving every
   trigger in place and firing. 001 calls function ownership "part of the
   tamper-evidence story, not an afterthought to it".
2. **A `SECURITY DEFINER` function runs as its owner.** `ledger_app` holds
   `EXECUTE` on two of them (I-35). Whoever owns those functions is the
   privilege a leaked application credential actually reaches through them.

And a third, operational: the bootstrap credential keeps a permanent `ADMIN
OPTION` on `ledger_owner`, which 001 acknowledges and answers by telling
operators to rotate or retire that credential after install. `DROP ROLE`
refuses while the role still owns objects, so that advice was not executable.

**What was wrong**: 001 transfers ownership with a catalogue sweep at the
bottom of its own file, and argues — correctly, about itself — that "sweeping
the catalogue instead of a list of names is what makes both classes impossible
here". It made them impossible *inside 001*. Nothing swept anything created
afterwards, and a full-repo grep for `relowner`/`proowner`/`OWNER TO` returned
only 001's own up/down pair: no test, no other migration, nothing. Measured on
a clean install of 001–015: 4 tables, 4 sequences and 9 functions owned by the
migration credential, including both `SECURITY DEFINER` partition functions and
every guard and audit trigger function 003/006/010 introduced.

**Why it stayed true for two audit rounds**: `grant_coverage_test.go` is the
strictest gate in this schema — an unclassified new table fails it outright —
and it reads ACLs. Ownership is not an ACL. Neither is a function's `EXECUTE`
privilege, which is the other half of the same blind spot (I-22's grant
coverage note).

**Enforced by**:
- `ledger_resweep_ownership()` (`019_ownership_resweep.up.sql`) — the sweep
  extracted from 001 as an idempotent, callable function, skipping objects
  already owned by `ledger_owner`. That skip is load-bearing, not an
  optimization: the migration credential holds `SET` but not `INHERIT` on
  `ledger_owner`, so once an object's owner IS `ledger_owner` it no longer
  passes Postgres's ownership check for it and a re-issued `ALTER` fails.
- `SELECT ledger_resweep_ownership();` as the last statement of
  `021_least_privilege_hardening.up.sql`, and of every migration batch after
  it. Last, and once: transferring ownership is a one-way door within a run,
  so a sweep placed before a migration that still has to `REVOKE` or `CREATE
  OR REPLACE` a 001-created object breaks that migration. 019's header
  documents the manoeuvre for a future migration that must modify an
  already-transferred object.
- `schema_migrations` is the sweep's one named exclusion, with its reason in
  the function body: golang-migrate creates it as the runner and 001 re-grants
  the runner access through a temporary membership upgrade only the bootstrap
  credential can perform. It is already `ledger_owner`'s after 001, so the
  exclusion costs nothing and the pin asserts that rather than assuming it.

**Pinned by**:
- `postgres.TestObjectOwnership_EverythingInPublicBelongsToLedgerOwner` —
  enumerates every relation and routine and fails on any not owned by
  `ledger_owner`. ⚠️ Expected to go red on a migration that creates an object
  and does not end with the sweep. The fix is the call, not an exception.
- `postgres.TestObjectOwnership_SecurityDefinerFunctionsRunAsLedgerOwner` —
  the same property stated as the thing that matters: every `prosecdef`
  function's owner, since that is the privilege its callers reach.
- `postgres.TestPartitionFunctions_OwnedByLedgerOwner` — I-35's own missing
  assertion, stated where its other pins live.
- `postgres.TestMigrate_InstallsUnderNonSuperuserBootstrapCredential` — the
  end state must be identical regardless of which credential installed it.

## I-58: A change that a guard lets through is recorded, and the record is not writable by the role it is about

(`docs/audits/2026-09-02-deep-audit/threat-model.md`, "account_policies 的守卫白名单恰好放行了它唯一要保护的三个风控开关" / "006 新建的两张审计表对 ledger_app 开放 INSERT"; `docs/plans/2026-09-02-remediation-contracts.md` §4, D-M3 / D-M4 / D-m10.)

**Rule**, in three parts:

1. **Coverage is derived, not listed.** Every table carrying a *partial*
   guard — a `BEFORE UPDATE` row trigger that is not the blanket
   `ledger_block_mutation()` refusal — carries an `AFTER UPDATE` trigger
   writing the before/after row into `config_table_changes`. "Partial guard"
   means some updates are meant to pass, and by construction the guard records
   none of the ones that do; that is exactly the population that needs a
   second layer. The predicate is I-22's ACL derivation run backwards, and it
   selects eleven tables where migration 006 named four.
2. **The record names the role that made the change, and `ledger_app` cannot
   write it.** The two DB-written audit tables (`config_table_changes`,
   `reconcile_scan_cursor_changes`) grant `ledger_app` no `INSERT` and no
   sequence `USAGE`; their trigger functions are `SECURITY DEFINER` owned by
   `ledger_owner` (I-57), and they record `session_user` — the role that
   authenticated, which `SET ROLE` cannot move.
3. **Somebody can read it.** `core.ConfigChangeReader`, reachable as
   `svc.ConfigHistory()`, queries all three forensic tables with filters and
   keyset paging.

**Why**: `account_policies` is the case that makes all three necessary at
once. It is the only DB-enforced freeze/overdraft floor, and its guard's
whitelist necessarily contains `status`, `min_balance` and
`enforce_min_balance` — `UpsertAccountPolicy` writes them. So the attack from
the previous audit round ("风控冻结、透支下限都可被一条 UPDATE 取消") still
succeeds verbatim, measured as `ledger_app`, and cannot be stopped at the
guard without breaking the legitimate write. Migration 006 then excluded the
table from the audit triggers, reasoning that it already had an
application-level trail in `account_policy_changes` — a table the application
writes, in the application's transaction, by the path an attacker with raw SQL
does not take. The result was the one table in the family that could neither
be stopped nor seen: zero rows in either trail after a successful unfreeze.

The two halves of part 2 are one finding, not two. An append-only table the
suspect can append to answers "who" with a value the suspect chose — measured
before migration 020, `ledger_app` inserted a row reading
`changed_by='ledger_owner'`, `changed_at` 30 days in the past — and can be
flooded until filtering by table or time is worthless. Making the writer
`SECURITY DEFINER` is only an improvement if the definer is the right role,
which is why this ships in the same batch as I-57.

Part 3 is not documentation polish. Migration 006 built the trail and nothing
read it: a full-repo grep outside migrations and `web/` found the table names
only in INVARIANTS prose and in tests. By `working-agreements` §3's own test —
if this step had never run, would anything visible be different? — the answer
was no on every surface an operator reaches. Evidence that cannot be read is
not detection.

**What is deliberately NOT claimed**: the application-written
`account_policy_changes` keeps its `ledger_app` `INSERT`, because it carries a
business `actor_id` no trigger can produce and revoking it would delete the
only operator attribution the ledger has. So the two trails answer different
halves — "which operator" and "which database role" — and only the second is
unforgeable. Read together, that asymmetry is itself the signal: a
`config_table_changes` row for `account_policies` with no matching
`account_policy_changes` entry is a change nobody in the application made.

**Enforced by**:
- `020_audit_trail_integrity_and_coverage.up.sql` — the catalogue-derived
  `DO` loop attaching the audit trigger, `SECURITY DEFINER` +
  `session_user` on both writer functions, and `REVOKE INSERT` /
  `REVOKE USAGE, SELECT` on the two tables and their sequences.
- `events` is the one carve-out, and it is a column set rather than a table:
  its delivery bookkeeping columns (`delivery_status`, `attempts`,
  `next_attempt_at`, `delivered_at`) are subtracted inside the `WHEN` clause,
  because they are rewritten on every poll and every retry and have nothing to
  do with the ledger's rules. A change to `journal_id`, `to_status` or
  `amount` — the columns 006 lists as the ones that must not be writable —
  still records. `bookings` and `reservations` are audited in full at business
  rate; that is a real capacity change and `docs/CAPACITY.md` sizes for it.
- The append-only guards from 006 still hold: audit rows cannot be updated or
  deleted, including by `ledger_owner`, whose privileges the `SECURITY
  DEFINER` functions now run with.

**Pinned by**:
- `postgres.TestPartialGuardTablesAreAudited` — derives the partial-guard
  population from `pg_trigger` and requires each to carry an audit trigger.
  ⚠️ Goes red when a migration adds a partial guard without one.
- `postgres.TestAccountPolicyEnforcementKnobChangeIsAudited` — runs the
  unfreeze + overdraft-floor statement as `ledger_app`, requires it to
  SUCCEED (the guard has to permit it), and requires the resulting audit row
  to carry the before/after and `changed_by='ledger_app'`.
- `postgres.TestLedgerAppCannotWriteTheAuditTrail` — the three forge attempts
  (both tables, plus `nextval` on the sequence), each measured as succeeding
  before 020, plus a subtest requiring the legitimate trigger path to still
  write, so that "the audit trail went quiet" cannot pass as a fix.
- `postgres.TestAuditTrailRowsStayImmutable` — `UPDATE`/`DELETE` refused on
  all three tables, run as the owner rather than as `ledger_app` so that an
  ACL refusal cannot stand in for the trigger's.
- `postgres.TestConfigHistory_ReadsBackATamperedRule` /
  `postgres.TestConfigHistory_ReadsScanCursorWrites` — tamper, then read the
  tamper back through `ledger.New(pool)` → `ConfigHistory()`, not off the
  tables. The first also asserts the absence described above: the same edit
  leaves no `account_policy_changes` row.

## I-59: A period close serializes against in-flight journal writes, and a journal that lands behind an active close line is observable

(2026-09-02 second-round audit: `concurrency.md` B-M5. Contract
`docs/plans/2026-09-02-remediation-contracts.md` Wave 2 D-lock.)

**Rule** (two halves, and the second is not optional):

1. **Barrier.** Every journal write path takes the *shared* period-close
   advisory lock (`AcquirePeriodReadBarrier`, key
   `hashtextextended('period:close', 0)`) in its own transaction immediately
   before reading the active close line, and holds it until that transaction
   ends. `PeriodCloseStore.ClosePeriod` takes the *exclusive* half before its
   INSERT. A close line therefore cannot become active while a writer that
   has already passed the gate is still in flight — the close waits for
   every such writer to COMMIT or ROLLBACK.
2. **Observable.** The `period_close_violations` reconciliation check counts
   journals whose `effective_at` is behind the active close line *and* whose
   `created_at` is later than that line's — non-zero is a finding. A barrier
   with nothing that can falsify it is an assertion, not a control
   (`working-agreements.md` §3).

**Why**: I-15 promised that last month's books are final. Its enforcement was
one plain READ COMMITTED read, and `ClosePeriod` participated in no lock at
all, so "the books are closed" held only for writers whose transactions had
not yet started. Real money follows: a backdated journal landing behind a
committed close silently changes a period whose reports were already
published, and the correction path I-2 mandates (reverse at the current open
date) was never taken because nobody knew.

**Enforced by**:
- `postgres/sql/queries/periods.sql` — `AcquirePeriodReadBarrier` (shared)
  and `TryAcquirePeriodCloseBarrier` (exclusive). The exclusive half is the
  non-blocking `pg_try_advisory_xact_lock`, polled by
  `postgres.acquirePeriodCloseBarrier` within
  `periodCloseBarrierBudget`. This is deliberate, not a shortcut: a
  *waiting* exclusive request is queued in PostgreSQL's lock manager ahead
  of subsequent shared requests, which would make every write path's
  internal lock order (balance locks, idempotency locks, journal row locks —
  and they do not all agree) a deadlock question with respect to this
  barrier. A non-blocking request never enters the wait queue, so the
  barrier is order-free: `postJournalWithQueries` is the only place that
  needs to know it exists.
- `postgres.LedgerStore.postJournalWithQueries` — takes the shared barrier
  immediately before `GetActivePeriodClose`; every write path funnels
  through this method.
- `postgres.PeriodCloseStore.ClosePeriod` — opens its own transaction in
  pool mode (the barrier is transaction-scoped; a bare autocommit INSERT
  would release the lock before the INSERT it is meant to protect) and
  returns `core.ErrTransient` naming the reason if the budget expires. The
  close line is **not** appended in that case.
- `service.FullReconciliationService.runCheckPeriodCloseViolations` — the
  `period_close_violations` check, backed by the `PeriodCloseViolations`
  query.

**Known resolution limit of the check** (stated because a blind spot
documented only in SQL will be read as unconditional): `journals.created_at`
and `period_closes.created_at` both default to `now()`, which in PostgreSQL
is the *transaction start* time. A writer whose transaction began before the
close line's transaction but committed after it compares as "written before
the close" and is not reported — precisely the TOCTOU the barrier closes,
which the check therefore cannot independently confirm. Confirming it would
need commit timestamps (`track_commit_timestamp`, off by default) or a
per-journal record of the close line it was checked against. What the check
does catch, and what nothing in the suite did before, is any journal that
reached the table without passing the gate at all: a raw INSERT, a future
write path that forgets the check, or a reopen/re-close sequence that leaves
history on the wrong side of the line.

**Residual, accepted**: a caller who composes `ClosePeriod` into a `RunInTx`
that also takes ledger locks holds the exclusive barrier while waiting for
them; a concurrent writer holding one of those locks while waiting for the
shared barrier then closes a genuine wait-for cycle. PostgreSQL's deadlock
detector reports it (40P01 → `normalizeStoreError` → `core.ErrTransient`), so
the outcome is a retryable error — never a journal that slipped past a
committed close line.

**Pinned by**:
- `TestClosePeriod_WaitsForInFlightBackdatedJournal` — the
  concurrency pin, driven from `ledger.New(pool)` + `RunInTx`: a close racing
  an in-flight backdated posting must not return before that writer's
  transaction resolves. Removing either half of the barrier makes it red.
- `TestClosePeriod_RejectsAfterBarrier` — the barrier does not change
  the ordinary path: a close with nothing in flight still succeeds promptly,
  and a later backdated posting is still refused.
- `TestReconcile_PeriodCloseViolations_ReportsForgedBackdatedJournal`
  — the check is registered in the suite, stays green for the normal state
  of a closed period, and reports a journal forged straight into the table
  behind the line.

## I-60: A panicking job tick, or a panicking Subscribe handler, never terminates the process

(2026-09-02 second-round audit: `operability.md` I-9 / I-10. Contract
`docs/plans/2026-09-02-remediation-contracts.md` Wave 2 D-ops.)

**Rule**: Every place this library runs consumer-supplied or third-party-
implementation-dependent code on a background tick — `service.Worker`'s
scheduled jobs, `service.Onchain`'s five chain jobs (a `ChainReader`/
`ChainScanner`/`Sweeper` bug reaches here), and a `Worker.Subscribe` handler
delivered through `service/delivery.LocalDispatcher` — recovers a panic from
that one call, converts it into an ordinary failure signal (a logged error
plus, for job ticks, `core.Metrics.JobPanicked`; for a Subscribe handler, an
ordinary handler error that schedules a retry exactly like a returned
`error` would), and continues running everything else. None of the three
call sites shares the same recover — `Worker` and `Onchain` each run their
own scheduling loop (`Onchain`'s five jobs do not route through `Worker`'s
loop at all) — so this is one rule enforced at three independent call sites,
not one mechanism inherited by all three.

**Why**: before this, a panic at any of these three points propagated
straight out of `Run`/`ProcessBatch`. `Worker.Run` and `Onchain.Run` are
both documented as typically launched `go worker.Run(ctx)` — an unrecovered
panic in a goroutine is fatal to the whole process, taking down every other
job and every in-flight request with it over one bug in a single job's tick
or one consumer's `Subscribe` handler. A webhook handler with the identical
bug only ever surfaces as an HTTP 500 (`middleware.Recoverer` on the HTTP
path); a `Subscribe` handler had no equivalent floor.

**Enforced by**: `service.Worker.safeRun` (unexported; wraps every
`runLoop` tick, reached through the exported `service.Worker.Run`),
`service.Onchain`'s own `safeRunTick` (unexported; same shape, independently
implemented because `Onchain` keeps its own scheduling loop rather than
routing through `Worker`'s), and `service/delivery.CallbackDeliverer`'s
per-handler recover inside its `Deliver` method — reached from the exported
`delivery.LocalDispatcher.ProcessBatch`, the entry point a consumer actually
calls.

**Pinned by**:
- `service.TestWorker_JobPanic_DoesNotCrashProcess` — a queuer that panics
  on its first call, run through `Worker.Run` end to end; asserts `Run`
  returns cleanly via context cancellation (not a propagated panic) and that
  `JobPanicked("rollup")` was emitted. Removing `safeRun`'s `recover()`
  crashes this test's own process instead of failing it.
- `service.TestOnchain_RunLoop_PanicDoesNotCrashProcess` — same shape
  against `Onchain`'s independent loop, proving the recover was not
  inherited from `Worker`.
- `delivery.TestLocalDispatcher_ProcessBatch_HandlerPanicIsRecovered` — a
  Subscribe handler that panics, driven through `LocalDispatcher.ProcessBatch`:
  asserts the call does not panic, the event is scheduled for retry (not
  silently dropped), and `EventDeliveryFailed` is emitted exactly as an
  ordinary handler error would produce.

## I-61: `core.Metrics`'s declared surface has no method without a production call site, or an explicitly tracked reason it does not yet have one

(2026-09-02 second-round audit: `operability.md` I-1 / I-10. Contract
`docs/plans/2026-09-02-remediation-contracts.md` Wave 2 D-ops.)

Before this wave, 12 of `core.Metrics`'s (then) 32 methods had zero
production call sites — the entire `postgres/` write layer had no
`core.Metrics` dependency at all, so `JournalPosted`, `JournalFailed`,
`ReserveCreated`/`Settled`/`Released`, and `IdempotencyCollision` were
declared, documented, and permanently silent. A consumer who read the
interface's doc comments and wired a dashboard against them would have
waited forever for a signal this library was never going to send. The wide,
one-method-per-signal shape `core.Metrics` deliberately uses (rather than a
handful of generic `Counter`/`Gauge` calls, see its own doc comment) makes
this failure mode easy to introduce silently: adding a method is a
compile-time no-op everywhere else, so nothing forces the corresponding call
site to exist.

**Rule**: every method `core.Metrics` declares has at least one call site
under non-test `service/` or `postgres/` source, **or** is named in
`observability/emission_coverage_test.go`'s `crossBranchExclusions` map with
the reason it does not yet have one and who owns closing the gap. An
exclusion is not a permanent carve-out: a second gate
(`TestCrossBranchExclusionsAreStillActuallyMissing`) fails the moment a call
site for an excluded method actually appears, forcing its removal from the
map instead of letting it rot into a silent, no-longer-true exemption.

**Why this is enforced by reflection against the interface, not a
hand-maintained list**: I-50 already states the general lesson for this
repository — a hand-written enumeration cannot notice the member nobody
thought to look at. `reflect.TypeOf((*core.Metrics)(nil)).Elem()`'s method
set is the interface's actual shape; the check tracks it automatically as
methods are added, rather than needing its own list edited in lockstep.

**Enforced by**: `observability/emission_coverage_test.go` — reflects the
metrics interface's live method set (not a hand-copied name list), walks
`service/` and `postgres/` non-test source as text for `.MethodName(`, and
checks the result against the `crossBranchExclusions` map, whose own doc
comment states the governance rule: entries are removed once their call site
lands, never added to silence a newly-introduced gap. Mirrors I-50's
inversion — the exclusion map is this gate's *output*, not a list someone
maintains by hand.

**Pinned by**:
- `observability.TestEveryMetricsMethodHasAProductionCallSite` — the gate
  itself; reverting any one of the postgres-layer `WithMetrics` wiring
  changes (I-M1) reproduces the exact 12-method failure this invariant
  describes.
- `observability.TestCrossBranchExclusionsAreStillActuallyMissing` — the
  anti-rot half: currently holds `ReservedAmount` and `PendingEvents` (both
  declared, neither wired — each needs a new aggregate query beyond this
  wave's merge budget, tracked in `TODO.md`'s breaking-change list), and
  fails if either gets a call site without being removed from the map first.
