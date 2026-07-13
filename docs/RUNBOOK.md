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

Backup & disaster recovery (PITR, RPO/RTO, restore drill) lives in its own
document: [`DR.md`](./DR.md).

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

Dead-lettered events (`delivery_status = 'dead'`, alert
`LedgerEventDeliveryDead`) are **parked, not lost** — the event row is the
system of record, delivery bookkeeping just marks it undeliverable.

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

**Alert source**: `IdempotencyCollision{journal_type_code="..."}` counter
spiking for a journal type.

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

## 10. Deployment security boundary

`authMiddleware` (`server/middleware_auth.go`) requires a bearer API key on
**every** endpoint, reads included — only the Kubernetes probes and the
webhook surface (channel-adapter HMAC) are exempt. Keys carry a scope
(`read` < `write` < `admin`, configured as `name:scope:secret` triples in
`API_KEYS`; see [`api.md`](./api.md)) and the key name is attached to every
access log line for audit.

Operational guidance:

- **Issue the least scope that works.** Reporting/dashboard consumers get
  `read`; the application that posts journals gets `write`; `admin` keys
  (metadata mutations, reconcile triggers, period close) belong to operators
  only and should be rare.
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

Standalone-service mode (`cmd/ledgerd`) should still not be exposed directly
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

**Alert source**: `deposit.reorged` event (emitted by the watcher's periodic
recheck of recently-confirmed deposit bookings against the canonical chain —
docs/plans/2026-07-11-crypto-deposit-sweep-design.md §6).

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
   ```sql
   -- Find the affected booking + its journal.
   SELECT uid, account_holder, currency_id, amount, status, channel_ref, journal_id
   FROM bookings WHERE uid = '<booking_uid>';
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
5. File the incident per the after-action checklist below regardless of
   outcome — a `manual`-policy deep reorg alert firing at all is worth
   understanding (chain instability, RPC provider issue, or a confirmation
   threshold set too low for that chain).

### `auto_reverse` — already handled, verify it landed correctly

If the consumer configured `ReorgPolicyAutoReverse`, the watcher posts the
reversal journal automatically on detection — no on-call action needed to
*initiate* the correction. On-call's job is to **verify** the automatic
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
the money moves. Selecting `auto_reverse` is an explicit risk acceptance by
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

**Alert source**: `deposit.review_required` (emitted by the deposit path's M3
compensating controls when a deposit clears its confirmation threshold but
must not yet be auto-credited — docs/plans/2026-07-11-crypto-deposit-sweep-design.md
§9). Two possible reasons, recorded on the alert and on the booking's
`metadata.review_reason`:

- `over_ceiling` — the amount exceeds the chain/token's configured
  `AutoCreditCeiling`.
- `reconcile_mismatch` — a second, independent confirmation source
  (`DepositConfirmer`) either does not see the transaction included, or sees a
  different amount than the primary sighting.

**Severity**: P2 by default — the deposit is safely parked, no ledger effect
has happened yet (invariant I-21: a `review` booking's `journal_uid` is
always empty). Escalate to P1 if `reconcile_mismatch` volume spikes (possible
sign of a compromised or lagging primary RPC source, or genuinely forged
sightings — exactly the unbounded-mint path this control exists to catch).

### Work the queue

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

## After-action checklist

For any P0/P1 incident:

- [ ] Postmortem doc filed.
- [ ] Did this invariant exist in [`INVARIANTS.md`](./INVARIANTS.md)? If yes,
      why didn't the test pin catch it? If no, add it.
- [ ] Add a regression test referencing the failing scenario.
- [ ] Add a reconcile check in `service/reconcile_full.go` if the failure
      pattern can be detected automatically.
- [ ] Update this runbook if the symptom or fix was not previously documented.
