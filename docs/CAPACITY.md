# Capacity Baseline & SLOs

What one ledger deployment can do, how to size it, and what to promise.
Companion to [`RUNBOOK.md`](./RUNBOOK.md) (what to do when a number goes
red). This library ships no Helm chart and no bundled Prometheus alert
rules (the service binary and its `deploy/` chart were removed -- see
`docs/audits/2026-08-25-financial-engineering/TODO.md` §0); wiring alert
rules on top of the metrics this document references is your composition
root's responsibility (see RUNBOOK.md's §14 note).

## 1. Reference benchmark numbers

From `postgres/benchmarks_test.go` (`go test ./postgres/ -bench=. -benchtime=3s -run='^$'`).
Reference host: Apple M3 Max, PostgreSQL 17 in Docker Desktop, single
goroutine — **use these for relative comparison across code changes; re-run
on your production-shaped hardware for absolute planning numbers.**

<!-- F-m12: numbers below measured at commit 9eb2a19 (2026-09-02, d-ops remediation branch, pre-I-M1-metrics-wiring baseline -- re-run after landing PostJournal's new metrics defer if you need a post-fix figure). Re-run and update this anchor whenever you refresh this table (§5) so the next reader can tell drift-from-code-change apart from drift-from-different-machine. -->

| Operation | Latency (serial) | Allocations | Notes |
|-----------|------------------|-------------|-------|
| `PostJournal` (2-entry, same account every time) | ~3.5 ms/op | ~17.2 KB, 261 allocs | Worst-case lock contention path |
| `PostJournal` (fan-out across accounts) | ~4.2 ms/op | ~17.3 KB, 264 allocs | ≈ same as single-account: overhead is dominated by the DB round-trips + idempotency advisory lock, not row contention |
| `GetBalance` (checkpoint + 100-entry delta) | ~0.9 ms/op | ~4.0 KB, 72 allocs | Checkpoint-delta read; delta length is bounded by rollup freshness |
| `ListComputedBalancesForHolders` | ~1.6 ms/op | ~37.6 KB, 577 allocs | Backs the `RequireVerifiedBalance` withdrawal gate's entries-only recompute (I-49) -- on the withdrawal critical path, not just reporting |
| Reserve → Settle full cycle | ~3.2 ms/op | ~12.1 KB, 224 allocs | The billing critical path (advisory lock + balance check + FSM + settle) |

Interpretation:

- **A single serial writer sustains ≈ 400 journals/s**; the write path is
  round-trip-bound, so concurrent writers scale until PostgreSQL saturates
  (rule of thumb: usable concurrency ≈ CPU cores of the DB, beyond which
  latency grows without throughput).
- **Balance reads are cheap (< 1 ms) while checkpoints stay fresh.** The
  read cost is `O(delta entries since checkpoint)` — this is why a rollup
  backlog alert (`ledger_rollups_pending`, [RUNBOOK §3](./RUNBOOK.md#3-rollup-queue-is-backlogged))
  and a checkpoint-age alert (`ledger_checkpoint_age_seconds`,
  [RUNBOOK §4](./RUNBOOK.md#4-checkpoint-age-is-climbing)) are latency
  alerts, not hygiene alerts. (This library ships no alert rules named
  `LedgerRollupBacklog` / `LedgerCheckpointAgeHigh` or anything else --
  build your own on the two metrics above.)
- **Reserve/settle ≈ one journal post.** Budget 2 journal-equivalents per
  metered billing operation (reserve+settle), 1 per simple transfer.

## 2. Sizing guide

**PostgreSQL is the capacity bottleneck; the ledger itself holds almost no state in process.**

- **Host application replicas**: add them for request fan-in, not for ledger
  throughput. Workers coordinate via SKIP LOCKED + advisory locks, so replicas
  do not multiply write capacity; the database does.

- **Connection pool** (`pgxpool` default: 4×CPU cores, max):
  `total connections = replicas × pool max` must stay under the DB's
  `max_connections` minus headroom for migrations/ops/psql (keep ≥ 20%
  free). For 2 replicas against a 4-core DB, a pool max of 16 each (32
  total) against `max_connections=100` is comfortable. Configure via
  `pool_max_conns` in `DATABASE_URL`.
- **Database**: NVMe-backed storage, `shared_buffers` 25% RAM as usual.
  Write throughput scales with WAL fsync capacity — this is what to upgrade
  when journal posting saturates.
- **Table growth**: `journal_entries` ≈ (entries per journal) × journals;
  2-row journals at 100 journals/s ≈ 17 M rows/day ≈ moderate; monthly
  partitions (I-13) keep indexes bounded — archive cold partitions per
  RUNBOOK §11.
- **`config_table_changes` growth (D-M3 / D-m10)**: the audit trigger
  coverage widened from 4 tables to 11, and two of those — `bookings` and
  `reservations` — are **business-rate** writes (once per lifecycle
  transition / once per settle-release), not occasional config edits. Each
  audit row stores `to_jsonb(OLD)` + `to_jsonb(NEW)`, i.e. two full row
  copies per change. `config_table_changes` growth is therefore on the
  same order as those tables' own write volume, not "config table, changes
  rarely" — size and retention-plan it accordingly (`events`' own
  delivery-bookkeeping columns are already excluded via the trigger's
  `WHEN` clause, so retry/delivery churn does not inflate this table).

## 3. Suggested SLOs

Starting points for a production money path — adjust to product reality and
**write the agreed numbers here**:

| SLO | Target | Measured by |
|-----|--------|-------------|
| Availability (write API) | 99.9% monthly | `up` + 5xx ratio on POST routes |
| Journal post latency | p99 < 50 ms (service-local) | `ledger_journal_post_seconds` histogram |
| Balance read latency | p99 < 25 ms | HTTP-level probe on `GET /balances/*` |
| Checkpoint freshness | age < 1 h for all classes | `ledger_checkpoint_age_seconds` (alert at 3600s) |
| Reconciliation | full reconciliation suite pass every hour; failures page immediately | `ledger_reconciliations_completed_total` / `ledger_reconcile_check_results_total` |
| Event delivery | 99% of events delivered < 5 min; dead-letters page within 30 min | `ledger_events_delivered_total` / `ledger_events_dead_total` |
| Durability | RPO ≤ 5 min, RTO ≤ 60 min | [`DR.md`](./DR.md) targets + quarterly drill |

## 4. Scaling signals (in order of likelihood)

1. `ledger_journal_post_seconds` p99 climbing with flat request rate → DB
   saturation (check DB CPU / IO / lock waits). Scale the database first.
2. `ledger_rollups_pending` persistently > threshold → rollup worker
   starved; raise `RollupBatchSize` / add a replica (SKIP LOCKED shares the
   queue) before touching the DB.
3. HTTP 429s (rate limiter) without DB stress → raise per-IP limits or add
   host application replicas behind the load balancer.
4. Balance read p99 degrading while post latency is flat → checkpoint age
   (see RUNBOOK §4), not traffic.

## 5. Re-baselining

Re-run the benchmark suite and update §1 whenever: the write path changes
(journal posting, idempotency, reservation FSM), PostgreSQL major version
bumps, or before committing to a new customer-facing SLO. One command:

```bash
go test ./postgres/ -bench=. -benchtime=3s -run='^$'
```
