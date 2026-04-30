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

---

## 1. Reconciliation failed

**Alert source**: `ReconcileCompleted{success="false"}` Prometheus counter or the
`POST /api/v1/reconcile` endpoint returning `balanced: false`.

**Severity**: P1. The ledger is reporting an internal accounting violation.

### Confirm it's real

```bash
# Run the full reconcile suite once more to make sure it's not a flake
curl -X POST http://ledger/api/v1/reconcile | jq .

# Or run all 10 checks at once via the service:
ledger-cli reconcile --full
```

A real failure includes a `details[]` array naming the dimension(s) that drift.
Each detail has `expected`, `actual`, and `drift` — do not panic at a
sub-cent drift; check the `drift` magnitude first.

### Investigate

The 10 reconcile checks (in `service/reconcile_full.go`):

| # | Check | Means |
|---|-------|-------|
| 1 | balanced system | Σ(debits) = Σ(credits) globally |
| 2 | per-currency balance | (1) but per currency |
| 3 | orphan entries | journal_entries with no matching journal |
| 4 | accounting equation | Σ(asset balances) = Σ(liability + equity) per currency |
| 5 | settlement netting | settlement classification cleanly nets to zero |
| 6 | non-negative user balances | no holder > 0 has balance < 0 |
| 7 | orphan reservations | reservations with no matching journal at settle time |
| 8 | pending journal timeout | journals stuck > N seconds in `pending` state |
| 9 | idempotency audit | duplicate keys (should be 0; UNIQUE prevents) |
| 10 | stale rollup | rollup queue items unclaimed for too long |

Match the failing check to the column in the error detail. Then:

- **Check 3 (orphan entries)** — almost always a manual `DELETE FROM journals`
  that didn't cascade. Restore from backup or post a correcting reversal.
- **Check 4 (accounting equation)** — the headline disaster. Stop accepting
  new writes (see §9), bisect the journal_id range to find the broken
  journal, post a reversal once identified.
- **Check 5 (settlement netting)** — usually a stuck FX or transfer leg
  (one side posted, the other didn't). Check `journals` table for orphan
  half-pair on the `settlement` classification.
- **Check 6 (negative user balance)** — a user got debited beyond their balance.
  Usually a missing `Reserve` step. Find the journal that drove the balance
  negative; reverse it; investigate the calling service.
- **Check 8 (pending journal timeout)** — the worker is stuck. See §3.

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
5. Write a postmortem; add a check to `service/reconcile_full.go` if the
   pattern can be detected automatically next time.

---

## 2. Solvency check failed

**Alert source**: `SolvencyCheck` returning `solvent: false` (margin < 0). Either
via `POST /api/v1/system/solvency` (when wired) or the system rollup query.

**Severity**: P0. The platform's ledger says it can't cover user liabilities.

### Confirm

```bash
ledger-cli solvency --currency 1
# or
curl http://ledger/api/v1/system/balances | jq '.rollups[] | select(.classification_code=="custodial")'
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

**Alert source**: `PendingRollups` gauge climbing, or
`GET /api/v1/system/health` reporting `rollup_queue_depth > 1000`.

**Severity**: P2. Reads are still correct (real-time delta), but checkpoints
are getting stale, which slows balance reads.

### Confirm

```sql
SELECT COUNT(*) FROM checkpoint_rollup_queue;
SELECT MAX(now() - created_at) FROM checkpoint_rollup_queue;
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

- **Dead worker**: restart the `service.Worker` (or the `ledgerd` binary if
  embedded).
- **Hot account**: increase rollup batch size or partition the worker by
  account holder modulo.
- **Capacity**: scale the worker count. See `cmd/ledgerd/main.go` for the
  worker loop config.

The critical fact: **balance reads stay correct** — the checkpoint+delta path
ensures that. Only checkpoint freshness (and rollup-table read latency) are
affected.

---

## 4. Checkpoint age is climbing

**Alert source**: `CheckpointAge{class_code="..."}` histogram >1h for any class.

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
SELECT id, url, last_status_code, last_error FROM webhook_subscribers;
```

### Resolution

- If a subscriber is dead, deactivate it: `UPDATE webhook_subscribers SET is_active=false WHERE id=...`.
- If transient, reset attempts: `UPDATE events SET attempts=0, next_attempt_at=now() WHERE ...`.
- If a subscriber's HMAC secret rotated and signatures fail, update the
  secret column and reset attempts.

---

## 6. Idempotency collision spike

**Alert source**: `IdempotencyCollision{journal_type_code="..."}` counter
spiking for a journal type.

**Severity**: P3 by default; P1 if the type is `withdraw_confirm` or anything
that moves real money.

### Investigate

A collision means two posts arrived with the same `idempotency_key`. The
second one returned `ErrDuplicateJournal` (correct behaviour). Causes:

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

**Alert source**: `JournalFailed{journal_type_code, reason}` counter.

**Severity**: depends on `reason`.

| reason | likely cause | action |
|--------|--------------|--------|
| `unbalanced` | bug in caller building entries | reproduce, file ticket on caller |
| `unauthorised_classification` | classification deactivated mid-flight | check `classifications.is_active` |
| `insufficient_balance` | user-side overdraft attempted | expected when caller didn't `Reserve` first |
| `currency_mismatch` | template params crossed currencies | bug in caller; check FX flow |
| `db_error` | pool exhausted / postgres down | check `system/health` and PG dashboards |

---

## 8. Common investigation queries

### Trace a booking end-to-end

```bash
ledger-cli trace --booking-id 12345
```

Or:

```sql
SELECT * FROM bookings WHERE id = 12345;
SELECT * FROM events  WHERE booking_id = 12345 ORDER BY occurred_at;
SELECT j.* FROM journals j
 JOIN events e ON e.journal_id = j.id
 WHERE e.booking_id = 12345
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

```sql
SELECT
  COALESCE(cp.balance, 0)
  + COALESCE(SUM(CASE WHEN je.entry_type='debit'  THEN je.amount END), 0)
  - COALESCE(SUM(CASE WHEN je.entry_type='credit' THEN je.amount END), 0) AS balance
FROM (SELECT 0::numeric AS balance, 0::bigint AS last_entry_id) seed
LEFT JOIN balance_checkpoints cp
  ON cp.account_holder = 42 AND cp.currency_id = 1 AND cp.classification_id = 1
LEFT JOIN journal_entries je
  ON je.account_holder = 42 AND je.currency_id = 1 AND je.classification_id = 1
 AND je.id > COALESCE(cp.last_entry_id, 0);
```

For debit-normal classifications add `(debit-credit)`; for credit-normal,
flip the sign at the end. Check `core.NormalSide` of the classification.

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

3. **Verify writes have stopped**:

   ```sql
   SELECT MAX(id), MAX(created_at) FROM journals;
   ```
   Re-run after a minute; the values should not change.

4. **After recovery**, restore privileges:

   ```sql
   GRANT INSERT ON journals, journal_entries, events, bookings TO ledger_app;
   ```

Reads remain available throughout. `GET /balances/*`, `GET /journals*`,
`GET /events*` are unaffected.

---

## After-action checklist

For any P0/P1 incident:

- [ ] Postmortem doc filed.
- [ ] Did this invariant exist in [`INVARIANTS.md`](./INVARIANTS.md)? If yes,
      why didn't the test pin catch it? If no, add it.
- [ ] Add a regression test referencing the failing scenario.
- [ ] Add a reconcile check in `service/reconcile_full.go` if the failure
      pattern can be detected automatically.
- [ ] Update this runbook if the symptom or fix was not previously documented.
