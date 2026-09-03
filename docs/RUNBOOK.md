# Operational Runbook

This document is for the on-call engineer responding to a ledger alert. Each
section answers: *what does the alert mean, how do I confirm it's real, and
what do I do?*

If you're new, read [`INVARIANTS.md`](./INVARIANTS.md) first — every alert in
this runbook corresponds to a violated or at-risk invariant from that document.

---

## Table of contents

1. [Reconciliation failed](#1-reconciliation-failed)
2. [Solvency check failed (custodial < liability)](#2-solvency-check-failed)
3. [Rollup queue is backlogged](#3-rollup-queue-is-backlogged)
4. [Checkpoint age is climbing](#4-checkpoint-age-is-climbing)
5. [Webhook delivery backlog](#5-webhook-delivery-backlog)
6. [Idempotency collision spike](#6-idempotency-collision-spike)
7. [Journal posting failures](#7-journal-posting-failures)
8. [Common investigation queries](#8-common-investigation-queries)
9. [Emergency: stop the ledger](#9-emergency-stop-the-ledger)
10. [Deployment security boundary](#10-deployment-security-boundary)
11. [Partition management & archival](#11-partition-management--archival)
12. [Deep reorg on a confirmed crypto deposit](#12-deep-reorg-on-a-confirmed-crypto-deposit)
13. [Large / unreconciled deposit parked in review](#13-large--unreconciled-deposit-parked-in-review)
14. [Onchain money-path metrics -- this library ships none of the alerting](#14-onchain-money-path-metrics----this-library-ships-none-of-the-alerting)
15. [A chain's sweep collection has stopped moving](#15-a-chains-sweep-collection-has-stopped-moving)
16. [P5 signing key rotation](#16-p5-signing-key-rotation)
17. [A booking's event was claimed by the wrong journal](#17-a-bookings-event-was-claimed-by-the-wrong-journal)
18. [A deposit was dead-lettered](#18-a-deposit-was-dead-lettered)

Backup & disaster recovery (PITR, RPO/RTO, restore drill) lives in its own
document: [`DR.md`](./DR.md).

---

## 1. Reconciliation failed

**Alert source**: `ledger_reconciliations_completed_total{success="false"}`
Prometheus counter or the `POST /api/v1/reconcile` endpoint returning
`balanced: false`.

**Severity**: P1. The ledger is reporting an internal accounting violation.

### Confirm it's real

```bash
# The global accounting equation only -- shape: {balanced, gap, details}
curl -X POST http://ledger/api/v1/reconcile | jq .

# The full check suite -- shape: {overall_passed, full_coverage, skipped_checks, checks[]}
ledger-cli reconcile --full --pubkey-hex <hex> --key-id <id>
```

⚠️ **These two return DIFFERENT shapes**, and everything below about
`checks[]` only applies to the second (`docs/api.md` documents the same split
for `POST /reconcile` vs `POST /reconcile/full`). `POST /api/v1/reconcile`
answers `{balanced, gap, details}` and has no `checks[]` at all.

A real failure of the first includes a `details[]` array naming the
dimension(s) that drift. Each detail has `expected`, `actual`, and `drift` —
do not panic at a sub-cent drift; check the `drift` magnitude first.

**Read `skipped_checks` before you read `overall_passed`.** A check that was
not RUN in this deployment does not appear in `checks[]` at all -- it is
named in `skipped_checks`, and `full_coverage` is false. So
`overall_passed: true` with a non-empty `skipped_checks` is not a clean bill
of health, it is a clean bill of health *for the checks that ran*:

```bash
ledger-cli reconcile --full --pubkey-hex <hex> --key-id <id> \
  | jq -e '.overall_passed == true and .full_coverage == true and (.skipped_checks // [] | length) == 0'
```

The common cause of a non-empty `skipped_checks` is exactly the flags above:
`unauthorized_journals` (I-32) is the ledger's only forgery detector, and it
cannot run without a `core.AuthVerifier`, which `ledger-cli` only wires when
given `--pubkey-hex` and `--key-id`. **A skipped check is also invisible to
`ledger_reconcile_check_results_total`** -- the series does not exist rather
than reporting a failure -- so a Prometheus rule written as
`... == 0` will not fire for it. Alert with `absent(...)` too, or assert on
`skipped_checks` from the report as above.

### Investigate

The reconcile checks (`service.FullReconciliationService`, in
`service/reconcile.go`; report field is `checks[].name`; the exact count
lives only in `TestFullReconciliation_AllPass`, not in this table — add a
row here when you add a check, but don't expect this table to be exhaustive
proof of the count):

| Check name | Means |
|------------|-------|
| `global_dr_cr_equality` | Σ(debits) = Σ(credits) globally, per currency — does NOT verify any individual journal (see `journal_dr_cr` below for that) |
| `checkpoint_balance` | fleet-wide, resumable scan: each (holder, currency) checkpoint vs. a full recompute from entries (I-23; C4b persisted cursor + `lap_dirty` in `reconcile_scan_cursors`) |
| `orphan_entries` | journal_entries with no matching journal |
| `accounting_equation` | Σ(debit-normal net) = Σ(credit-normal net) per currency |
| `settlement_netting` | settlement classification cleanly nets to zero outside the grace window |
| `non_negative_balances` | no holder > 0 has balance < 0 |
| `role_less_liability` | no user-side (holder > 0), non-system classification with a nonzero balance is missing a `balance_role` (M-4/I-37) — such a balance is silently excluded from `SolvencyReport.Liability`. Not limited to credit-normal: `main_wallet`, the canonical real liability, is debit-normal |
| `untagged_holder_kind` | no journal type visible in the holder transaction view (posted against a role-bearing classification for a user holder) is missing a `holder_kind` (M-7 follow-up/I-44) — no financial consequence (its transactions already read the disclosed `kind: "other"` fallback), but nothing else surfaces the gap |
| `orphan_reservations` | reservations with no matching journal |
| `idempotency_uniqueness` | duplicate `idempotency_key` (should be 0; UNIQUE index prevents) |
| `stale_rollup_queue` | rollup queue items unclaimed for too long |
| `journal_dr_cr` | genuine per-journal, per-currency balance (M1/I-24) — catches two journals that are each individually unbalanced but net to zero in aggregate, which `global_dr_cr_equality` structurally cannot see |
| `system_rollup_integrity` | `system_rollups.total_balance` vs. a fresh recompute from entries directly — never via checkpoints, which is the pollution source `system_rollups` would otherwise inherit (I-23) |
| `snapshot_integrity` | `balance_snapshots` for the most recent `snapshot_date` vs. a fresh recompute from entries (I-23) |
| `unauthorized_journals` | samples journals claiming a P5 signature and re-verifies it (I-32). Without a `core.AuthVerifier` (wired via `SetAuthCheck`, or `ledger-cli reconcile --full --pubkey-hex/--key-id`) this check **does not appear in `checks[]` at all** -- it is named in `skipped_checks` and `full_coverage` goes false. Never-signed journals are a coverage gap, not tamper evidence, so they are skipped rather than flagged |

Match the failing check's `name` to the entries in `checks[].findings`. Then:

- **`orphan_entries`** — almost always a manual `DELETE FROM journals`
  that didn't cascade. Restore from backup or post a correcting reversal.
- **`accounting_equation`** — the headline disaster. Stop accepting
  new writes (see §9), bisect the journal_id range to find the broken
  journal, post a reversal once identified.
- **`global_dr_cr_equality`** — same class of disaster as `accounting_equation`,
  at fleet-wide granularity. Stop new writes, bisect by date (see "Common
  queries" below), post a reversal once identified.
- **`journal_dr_cr`** — a SPECIFIC journal is unbalanced by currency (I-1/I-24).
  The finding logs the offending `journal_id`/`currency_id` internally (never
  in the report itself, I-18) — check server logs for
  `"service: reconcile: unbalanced journal sample"`. Post a reversal for that
  journal; do not touch `journal_entries` directly. If this fires at all on a
  journal newer than migration `044`, treat it as a security incident: the
  DB-layer trigger should have rejected the insert outright.
- **`settlement_netting`** — usually a stuck FX or transfer leg
  (one side posted, the other didn't). Check `journals` table for orphan
  half-pair on the `settlement` classification.
- **`non_negative_balances`** — a user got debited beyond their balance.
  Usually a missing `Reserve` step. Find the journal that drove the balance
  negative; reverse it; investigate the calling service.
- **`role_less_liability`** — a deployer created a non-system classification
  (posted to a user holder, so it looks like a liability) without tagging it
  with a `balance_role`. This fires regardless of `normal_side` -- this
  library's real liabilities are not all credit-normal (`main_wallet`, the
  canonical one, is debit-normal). This is not corrupted data — the finding's
  `balance` figure is real and correct — the problem is that
  `SolvencyReport.Liability` currently excludes it entirely, so solvency
  reports look better than they are by exactly that amount. Fix by tagging
  the classification's `balance_role` via `ClassificationStore.SetBalanceRole`:
  `available`/`pending`/`locked` if it should count as spendable-money the
  user can reserve against, or `memo` if it is a deliberate non-liability
  cost/memo account (the `fee_expense` shape) — or confirm it should have
  been `is_system` instead (recreate/relabel per your migration process —
  this library never mutates `is_system` after creation).
- **`untagged_holder_kind`** (M-7 follow-up, I-44) — a journal type that
  shows up in a holder's transaction list has no `holder_kind`. Not a
  correctness bug — the finding's journal type code and uid are real, and
  its transactions already render `kind: "other"` on the wire, the same
  disclosed fallback a deliberately-untyped journal type would produce — the
  gap is purely visibility: nothing else tells you this happened. Fix by
  tagging it via `JournalTypeStore.SetHolderKind` with whichever
  `core.HolderTxKind` fits (`deposit`/`withdrawal`/`transfer`/`fee`/
  `adjustment`), or leave it untagged if `other` genuinely is the right
  bucket — this check will keep re-flagging it either way until you call
  `SetHolderKind` once, even if `other` was the deliberate answer.
- **`checkpoint_balance` / `system_rollup_integrity` / `snapshot_integrity`
  (checkpoint / system_rollups / balance_snapshots drift, I-23)** —
  **do not** just re-run reconcile and move on: these three all mean a
  materialized cache disagrees with `journal_entries`, which is exactly the
  class of drift a leaked DB credential (direct `UPDATE`) would produce.
  1. Read the finding's `detail` for the affected dimension and drift amount.
  2. Confirm it's real: `journal_entries` is the ground truth. Recompute by
     hand with the query in §8 ("Compute live balance for an account") and
     compare against the checkpoint/rollup/snapshot value the finding cites.
  3. If real, treat it as a security incident, not routine drift — determine
     whether it is isolated arithmetic error (rollup worker bug) or a sign of
     unauthorized DB writes (audit `pg_stat_activity` / connection logs / who
     has `ledger_app` or superuser credentials) before repairing anything.
  4. **Repair, don't just re-run**: `balance_checkpoints` drift is fixed via
     `CheckpointIntegrityStore.RebuildCheckpoint(ctx, holder, currencyUID,
     classificationUID, actorID)` (locks the dimension, refuses with
     `ErrRollupPending` if a rollup item is still in flight — drain it first,
     `service/reconcile.go` check #10 / §3 of this runbook). `system_rollups`
     self-heals on the next `RefreshSystemRollups` tick once the checkpoints
     it sums are correct. `balance_snapshots` drift for a historical date is
     fixed via `SnapshotBackfillService.BackfillSnapshots(fromDate, toDate)`.
     **None of these run automatically** — detection (reconcile) and
     correction are deliberately separate so an active incident's evidence
     isn't destroyed by an auto-repair.
  5. **A manual repair is not evidence-free either.** The moment
     `RebuildCheckpoint` overwrites the checkpoint, the drift is gone from
     `balance_checkpoints` — same evidence-destroying shape as an automatic
     repair would have, just with a human in the loop. Every call durably
     records itself (before/after balances + watermarks + drift + `actor_id`)
     in the append-only `checkpoint_rebuilds` table, in the same transaction
     as the overwrite. Pull the full history for a dimension before or after
     repairing it:
     ```sql
     SELECT * FROM checkpoint_rebuilds
     WHERE account_holder = $1 AND currency_id = $2 AND classification_id = $3
     ORDER BY created_at DESC;
     ```
     A non-zero `drift` row is the durable proof a poisoned checkpoint
     existed — attach it to the incident postmortem, not just the log line.
- **`unauthorized_journals`** (I-32) — a sampled journal claims a P5
  signature (`auth_status = signed`) but fails `core.VerifyJournalAuth` on
  re-check: treat this as a confirmed forgery, not drift. It means an
  attacker (or a bug) produced a journal that looks signed without holding
  the `core.Attestor`'s private key. Stop new writes for the affected
  dimension, identify every entry the forged journal touched, and check
  `postgres.VerifiedBalanceStore.VerifiedBalance` / the `RequireVerifiedBalance`
  `Reserve` gate (I-32) before paying anything out of that dimension. If this
  check is **missing from `checks[]`** (and named in `skipped_checks`), it
  means no `core.AuthVerifier` was wired via `SetAuthCheck` for this run —
  that is a coverage gap in the reconcile job's own configuration, not a
  finding about the ledger, and it is not something you will find by looking
  for a failed entry inside `checks[]`. Do not read the absence as a pass.

### `reservation discharge claim does not verify` (Warn log, I-65)

Not a reconcile check — a **log line only**, emitted by the gated `Reserve`
path when a `reservation_operation_receipts` or
`reservation_settlement_legs` row fails `core.VerifyReservationDischargeAuth`.

It is a log line rather than an error on purpose: the gate's reaction is to
keep holding that reservation's full `reserved_amount`, which is the safe
answer, and the caller only sees a smaller available balance. Making it an
error would let a single forged `INSERT` render a holder permanently
un-reservable. So this Warn is the only evidence the event happened.

Triage, in order:

1. **Was signing recently turned on, or is this an old row?** Claims written
   before migration `028`, and claims written from inside a consumer's own
   transaction (`Settle`/`Release` composed inside `RunInTx`, which cannot be
   signed — I-65), carry no signature at all and log here legitimately. The
   log includes the `operation` and `idempotency_key`; check the row's
   `created_at` against the deployment's cutover.

    ```sql
    SELECT o.operation, o.idempotency_key, o.amount, o.created_at,
           length(o.auth_signature) AS sig_len, o.auth_key_id
    FROM reservation_operation_receipts o
    JOIN reservations r ON r.id = o.reservation_id
    WHERE r.uid = '<reservation_uid>'
    UNION ALL
    SELECT 'settle_partial', l.idempotency_key, l.amount, l.created_at,
           length(l.auth_signature), l.auth_key_id
    FROM reservation_settlement_legs l
    JOIN reservations r ON r.id = l.reservation_id
    WHERE r.uid = '<reservation_uid>'
    ORDER BY created_at;
    ```

    `sig_len = 0` on a row older than the cutover, or on one written through
    `RunInTx`, is expected. Nothing to do; the hold recycles at `expires_at`.

2. **`sig_len = 0` on a row that should have been signed** → treat as
   `unauthorized_journals` above: something appended a discharge claim
   without going through this library. Those tables refuse `UPDATE` and
   `DELETE` (migration `006`), so the row is preserved evidence.

3. **`sig_len > 0` but verification fails** → the row was written signed and
   then altered, which takes `ledger_owner` or a superuser. Confirmed
   tampering above the application credential. Escalate as for
   `unauthorized_journals`, and check `anchor_observations` / the attestation
   chain for the same window.

4. **`auth_key_id` names a key this deployment no longer holds** → key
   rotation (I-45), not tampering. The verifier must keep retired public keys
   for as long as any unexpired reservation's claims reference them.

### Common queries (Postgres)

```sql
-- Find the offending account dimension
SELECT account_holder, currency_id, classification_id,
       SUM(CASE WHEN entry_type='debit' THEN amount ELSE -amount END) AS net
FROM journal_entries
GROUP BY 1,2,3
HAVING SUM(CASE WHEN entry_type='debit' THEN amount ELSE -amount END) <> 0;

-- Bisect by date range
SELECT MIN(id), MAX(id), COUNT(*) FROM journals
 WHERE created_at >= now() - interval '24 hours';
```

### Resolution

Reconciliation drift is *symptom*, not cause. Always:
1. Stop new writes for the affected dimension if drift is large.
2. Find the originating journal(s).
3. Post a reversal journal (never `UPDATE`/`DELETE`).
4. Re-run reconcile to confirm clean.
5. Write a postmortem; add a check to `service/reconcile.go` (FullReconciliationService) if the
   pattern can be detected automatically next time.

---

## 2. Solvency check failed

**Alert source**: `SolvencyCheck` returning `solvent: false` (margin < 0). Either
via `GET /api/v1/platform/solvency` (a permanent route, not conditional on
anything being "wired") or the system rollup query. This library ships no
Prometheus metric for solvency itself (`core.Metrics`' 40+ methods have no
solvency-shaped signal) — a deployment that wants to alert on this must run
`SolvencyCheck` on its own schedule and emit its own metric from the result.

**Severity**: P0. The platform's ledger says it can't cover user liabilities.

### Confirm

```bash
# --currency takes the currency UID, not the internal numeric id -- if you
# only have the id, `ledger-cli currencies` lists uid alongside it.
ledger-cli currencies
ledger-cli solvency --currency <currency-uid>
# or
curl http://ledger/api/v1/system/balances | jq '.data.list[] | select(.classification_uid=="custodial")'
```

Compare:
- **Liability** — Σ(user-side balances across all classifications for the currency).
- **Custodial** — Σ(system-side balance on `custodial` classification).

If `custodial < liability`, the ledger reports under-collateralization.

### Investigate

A real solvency failure does **not** mean money was stolen — the ledger sees
its own books only. Three plausible causes:

1. **Withdrawal posted but custodial not debited** — the withdraw journal is
   unbalanced or skipped a leg. Check recent `withdraw_confirm` journals.
2. **Deposit credited but custodial not credited** — symmetric: a deposit
   confirmed without crediting custodial. Check recent `deposit_confirm`.
3. **External custody loss not yet reflected** — funds physically moved out
   (chargeback, hot-wallet sweep, etc.) but the ledger wasn't told. Post a
   `capital_loss` journal to reconcile against external custody. See
   `presets/capital.go` for the pattern.

### Resolution

- (1) and (2): bisect, post reversal of broken journal, re-post correctly.
- (3): post the missing capital adjustment journal. Solvency margin should
  now match the off-chain custody figure.

---

## 3. Rollup queue is backlogged

**Alert source**: `ledger_rollups_pending` gauge climbing, or
`GET /api/v1/system/health` reporting `rollup_queue_depth > 1000`.

**Severity**: P2. Reads are still correct (real-time delta), but checkpoints
are getting stale, which slows balance reads.

### Confirm

```sql
SELECT COUNT(*) FROM rollup_queue WHERE processed_at IS NULL AND failed_attempts < 10;
SELECT MAX(now() - created_at) FROM rollup_queue WHERE processed_at IS NULL AND failed_attempts < 10;
```

### Investigate

The rollup worker uses `SKIP LOCKED` claims. If many workers run, each gets a
slice. If the queue grows, either:
1. Worker is dead (no claims being taken).
2. Workers are alive but the per-item processing is slow (hot account).
3. Throughput exceeds capacity.

Check worker logs for repeated errors. A single hot account with millions of
entries per checkpoint cycle can monopolize a worker.

### Resolution

- **Dead worker**: restart the `service.Worker` (or the host application if
  embedded).
- **Hot account**: increase rollup batch size or partition the worker by
  account holder modulo.
- **Capacity**: scale the worker count. See `examples/fullstack/backend/main.go` for the
  worker loop config.

The critical fact: **balance reads stay correct** — the checkpoint+delta path
ensures that. Only checkpoint freshness (and rollup-table read latency) are
affected.

### Stuck rollup items (B-m10)

A rollup_queue item that fails `failed_attempts` times (currently 10) stops
being dequeued at all — `DequeueRollupBatch` filters `failed_attempts < 10`,
so a permanently-broken item does not retry forever, but it also does not
disappear from anywhere on its own. It is excluded from
`ledger_rollups_pending` above (so an actually-draining backlog and a
permanently-stuck item don't read as the same alert), and counted instead by
its own gauge:

```
ledger_rollups_stuck > 0
```

**Confirm and diagnose**:

```sql
SELECT id, account_holder, currency_id, classification_id, failed_attempts, created_at
FROM rollup_queue
WHERE processed_at IS NULL AND failed_attempts >= 10;
```

Check worker logs for that `(account_holder, currency_id, classification_id)`
around the time `failed_attempts` climbed — the underlying cause is whatever
made `RollupService.processItem` fail 10 times in a row (a poisoned
checkpoint row, a classification with a corrupted `normal_side`, a
long-running DB contention issue). Fix the underlying cause first; resetting
the claim without fixing it just spends the next 10 attempts the same way.

**Resolution** — once the underlying cause is fixed, put the item back into
the eligible set:

```bash
ledger-cli rollup reset-claim --id <rollup_queue.id>
```

This clears `claimed_until` and resets `failed_attempts` to 0. There is no
other way back in short of hand-written SQL — this is the one write action
`ledger-cli` exposes outside `reconcile --full`'s resume cursor (see its
package doc, `cmd/ledger-cli/main.go`).

---

## 4. Checkpoint age is climbing

**Alert source**: `ledger_checkpoint_age_seconds{class="..."}` gauge (not a
histogram — see [§14](#14-onchain-money-path-metrics----this-library-ships-none-of-the-alerting)'s
doc-vs-code note) >1h for any class.

**Severity**: P3. Same as §3 with a different symptom — usually the same fix.

### Investigate

```sql
SELECT classification_id, MAX(now() - last_entry_at) AS age
FROM balance_checkpoints
GROUP BY 1 ORDER BY age DESC LIMIT 10;
```

If one classification is way ahead of others, that's the hot spot. Otherwise
the worker itself is slow / stopped.

---

## 5. Webhook delivery backlog

**Alert source**: rising count of events with `attempts > 3` and
`next_attempt_at < now()`.

**Severity**: P2. Consumers are not getting events; ledger correctness
unaffected.

### Confirm

```sql
SELECT COUNT(*) FROM events
 WHERE journal_id IS NOT NULL
   AND attempts >= 5
   AND next_attempt_at < now();
```

### Investigate

For each subscriber: check whether their endpoint is up. The deliverer uses
`retryDelay(attempts)` exponential backoff. After `MaxAttempts`, events are
parked.

```bash
# Inspect the subscribers
SELECT id, url, last_status_code, last_error, last_attempt_at FROM webhook_subscribers;
```

### Resolution

- If a subscriber is dead, deactivate it: `UPDATE webhook_subscribers SET is_active=false WHERE id=...`.
- If transient, reset attempts: `UPDATE events SET attempts=0, next_attempt_at=now() WHERE ...`.
- If a subscriber's HMAC secret rotated and signatures fail, update the
  secret column and reset attempts.

### Delivery semantics (read before building a consumer)

Webhook delivery is **at-least-once**, not exactly-once and not ordered:

- **At-least-once**: a failed attempt is retried with exponential backoff
  (`retryDelay`: 1m, 5m, 30m, 2h, 24h) until `MaxAttempts` is exhausted, after
  which the event is parked (`delivery_status = 'dead'`). A subscriber may
  therefore receive the same event more than once — e.g. the HTTP POST
  succeeded but the response was lost before the deliverer recorded it.
- **Retries can arrive out of order**: because failed events are retried on a
  backoff schedule while newer events for the same booking keep being
  delivered on the normal cadence, a *retried* older event can reach a
  subscriber's endpoint *after* a newer event for the same booking has
  already been delivered. Consumers must not assume "later HTTP request" means
  "later ledger event."
- **Consumer requirements**: dedupe on the `X-Ledger-Event-ID` header (each
  event has a stable, unique ID) and treat delivery as idempotent — reprocessing
  the same event ID must be a no-op. Do not infer booking state purely from
  the order requests arrive in; if ordering matters, compare event
  timestamps/IDs from the payload itself, not arrival order.

### Dead-letter handling & retention

Dead-lettered events (`delivery_status = 'dead'`) are **parked, not lost** —
the event row is the system of record, delivery bookkeeping just marks it
undeliverable. This library ships no alert rule named `LedgerEventDeliveryDead`
or anything else — that name does not exist anywhere in this repository.
Build your own alert on `increase(ledger_events_dead_total[window]) > 0`
(I-N17: a rule name a search cannot find is worse than an inline expression).

1. Find them:
   ```sql
   SELECT id, classification_code, to_status, attempts, occurred_at
     FROM events WHERE delivery_status = 'dead' ORDER BY id;
   ```
2. Fix the cause (subscriber down / signature mismatch / bad URL — see the
   subscriber checks above).
3. Requeue — reset the delivery bookkeeping; the event payload is untouched:
   ```sql
   UPDATE events SET delivery_status = 'pending', attempts = 0, next_attempt_at = now()
    WHERE delivery_status = 'dead' AND id IN (...);
   ```

**Retention policy**: `events` rows are part of the audit trail (each links
a booking transition to its journal) and are **never deleted** — the
delivery columns ride on the same row, so there is no separate delivery
table to prune. Growth is one row per transition, cheap relative to
`journal_entries`. If events volume ever warrants lifecycle management, the
sanctioned path is the same as journal entries — range-partition and
archive detached partitions (RUNBOOK §11) — not `DELETE`.

---

## 6. Idempotency collision spike

**Alert source**: `ledger_idempotency_collisions_total{journal_type="..."}`
counter spiking for a journal type.

**Severity**: P3 by default; P1 if the type is `withdraw_confirm` or anything
that moves real money.

### Investigate

A collision means two posts arrived with the same `idempotency_key`. For a
same-payload replay, the second call should return the original journal/result
without posting again. If the second call returned `ErrConflict`, the same key
was reused with a different payload. Causes:

1. **Client retry logic working as designed** — expected, low rate.
2. **Bad client generating non-unique keys** — e.g. a timestamp-based key
   colliding under high traffic. Check the calling service's key derivation;
   it must be `{op}-{userID}-{requestUID}`, not a timestamp.
3. **Replay attack** — a third party is replaying captured webhook
   payloads. Check `webhooks/{channel}` ingress logs.

### Resolution

- (2): coordinate with the client team to fix key derivation.
- (3): check HMAC verification is enforced on the inbound channel; rotate
  shared secret if compromise suspected.

---

## 7. Journal posting failures

**Alert source**: `ledger_journals_failed_total{journal_type, reason}` counter.

**Severity**: depends on `reason`.

`reason` is a bounded set of Go constants, not free text -- the single
source of truth is `classifyJournalFailureReason` in
`postgres/metrics_reasons.go`. This table exists to save a code lookup on
call, not the other way around; if it and the code ever disagree, the code
wins.

| reason | means | likely cause | action |
|--------|-------|--------------|--------|
| `unbalanced` | `core.ErrUnbalancedJournal` | bug in caller building entries, or a genuine per-currency imbalance caught before any DB write | reproduce, file ticket on caller |
| `account_policy` | `core.ErrAccountFrozen` / `core.ErrAccountClosed` | the holder's account was frozen/closed mid-flight (see `AccountPolicyStore`) | check `account_policies` for that holder; confirm the freeze was intentional |
| `insufficient_balance` | `core.ErrInsufficientBalance` | user-side overdraft attempted | expected when caller didn't `Reserve` first |
| `period_closed` | `core.ErrPeriodClosed` | journal's `effective_at` is before the active accounting period close line (I-15) | caller is backdating into a closed period; fix the caller's `effective_at`, do not reopen the period |
| `unauthorized` | `core.ErrUnauthorizedJournal` / `ErrUnknownAuthKey` / `ErrAttestorUnavailable` | signing failed (WithAttestor's KMS call errored) or a verifier rejected a signature | check the Attestor/KMS's own health; see [§16](#16-p5-signing-key-rotation) for a rotation-shaped cause |
| `duplicate` | `core.ErrDuplicateJournal` | the `journals_idempotency_key_key` unique constraint fired directly (rare -- usually caught earlier as an idempotency replay, not a failure) | investigate as a possible race in the caller's retry logic |
| `conflict` | `core.ErrConflict` | idempotency key reused with a divergent payload (also emits `IdempotencyCollision`, see §6), or an `event_uid` already linked to another journal | see §6's investigation steps |
| `not_found` | `core.ErrNotFound` | journal referenced an unresolvable currency/classification/event/reversal-target uid | check the caller passed a real uid, not a stale one |
| `validation` | `core.ErrInvalidInput` / `core.ErrPrecisionExceeded` | malformed request: empty entries, non-positive amount, amount exceeding the currency's configured exponent | bug in caller; check request shape against `core.JournalInput.Validate` |
| `db_error` | anything else (unclassified) | pool exhausted / postgres down / an unmapped driver error | check `system/health` and PG dashboards |

---

## 8. Common investigation queries

### Trace a booking end-to-end

```bash
ledger-cli trace --booking-uid <booking-uid>
```

Or:

Or, in SQL. Every identifier this system hands you -- alerts, `ledger-cli`,
`GET /deposits/reviews`, the dead-letter queue -- is a **uid**; the numeric
`id` is internal and never crosses a boundary (api-contract.md §3), so these
key on uid and do the join for you:

```sql
SELECT * FROM bookings WHERE uid = '<booking-uid>';

SELECT e.* FROM events e
 JOIN bookings b ON b.id = e.booking_id
 WHERE b.uid = '<booking-uid>'
 ORDER BY e.occurred_at;

SELECT j.* FROM journals j
 JOIN events e   ON e.journal_id = j.id
 JOIN bookings b ON b.id = e.booking_id
 WHERE b.uid = '<booking-uid>'
 ORDER BY j.id;
```

### Find every journal that touched an account dimension

```sql
SELECT DISTINCT j.id, j.created_at, j.idempotency_key
FROM journals j
JOIN journal_entries je ON je.journal_id = j.id
WHERE je.account_holder = 42
  AND je.currency_id = 1
ORDER BY j.id DESC
LIMIT 50;
```

### Compute live balance for an account

This is the hand-recompute [§1](#1-reconciliation-failed) sends you to when
`checkpoint_balance` / `system_rollup_integrity` / `snapshot_integrity` fail
— the case where a materialized cache disagrees with `journal_entries` and
you need the ground truth, not another read of the cache:

```sql
SELECT
  COALESCE(cp.balance, 0)
  + COALESCE((
      SELECT SUM(CASE WHEN je.entry_type = 'debit' THEN je.amount ELSE -je.amount END)
      FROM journal_entries je
      WHERE je.account_holder = k.holder
        AND je.currency_id = k.currency
        AND je.classification_id = k.class
        AND je.id > COALESCE(cp.last_entry_id, 0)
    ), 0) AS balance
FROM (SELECT 42::bigint AS holder, 1::bigint AS currency, 1::bigint AS class) k
LEFT JOIN balance_checkpoints cp
  ON cp.account_holder = k.holder
 AND cp.currency_id = k.currency
 AND cp.classification_id = k.class;
```

The result is the **debit-normal** balance (`debit - credit`). For a
credit-normal classification, negate it. Check `core.NormalSide` of the
classification — `ledger-cli classifications` prints it.

> This query used to mix `COALESCE(cp.balance, 0)` with `SUM(...)` and no
> `GROUP BY`, so it did not run at all (`ERROR: column "cp.balance" must
> appear in the GROUP BY clause`). It was the tool §1 named for a suspected
> credential leak, which is the worst possible moment to find that out.

### The onchain tables (deposits the ledger did not book, and why)

Five tables carry the pull path's own state. Every SQL block below was run
against a migrated database.

**Dead-lettered deposits that still have no booking** — a transfer that IS on
chain, to a registered address, that this ledger refused and then scanned
past. See [§18](#18-a-deposit-was-dead-lettered) for what to do about one;
`ledger-cli dead-letters list --unbooked-only` is the same question with the
payload decoded.

```sql
SELECT dl.uid, dl.chain_id, dl.tx_hash, dl.txlog_seq, dl.reason, dl.created_at,
       dl.payload->>'amount' AS amount, dl.payload->>'to' AS deposit_address
FROM ingest_dead_letters dl
WHERE NOT EXISTS (SELECT 1 FROM bookings b WHERE b.idempotency_key = dl.idempotency_key)
ORDER BY dl.created_at
LIMIT 50;
```

**Open deposit chain anomalies** — deep reorgs and wrongly-failed deposits
awaiting an operator's close-out ([§12](#12-deep-reorg-on-a-confirmed-crypto-deposit)).
`resolved_at` at the Unix epoch means OPEN (this schema has no NULLs):

```sql
SELECT r.kind, r.booking_uid, r.chain_id, r.tx_hash, r.journal_uid,
       r.detected_at, r.last_seen_at,
       b.status, b.amount, b.account_holder
FROM deposit_reorgs r
JOIN bookings b ON b.uid = r.booking_uid
WHERE r.resolved_at <= 'epoch'
ORDER BY r.detected_at
LIMIT 50;
```

**Forward-scan cursors** — the single value deciding which chain blocks this
ledger can still see. `last_scanned_block` not moving between two runs of
this query is the same fact `ledger_chain_cursor_advance_age_seconds`
reports, and it is what a watcher stall looks like in the database:

```sql
SELECT chain_id, last_scanned_block, updated_at, now() - updated_at AS since_write
FROM chain_cursors ORDER BY chain_id;
```

**Registration rescans still owed** — the "deposit sent before the address
was registered" backfill (`ledger_registration_rescan_failed_total`):

```sql
SELECT uid, chain_id, address, next_block, status, attempts, available_at, last_error
FROM registration_rescans
WHERE status <> 'completed'
ORDER BY available_at
LIMIT 50;
```

**In-flight sweeps** — see [§15](#15-a-chains-sweep-collection-has-stopped-moving);
`pending` is included deliberately, because that is the state an orphaned
broadcast is stuck in:

```sql
SELECT uid, metadata->>'chain_id' AS chain_id, metadata->>'token' AS token,
       metadata->>'nonce' AS nonce, status, channel_ref, updated_at
FROM bookings
WHERE classification_id = (SELECT id FROM classifications WHERE code = 'sweep')
  AND status IN ('pending', 'sent')
ORDER BY updated_at ASC;
```

### List all reversal chains

```sql
WITH RECURSIVE chain AS (
  SELECT id, reversal_of, idempotency_key, 0 AS depth FROM journals WHERE reversal_of IS NULL
  UNION ALL
  SELECT j.id, j.reversal_of, j.idempotency_key, c.depth + 1
  FROM journals j JOIN chain c ON j.reversal_of = c.id
)
SELECT * FROM chain WHERE depth > 0 ORDER BY depth DESC, id;
```

---

## 9. Emergency: stop the ledger

If you need to stop accepting new writes immediately (e.g. detected
corruption):

1. **Application level** — toggle a feature flag in the calling services;
   the ledger itself has no global "off switch". If you control the API
   gateway, return `503` for `POST /api/v1/journals*` and `POST /api/v1/bookings*`.

2. **Database level** — revoke INSERT on `journals` and `journal_entries`
   from the application user:

   ```sql
   REVOKE INSERT ON journals, journal_entries, events, bookings FROM ledger_app;
   ```

   **This alone does not freeze `journal_entries`'s partitions** (m-4,
   `.local/independent-review-2026-08-26.md`). `REVOKE` on the parent only
   changes the parent's own ACL entry; migration 008 additionally granted
   column-level INSERT directly on each partition that existed at its
   install time (`journal_entries_yYYYYmMM`, `journal_entries_default`), and
   a `REVOKE` issued against the parent's name does not touch those
   per-partition grants. The application's actual write path is unaffected
   either way — it always names the parent (`postgres/sql/queries/journals.sql`'s
   `InsertJournalEntry`), and Postgres checks a routed INSERT's privilege
   against the table named in the statement, not the partition it lands in
   (same rule the role table's "Operational notes" below documents for the
   opposite, grant-time direction) — so step 2 by itself is sufficient to
   stop the application. It is **not** sufficient to stop a leaked
   `ledger_app` credential used to INSERT directly against a partition's own
   name, which is the threat model migration 006/007/008 were written
   against. To close that path too, revoke every partition individually:

   ```sql
   DO $$
   DECLARE
       r RECORD;
   BEGIN
       FOR r IN
           SELECT c.relname
           FROM pg_partition_tree('journal_entries'::regclass) pt
           JOIN pg_class c ON c.oid = pt.relid
           WHERE pt.relid <> 'journal_entries'::regclass
       LOOP
           EXECUTE format('REVOKE INSERT ON public.%I FROM ledger_app', r.relname);
       END LOOP;
   END $$;
   ```

   Restoring this at recovery time means re-running 008's own per-partition
   GRANT loop (step 4 below already documents that loop for the parent-vs-id
   reason; the same loop closes this one too, since it targets every
   partition, not just the parent).

3. **Verify writes have stopped**:

   ```sql
   SELECT MAX(id), MAX(created_at) FROM journals;
   ```
   Re-run after a minute; the values should not change.

4. **After recovery**, restore privileges:

   ```sql
   GRANT INSERT ON journals, events, bookings TO ledger_app;
   ```

   `journal_entries` is deliberately **not** in that statement. A blanket
   `GRANT INSERT ON journal_entries TO ledger_app` reopens the table-level
   grant migration 008 (I-42) closed — it hands `ledger_app` INSERT on
   every column again, `id` included, which is exactly the leaked-credential
   path 008 exists to close (`postgres.TestJournalEntries_DuplicateIDAcrossPartitions_Rejected`
   is the pin that fails once this happens). CI cannot catch this after the
   fact: its grant-coverage gate runs against a freshly migrated database,
   not this production instance's ACL state post-recovery. Restore the
   column-level grant instead, by re-running 008's own DO loop (it derives
   every partition from the catalog rather than a hardcoded list, so it is
   safe to re-run regardless of how many monthly partitions exist today):

   ```sql
   DO $$
   DECLARE
       r RECORD;
   BEGIN
       FOR r IN
           SELECT c.relname
           FROM pg_partition_tree('journal_entries'::regclass) pt
           JOIN pg_class c ON c.oid = pt.relid
       LOOP
           EXECUTE format(
               'GRANT INSERT (journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at) ON public.%I TO ledger_app',
               r.relname);
       END LOOP;
   END $$;
   ```

   Verify before declaring recovery complete: as `ledger_app`,
   `INSERT INTO journal_entries (id, journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at) VALUES (...)`
   must still fail with `permission denied` (`SQLSTATE 42501`) — an
   explicit-`id` INSERT succeeding means 008's protection did not survive
   this recovery.

   The same trap exists in step 14 of `001_baseline.up.sql`'s own comment,
   which points at its ACL-derivation loop as the "better" way to grant a
   future table — that loop also issues a table-level `GRANT SELECT, INSERT`
   and would silently undo 008 the same way if ever re-run against
   `journal_entries` by name.

Reads remain available throughout. `GET /balances/*`, `GET /journals*`,
`GET /events*` are unaffected.

**Run steps 2 and 4 as `ledger_owner` (or a superuser)** — `ledger_app`
cannot grant or revoke its own privileges (it has no admin option on
anything), and neither can `ledger_ro`. The connection your `DATABASE_URL`
uses for migrations already has this authority.

### Database roles (`ledger_owner` / `ledger_app` / `ledger_ro`)

`001_baseline` (docs/plans/2026-08-21-tamper-evident-ledger-design.md §3)
creates three least-privilege roles, grants them, revokes `PUBLIC`'s
default schema access, and transfers every table/sequence to `ledger_owner`
— all in the same migration that first creates the schema.

> ℹ️ **`Migrate()`'s authority is scoped to the connection it migrates on,
> not to the credential you hand it.** For a non-superuser runner it switches
> that one connection to `ledger_owner` (`SET ROLE`) and runs migrations
> `002..N` there. Other sessions on the same credential see nothing: while a
> migration run is in flight, `pg_has_role(current_user, 'ledger_owner',
> 'USAGE')` on such a session is still false and `DROP TRIGGER
> journal_entries_no_update` still fails with `42501` — pinned, with a
> deterministically parked run, by
> `TestMigrate_WindowIsNotVisibleToOtherSessionsOfTheSameCredential`.
>
> This replaces a mechanism that granted the *role* `ledger_owner WITH
> INHERIT TRUE` for the duration of each migration. `pg_auth_members` is
> cluster-wide and role-scoped, so that made **every** session on the
> migration credential owner-equivalent for the length of the run — including
> the application's own pool, in a single-credential deployment. Measured, not
> theorised: a second connection dropped `journal_entries_no_update` mid-run
> and `Migrate` still returned `nil`, i.e. I-22 did not hold while a deploy
> was in flight (2026-09-02 adversarial re-review,
> `w3-review/money-path.md` M-5).
>
> ⚠️ **What is left, and what `Migrate()` now does about it.** `SET ROLE`
> needs a membership carrying the `SET` option, and `001` deliberately leaves
> the runner without one (its closing `REVOKE` deletes the row `CREATE ROLE`
> created; only the creator's permanent `ADMIN OPTION` survives). So on a
> fresh install `Migrate()` grants itself the narrowest membership that
> permits the switch — `WITH SET TRUE, INHERIT FALSE` — and revokes it before
> returning; `pg_auth_members` ends where it started. That membership confers
> nothing on a session that does not deliberately switch roles, and it is
> nothing the credential could not grant itself at any other moment via that
> same `ADMIN OPTION`. But an application authenticating as the migration
> credential *could* issue `SET ROLE ledger_owner` itself, and a compromised
> one would.
>
> **So that deployment is refused rather than tolerated.** Before arranging
> anything, and again once the membership exists, `Migrate()` counts the other
> sessions connected as the migration credential (`pg_stat_activity`,
> excluding its own connections by `application_name = azex-ledger-migrate`).
> If there are any it revokes whatever it granted and returns:
>
> ```
> postgres: migrate: refusing to run: 1 other session(s) are connected as
> "ledger_app_deploy" (pg_stat_activity, application_name: myapp). Migrations
> after 001_baseline need a connection that can act as ledger_owner, and every
> session holding this credential can reach that role deliberately for as long
> as the run lasts -- including an application pool. Give migrations their own
> credential (MIGRATE_DATABASE_URL, separate from the application's
> DATABASE_URL), or stop the application before migrating. A superuser or
> ledger_owner connection needs no arrangement and is not subject to this check
> ```
>
> A superuser or `ledger_owner` credential arranges nothing and is never
> subject to the check.
>
> **What the check cannot cover**: it runs at the start, so it binds sessions
> that already exist. A connection opened on the migration credential *while*
> the run is in progress can still `SET ROLE ledger_owner` deliberately and
> drop a guard — measured, and pinned as such. That is bounded (the membership
> is `SET`-only, so nothing is inherited; it is revoked when `Migrate`
> returns; a superuser or `ledger_owner` credential never has one), and it is
> removed entirely by the requirement below. Do not read the guard as making a
> shared credential safe: it makes the common case fail loudly instead of
> quietly.
>
> Two requirements follow, and the shipped examples demonstrate both:
>
> 1. Point migrations at their own credential —  `MIGRATE_DATABASE_URL`
>    (superuser, `ledger_owner` itself, or a role that can `SET ROLE
>    ledger_owner` / holds `ADMIN OPTION` on it) — and the application at
>    `DATABASE_URL` (`ledger_app`). Every `examples/*/main.go` reads them
>    separately and logs a warning when it has to fall back to one URL for
>    both.
> 2. Run migrations when the application is not serving on that credential
>    (a deploy step or an init container, not an in-process call on a live
>    pod). If your deployment must migrate in-process, use a superuser or
>    `ledger_owner` connection for it: neither needs the temporary
>    membership, so neither leaves anything to reason about — and neither is
>    subject to the refusal above, which a rolling deploy sharing one
>    credential would otherwise hit on every pod that is not first.

| Role | Can do | Used by |
|---|---|---|
| `ledger_owner` | Owns every table/sequence — the only role with DDL (`ALTER`/`DROP`/`TRUNCATE`/trigger management/partition create). Has schema `USAGE`+`CREATE` | Schema migrations, once your composition root's `postgres.Migrate(databaseURL)` call is pointed at it -- this library ships no Helm chart or migration job of its own |
| `ledger_app` | `SELECT`/`INSERT`/`UPDATE` on ordinary tables; `SELECT`/`INSERT` only on `journal_entries` (never `UPDATE`/`DELETE` — append-only). No DDL of any kind | the host application's serving processes, once their `DATABASE_URL` points at it |
| `ledger_ro` | `SELECT` everywhere (full tables, not scoped views yet — see follow-up below) | Metabase / BI / reporting — this is the role a credential leak should cost you, not a superuser session |

**Why the split**: GRANT-based privileges alone are not a defense against a
compromised application credential — a connection that owns its tables (or
is superuser) can `DROP TRIGGER` the append-only guards, `TRUNCATE`
`journal_entries`, or silently detach a partition, regardless of what GRANT
says. Postgres cannot confer `ALTER`/`DROP`/`TRUNCATE`/trigger-management
rights through `GRANT` alone — only object ownership (or superuser) grants
them — so `ledger_app` never owning anything is what makes I-22
(`docs/INVARIANTS.md`) true.

Operational notes:

- **The connection that runs `001_baseline` must be able to `CREATE ROLE`**
  (superuser, or a role with the `CREATEROLE` attribute) — this is the
  install-time prerequisite for a fresh database (README "Quick Start").
  If any of `ledger_owner` / `ledger_app` / `ledger_ro` already exists on
  this cluster carrying `SUPERUSER`, `CREATEDB`, `CREATEROLE`,
  `REPLICATION` or `BYPASSRLS`, migration `007` clears it — or, if the
  migration credential lacks the authority to clear that particular
  attribute, **stops the install with an actionable error naming the role
  and the attribute**. Postgres gates each of those attributes on the
  altering role holding the same one, so a `CREATEROLE`-only credential
  genuinely cannot strip `SUPERUSER`. Fix it with a superuser connection
  and re-run; do not work around it by granting the migration credential
  superuser.
- **Every migration after `001` needs `ledger_owner`'s privileges**,
  because `001`'s last act transfers every object it created to that
  role. `Migrate()` arranges this itself: it applies `001` alone on the
  credential you gave it, then opens **one** connection, switches that
  connection to `ledger_owner` (`SET ROLE`), and runs `002..N` on it. A
  superuser, or a connection as `ledger_owner` itself, skips the switch
  and migrates exactly as it always did. Where the credential cannot yet
  switch, `Migrate()` grants itself `ledger_owner WITH SET TRUE, INHERIT
  FALSE` for the run and revokes it on every exit path, using the `ADMIN
  OPTION` Postgres permanently gives the creator of a role — it is not a
  privilege the credential did not already command, only a bounded,
  explicit arrangement instead of a permission error two migrations in.
  The credential must be **one of the three**: superuser, `ledger_owner`
  itself, or a role that can `SET ROLE ledger_owner` — directly, or by
  holding `ADMIN OPTION` on it (the only way a role other than
  `ledger_owner`'s own creator gets there). Any other credential is
  refused before a single migration runs, with a message naming all
  three ways out, rather than failing partway through with a
  `pg_authid`-shaped permission error (see `postgres.Migrate`'s own doc
  comment, the source for this list).
  - On that same non-superuser path, `Migrate()` also refuses to run
    while **any other session is connected as the migration credential**
    — see the callout above. If a deploy fails with "refusing to run: N
    other session(s)", the fix is a separate `MIGRATE_DATABASE_URL`, not
    a retry: `SELECT pid, application_name, client_addr, state FROM
    pg_stat_activity WHERE usename = '<migration role>'` names what is
    holding it.
  - One consequence worth knowing before a shared cluster surprises you:
    on that non-superuser path `002..N` execute **as `ledger_owner`**, so
    they no longer carry the runner's own role attributes. The only
    statement in the set that wants them is migration `007`'s attribute
    hardening, and only when one of the three roles pre-existed on the
    cluster holding `SUPERUSER`/`CREATEROLE`/… — `007` then stops the
    install with its actionable message asking for a superuser
    connection, where a `CREATEROLE` runner might previously have been
    able to strip the attribute itself. A fresh install issues zero such
    statements and is unaffected.
- **`Migrate()` also needs `CONNECT` on the cluster's `postgres` maintenance
  database** (`docs/INVARIANTS.md` I-47) — it acquires a cluster-wide
  migration lock there before touching the target database, to serialize
  against every other `Migrate()` call on the same cluster (this is a
  real, non-optional prerequisite: `CONNECT` failing surfaces as
  "connect to maintenance database", which does not point at the root
  cause on its own — if you see that error, check `CONNECT` on
  `postgres` first). `001_baseline`'s `CREATE ROLE`/role-membership
  statements and `007`'s `ALTER ROLE` statements write cluster-wide
  shared catalog rows (`pg_authid`, `pg_auth_members`), not
  database-local ones, so two installs running at once on the same
  cluster — Aaron's shared local `dev-postgres` (`infra.md`), CI's shared
  Postgres service container, or a multi-replica deployment migrating
  from more than one pod — race those rows unless something outside the
  target database serializes them. `CONNECT` on `postgres` is granted to
  `PUBLIC` by default; if your cluster revokes it, grant it back to
  whichever role runs `Migrate()`.
  The lock acquisition is a **poll with a bounded budget** (default 5
  minutes, configurable via `postgres.WithMigrateLockBudget`), not an
  unbounded block — each retry logs an Info line "postgres: migrate:
  waiting for cluster migration lock" (falls back to `slog.Default()` if
  no logger was injected). If the budget is exhausted, `Migrate()` fails
  and names the advisory key in the error. To check who's actually
  holding it:
  ```sql
  -- run against the cluster's postgres maintenance database
  SELECT * FROM pg_locks WHERE locktype = 'advisory' AND objid = 2573143714;
  ```
  Use `postgres.MigrateContext(ctx, url, opts...)` instead of `Migrate`
  when you need the wait itself to be cancellable (e.g. from a shutdown
  signal).
- **No passwords are set by any of these migrations.** Set them out-of-band
  (`ALTER ROLE ledger_app WITH PASSWORD '...'`) through whatever secrets
  pipeline you already use for `DATABASE_URL` — never commit one to a
  migration file or to git (`infra.md`).
- **Partition maintenance runs as `ledger_app`, through two `SECURITY
  DEFINER` functions** (`ledger_create_monthly_partition` /
  `ledger_rebalance_default_partition`, `docs/INVARIANTS.md` I-35).
  Whatever process runs `PartitionService.EnsureUpcoming` uses the
  ordinary app pool; `postgres/partition_store.go` issues no DDL of its
  own. **Do not give the serving pool a `ledger_owner` connection.** That
  was this runbook's previous instruction and it is the one thing
  migration `007` exists to make unnecessary: an owner-privileged pool
  can `TRUNCATE journal_entries_default`, and `TRUNCATE` does not fire
  row-level triggers, so it walks straight past `journal_entries`'
  no-DELETE guard. A deployment that followed the old text turned I-2's
  append-only guarantee back into a convention — by the book.
- `ledger_app`'s grant on `journal_entries` covers the parent table and
  every partition that exists at install time. Partitions created *after*
  that inherit access through the parent table name — Postgres checks
  privileges against the partitioned table you name in a query, not the
  partition it physically routes to. Pinned by
  `postgres.TestLedgerAppInsertsIntoPartitionCreatedAfterGrant` rather than
  assumed.
- **TODO (tracked, not yet scheduled): scope `ledger_ro` down to aggregate
  views instead of full-table `SELECT`.** Design doc §3 prefers this;
  baseline ships full-schema `SELECT` because no reporting views exist yet.
  Full `SELECT` is still strictly less than a superuser session, but it is
  not the end state — `ledger_ro` can currently read every
  journal/booking/holder row in the system.
- **Every migration that adds a table/sequence must GRANT `ledger_app`/
  `ledger_ro` on it itself** — the baseline's `ALTER DEFAULT PRIVILEGES`
  deliberately covers only `ledger_owner`. **And if the table carries an
  append-only mutation guard (a `BEFORE UPDATE` trigger calling
  `ledger_block_mutation()`, same as `journal_entries`), it must NOT get
  `UPDATE` in that grant** — an ACL that disagrees with its own trigger is
  the same class of mistake.
  `postgres.TestGrantCoverage_EveryTableHasExpectedLedgerAppAndLedgerRoGrants`
  / `TestGrantCoverage_EverySequenceHasExpectedGrants` (`docs/INVARIANTS.md`
  I-22) enumerate every table/sequence, derive the append-only set from
  `information_schema.triggers`, and catch both classes of mistake
  automatically — a migration landing without its own GRANT, or with an ACL
  that disagrees with its own trigger, fails these tests, not silently ships
  a defense-in-depth gap.
- **`ledger_signed_amount` / `ledger_signed_delta` / `ledger_reject_unknown_normal_side`
  (migration 009) are `REVOKE ALL ... FROM PUBLIC` and granted `EXECUTE` only
  to `ledger_app` and `ledger_ro`** (m-11, `.local/independent-review-2026-08-26.md`).
  This is intentionally fail-closed the same way every other grant in this
  table is, but it means a role outside those two — a BI account, a
  monitoring credential, a read replica's analytics role — hits `permission
  denied for function ledger_signed_amount` if it ever queries anything that
  calls these (`postgres/sql/queries/*.sql`'s balance/reconcile queries all
  do). That is a function-level 42501, not the row/column-level error the
  rest of this table describes — if a "fourth" read-only role is ever
  introduced, it needs its own `GRANT EXECUTE` on these three functions, not
  just table `SELECT`.

### Config tampering forensics: who changed the rule that decides where money goes (D-M5)

Three tables record who changed a configuration row, distinct from the
money-path journals themselves:

| Table | Written by | `changed_by` / actor |
|---|---|---|
| `config_table_changes` | DB trigger, on every table carrying a partial (whitelisted-column) guard — 11 tables as of D-m10, not just the original 4 | the authenticated role; `ledger_app` has no `INSERT` on this table (D-M4), so this column cannot be forged by a compromised app credential |
| `reconcile_scan_cursor_changes` | DB trigger, every write to `reconcile_scan_cursors` | same as above |
| `account_policy_changes` | **application code** (`UpsertAccountPolicy`), not a trigger | business `actor_id` — this is the row that says WHO in the product initiated the freeze/limit change |

**Read them together, not separately.** A `config_table_changes` row with
`table_name = 'account_policies'` that has no corresponding row in
`account_policy_changes` for the same change means the row was modified
without going through the application layer — this is the exact signature
of the D-M3 attack (unfreeze + `min_balance = -1000000` set directly).
That attack still succeeds today — the trigger's column whitelist has to
permit those columns because `UpsertAccountPolicy` needs to write them —
but it can no longer happen without leaving this cross-table
inconsistency as a trace.

**Read access**: `svc.ConfigHistory()` (`core.ConfigChangeReader`, exposed
on the facade) or `postgres.NewConfigHistoryStore(pool)` directly:

```go
ListConfigChanges(ctx, core.ConfigChangeFilter)        ([]core.ConfigChange, nextCursor string, err error)
ListScanCursorChanges(ctx, core.ConfigChangeFilter)    ([]core.ScanCursorChange, nextCursor string, err error)
ListAccountPolicyChanges(ctx, core.ConfigChangeFilter) ([]core.AccountPolicyChange, nextCursor string, err error)
```

`core.ConfigChangeFilter{TableName, CheckName, AccountHolder, Since, Until, Cursor, Limit}`
— zero value means unfiltered; results are newest-first; `nextCursor == ""`
means you've reached the end. From the CLI:

```bash
ledger-cli config-history --table account_policies --since 30d
ledger-cli config-history --check checkpoint_balance --since 7d
ledger-cli config-history --holder 42
```

---

## 10. Deployment security boundary

`authMiddleware` (`server/middleware_auth.go`) requires a bearer API key on
**every** endpoint, reads included — only the Kubernetes probes and the
webhook surface (channel-adapter HMAC) are exempt. Keys carry a scope
(`read` < `write` < `admin`, configured as `name:scope:secret` triples in
`API_KEYS`; see [`api.md`](./api.md)) and the key name is attached to every
access log line for audit.

Keys may also carry independent **capabilities** — privilege bits no Scope
implies, not even `admin` — via a `+`-joined suffix on the scope field
(`name:scope+capability:secret`). Today there is one: `deposit_review`
(`server.CapabilityDepositReview`), required by
`POST /deposits/{uid}/review/approve` and `/reject`. This exists so the key
that ingests/creates deposit bookings (`write` scope: `POST /bookings`,
`POST /bookings/{uid}/transition`) does not automatically get to approve
its own review — a `write` key with no `+deposit_review` suffix cannot
resolve reviews at all, regardless of scope (W3-A, mi2:
`docs/bugs/2026-07-11-m3-security-review.md`). Grant it on a dedicated
reviewer key (e.g. `reviewer:read+deposit_review:secret`), or on the same
operational key if a single-operator deployment deliberately chooses that
policy — the library only makes the separation *possible*, it does not
impose it.

Operational guidance:

- **Issue the least scope that works.** Reporting/dashboard consumers get
  `read`; the application that posts journals gets `write`; `admin` keys
  (metadata mutations, reconcile triggers, period close) belong to operators
  only and should be rare.
- **Separate the ingester key from the reviewer key.** If your deployment
  runs the crypto-deposit add-on with a review gate configured
  (`AutoCreditCeiling`/`ReconcileCeiling`), do not grant `deposit_review` to
  the same key that drives ingestion — that reintroduces the self-approve
  path the capability exists to close.
- **One key per consumer, never shared** — the key name is your audit trail
  ("which caller did this"). Rotate by appending a new triple, deploying
  consumers, then removing the old triple.
- A `read` **API key** sees every holder — treat `read` keys as sensitive.
  For end-user traffic, do NOT hand out keys at all: use **holder tokens**
  below.

### Holder tokens (end-user wallet surface)

`HOLDER_TOKEN_SECRET` (min 32 bytes) enables the holder wallet surface:
`GET /api/v1/holder/{balances,transactions,holds}` authenticate with a
stateless HMAC token (`lht_` prefix on the same bearer header), bound to ONE
holder, read-only, default 15m TTL. Your backend mints them per session via
`POST /api/v1/holder-tokens` (write scope) or in-process with
`server.MintHolderToken`; library hosts can mount `server.HolderHandler`
(the three read endpoints, zero admin routes) into their own router.

- Leak blast radius: one holder, read-only, until `exp` — no key rotation
  needed for a leaked token, it ages out.
- Revocation is global: rotate `HOLDER_TOKEN_SECRET` and every outstanding
  token dies; there is deliberately no token table to operate.
- Access logs carry `holder:<id>` as the principal for these requests.
- Capacity: this surface serves END-USER traffic (every wallet page view),
  a different profile from operator/admin calls. Balances ride the
  checkpoint+delta read path (cheap, see CAPACITY.md); transactions are a
  per-page aggregate over the holder's recent journals. Rate-limit at your
  edge accordingly — the mounted sub-router deliberately ships none.

### What this means for deployment

The mounted HTTP layer (`server.NewWithConfig`) should still not be exposed directly
to the public internet:

- Run it inside a private network (VPC-only ingress) or behind a gateway —
  defense in depth on top of bearer auth, and the transport that gives you
  TLS termination.
- Library-mode consumption is unaffected: there is no HTTP surface, and your
  own application owns the auth boundary.

### Trusting proxy headers for client IP (`TRUSTED_PROXY_CIDRS`)

The per-IP rate limiter and access logs key on `r.RemoteAddr`, which is the
socket peer by default. Behind a proxy that is fine for security but useless
for attribution — every request appears to come from the proxy, so all
clients share one rate-limit bucket. Set `TRUSTED_PROXY_CIDRS` to the CIDR
ranges of your edge proxies/ingress to derive the real client IP instead.

The trust is **peer-gated**: headers are honored only when the socket peer is
itself inside a configured range, so a direct caller (anyone who can reach the
pod outside the proxy path) cannot forge `X-Forwarded-For` / `X-Real-IP` /
`True-Client-IP` to evade the limiter or poison logs. This is why the flag
takes CIDRs rather than a bare on/off toggle — the ranges are the trust
boundary, machine-enforced.

- **Precondition still holds**: the pod must be reachable *only* through those
  proxies. In Kubernetes the pod/ClusterIP is directly reachable in-cluster,
  so include only the ingress ranges you actually front the service with, and
  use a NetworkPolicy to block direct pod access from other workloads.
- Invalid CIDRs fail pod startup fast (no silent fallback to "trust nothing").
- Leaving it empty is the safe default: no header is trusted, IPs are the
  socket peer. If you rely on the limiter for abuse control behind a proxy and
  see all traffic in one bucket, this is the knob you forgot.

Enabling it in a deployment that is *also* directly reachable, or setting
ranges wider than your real proxies, reopens IP spoofing of the rate limiter
and access logs — treat that the same as the exposed-port incident below.

### If you find the port open to the public internet

Treat it the same as any other exposed-data incident:

1. Put it behind a private network / gateway immediately (see above).
2. Check access logs (`requestLoggerMiddleware` output) for `GET` traffic
   from unexpected sources during the exposure window.
3. File a postmortem — this is a P1, not a shrug.

### Choosing an Anchor carrier (P6 external anchor)

`core.Anchor` (`core/interfaces.go`) publishes the P6 batch-attestation head
somewhere the ledger's own database credentials cannot reach (design doc
§8.3). This library ships only `anchordev.LocalFileAnchor` — a dev/test
implementation that is explicitly not a production carrier (a file on the
same host as the ledger's own DB is not an equivalent simplification here;
see that package's doc comment). **Which carrier to use in production is a
deployment decision this library does not make for you** — the design doc
is deliberate about this: the anchor's carrier is "a real, unresolved
deployment choice, not deferred out of laziness."

What follows are the properties any carrier must have, not a
recommendation of which one. `anchortest.RunConformance` (see that
package's doc comment) machine-checks the parts of this that are
observable through `core.Anchor`'s method set; the rest has to be verified
by reading your own adapter and its deployment wiring, not by a test.

A conformant production carrier must be:

1. **Somewhere the ledger's DB credentials cannot reach.** The same
   credential leak that exposes `DATABASE_URL` must not also hand the
   attacker write access to the anchor — otherwise they can rewrite both
   sides of the check the anchor exists to make possible. This rules out
   another table in the same database, and any storage the ledger's own
   service credentials can write to. In practice: a separate cloud
   account/tenant at minimum, ideally with its own separate credential
   material never issued to the ledger's own deployment. **Not machine
   checkable** — `anchortest` has no DB handle to compare against; this
   has to be verified by reading your composition root's wiring (which
   credentials construct the `core.Anchor` adapter vs. which construct the
   `pgxpool.Pool`).
2. **Append-only / immutable once written.** A carrier that lets anyone
   (including your own ledger's operators) overwrite a previously
   published `(seq, head)` pair defeats P6's purpose — an attacker who
   compromises the DB and can also freely rewrite the anchor can make
   their tampering self-consistent. Object-lock / WORM modes, a
   write-once bucket policy, or a public chain's own immutability are the
   usual shapes. `anchortest.RunConformance`'s
   `MismatchedReplayErrorsAndDoesNotCorrupt` phase checks the half of this
   that's observable through `Publish` (it refuses a different value for
   an already-published seq) — it cannot prove nothing else (an admin
   console, a bucket-policy change, a second credential) can still mutate
   the underlying bytes out of band.
3. **Independently readable without going through the ledger's own
   service.** `ledger-cli verify` (design doc §8.4) must be able to fetch
   the trusted head even if the ledger's application and database are
   both compromised or unreachable — otherwise the anchor only ever
   confirms what the (possibly-compromised) ledger already claims about
   itself. `anchortest.RunConformance`'s
   `IndependentlyConstructedClientSeesSameState` phase checks exactly
   this: a second, freshly constructed client must see what an earlier
   one published.
4. **Cheap enough to sustain the attestation cadence.** `AttestInterval`
   defaults to 60s (`service.DefaultWorkerConfig`); the P6 job publishes
   on that cadence indefinitely for the life of the deployment (roughly
   1,440 publishes/day, each a few dozen bytes — design doc §8.3: "内容
   （几十字节）"). Whatever the per-write cost of your chosen carrier is,
   multiply it out at that frequency before committing to it, and again
   if you shorten the interval.

Failure handling is the caller's responsibility, not the carrier's: design
doc §8.3 requires a local retry queue + alerting in front of `Publish`, and
that `Publish` failures never block journal writes (P6 is a sidecar, not a
dependency of ledger availability). `ledger-cli verify` treats an
unreachable anchor as `NOT_RUN`, fail-closed — never `VERIFIED` (design doc
§8.4).

### Deploying the R2 carrier (`anchors/r2`, board #55)

`anchors/r2` (import path `github.com/azex-ai/ledger/anchors/r2`) is a
production `core.Anchor` carrier on Cloudflare R2 + Object Lock, chosen
(2026-08-29) because every project's domains already sit in Cloudflare, R2
supports Object Lock natively, and a second Cloudflare account is pure
configuration — no new vendor to onboard. It is a separate Go module from
the root `github.com/azex-ai/ledger` module (mirroring `chains/evm`'s split
for go-ethereum): the AWS SDK it depends on never enters the root module's
`go.mod`/`go.sum`, so importing this library does not pull the AWS SDK into
a consumer who uses a different carrier (or none at all). Only a consumer
that explicitly imports `anchors/r2` compiles against it.

It speaks the plain S3 API (`github.com/aws/aws-sdk-go-v2/service/s3`), so
it is exercised in this repo's own test suite against a local MinIO
container rather than real R2 — the carrier's actual reachability,
credentials, and Object Lock configuration on the real bucket are verified
by the deployer, per point 1 of the four carrier properties above (not
something a black-box Go test run in CI can prove). The MinIO
testcontainers dependency lives in a test-only sibling module,
`anchors/r2/internal/miniotest`, so it is not a direct dependency of
`anchors/r2` itself and does not reach a consumer's dependency graph (MJ-6,
2026-08-29 review; the same split `internal/postgrestest` uses for the root
module — see the root CLAUDE.md "go.work" gotcha for the exact SBOM/lockfile
semantics).

**Consuming the submodule today (MJ-7, not yet closed).** `anchors/r2` and
`chains/evm` pin the root module with a local `replace ... => ../..` for
in-repo development, and the release workflow does not yet rewrite that
`replace`/`require` to a published version or push a submodule-scoped tag
(`anchors/r2/vX.Y.Z`). Go ignores a dependency's own `replace` directives,
so `go get github.com/azex-ai/ledger/anchors/r2@<tag>` from an external
module does **not** resolve as-is. Until the release CI is extended to
version the submodules, consume them from a local checkout via a
parent-directory `go.work` (see the README's "Local Development with
go.work"), not `go get`. Tracked as a release-engineering follow-up.

**What a deployer must set up, before wiring `r2.New` into the
composition root:**

1. **A separate Cloudflare account** from the one that runs the ledger's
   own deployment (property 1 above — the same credential leak that
   exposes `DATABASE_URL` must not also unlock the anchor). This is the
   account that owns the bucket, not just a different R2 API token in the
   same account.
2. **A bucket with Object Lock enabled**, created with a **default
   compliance-mode retention period** (Cloudflare R2 dashboard or API, at
   bucket-creation time — Object Lock cannot be enabled on an
   already-created bucket). Choose the retention period to comfortably
   outlive how long a compromise could plausibly go undetected; compliance
   mode means **not even the account root** can shorten it or delete a
   locked object version early (that is the point — see property 2 above).
   Object Lock requires bucket versioning, which R2 enables automatically
   alongside it.
3. **Two separate R2 API tokens, scoped to exactly the permissions each
   side needs — never one token shared by both:**
   - **Ledger-side token** (the one `r2.Config.AccessKeyID` /
     `SecretAccessKey` holds in the ledger's own deployment, driving
     `Publish`): scope to `GetObject` + `PutObject` + **`ListBucket`** on
     the anchor's bucket — ideally restricted to the object-key prefix
     `r2.Config.Key` names. `GetObject` is required because `Publish`
     reads the seq's own object back before treating a repeat as an
     idempotent replay; `ListBucket` is required because `Head` resolves
     the highest published seq by listing the prefix (one object per seq —
     see below). This token must never carry `DeleteObject`,
     `PutBucketPolicy`, or any other administrative scope. ⚠️ Object Lock
     retention does NOT make that optional: retention prevents deleting a
     specific object VERSION, but a plain `DELETE` writes a delete marker,
     which is permitted under retention and hides the object from a
     listing — i.e. it can lower `Head`. Not granting `DeleteObject` is
     the actual defence; `anchor_observations` (I-55) is the backstop that
     turns such a regression into TAMPERED instead of a benign backlog.
   - **Verification-side token** (whatever reads the anchor independently
     — an auditor's own tooling, or the `verify-with-r2` example under
     `examples/`, design doc §8.4): **read-only** — `GetObject` +
     `ListBucket` (and `ListObjectVersions` if your verification tooling
     wants to enumerate version history rather than just the seq
     objects). This token must never carry `PutObject`. `ledger-cli
     verify`'s `--anchor-file` flag is the local-file anchor only, not the
     production verification entry point for R2 — `cmd/ledger-cli` lives
     in the root module and `anchors/r2` is a separate module (its AWS SDK
     must not become a dependency of every consumer's binary); a consumer
     that wants CLI-driven R2 verification calls
     `service.VerifyLedger(ctx, r2Anchor, cfg)` from its own composition
     root, no library change needed.
4. **Wire `r2.Config`** in the consumer's composition root with the
   ledger-side token's credentials, the bucket name, the R2 endpoint
   (`https://<account-id>.r2.cloudflarestorage.com`), and the object-key
   **prefix** to publish under — then pass the constructed `*r2.Anchor` as
   the `core.Anchor` the P6 worker publishes through. Nothing in
   `anchors/r2` reads the environment itself (mirrors `chains/evm`'s
   `ClientSet` — see that package's doc comment); all of the above is
   explicit `r2.Config` fields the composition root supplies.

`anchors/r2` stores **one immutable object per `seq`**, at
`<r2.Config.Key>/seq-<20-digit-zero-padded>.json`. `Publish` creates the
seq's object with a conditional write (`If-None-Match: "*"`) and never
overwrites an existing one; `Head` lists the prefix and resolves the
HIGHEST seq present, not the most recent write. That is what makes
`core.Anchor.Head`'s no-regression contract (I-56) true against the
credential this token actually holds — the previous single-mutable-key
layout did not: one out-of-band `PutObject` of an older seq rolled the
published head backwards, and the Object-Lock-protected older versions
that held the truth were read by nothing.

Object versioning (mandatory alongside Object Lock) is still useful, but
be precise about what for: it is a **post-hoc forensic record a human can
read**, not something any verification path consults. `VerifyLedger` never
enumerates version history. Retention's operational value here is that a
published seq's object cannot be destroyed version-by-version.

**Verify status semantics** (narrowed since the above was first written —
align any dashboard/alert built on `ledger-cli verify`'s exit status to
this):
- `NOT_RUN` — the anchor reports empty while the DB chain is non-empty
  (and no higher historical observation exists) — external verification
  did not run at all. Non-zero CLI exit.
- `TAMPERED` — the anchor is below a previously observed value (rollback
  or erasure), or the anchor is ahead of the DB chain.
- `DRIFT` — **only** two benign, self-healing backlog shapes: the anchor
  has published before but is currently behind, or entries exist that
  are not yet covered by an attestation batch but all carry a valid
  authorization. `DRIFT` still exits 0; the report's `uncovered_entries` /
  `anchor_seq` / `last_observed_anchor_seq` fields let a scheduled job
  decide whether to page on it. Because `DRIFT` no longer includes "anchor
  reported empty" (that is `NOT_RUN` now), a scheduled verification job can
  safely treat a nonzero `DRIFT` as opt-in-alertable without risking a
  false page on every anchor cold-start.

---

## 11. Partition management & archival

`journal_entries` is range-partitioned by `created_at` into monthly
partitions (`journal_entries_yYYYYmMM`); the worker's `partition` job keeps
`PartitionMonthsAhead` (default 3) months pre-created and the
`journal_entries_default` catch-all empty (see INVARIANTS I-13).

### If the partition job errors / default partition has rows

The job log line `partition: journal_entries_default held rows` means rows
landed outside every named partition — the job rebalances them
automatically, but find out why (`created_at` outliers usually mean a badly
skewed clock somewhere):

```sql
SELECT min(created_at), max(created_at), count(*) FROM journal_entries_default;
```

**Blast radius of the automatic rebalance**: the self-heal detaches and
re-attaches the default partition in one transaction (non-CONCURRENT — it
needs atomicity), which holds an ACCESS EXCLUSIVE lock on `journal_entries`
for the duration of the row move. Near-empty default = milliseconds; a large
stranded backlog = every ledger read/write blocks until it commits. If the
row count above is large, prefer a maintenance window: scale writers down,
let the partition job run once, scale back up.

### Archiving old partitions

The ledger is append-only, so old months are cold immutable data. When a
partition ages out of the hot query window (balance reads only scan entries
after the latest checkpoint; audit reads are the long tail):

1. **Verify** the month is fully rolled up and snapshotted (checkpoints are
   current, `ledger-cli reconcile --full` passes).
2. **Detach without locking** (Postgres 14+):
   `ALTER TABLE journal_entries DETACH PARTITION journal_entries_y2026m01 CONCURRENTLY;`
3. **Dump** the detached table to your archive store
   (`pg_dump --table=journal_entries_y2026m01 …`), verify the dump restores,
   then drop the table.
4. **Record** the archive location in the ops log. `docs/DR.md` retention
   still applies — the archive is part of the auditable history, not
   disposable.

Do NOT detach partitions younger than your reconciliation + audit horizon.
Balances are unaffected by detaching only if checkpoints already cover the
detached range — that's what step 1 verifies. When in doubt, don't archive:
storage is cheaper than a hole in the audit trail.

## 12. Deep reorg on a confirmed crypto deposit

**Alert source**: `ledger_deposit_reorg_detected_total{chain_id}` (emitted by
the watcher's periodic recheck of recently-confirmed deposit bookings against
the canonical chain — docs/plans/2026-07-11-crypto-deposit-sweep-design.md
§6), plus the `service: onchain: deep reorg detected` log line. There is **no
`deposit.reorged` event**: this section used to name one, and nothing in the
library has ever emitted it.

**Which booking**: `ledger-cli reorgs list`. The counter tells you a reorg
happened; the anomaly row tells you which deposit, which journal, when it was
first seen and whether it is still true — and it outlives the recheck window
that stops re-examining the booking (I-63).

**Severity**: P1 — a confirmed deposit's underlying transaction has
disappeared from the chain. If unresolved, the ledger credits a user for
funds that never (or no longer) settled.

**Context**: `ReorgPolicy` (consumer-configured on the crypto-deposit
add-on) governs what happens next. **`manual` is the default.**

### `manual` (default) — on-call resolves by hand

1. **Verify it's real, not an RPC blip.** Check the transaction against a
   second, independent RPC provider or a public block explorer for the
   chain. A single lagging/misbehaving node reporting "not found" is not a
   reorg — do not act on one source alone.
   ```bash
   # The queue, oldest first: kind, booking_uid, chain_id, tx_hash,
   # journal_uid, detected_at, last_seen_at.
   ledger-cli reorgs list
   ```
   ```sql
   -- Or the same anomaly joined to the booking it names.
   SELECT r.kind, r.tx_hash, r.detected_at, r.last_seen_at,
          b.uid, b.account_holder, b.currency_id, b.amount, b.status, b.channel_ref
   FROM deposit_reorgs r
   JOIN bookings b ON b.uid = r.booking_uid
   WHERE r.resolved_at <= 'epoch'
   ORDER BY r.detected_at;
   ```
2. **Confirm the transaction is genuinely gone** (not just re-mined at a
   different block height — re-orgs commonly re-include a transaction one or
   two blocks later with the same effect, which is not a reversal case).
3. **If genuinely reorged out**, post a reversal journal for the booking's
   linked journal (never `UPDATE`/`DELETE` — see INVARIANTS I-2):
   ```
   POST /api/v1/journals/{journal_uid}/reverse
   { "reason": "deposit reorged: tx <tx_hash> no longer canonical on chain <chain_id>, verified against <second RPC/explorer>" }
   ```
   This posts a balanced reversing entry set; it does not delete or mutate
   the original journal. The booking's `status` stays `confirmed` in its own
   record — the correction lives in the journal, not the booking (I-2's
   append-only rule applies here exactly as it does everywhere else).
4. **If the transaction reappears** (re-mined, same effect) before you've
   posted a reversal: no action needed, this was a false alarm — file it as
   one for tuning the recheck window/confirmation threshold if it recurs.
5. **Close the anomaly out.** Nothing else will: the transaction is still
   off chain after you reverse it, so the recheck loop keeps finding the
   anomaly true and keeps re-alerting — every tick, forever — until an
   operator says what was done about it.
   ```bash
   ledger-cli reorgs resolve --booking-uid <uid> --kind deep_reorg \
     --note "reversed journal <uid>, verified absent on <explorer/second RPC>"
   ```
   `--note` is required: it is the only record of why an alert stopped
   firing, and "we looked at it" must be distinguishable from "somebody
   silenced it". A second `resolve` on the same anomaly affects zero rows
   and reports not-found rather than overwriting the first operator's note.
6. File the incident per the after-action checklist below regardless of
   outcome — a `manual`-policy deep reorg alert firing at all is worth
   understanding (chain instability, RPC provider issue, or a confirmation
   threshold set too low for that chain).

**The other kind on that queue**: `shallow_reorg_failed` is a deposit this
watcher itself failed (its transaction disappeared before reaching the
confirmation threshold) whose transaction is **back on chain**. `failed` is
terminal and the booking's idempotency key absorbs every future sighting of
the same transfer, so the ledger cannot re-credit it — only a human can make
the holder whole. It is the loudest case in the subsystem
(`deposit.failed_tx_returned`) and it has the same close-out:
`ledger-cli reorgs resolve --kind shallow_reorg_failed`.

### `auto_reverse` — already handled, verify it landed correctly

If the consumer configured `ReorgPolicyAutoReverse`, the watcher posts the
reversal journal automatically — **after** `WithDeepReorgMisses` consecutive
observations that the transaction is not on chain (default 3, the same
evidence bar the shallow path uses; I-69). Detection itself is not delayed:
the anomaly row is opened and the counter fires on the FIRST observation, so
what you see first is `auto-reverse withheld: waiting for corroboration`, and
the reversal follows only if the chain keeps saying the same thing. No
on-call action is needed to *initiate* the correction. On-call's job is to **verify** the automatic
reversal was itself correct (not a false positive from a flaky RPC node):

1. Check the reversal journal exists and references the right original
   journal (`reversal_of`).
2. Re-verify the underlying tx status against a second RPC/explorer, same as
   step 1 above — if the automatic reversal was a false positive (the tx was
   never actually reorged out), you now need to reverse the reversal
   (post a new correcting journal) and communicate the double-correction to
   the user.

**Risk statement (read before anyone asks to switch to `auto_reverse`)**:
`auto_reverse` trades a manual verification step for automatic remediation
speed. A false positive (a lagging node, a brief RPC provider outage, a
too-short recheck window) auto-debits a user with no human in the loop before
the money moves — which is why it now takes `WithDeepReorgMisses` consecutive
observations rather than one, and why a deployment that wants a higher bar
should raise it. Selecting `auto_reverse` is an explicit risk acceptance by
whoever configures the consumer's `ReorgPolicy` — it is not a "safer"
default, and `manual` remains the default for exactly this reason (design
doc §6).

---

## 13. Large / unreconciled deposit parked in review

> ⚠️ **MUST READ before enabling onchain deposit ingestion**: `AutoCreditCeiling`
> has **no safe default**. `service.Onchain.Run` refuses to start if ANY
> `ChainConfig.CreditTokens` entry left it at the zero value (unconfigured) —
> you must explicitly set either a positive ceiling (deposits above it park in
> `review` instead of auto-crediting) or `core.UnboundedAutoCredit` (an
> explicit, reviewed acceptance that a single RPC sighting may credit any
> amount, with no cap at all). There is no way to silently skip this decision:
> not setting it is a startup error, not "pre-M3 behavior." See
> docs/COOKBOOK.md's crypto-deposit recipe §7 and
> docs/plans/2026-07-11-crypto-deposit-sweep-design.md §9.2. `ReconcileCeiling`
> is unaffected — leaving it at zero is a legitimate choice (no reconciliation
> gate), since `AutoCreditCeiling` is what actually bounds mint exposure.
> Whenever the reconciliation gate IS active (`DepositConfirmer` configured
> and `ReconcileCeiling` positive for a token), `ReconcileFailureLimit` has
> the same no-safe-default treatment (W3-A, mi5) — `Run()` refuses to start
> without it, so a persistent second-source outage escalates to `review`
> instead of leaving a legitimate deposit stuck in `confirming` forever.

**Alert source**: `deposit.review_required` (emitted by the deposit path's M3
compensating controls when a deposit clears its confirmation threshold but
must not yet be auto-credited — docs/plans/2026-07-11-crypto-deposit-sweep-design.md
§9). Four reasons appear on a booking's `metadata.review_reason`; a fifth
value exists on the METRIC only and is not a booking status at all (see
below):

- `over_ceiling` — the amount exceeds the chain/token's configured
  `AutoCreditCeiling`.
- `reconcile_mismatch` — a second, independent confirmation source
  (`DepositConfirmer`) either does not see the transaction included, or sees a
  different amount than the primary sighting.
- `reconcile_unavailable` (W3-A, mi5) — the second source has errored
  (unreachable, timeout, 5xx — as opposed to `reconcile_mismatch`'s "reachable
  but disagrees") on `ReconcileFailureLimit` consecutive attempts for this
  booking. Unlike the other two reasons this is an *availability* signal, not
  a disagreement to adjudicate: the deposit itself may well be entirely
  legitimate, sitting here only because your second RPC provider has been
  down. Treat it as "go check the `DepositConfirmer` backing service", then
  resolve the same way as any other review once it is confirmed genuine.
- `token_unconfigured` (G-M6) — the booking's token is no longer in the
  chain's `CreditTokens` by the time it reached its threshold (a config
  rollback, a contract migration). Its ceilings are then unknowable, and
  "unknowable" must not read as "unbounded". The deposit is real; decide
  whether the token belongs back in the allowlist before approving.
- `onchain_unverified` (money-out C-2) — **the chain does not corroborate
  this booking.** Before crediting, the recheck loop re-reads the block the
  booking names and requires a log carrying its tx hash, log position, token,
  amount and a recipient registered to its holder; this reason means that
  re-read failed `WithConfirmationEvidenceMisses` times in a row (default 3).
  Treat it as a **P1 security event, not a deposit question**: the ordinary
  causes of a real deposit failing this (a node that cannot see its own
  recent block) clear themselves within a tick or two, so a booking that
  reaches here either references a transaction that does not exist or claims
  an amount/token/recipient the log does not carry. Verify the transaction
  independently and, if there is no such transfer, treat the row as forged —
  a booking `INSERT` is exactly the shape a leaked `ledger_app` credential
  produces (see `docs/audits/2026-09-03-independent-review/money-out.md`
  C-2), and approving it credits the attacker.

And one metric-only value, which is **not** a booking status and will never
appear in the review queue:

- `shallow_reorg_returned` (G-M1) — a deposit this watcher automatically
  FAILED as a shallow reorg turned out to be on chain after all. The booking
  is terminal and its idempotency key absorbs every future sighting, so the
  ledger cannot credit it; only a human can make the holder whole. Working it
  through `GET /deposits/reviews` will show an empty queue — go to
  [§12](#12-deep-reorg-on-a-confirmed-crypto-deposit)'s anomaly queue
  (`ledger-cli reorgs list`, kind `shallow_reorg_failed`) instead.

**Severity**: P2 by default — the deposit is safely parked, no ledger effect
has happened yet (invariant I-21: a `review` booking's `journal_uid` is
always empty). Escalate to P1 if `reconcile_mismatch` volume spikes (possible
sign of a compromised or lagging primary RPC source, or genuinely forged
sightings — exactly the unbounded-mint path this control exists to catch),
or if `reconcile_unavailable` volume spikes (your second source is down,
not the deposits).

### Work the queue

> Approving/rejecting requires an API key holding `CapabilityDepositReview`
> (`+deposit_review` on its scope, §10) — the key that ingests deposits does
> not get this for free, by design (W3-A, mi2).

1. **List pending reviews**:
   ```
   GET /api/v1/deposits/reviews?limit=50
   ```
   Returns deposit bookings currently in `review`, oldest first, cursor
   paginated. Each entry's `metadata.review_reason` tells you why it's here,
   and `amount` / `account_holder` / `channel_ref` (chain tx hash + log
   index) give you everything needed to verify against a block explorer or a
   second RPC provider.
2. **Verify the deposit independently** — same due diligence as an
   auto-reversed reorg (§12): check the transaction, its confirmations, and
   its amount against a source you trust that is *not* the primary sighting
   path (a second RPC provider, a public explorer, or your own
   `DepositConfirmer` backing service if one is configured).
3. **If genuine** (real deposit, just over the ceiling, or the reconciliation
   mismatch was a transient RPC blip and the amount independently checks
   out):
   ```
   POST /api/v1/deposits/{uid}/review/approve
   ```
   This posts the deposit's `deposit_confirm` journal through the exact same
   code path a normal auto-confirmed deposit uses (cross-linked via
   `event_id` — I-21) and moves the booking to `confirmed`. Idempotent: safe
   to retry, a second call on an already-confirmed booking is a no-op.
4. **If not genuine** (sighting does not independently verify, amounts
   disagree and you cannot reconcile them, or you suspect a forged/duplicated
   sighting):
   ```
   POST /api/v1/deposits/{uid}/review/reject
   { "reason": "<why -- goes on the booking's audit trail>" }
   ```
   Moves the booking to `failed`. **No journal is ever posted** — the
   deposit is never credited (I-21). Idempotent: safe to retry.
5. Calling either endpoint on a booking that is not currently in `review`
   (already resolved, or never routed there) returns a 409 conflict, not a
   silent no-op or a forced transition — if you see this, someone else
   already resolved it (or you have the wrong `uid`).

### Tuning false-positive rate

A high volume of `over_ceiling` reviews for legitimate large depositors means
`AutoCreditCeiling` is set too low for that chain/token — raise it (a
config-only change, not a code change). A high volume of
`reconcile_mismatch` against a stable `DepositConfirmer` backing service
more often signals a real problem with the *primary* sighting source (RPC
lag, wrong contract address, log-parsing bug) than the reconciliation
control being wrong — investigate the primary source before touching
`ReconcileCeiling`.

---

## 14. Onchain money-path metrics -- this library ships none of the alerting

> **Context (26-08-25 audit, `operability.md`)**: `observability/prometheus.go`
> registers every metric below; this library ships **no Prometheus alert
> rules of its own** (the service binary and its `deploy/` Helm chart were
> removed -- see `docs/audits/2026-08-25-financial-engineering/TODO.md` §0).
> Wiring a `PrometheusMetrics` registry into a scrape endpoint and writing
> alert rules on top of it is entirely your composition root's
> responsibility. This section is the "what does each metric mean and what
> do I do" reference that a from-scratch alert rule needs -- it existed for
> neither the onchain metrics nor `balance_drift_units` before this fix.

### Payment-affecting counters (page on any nonzero rate)

| Metric | Means | Action |
|---|---|---|
| `ledger_sweep_unattributed_total{chain_id}` | A sweep batch collected a token not in that chain's `CreditTokens` allowlist -- value moved to the factory's treasury with **no corresponding user ledger balance**. | Solvency-adjacent: identify the token and amount from the sweep transaction on-chain, decide whether to add it to `CreditTokens` retroactively (crediting the affected users) or treat it as an operational recovery (capital adjustment journal, `presets/capital.go`). Do not ignore -- this is unattributed custody, not free money. |
| `ledger_deposit_reorg_detected_total{chain_id}` | A previously-confirmed deposit's transaction has disappeared from the canonical chain (deep reorg). | Go straight to [§12](#12-deep-reorg-on-a-confirmed-crypto-deposit) -- this metric is that section's alert source. |
| `ledger_registration_rescan_failed_total{chain_id}` | `EnsureDepositAddress`'s background historical rescan (catching deposits sent before an address was registered) failed and did not retry to completion. | The "deposit sent before registration" gap stays open for that address/chain until a retry succeeds. Check watcher logs for the specific address/chain; a persistently failing rescan on one chain usually means an RPC provider issue on that chain specifically, not a code bug. |
| `ledger_deposit_ingest_dead_lettered_total{chain_id, reason}` | A transfer that IS on chain, to a registered address, in a whitelisted token, that this ledger decided **never to book** — after which the forward scan moved past it. No booking exists, so nothing else in the system will ever revisit it. | Go straight to [§18](#18-a-deposit-was-dead-lettered). `reason` is bounded: `payload_conflict`, `currency_unregistered`, `precision_exceeded`, `account_unavailable`, `period_closed`, `invalid_input`, `watcher_wedged`, `unclassified` — §18 has one line of triage each. |
| `ledger_sweep_orphaned_broadcast_total{chain_id}` | A sweep booking is `pending` at a nonce the signer has already spent: a broadcast whose transaction hash was lost before it could be persisted. The sweep path refuses to rebroadcast (it cannot tell "landed" from "still pending" without the hash). | This is the **only** condition that blocks a (chain, token)'s collection indefinitely — every later tick fails identically until a human acts. [§15](#15-a-chains-sweep-collection-has-stopped-moving)'s "booking stuck in pending at a spent nonce" has the recovery. |
| `ledger_deposit_review_required_total{chain_id, reason="onchain_unverified"}` | The chain does not corroborate a booking the recheck loop was about to credit (money-out C-2). | Treat as a security event, not a deposit question — [§13](#13-large--unreconciled-deposit-parked-in-review) explains why and what to check. Other `reason` values are ordinary review traffic; see the backlog table below. |

### Backlog / degradation gauges and counters (page on sustained growth, not a single blip)

| Metric | Means | Action |
|---|---|---|
| `ledger_chain_cursor_lag_blocks{chain_id}` | Blocks behind the chain tip the deposit watcher's cursor currently is. | **Not a liveness signal — see the row below before writing an alert on it.** It can only be computed once `LatestBlock` has answered, so an RPC outage, a database outage or a rejected `eth_getLogs` leaves it FROZEN at its last reading rather than climbing. When it *does* climb, the meaning is real: the watcher is seeing blocks but its cursor is being held still (usually an ingest failure it refuses to scan past, I-52), or it is too slow for that chain's block rate. |
| `ledger_chain_cursor_advance_age_seconds{chain_id}` | Seconds since this process last observed that chain's cursor MOVE. Reported on every watcher tick, including the ticks that fail before the tip is known. | This is the cursor-liveness signal. On a healthy chain it stays near the watch interval (15s by default) whatever the block rate, because the cursor advances to the safe tip every tick. Climbing without bound = the scan is failing, or the chain's head has stopped moving as far as this deployment can see; both are on-call events and neither shows in the lag gauge. ⚠️ It is a PER-PROCESS reading: a replica that keeps losing the per-chain advisory lock never scans, so alert on the **minimum across replicas**, not on any single series. |
| `ledger_dead_letters_unbooked` / `ledger_dead_letter_oldest_age_seconds` | The dead-letter queue's depth and the age of its oldest still-unbooked row, sampled once per deep-reorg recheck tick. | Self-clearing: a row whose deposit was booked in the end (replayed, or the cause self-healed) leaves the gauge on its own, so a non-zero depth means work that is still owed. Depth alone is not enough — a depth of 3 seconds old is an inbox, the same depth a week old is a forgotten queue. [§18](#18-a-deposit-was-dead-lettered). |
| `ledger_job_tick_completed_total{job}` / `ledger_job_tick_failed_total{job}` / `ledger_job_tick_skipped_locked_total{job}` / `ledger_job_panicked_total{job}` | Per-tick outcome of every background job, this library's generic worker loops included. `increase(ledger_job_tick_completed_total{job="..."}[window]) == 0` is the stalled-job alert; `skipped_locked` is what distinguishes "another replica is doing the work" from "nobody is". | The onchain jobs and their exact labels: `onchain_watch:<chain_id>` (forward scan, per chain), `sweep:<chain_id>` (collection, per chain — the prefix is `sweep:` and not `onchain_sweep:` because that string is also the advisory-lock key, and renaming it would let two releases sweep the same nonce during a rolling deploy), `onchain_recheck` (drives pending/confirming deposits to confirmed — **the loop that actually credits pull-path deposits**), `onchain_reorg_recheck` (the only reorg detector; also samples the dead-letter backlog), `onchain_registration_rescan`. A panicking tick counts as both `panicked` and `failed`. |
| `ledger_rollup_items_failed_total` | A rollup queue item's claim was released after a failed processing attempt (`RollupService.processItem` returned an error). | Check `RollupItemFailed`-adjacent logs (`"service: rollup: process item failed"`) for the specific `holder`/`currency_id`/`classification_id` and the underlying error. That dimension's checkpoint stops advancing until a retry succeeds -- balance reads stay correct via the delta path meanwhile ([§3](#3-rollup-queue-is-backlogged)'s "critical fact"), so this is urgency-by-volume, not an immediate correctness incident. |
| `ledger_template_failed_total{template, reason}` | An `entry_templates` execution failed (`TemplateFailed`). | The triggering business operation did not get its accounting posted. Cross-reference `reason` against [§7](#7-journal-posting-failures)'s table (same reason vocabulary) and find the caller that invoked the template. |
| `ledger_sweep_address_unreadable_total{chain_id}` | `ChainScanner.ScanBalances` could not read one or more deposit addresses' on-chain balance this sweep round. | Those addresses are excluded from that round's sweep-eligible set (not defaulted to zero) and simply retry next cycle. A single occurrence is an RPC hiccup; a sustained nonzero rate means that chain's RPC provider is degraded -- check node/provider health for that chain. |
| `ledger_deposit_review_required_total{chain_id, reason}` | A deposit reached its confirmation threshold but was routed to human review instead of auto-crediting (M3 compensating controls, design doc §9). `reason` is one of `over_ceiling`, `reconcile_mismatch`, `reconcile_unavailable`, `token_unconfigured`, `onchain_unverified` — plus `shallow_reorg_returned`, which is NOT a booking status and will not appear in the review queue. | Go straight to [§13](#13-large--unreconciled-deposit-parked-in-review) -- this metric is that section's alert source, and it lists what each reason means. `onchain_unverified` is in the payment-affecting table above instead: it is a security signal, not review traffic. |

### `balance_drift_units{class, currency_uid}` -- read this together with `reconcile_gap_units`

This gauge reports the magnitude of a debit-normal account going negative,
as observed by the rollup worker (`service.RollupService.processItem`) --
**0 when the most recently processed item for that (class, currency) label
is healthy, positive when it is not**. It is a *different* signal from
`reconcile_gap_units` ([§1](#1-reconciliation-failed)'s `checkpoint_balance`
/ `global_dr_cr_equality` checks), which is the actual checkpoint-tamper
detector: `reconcile_gap_units` catches a checkpoint that disagrees with a
fresh recompute from `journal_entries` (the class of drift a leaked DB
credential's direct `UPDATE` would produce); `balance_drift_units` catches
a *business-logic* violation (a debit-normal account was allowed to go
negative -- usually a missing `Reserve` step, same as
[§1](#1-reconciliation-failed)'s `non_negative_balances` check, just
observed at rollup time instead of reconcile time).

**If you are writing your own alert rule for these**: do not combine them
into one rule the way this library's now-removed `deploy/` chart used to
(`ledger_balance_drift_units != 0 or ledger_reconcile_gap_units != 0`).
Before this fix, `balance_drift_units` never reset to zero once triggered
(it was fed the raw balance, not the drift, and only ever `.Set()` on the
violation branch) -- a real checkpoint-tamper event and a stale
`balance_drift_units` reading that never clears look identical, and
silencing the alert to stop the false alarm silences the real detector
too. It is fixed now (this gauge clears to 0 on the next healthy rollup for
that dimension), but the two remain **independent business questions**
("did a checkpoint get tampered with" vs. "did an account go negative") and
belong in **independent alert rules** even so.

**M-3 fix (`.local/independent-review-2026-08-26.md`, board #43, same date as
this document but a later batch than the paragraph above): even fixed,
`balance_drift_units` is still not safe to alert on alone.** Its label set
is `(class, currency_uid)` WITHOUT holder (kept deliberately bounded), so a
HEALTHY item for a DIFFERENT holder sharing that label legitimately
`.Set()`s the same series back to 0 immediately after a genuinely-still-open
violation for a FIRST holder was reported -- the exact self-clearing
behavior the paragraph above describes as "fixed" is also what makes a real,
ongoing violation invisible the moment any other holder in the same bucket
is next processed. **Alert on `ledger_negative_balance_detected_total{class,
currency_uid}` instead** (`core.Metrics.NegativeBalanceDetected`, a monotonic
counter incremented on the same violation branch `balance_drift_units`
reads from): `increase(ledger_negative_balance_detected_total[window]) > 0`
cannot be un-incremented by an unrelated holder's healthy item the way the
Gauge can. Keep `balance_drift_units` for dashboards (a coarse "most recent
reading for this label" indicator), just not as the alerting source of
truth.

## 15. A chain's sweep collection has stopped moving

**Alert source**: `ledger_sweep_orphaned_broadcast_total{chain_id}` for the
one variant that never self-heals (see "booking stuck in pending at a spent
nonce" below), and `ledger_job_tick_failed_total{job="sweep:<chain_id>"}` for
every other failing sweep tick. There is still no metric for "this specific
nonce is taking a long time", so the ordinary version of this section is
triggered by noticing the watcher is healthy (deposits are still being seen)
but no `sweep`-classification bookings
for a chain/token have reached `confirmed` in longer than expected, or by a
user/support report that a chain's treasury balance has stopped growing
despite deposits continuing to arrive at CREATE2 addresses.

**Severity**: P2 -- funds are not lost (they sit safely at each deposit
address; deposits themselves keep landing), but they are not making it to
the treasury. Escalate to P1 if it has been stuck long enough that it looks
like it will never self-resolve (see below).

### Confirm

```sql
-- Find sweep bookings stuck in "sent" (broadcast but not yet confirmed) for
-- this chain/token, oldest first.
SELECT uid, metadata->>'chain_id' AS chain_id, metadata->>'token' AS token,
       channel_ref, status, updated_at
FROM bookings
WHERE classification_id = (SELECT id FROM classifications WHERE code = 'sweep')
  AND status = 'sent'
ORDER BY updated_at ASC;
```

A booking sitting in `sent` for much longer than `SweepPolicy.Interval` is
the "stuck" signal `Onchain.recheckSweepSent`'s gas-bump retry loop exists
to self-heal. Check the transaction at `channel_ref` (or the booking's
`metadata` for a fresher hash if this process has bumped it more than
once, though that tracking is in-memory only -- see below) on a block
explorer: if it shows `replacement transaction underpriced` failures in
your node's logs, the retry loop is fighting an underpriced replacement.

### Background: what "stuck" means here and the fix (26-08-26)

`Onchain.recheckSweepSent` gas-bumps (rebroadcasts at the same nonce, fee
+12.5%) a sweep transaction that has been in `sent` longer than
`sweepStuckAfter`. Before this fix, the fee floor for that bump came ONLY
from `chains/evm.Sweeper`'s own in-memory `lastFee` map -- wiped by every
process restart. A restart mid-retry (routine deploy, crash, OOM) meant the
next bump attempt quoted off the current market rate with **no bump at
all**, which is very likely to be *underpriced* relative to whatever is
genuinely still pending on chain if the original stall was caused by a gas
spike (the common case) -- the replacement gets rejected by the node
forever, and every later bump attempt inherits the identical blind spot.
**The chain's entire sweep collection stalls with no self-healing path**,
because EVM nonces are strictly sequential: that one stuck nonce blocks
every later sweep for the same signing key, even on unrelated chains/tokens
sharing it.

The fix: `Sweeper.BatchSweep` now also reads the ACTUAL fee of the
transaction at the caller-supplied `priorTxHash` straight from the chain
(`TransactionByHash`) before falling back to its in-memory map. The
booking's persisted `ChannelRef` (durable, survives a restart) is one
source of that hash; if this process has bumped more than once without a
restart, its own in-memory tracking (fresher, since `ChannelRef` only ever
reflects the first broadcast) is preferred automatically.

**Residual limitation, disclosed rather than hidden**: this closes the
"underpriced forever" failure mode, but the gas-bump *attempt counter*
(`SweepPolicy`'s implicit bump cap, tracked in `Onchain.sweepBump`, also
in-memory) still resets to zero on a restart. A chain that restarts
repeatedly while a sweep is stuck could therefore retry more times than
its configured cap intends before finally giving up and marking the
booking `failed` -- extra gas spend, not a correctness or fund-safety
issue (each retry now correctly outbids whatever is really pending, so it
either succeeds or is itself superseded by the next bump). Persisting the
bump count durably (so it survives a restart too) is tracked as follow-up
work, not done in this pass.

### Resolution

- If the transaction is genuinely stuck (underpriced, confirmed via block
  explorer / node logs): wait for the next scheduled `recheckSweepSent`
  tick -- with the fix above, it will now successfully out-bid the
  genuinely pending transaction. No manual intervention needed in the
  common case.
- If it has exceeded `SweepPolicy`'s bump cap and transitioned to `failed`:
  **the nonce is not necessarily freed.** `failed` means the transaction was
  broadcast repeatedly at that nonce and never observed included — from the
  signer EOA's perspective that slot is still "next", and `NextNonce`
  (`PendingNonceAt`) will keep reporting it until the stuck transaction
  lands, the node's mempool drops it, or somebody replaces it with a
  self-paying transaction at the same nonce. The library's own revival path
  (`reviveFailedSweep`) re-requests a fresh nonce precisely because it cannot
  assume otherwise. So: check the nonce sequence first (last bullet below);
  if an earlier unconfirmed nonce is still sitting there, adjusting
  `GasCeiling` changes nothing, because EVM nonces are strictly sequential
  and everything after it is blocked. Once the sequence is clean, investigate
  why gas stayed elevated long enough to exhaust the retry budget (a
  sustained network-wide gas spike, not a bug) and either raise
  `SweepPolicy.GasCeiling` for that chain/token if it was set too
  conservatively, or wait for gas to normalize — the next tick revives the
  failed booking on a fresh nonce by itself. Either way THIS batch's funds
  are still sitting at their deposit addresses, uncollected, not lost.
- If a chain's sweeps have stopped ENTIRELY (not just one stuck nonce): check
  for a bad nonce anywhere in the sequence first (`NextNonce` uses
  `PendingNonceAt`, so any earlier unconfirmed nonce for the signing key --
  including a manually-sent transaction outside this library -- blocks
  everything after it).

### A booking is stuck in `pending` at a spent nonce (`sweep.orphaned_broadcast`)

**Signal**: `ledger_sweep_orphaned_broadcast_total{chain_id}` nonzero, an
Error log line `service: onchain: sweep.orphaned_broadcast`, and every
subsequent tick for that (chain, token) failing with *"booking … is pending
at nonce N but the signer's pending nonce is already M -- the earlier
broadcast's tx hash was lost"*.

**What happened**: a tick broadcast the batch successfully and then failed
before it could persist the transaction hash (`BatchSweep` succeeded ->
`Transition(sent)` failed). The booking is left in `pending` with an empty
`channel_ref`, while the nonce it holds has been consumed on chain. The
sweep path refuses to rebroadcast, because without the hash it cannot tell
whether that first transaction landed (rebroadcasting would then get "nonce
too low" forever) or is still pending (the replacement would go out
underpriced). This fails closed, which is correct, and it is the only
condition in the subsystem that blocks an outbound channel **indefinitely**:
`findInFlightSweep` sees the booking every tick and returns the same
conflict.

**Recover it — the tx hash is findable, because the nonce is on the row**:

1. Identify the booking and its nonce:
   ```sql
   SELECT uid, metadata->>'chain_id' AS chain_id, metadata->>'token' AS token,
          metadata->>'nonce' AS nonce, metadata->>'addresses' AS addresses,
          status, channel_ref, updated_at
   FROM bookings
   WHERE classification_id = (SELECT id FROM classifications WHERE code = 'sweep')
     AND status = 'pending'
   ORDER BY updated_at ASC;
   ```
2. Look that nonce up on a block explorer **by the signer EOA + nonce** (not
   by hash — the hash is what was lost). Every explorer indexes an account's
   transactions by nonce; this is the one and only way to recover the hash.
3. **If a transaction exists at that nonce** (landed, or still in the
   mempool), hand the booking back its hash by transitioning it to `sent`:
   ```
   POST /api/v1/bookings/{uid}/transition
   { "to_status": "sent", "channel_ref": "<recovered tx hash>",
     "source": "onchain", "idempotency_key": "sweep-recover-<uid>-<tx hash>" }
   ```
   From there the ordinary loop takes over with no further manual steps: the
   next tick calls `TxIncluded` on that hash and either confirms the booking
   (if it landed) or gas-bumps it (if it is still pending).
4. **If the nonce was consumed by something that is not our sweep** (a
   manually-sent transaction from the same EOA — which should not happen, the
   signer is single-deployment by design), the batch was never broadcast.
   Move the booking out of the way so the revival path can re-dispatch it at
   a fresh nonce: transition it to `sent` with the *consuming* transaction's
   hash as `channel_ref` (the lifecycle only allows `pending -> sent`), then
   let it exhaust its bump budget to `failed`, or transition it to `failed`
   directly with the same shape of call. The next sweep tick finds the failed
   booking and revives it on a fresh nonce (`reviveFailedSweep`) — same
   booking, same audit trail.
5. Whatever the outcome, note the incident: an orphaned broadcast means a
   broadcast succeeded while the database write after it did not, which is
   worth understanding on its own (a connection pool exhausted mid-sweep, a
   database failover, a process kill between the two).

---

## 16. P5 signing key rotation

The library's own verifier (`authdev.LocalVerifier`) now holds a SET of
public keys. Rotation is additive: register the new key alongside every
retired one.

```go
verifier, err := authdev.NewLocalVerifierSet(map[string]ed25519.PublicKey{
    "prod-2026-01": oldPub, // retired, MUST stay registered
    "prod-2026-09": newPub, // current
})
```

`ledger-cli verify` takes the same set: pass `--pubkey` once per key, as
`keyID:hex` (repeatable).

Three things an operator has to know before rotating:

1. **Retired public keys must stay registered forever.** Journals are
   append-only and carry the key id they were signed with. Drop a retired
   key and every journal signed under it fails verification with
   `ErrUnknownAuthKey` -- which means, with `RequireVerifiedBalance=true`,
   that withdrawals are refused for every holder with history. That is
   fail-closed by design, not a bug, and it is not a small blast radius: it
   is every existing user.
2. **Register before you sign.** Deploy the verifier holding both keys
   first; only then switch the Attestor to the new private key. The reverse
   order leaves a window where freshly signed journals cannot be verified.
3. **Rotation does not shrink a leaked key's power.** `authdev.LocalVerifier`
   has no validity window: a key id it holds verifies any digest, forever.
   Because the retired key must stay registered (point 1), rotating a
   COMPROMISED key out does not stop it from signing a newly forged journal
   that this verifier will accept. Containing a leaked signing key needs a
   verifier that checks the journal's `effective_at` against a per-key
   `NotAfter` -- the dev implementation deliberately does not, and says so
   in its own doc comment. Treat a leaked signing key as an incident, not a
   rotation.

---

## 17. A booking's event was claimed by the wrong journal

**Symptom**: a booking's settling journal is refused forever with
`event "<uid>" is already linked to a journal: conflict`
(`ledger_journals_failed_total{reason="conflict"}`, §7), and
`bookings.journal_id` points at a journal that has nothing to do with that
booking's settlement. Everything downstream of that booking stops: no
accounting record, no further journal-bearing transition, and the booking
sits in its lifecycle indefinitely.

**How it happens**: `event_uid` is a caller-supplied link and I-51 rule 4 only
requires the claiming journal to touch the booking's
`(account_holder, currency)` — deliberately weak, because amounts and
classifications legitimately vary across fees, spreads and multi-leg
settlements. A buggy caller (or a credential with write scope) can therefore
take the link with any journal at all, and both `events.journal_id` and
`bookings.journal_id` are set-once. Journals are append-only, so the claimant
cannot be deleted.

**Confirm** — find what is holding the link, and satisfy yourself it is wrong:

```sql
SELECT e.uid            AS event_uid,
       e.to_status,
       b.uid            AS booking_uid,
       b.amount         AS booking_amount,
       j.uid            AS claiming_journal_uid,
       j.idempotency_key,
       j.source,
       j.total_debit,
       j.created_at
FROM events e
JOIN journals j ON j.id = e.journal_id
LEFT JOIN bookings b ON b.id = e.booking_id
WHERE e.uid = '<event-uid>';
```

A claim is wrong when that journal's legs are not this booking's settlement —
typically a much smaller amount, an unrelated classification, or a source
belonging to a different flow. **If it moved money, it is still a real
journal**: fix that separately with a reversal (I-51). Do not treat the
unlink as an undo.

**Resolve** — owner-only, one statement:

```sql
-- as ledger_owner (or a superuser). ledger_app gets 42501 by design.
SELECT ledger_unlink_event_journal('<event-uid>'::uuid);
```

It clears `events.journal_id`, and `bookings.journal_id` too when the booking
holds the same journal (never a different one). It refuses loudly if the
event does not exist or holds no link, so a runbook step cannot report
success having done nothing. Then re-drive the booking's settlement through
the normal path; the real journal can now claim the event.

**Afterwards**:

- The claiming journal is left exactly as it was, and its own
  `journals.event_id` still points at the event. That is deliberate: the row
  is append-only and it is the evidence. Nothing reads journals by
  `event_id`, and `event_id` is not part of the signed digest, so the stale
  pointer affects nothing but a manual query.
- The repair is audited twice: `config_table_changes` gets a row with
  `table_name = 'ledger_unlink_event_journal'` naming the event, both journal
  ids and the authenticated role, plus migration 020's automatic before/after
  rows for `events` and `bookings`. Include them in the postmortem.
- Find the caller. A legitimate service that claims events it does not own is
  the actual defect; the unlink only clears the symptom.

---

## 18. A deposit was dead-lettered

**Alert source**: `ledger_deposit_ingest_dead_lettered_total{chain_id, reason}`
— page on any nonzero rate. Backlog:
`ledger_dead_letters_unbooked` / `ledger_dead_letter_oldest_age_seconds`.

**Severity**: P1. A dead letter means a transfer that **is on chain**, to an
address this ledger issued, in a token this ledger credits, which the
ingestion path refused — after which the forward scan moved past it. Nothing
will revisit it on its own: no booking was created, so no recheck loop can
see it; the cursor is past its block, so no forward scan will; registration
rescans only cover newly registered addresses. Somebody's money arrived and
the ledger decided not to know about it.

Skipping is nevertheless the right behaviour for a deterministic rejection —
holding the cursor for one unbookable sighting would turn it into "this chain
ingests nothing, ever again" (I-52) — which is why the row, this section and
the replay exist.

### Confirm and triage

```bash
# The queue, newest first, with the sighting decoded (amount, token,
# recipient) and `booked` telling you whether it has since been credited.
ledger-cli dead-letters list --unbooked-only --limit 50

# Everything recorded about one of them, including the exact payload a
# replay would re-drive.
ledger-cli dead-letters show --uid <dead-letter-uid>
```

`booked: true` means the deposit was credited in the end — by an earlier
replay, or because the cause healed itself — and the row is history, not
work. That answer is recomputed from `bookings` on every read; nothing has to
remember to resolve anything.

The metric's `reason` label is a bounded classification of *why*, and each
one has a different fix:

| `reason` | What it means | What fixes it |
|---|---|---|
| `currency_unregistered` | The token's configured `CurrencyCode` names no currency in this ledger. | Create the currency (`POST /api/v1/currencies`), then replay. Configuration, fully recoverable. |
| `precision_exceeded` | The amount has more decimal places than the currency's `exponent` can represent. | Normally impossible after startup — `Onchain.Run` refuses to start when a token's `Decimals` exceeds its currency's `exponent` (I-69). Reaching it means the currency's exponent changed under a running deployment, or `Run()` was never called. Fix the exponent mismatch, then replay. |
| `payload_conflict` | An existing booking holds this sighting's idempotency key with a **different payload**. | A normalization bug on the producing side, not a chain event (design doc §6). Compare the dead letter's payload against the existing booking (`ledger-cli trace`); do NOT replay until you know which of the two is right. |
| `account_unavailable` / `period_closed` | The holder's account is frozen/closed, or the accounting period is closed. | Usually self-healing noise: the booking is created before the journal is attempted, so these normally surface on a booking the recheck loop keeps retrying — expect `booked: true` on the row shortly. If not, unfreeze / reopen, then replay. |
| `watcher_wedged` | Not a verdict about this sighting at all: the chain's scan has failed for several consecutive ticks and this sighting is one of the ones in the way. **The cursor has NOT moved past it.** | Fix the underlying failure (the watcher logs name it); the sighting is re-ingested by itself. Nothing to replay. |
| `invalid_input` / `unclassified` | The sighting is malformed, or the error matched no known sentinel. | Read the row's `reason` text — it is the raw error. `unclassified` is worth reporting: the classifier defaults unknown errors to *retryable*, so reaching this label means something else marked it permanent. |

### Replay one, after fixing the cause

```
POST /api/v1/deposits/dead-letters/{uid}/replay      # capability: deposit_review
```

or, in library mode, `service.Onchain.ReplayDeadLetter(ctx, uid)`.

This re-drives the recorded sighting through the **real** ingestion path —
the same `IngestDeposit` a watcher sighting takes, review gate included — so
it is idempotent (a sighting already booked resolves to the same booking and
posts nothing new) and it cannot be used to credit something the ordinary
path would have refused.

> **Why this is not a `ledger-cli` command.** Re-driving a sighting needs the
> chain set: a token's currency code and its auto-credit ceilings are Go
> configuration in your composition root, not rows in the database. A tool
> holding only `DATABASE_URL` could only offer a replay by asking the
> operator to re-type the mint bounds at 3am, which puts the money fence on
> the wrong side of the keyboard. So the CLI lists and shows, and the replay
> is served by the process that already holds the configuration.

A replay that answers `400` with *"this ledger has nothing to book for that
sighting"* means the address is not registered or the token is not in that
chain's `CreditTokens` — deliberately an error rather than a silent no-op,
since you asked for something to happen.

### Related queues

- `ledger-cli reorgs list` — chain anomalies awaiting close-out
  ([§12](#12-deep-reorg-on-a-confirmed-crypto-deposit)).
- `GET /api/v1/deposits/reviews` — deposits parked for a human
  ([§13](#13-large--unreconciled-deposit-parked-in-review)).
- The SQL for both, plus cursors and registration rescans, is in
  [§8](#8-common-investigation-queries).

---

## After-action checklist

For any P0/P1 incident:

- [ ] Postmortem doc filed.
- [ ] Did this invariant exist in [`INVARIANTS.md`](./INVARIANTS.md)? If yes,
      why didn't the test pin catch it? If no, add it.
- [ ] Add a regression test referencing the failing scenario.
- [ ] Add a reconcile check in `service/reconcile.go` (FullReconciliationService) if the failure
      pattern can be detected automatically.
- [ ] Update this runbook if the symptom or fix was not previously documented.
