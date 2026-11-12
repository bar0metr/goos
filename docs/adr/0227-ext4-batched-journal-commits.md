# ADR 0227: ext4 batched journal commits

## Status

Accepted. Changes the durability rule of ADR 0225 and ADR 0226: an ext4
operation is no longer durable when the service replies, only after a
sync. Keeps the commit order, the replay and the recovery of ADR 0225.

## Context

ADR 0225 gave every ext4 operation its own JBD2 transaction and committed
it before the service replied. A transaction costs two device flushes, a
descriptor, a copy of each changed metadata block in the log, a commit
block, and the home writes. ADR 0226 noted the cost and left batching to
the release performance work. On the canonical test volume a create cost
2.03 flushes and 13.95 device writes, an appended 4 KiB block 2.03 and
11.08, and a create followed by an unlink 4.04 and 28.02. Under KVM a
flush reaches the host's disk cache, so it bounds what a GoOS volume can
do per second.

Linux ext4 batches. A `write` or a `rename` that returned is not durable
until `fsync`, `syncfs`, the commit interval (five seconds by default) or
an unmount. Applications that need durability call `fsync`; the 0.3.0
specification asks for that contract and no more.

## Decision

**Layers.** Each operation still runs in a transaction of its own, staged
in memory. That layer now sits over a pending transaction holding every
operation that finished since the last commit. Reads look at the
operation's layer, then the pending one, then the device. An operation
that fails drops its own layer and leaves the pending one as it was: a
block the pending transaction holds is copied into the operation's layer
before the operation changes it. An operation that succeeds stages its
inodes and superblock counters and folds its layer into the pending one.

**Accounting.** A block both layers hold counts once against the limits
of ADR 0224, since the commit writes it once. Blocks either layer freed
stay unavailable until the commit, so the pending transaction never frees
and reuses a block, which replay depends on. Free block and inode counts,
the orphan list and `df` see the operations still in memory.

**When the batch commits.**

- `Sync`, which `fsync`, `fdatasync`, a directory `fsync` and
  `OperationFlush` reach;
- unmount, before the reclaim of ADR 0225;
- the mount: its own superblock transaction, then replay and orphan
  cleanup are durable before the service answers the VFS;
- the next step of an operation, when it would not fit beside the batch.
  The batch commits alone, so the running operation does not split. Only
  an operation too large for one transaction commits part of itself, as
  ADR 0224 already allowed for writes, truncates and releases;
- a finished operation that leaves the batch past half of a transaction:
  128 metadata blocks, 128 file data blocks or 256 freed runs;
- the service's commit interval, one second after the first operation of
  a batch finished. If the service cannot read its clock it commits at
  once;
- the VFS going away: the service commits what it holds before it exits.

**Failure.** The operations in the pending transaction have already
answered. A commit that fails leaves the volume failed, the service exits
as ADR 0226 describes, and the replacement replays what committed before.
Operations that answered since then are lost, as unsynced writes are when
Linux crashes. A `Sync` that answered success is never lost.

**Storage sync.** The VFS storage sync now asks every attached additional
volume to commit before it flushes the persistent volume. A volume that
refuses fails the sync, so `reboot` and `poweroff` report it as they
report a persistent volume that refuses. A new shell builtin, `sync`, sends
that request through PID 1: `GCTL0001` moves to version 6 for the new
control operation `ControlSync`, and refuses version 5.

**Coalesced writes.** A commit writes consecutive blocks, file data,
the log body or home metadata, as one device write of up to 128 KiB, the
bulk window of the service's block endpoint, instead of one write per
block.

**Checksums.** CRC32C folds eight bytes per step with eight 256-entry
tables. A directory block that passed its checksum keeps that result
while it stays in the block cache, for the directory seed it was checked
under; a block read again, or rewritten by a commit, is checked anew.

**Diagnostics.** `ext4.Status` reports `PendingBlocks`, the blocks the
batch holds.

## Durability

| Power cut after | Next writable mount finds |
|---|---|
| an operation returned, no sync since | the tree after the last commit, or after any later commit that had finished |
| a `Sync` returned | at least every operation that returned before it |
| any point inside a commit | the tree before that commit or after it, as ADR 0225 lists |

The commit order inside one transaction is unchanged: file data at home,
the log body, a flush, the commit block, a flush, then the home metadata. A
write that covers several blocks may tear at any sector, as any write
may; the power-cut tests record it block by block.

## Evidence

- `TestOperationCost` runs each workload with a sync after every
  operation and batched, and fails when a batched operation costs a tenth
  of a flush or more. On the canonical volume, 200 operations each:

  | Workload | Before | Synced | Batched |
  |---|---|---|---|
  | create | 2.03 / 13.95 | 2.01 / 6.86 | 0.01 / 0.04 |
  | append 4 KiB | 2.03 / 11.08 | 2.01 / 6.03 | 0.02 / 0.10 |
  | overwrite 4 KiB | 2.03 / 5.45 | 2.00 / 4.03 | 0.01 / 0.05 |
  | create and unlink | 4.04 / 28.02 | 2.01 / 7.01 | 0.01 / 0.04 |

  Each cell gives flushes and device writes per operation. "Before" is
  ADR 0225's commit per operation; "Synced" calls `Sync` after each
  operation, as an application calling `fsync` each time would; both new
  columns write consecutive blocks of a commit, data, log or home
  metadata, as one device write of up to 128 KiB, the bulk window of the
  service's block endpoint.

- `TestPowerCutBatched` cuts power at 150 points of the 71-operation
  crash workload synced every fifth operation, and accepts only trees from
  the last completed sync through the operation in flight.
  `TestPowerCutRecovery` keeps the old model with a sync after every
  operation. `TestSyncCommitsBatch` checks that a cut keeps what a sync
  covered and loses an operation after it.
- `TestFailedOperationKeepsTheBatch` fails a duplicate create and a write
  past the free space inside one batch; the counts, the earlier files and
  the later one survive the unmount.
- `TestSyncVolumesCommitsExt4` (VFS) and `TestSyncRefusesVersion5`
  (control protocol).
- `make test-ext4-mutations`: the pinned `e2fsck` replays the journals of
  six exported cuts and finds the tree GoOS recovery finds.
- `make test-ext4-rw` syncs before each injected fault and before the
  guest is cut off, and checks a write that only the commit interval made
  durable across a killed instance.

## Consequences

Small operations cost a fraction of a flush, and a batch writes each
changed block once instead of once per operation. An application that
does not call `fsync` can lose up to one second of answered operations
when the service or the machine dies, which matches Linux with a shorter
window. The memory a volume holds grows by at most one pending
transaction, the bound ADR 0224 already set for one transaction.
