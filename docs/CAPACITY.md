# Capacity Baseline & SLOs

What one ledger deployment can do, how to size it, and what to promise.
Companion to [`RUNBOOK.md`](./RUNBOOK.md) (what to do when a number goes
red) and the Helm alert thresholds (`metrics.prometheusRules.thresholds`).

## 1. Reference benchmark numbers

From `postgres/benchmarks_test.go` (`go test ./postgres/ -bench=. -benchtime=3s -run='^$'`).
Reference host: Apple M3 Max, PostgreSQL 17 in Docker Desktop, single
goroutine — **use these for relative comparison across code changes; re-run
on your production-shaped hardware for absolute planning numbers.**

| Operation | Latency (serial) | Allocations | Notes |
|-----------|------------------|-------------|-------|
| `PostJournal` (2-entry, same account every time) | ~2.5 ms/op | ~13 KB, 288 allocs | Worst-case lock contention path |
| `PostJournal` (fan-out across accounts) | ~2.4 ms/op | ~13 KB, 291 allocs | ≈ same as single-account: overhead is dominated by the DB round-trips + idempotency advisory lock, not row contention |
| `GetBalance` (checkpoint + 100-entry delta) | ~0.7 ms/op | ~4 KB, 86 allocs | Checkpoint-delta read; delta length is bounded by rollup freshness |
| Reserve → Settle full cycle | ~2.7 ms/op | ~17 KB, 346 allocs | The billing critical path (advisory lock + balance check + FSM + settle) |

Interpretation:

- **A single serial writer sustains ≈ 400 journals/s**; the write path is
  round-trip-bound, so concurrent writers scale until PostgreSQL saturates
  (rule of thumb: usable concurrency ≈ CPU cores of the DB, beyond which
  latency grows without throughput).
- **Balance reads are cheap (< 1 ms) while checkpoints stay fresh.** The
  read cost is `O(delta entries since checkpoint)` — this is why
  `LedgerRollupBacklog` / `LedgerCheckpointAgeHigh` are latency alerts, not
  hygiene alerts.
- **Reserve/settle ≈ one journal post.** Budget 2 journal-equivalents per
  metered billing operation (reserve+settle), 1 per simple transfer.

## 2. Sizing guide

**PostgreSQL is the capacity bottleneck; ledgerd is nearly stateless.**

- **ledgerd replicas**: 2 for availability (PDB keeps 1 through drains).
  Add replicas for HTTP fan-in, not for ledger throughput — workers
  coordinate via SKIP LOCKED + advisory locks, so replicas don't multiply
  write capacity, the database does.
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

## 3. Suggested SLOs

Starting points for a production money path — adjust to product reality and
**write the agreed numbers here**:

| SLO | Target | Measured by |
|-----|--------|-------------|
| Availability (write API) | 99.9% monthly | `up` + 5xx ratio on POST routes |
| Journal post latency | p99 < 50 ms (service-local) | `ledger_journal_post_seconds` histogram |
| Balance read latency | p99 < 25 ms | HTTP-level probe on `GET /balances/*` |
| Checkpoint freshness | age < 1 h for all classes | `ledger_checkpoint_age_seconds` (alert at 3600s) |
| Reconciliation | full 10-check pass every hour; failures page immediately | `ledger_reconciliations_completed_total` / `ledger_reconcile_check_results_total` |
| Event delivery | 99% of events delivered < 5 min; dead-letters page within 30 min | `ledger_events_delivered_total` / `ledger_events_dead_total` |
| Durability | RPO ≤ 5 min, RTO ≤ 60 min | [`DR.md`](./DR.md) targets + quarterly drill |

## 4. Scaling signals (in order of likelihood)

1. `ledger_journal_post_seconds` p99 climbing with flat request rate → DB
   saturation (check DB CPU / IO / lock waits). Scale the database first.
2. `ledger_rollups_pending` persistently > threshold → rollup worker
   starved; raise `RollupBatchSize` / add a replica (SKIP LOCKED shares the
   queue) before touching the DB.
3. HTTP 429s (rate limiter) without DB stress → raise per-IP limits or add
   ledgerd replicas behind the LB.
4. Balance read p99 degrading while post latency is flat → checkpoint age
   (see RUNBOOK §4), not traffic.

## 5. Re-baselining

Re-run the benchmark suite and update §1 whenever: the write path changes
(journal posting, idempotency, reservation FSM), PostgreSQL major version
bumps, or before committing to a new customer-facing SLO. One command:

```bash
go test ./postgres/ -bench=. -benchtime=3s -run='^$'
```
