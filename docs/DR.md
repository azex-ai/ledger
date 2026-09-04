# Backup & Disaster Recovery

This document defines how a ledger deployment survives losing its database —
the only stateful component. It covers the backup strategy (PITR), the
recovery targets (RPO/RTO), the restore procedure, and — most importantly —
how to **prove** a restored ledger is correct using the ledger's own
invariant machinery.

This repository is a library, so the database it describes is the consumer's:
the ledger schema lives in the host application's PostgreSQL, and these
backups belong to that application's DR plan. What is specific to the ledger
is section 5 — proving a restored database is a correct ledger, which the
ledger's own invariant machinery can answer and a generic restore check
cannot.

> A double-entry ledger is exactly the kind of system where an unverified
> backup is indistinguishable from no backup. The restore drill below is not
> optional hygiene — schedule it.

---

## 1. What must be backed up

| Asset | Where | Why |
|-------|-------|-----|
| PostgreSQL cluster (all ledger tables) | your DB host / managed service | The entire ledger state: journals, entries, checkpoints, bookings, events, reservations, snapshots |
| `DATABASE_URL`, and any signing key configured through `ledger.WithAttestor` | your secrets manager | Recovery must be able to start the host application, not just restore rows. A lost signing key does not invalidate past journals -- their signatures stay verifiable against the recorded `auth_key_id` -- but nothing new can be signed under it |
| The host application's own deployment manifests | git | Rebuild the runtime that embeds the ledger |

Nothing else in the ledger is stateful. Everything it holds is in PostgreSQL.

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

