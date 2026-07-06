# Backup & Disaster Recovery

This document defines how a ledger deployment survives losing its database —
the only stateful component. It covers the backup strategy (PITR), the
recovery targets (RPO/RTO), the restore procedure, and — most importantly —
how to **prove** a restored ledger is correct using the ledger's own
invariant machinery.

Everything here is about the standalone-service deployment (`cmd/ledgerd` +
PostgreSQL). Library-mode consumers embed the ledger schema in their own
database; the same principles apply, but backups belong to the host
application's DR plan.

> A double-entry ledger is exactly the kind of system where an unverified
> backup is indistinguishable from no backup. The restore drill below is not
> optional hygiene — schedule it.

---

## 1. What must be backed up

| Asset | Where | Why |
|-------|-------|-----|
| PostgreSQL cluster (all ledger tables) | your DB host / managed service | The entire ledger state: journals, entries, checkpoints, bookings, events, reservations, snapshots |
| `API_KEYS`, `EVM_WEBHOOK_SECRET`, `DATABASE_URL` | your secrets manager | Recovery must be able to boot ledgerd, not just restore rows |
| Deployment manifests (Helm values / compose env) | git | Rebuild the runtime |

Nothing else is stateful. `ledgerd` pods/containers are disposable; the
Docker image is reproducible from a git tag.

Migrations are embedded in the binary (`postgres/sql/migrations`, embed.FS),
so a restored database plus the matching release tag is always
schema-consistent — restore the data, run the same image version that wrote
it, then upgrade normally.

## 2. Backup strategy: base backups + WAL archiving (PITR)

The ledger is append-only and write-heavy on a small number of tables.
Point-in-time recovery (PITR) is the right shape: periodic base backups plus
continuous WAL archiving, restoring to any moment before the incident.

**Managed PostgreSQL** (RDS, Cloud SQL, Neon, …): enable the provider's PITR
feature and confirm two numbers — the WAL/transaction-log retention window
(≥ 7 days recommended) and the automated base-backup cadence (daily). That
satisfies this section; skip to §3.

**Self-hosted**: run one of the standard PITR agents — `pgBackRest` or
`wal-g` — with:

- **Base backup**: daily full (or weekly full + daily incremental).
- **WAL archiving**: continuous (`archive_command` / agent streaming). This
  is what gets RPO to seconds.
- **Off-site storage**: object storage in a different failure domain than
  the database (different region or at minimum different account/bucket
  with independent credentials — a compromised DB host must not be able to
  delete its own backups).
- **Retention**: ≥ 30 days of base backups, ≥ 7 days of WAL. Longer if your
  compliance regime says so; the ledger itself is append-only, so old
  journal history is also in every newer backup.

Do **not** rely on nightly `pg_dump` alone: a logical dump loses everything
since the last dump (RPO = up to 24h) and restores slowly at ledger scale.
A monthly `pg_dump` on top of PITR is fine as a belt-and-suspenders logical
copy and for environment cloning.

## 3. Recovery targets

Defaults for a production money-path deployment — tighten or relax per
product, but write the chosen numbers down here:

| Target | Value | Rationale |
|--------|-------|-----------|
| **RPO** (max data loss) | ≤ 5 minutes | Continuous WAL archiving; anything lost is the archive lag window |
| **RTO** (max downtime) | ≤ 60 minutes | Restore base + replay WAL + verify invariants + redeploy |
| **Verification** | mandatory, in RTO | A restored ledger that hasn't passed reconcile+solvency is not "recovered" (§5) |

The RPO window is exactly the money that can vanish. If ≤ 5 minutes of
journals is not acceptable for your product, use synchronous streaming
replication to a standby (RPO ≈ 0) and treat PITR as the second line.

## 4. Restore procedure

1. **Stop writes.** Scale ledgerd to zero (or revoke write access — see
   RUNBOOK [§9 Emergency: stop the ledger](./RUNBOOK.md#9-emergency-stop-the-ledger)).
   Restoring under live writes guarantees a split-brain ledger.
2. **Pick the recovery point.** Latest WAL position for infrastructure loss;
   a pre-incident timestamp for data corruption (e.g. a bad manual journal
   batch — though prefer reversal journals over PITR for logical errors;
   PITR discards *everything* after the point, not just the bad rows).
3. **Restore into a NEW instance/cluster.** Never restore over the incident
   database — it is evidence, and you may need a second attempt.
4. **Replay WAL** to the chosen point (`pgbackrest restore --type=time ...` /
   `wal-g backup-fetch` + `recovery_target_time`).
5. **Verify** (§5) against the restored database **before** exposing it.
6. **Repoint** `DATABASE_URL` at the restored instance, deploy the **same
   image version** that was running at the recovery point, then upgrade to
   current if needed (migrations are embedded and forward-only).
7. **Re-enable writes** and watch the RUNBOOK dashboards for one full
   rollup + reconcile cycle.
8. **File the postmortem** (RUNBOOK after-action checklist).

### Duplicate-delivery healing after PITR

Restoring to T discards journals after T, but the outside world (chain
scanners, PSPs) already saw your webhooks/responses. This is where the
idempotency contract pays out: re-sent events and client retries with the
same idempotency keys re-post cleanly; keys the restore rolled back simply
get re-created. Ask upstream channels to replay events from T-onwards
(e.g. re-scan blocks) — replay is safe by construction (I-4).

## 5. Verification: prove the restored ledger is whole

A restored ledger must pass the same invariant checks the live system runs
(this is the reason `cmd/ledger-cli` is read-only — point it anywhere):

```bash
export DATABASE_URL=<restored instance>

# 1. Full reconciliation — all 10 checks (I-1..I-13 coverage):
ledger-cli reconcile --full          # must print PASS on every check

# 2. Solvency per active currency:
ledger-cli solvency --currency <uid> # custodial >= user liability

# 3. Spot-check recent history against an external record you trust
#    (bank statement, chain explorer, PSP dashboard):
ledger-cli journals --limit 50
```

Only after all three: the restore is real.

### Scheduled backup validation (quarterly drill)

Backups rot silently — credentials expire, buckets fill, `archive_command`
breaks after a Postgres upgrade. Once a quarter:

1. Restore the latest base backup + WAL into a scratch instance (steps 3–5
   above — never touching production).
2. Run the §5 verification suite against it.
3. Record in the ops log: restore duration (your live RTO number), WAL lag
   at restore time (your live RPO number), and the reconcile output.
4. Tear the scratch instance down.

If the drill has not been run in the last quarter, treat the system as
having **no verified backup** and prioritize accordingly.

## 6. Monitoring the backup pipeline

Alert on these (wire into the same alerting as the RUNBOOK scenarios):

- WAL archive lag > 15 minutes (`pg_stat_archiver.failed_count` climbing,
  or your agent's lag metric) — this is your RPO eroding in real time.
- Base backup age > 36h (daily cadence missed).
- Backup storage credential expiry / permission errors.
- Restore-drill deadline exceeded (quarterly).

## See also

- [`RUNBOOK.md`](./RUNBOOK.md) — incident response, including
  [§9 Emergency: stop the ledger](./RUNBOOK.md#9-emergency-stop-the-ledger)
- [`INVARIANTS.md`](./INVARIANTS.md) — what §5 verification actually proves
- PostgreSQL docs: *Continuous Archiving and Point-in-Time Recovery*
