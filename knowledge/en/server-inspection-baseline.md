# Server inspection baseline

## What the inspection checks

| Check | Criteria | Common false alarms |
| --- | --- | --- |
| CPU load | 1-minute load per core >1 warn / >2 critical | Short spikes during the backup window are normal |
| Memory | available <20% warn / <10% critical; swap >50% watch | Linux caches with free memory — read **available**, not free |
| Disk partitions | usage ≥80% warn / ≥90% critical | Virtual filesystems (tmpfs/overlay) are excluded |
| Temperature | ≥80°C warn / ≥90°C critical | **On VMs reading no sensor is normal** — reported as "unknown" |
| Critical logs | kernel panic/OOM/hardware errors, I/O errors | dmesg covers *since boot*, so it may hold old entries |
| Service status | failed systemd units | A failed oneshot job does not necessarily affect service |

**"Unknown" is not "healthy"**: when the temperature or the logs cannot be read, the report
says so explicitly — do not read that as all-clear.

## How to read the report

1. Start with the verdict bar: critical / warning / ok / unknown.
2. Then read "Impact & remediation" — each problem comes with its impact and ordered steps.
3. The evidence block is the raw output; check it if you doubt a conclusion.
4. Findings tagged "rule library" come from the built-in failure signature table; findings
   tagged "model analysis" were written by an LLM and need your own judgement.

## Escalation

- **Critical** (data possibly damaged or service already down): act now; confirm the backup is
  usable before touching anything.
- **Warning** (degrading but still running): handle this week, record it first.
- **Unknown**: fix the collection path (add a sensor, open log permissions) and re-run.

## Log time windows

Log entries come from three sources with different windows — keep them apart when reading:

- `journalctl -p err`: the last 24 hours
- `dmesg`: the ring buffer **since boot**, so it may describe events weeks old
- `/var/log` tails: the last lines of the files

So "N error entries found" does not mean "N just happened". To judge age, look at the leading
timestamp (journald gives absolute time; dmesg gives seconds since boot).