1. **Stop writes.** Stop the host application (or revoke write access — see
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

A restored ledger must pass the same invariant checks the live system runs.
`cmd/ledger-cli` is safe to point at a restored/suspect instance for this,
but it is **not** purely read-only (I-M6): `reconcile --full` persists its
resume cursor to `reconcile_scan_cursors`, a real write. That write is
harmless evidence-wise (it advances the *check's own* bookkeeping, not
ledger data), but if you are examining a database that is itself under
forensic hold for an unrelated incident, run `reconcile --full` (and only
that command) against a **clone**, per the same rule any other write tool
would follow. Two other commands also write, and neither is part of this
verification: `rollup reset-claim` and `reorgs resolve` are the tool's two
deliberate operator write actions (see `cmd/ledger-cli/main.go`'s package
doc). Every command this section uses (`solvency`, `journals`, `balance`,
`trace`, ...) only reads.

```bash
export DATABASE_URL=<restored instance>

# 1. Full reconciliation — the complete check suite (I-1..I-13 + I-23/I-24/I-32 coverage).
# jsonOut prints the raw report to stdout (no HTTP envelope) — overall_passed
# is true only when every check in checks[] passed:
ledger-cli reconcile --full | jq -e '.overall_passed == true'

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

### Drill log

**2026-08-26 — first drill ever run against this procedure.** Before this
date §5's three-step verification had only been read, never executed
(2026-08-25 financial-engineering audit §6: "restore flow only got static
review"). Executed in an isolated Docker network (`dr-drill-net`, host ports
15432–15435), never touching the shared local-dev Postgres on 5432 — see
`~/.claude/rules/infra.md`. All scratch containers, volumes, and the network
were torn down afterward; nothing was committed except this record.

Procedure: `postgres:17.2-alpine` primary with `archive_mode=on` and a
file-based `archive_command` to a bind-mounted directory (stand-in for
pgBackRest/wal-g's WAL shipping — this repo has no managed-Postgres account
to test the provider-PITR path against, so this drill exercises the
self-hosted §2 mechanism, which is the stricter case). Seeded via
`examples/embed`'s pattern (currency/classification/journal-type bootstrap +
`PostJournal`) run through a throwaway seed program: 20 journals
(the "known-good" batch), a `pg_basebackup`, 20 more journals, an explicit
WAL segment switch to mark the recovery point, then 20 more journals
representing data to be discarded (a bad batch, modeling the "logical
corruption" §4 step 2 scenario). Restored into a **new** container from the
base backup + archived WAL with `recovery_target_time` set to the point
between the good and bad batches, `recovery_target_action=promote`.

Results:

- **RTO (mechanical restore, this dataset)**: Postgres itself went from
  `database system was interrupted` to `ready to accept connections` in
  **~0.45s** (see container log timestamps 15:33:03.925 → 15:33:04.379) —
  base backup restore + WAL replay + promotion. At 120 journal-entry rows
  this number does not extrapolate to production scale (`docs/CAPACITY.md`
  is the place for a sizing-driven RTO estimate); it confirms the mechanism
  is correct and fast, not a production timing benchmark.
- **RTO (wall clock, including drill tooling)**: ~110s from "stop writes"
  to "reconcile --full all green" — dominated by manually building
  `ledger-cli`, `chown`-ing the copied data directory for the container,
  and running the verification commands one at a time, not by Postgres.
- **RPO**: the drill picked its own recovery point (mid-stream, by design)
  rather than measuring live archive lag against an actual incident.
  `pg_stat_archiver` on the primary showed `failed_count=0` throughout —
  this is the metric §6 says to alert on for a production RPO number; the
  drill validated the *mechanism* reads correctly, not a live-lag figure.
- **Verification (§5)**: `ledger-cli reconcile --full` — **every runnable
  check `passed: true`**, `overall_passed: true` (a literal check count
  is deliberately not given here: the suite has kept growing since this
  drill was run, and any number written down would drift again — see
  `TestReconcileFullFlagUsage_DoesNotHardcodeACheckCount`).
  `full_coverage: false` at drill time (2026-08-26) because `reconcile --full`
  had no flag to wire an `AuthVerifier` at all, so `unauthorized_journals`
  could never run from the CLI regardless of how the seed script was
  written — a product gap (I-R2), not a one-off seed-script omission as
  first assumed here. Fixed 2026-09-02: `ledger-cli reconcile --full
  --pubkey-hex <hex> --key-id <id>` now covers it (see the CLI's own
  `-h` output).)
  `ledger-cli solvency --currency <USDT uid>` → `"solvent": true`.
  `ledger-cli journals --limit 50` → all 40 expected journals present
  (20 good + 20 good), the 20 "bad" batch correctly absent — PITR cut
  exactly where intended.
- **Full logical restore also tested** (the §2 "belt-and-suspenders"
  `pg_dump`): restoring a `pg_dump -Fc` of the primary into a brand-new
  instance reproduced `max(id) == journal_entries_id_seq.last_value`
  exactly (120 == 120) — `pg_dump` emits an explicit `setval()` for the
  actual sequence state at dump time, so this path is sequence-safe too.
  One friction point found and worth fixing in this doc later: restoring
  into a **truly virgin** cluster (one `ledger.Migrate` never touched)
  throws `GRANT ... role "ledger_app" does not exist` errors during
  `pg_restore` for every role-scoped GRANT the dump captured — harmless
  (`pg_restore --no-owner` + role errors are non-fatal, data still lands),
  but §4 doesn't currently tell the operator to expect it. Not fixed here
  per this task's scope (docs-only, and this is upstream of the sequence
  question this drill was chartered to answer).

Tear-down verified: `docker ps` after cleanup showed zero `dr-drill-*`
containers/networks and `dev-postgres` (shared local-dev instance, port
5432) unaffected throughout.

### Investigated: does restore ever regress `journal_entries_id_seq` below `max(id)`?

Migration `008_journal_entries_id_sequence_only.up.sql`'s own comment names
two triggers for the cross-partition duplicate-`id` risk it closes for
`ledger_app`: an explicit-`id` `INSERT` under a leaked credential, **or**
"a sequence that regresses after a PITR restore and starts re-issuing ids
the table has already seen in an older partition." Only the first is
guarded by 008 (a column-level `GRANT` that blocks `ledger_app` specifically
from naming `id`). This drill tested the second claim directly, since
nothing in this repo — not `DR.md` §4, not any of `reconcile`'s checks — ever
checks sequence health, so if the claim is true it's a live, unguarded gap.

**Verdict: the claim does not hold for either restore path this repo
documents or endorses (§2/§4 physical PITR, §2's logical `pg_dump`
belt-and-suspenders copy). Evidence:**

- After the physical-PITR drill above discarded the 20-journal "bad" batch
  (which had already consumed sequence values through `nextval()` before
  being rolled back — ids up to 120), the restored instance's
  `journal_entries_id_seq.last_value` was **106**, against a restored
  `max(id)` of **80**. The sequence came back *ahead* of the highest
  surviving row, not behind it — the opposite of the regression the
  migration comment describes.
- This is not a coincidence of this dataset: PostgreSQL WAL-logs a
  sequence's counter in advance of actual consumption (looking ahead by a
  fixed block, independent of transaction commit/rollback) specifically so
  that crash recovery — which is mechanically what PITR replay and replica
  promotion both are — can never re-issue a value that `nextval()` may
  already have handed to a caller pre-crash. `nextval()`'s WAL record is
  always written before the row that consumes its value can be built, let
  alone committed, so any replay that reaches far enough to show a given
  row also necessarily replayed that row's `nextval()` log entry first.
  This is true independent of *how* the WAL is delivered — file-based
  `archive_command` (this drill), streaming replication + promotion, or a
  managed provider's PITR — because it's a property of the WAL stream
  itself, not of the restore tooling.
- The logical path (`pg_dump`/`pg_restore`) is safe by a different,
  simpler mechanism: `pg_dump` captures the sequence's actual `last_value`
  at dump time via an explicit `setval()` call in the dump, and restoring
  it reproduced `max(id) == last_value` exactly (120 == 120, see above).

**What this drill did not settle**: a restore that mixes old row data
(explicit `id`s) into an *already-diverged, already-running* live
database — as opposed to restoring into a brand-new instance per §4 step 3
— was attempted and abandoned rather than forced. `journal_entries` is
partitioned (composite PK is partition-local, the exact gap 008 closes for
`ledger_app`) but the `journals` table it references is not (single-column
PK, global), so a naive partial restore hits a `journals` PK conflict
before the `journal_entries` duplicate-id question is even reachable —
producing either an outright error or FK-satisfied-but-semantically-wrong
data, not the clean silent duplicate the migration comment describes.
Reproducing the exact scenario cleanly would require hand-crafting an
explicit-`id` `INSERT` under an elevated role (`ledger_owner`/superuser,
which 008 does not restrict) rather than replaying an actual backup —
which is the ad hoc "just restore the affected rows" shortcut §4 step 2
already tells operators to avoid for logical errors, for an unrelated
reason (PITR discards everything after the point, not just bad rows). That
makes it an **operator-access-control** question (should `ledger_owner`
itself be reachable for ad hoc writes outside a migration?), not a
**restore-procedure** question, so it is out of this drill's scope; noted
here rather than pursued further to avoid manufacturing a scenario no real
restore path produces.

**Already decided (2026-08-26)**: migration 008's comment is left
as-written — already-applied migrations are never edited
(`deployment.md`) — but the "sequence that regresses after a PITR restore"
clause it names as a trigger is corrected in
[`docs/INVARIANTS.md`](./INVARIANTS.md)'s I-42 "Correction (2026-08-26)"
section: that claim is false (PostgreSQL WAL-logs sequence advancement
*ahead* of `nextval()` consumption, so a restored sequence comes back at or
ahead of the highest id in the table, never behind — measured directly on
this drill's restored instance, see I-42). No code or `reconcile` check
changes were needed: there is no verified failure mode for either checks or
restore steps to guard against.

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
