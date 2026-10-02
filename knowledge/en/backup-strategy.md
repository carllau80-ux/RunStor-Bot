# Backup strategy design

## Three backup types and what they cost

| Type | Backup speed | Restore speed | Restore granularity | Typical use |
| --- | --- | --- | --- | --- |
| Full | Slow, large | Fast | Whole set | Weekly baseline |
| Incremental | Fast | Needs the full + the chain | To a point in time | Daily |
| Logical export (dump) | Slow | Slow | Single DB/table | Small DBs, migration, cross-version |

**The longer the incremental chain, the slower the restore and the more a single bad
increment hurts.** A common compromise: one full per week, incremental daily, and a monthly
merge that folds older increments into the baseline.

## Retention

Aim for **3-2-1**: at least 3 copies, on 2 kinds of media, 1 off-site (or offline).

Rules of thumb (adjust to your compliance requirements):

- daily: keep 7 days
- weekly: keep 4-5 weeks
- monthly: keep 12 months
- yearly: per compliance (often 3-7 years)

More retention is not automatically better: expired backups consume space and stretch the
backup window — and **a backup that was never verified is not a backup**.

## Turning RPO / RTO into configuration

- **RPO (how much data you may lose)** drives frequency and whether archiving is needed.
  RPO of 8 hours means at least one backup every 8 hours; databases also need archiving,
  otherwise you can only restore to the last backup point.
- **RTO (how long recovery may take)** drives physical vs logical and whether a recovery
  environment must be ready. A database with a 30-minute RTO cannot be restored from a
  logical export.

Write those two numbers down first, then derive frequency, type and retention. Doing it the
other way round produces policies that look diligent but cannot actually be restored.

## Restore drills

At least quarterly, and record: duration, success, what was missing. Drill in an **isolated
environment** — never against production. A drill also verifies that the backup can be *read*;
plenty of incidents happen because the media was bad while the job kept reporting success.

## Common causes of backup failure

1. Out of disk space (most common).
2. Target media unreachable (NFS/iSCSI mount dropped).
3. Database archive destination full; the backup waits for archiving.
4. Backup window overlapping peak load, timing out on lock waits.
5. Expired credentials (a domain account password change breaks the job and nobody notices).

Monitor the backup jobs themselves: **alert after two consecutive failures**, rather than
discovering it during a restore.
