# Storage and disk troubleshooting

## ZFS checksum mismatch

**What it means**: data read from disk does not match its checksum. ZFS repairs it from a
redundant copy when it can; if it cannot, every copy of that block is corrupt — **silent data
corruption**, more dangerous than a failed disk.

Order of operations:

1. Record the affected file list from `zpool status -v` (put it in the ticket).
2. Make sure the latest backup is **not on the same pool**, and verify it is readable.
3. Run `zpool scrub <pool>` so ZFS repairs what it can.
4. Check hardware: memory (`memtest86+`), cables, HBA/backplane, power — these errors often
   come from the link rather than the disk.
5. If vdev error counts keep growing, replace that disk soon.

## Pool degraded (DEGRADED / FAULTED)

DEGRADED still does I/O, but **one more failed disk can take the whole pool down**; FAULTED
means the datasets already fail reads and writes.

1. `zpool status` to see which disk is missing and whether a resilver can run.
2. After replacing, `zpool replace` and wait for resilver — avoid heavy I/O meanwhile.
3. A slot that repeatedly drops disks usually means the backplane/cable/power, not the disk.

## Disk I/O errors

`blk_update_request: I/O error`, `medium error` and friends are device-level failures.

1. `smartctl -a /dev/sdX` for reallocated/pending sectors and the CRC error count.
2. CRC errors usually point at the link (cable/backplane/power), not the disk.
3. If the filesystem remounted read-only (`Remounting filesystem read-only`), stop the
   application before considering fsck.
4. Confirm data is readable and backups usable; prepare a replacement disk.

## iSCSI link: login rejected

`iSCSI Login negotiation failed`, `Security negotiation failed` or
`is not authorized to access iSCSI target portal group` means the target refused the initiator.

1. Check the ACL on the target: is the initiator IQN (`cat /etc/iscsi/initiatorname.iscsi`)
   in the allow list?
2. Check CHAP: username/password in `/etc/iscsi/iscsid.conf` must match the storage side, and
   decide whether mutual authentication is required.
3. Confirm port 3260 is reachable and not blocked by a firewall.
4. After fixing: `systemctl restart iscsid open-iscsi`, then `iscsiadm -m session -R`.

> `MODE SENSE: unimplemented page` is the kernel's "not supported" reply while probing device
> capabilities. **It does not mean reads or writes fail** (common with iSCSI targets, tape
> drives and virtual devices). Only act when the same device also logs I/O errors or slows down.

## Out of space

1. `df -h` to find the partition; `du -xh --max-depth=1 / | sort -h` for the biggest consumers.
2. Clean logs and temporary files first; confirm logrotate is working.
3. `lsof +L1` for deleted-but-still-open files held by processes.
4. Long term: grow or migrate, and add a capacity alert for that partition.

> A full root filesystem also breaks logins and temporary file creation, which looks like
> "the system is broken" — rule it out with `df -h` first.
