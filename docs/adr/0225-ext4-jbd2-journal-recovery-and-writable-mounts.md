# ADR 0225: ext4 JBD2 journal, recovery and writable mounts

## Status

Accepted. Extends ADR 0220 (read-only core and feature contract), ADR 0222
(volume instances) and ADR 0224 (mutation engine), and replaces the part
of ADR 0224 that kept `writeSupported` false and limited mutation to the
unexported `mountScratch`. The ext4 service, the adapter, PID 1 and the
block service are unchanged: they still mount additional volumes
read-only, and no GoOS client can write ext4 yet. ADR 0226 connects them. ADR 0227
replaces the durability rule below: operations now commit in batches, and
`Sync` commits the batch instead of checkpointing.

## Context

ADR 0224 left a mutation engine whose commit wrote metadata in place.
A power cut between two of those writes damaged the volume, and an
operation that spans transactions (deleting a large file, shrinking one)
could stop half done with nothing to finish it. Linux solves both with the
JBD2 journal and the orphan list, and Linux and e2fsck will replay a
journal another writer left behind. GoOS needs the same on-disk contract to
write volumes that Linux accepts after a crash.

## Decision

**Journal format.** `userspace/ext4` writes the internal JBD2 journal of
the volume itself, in the format the Linux 6.12 documentation describes:
descriptor blocks with tags and the metadata copies after them, revoke
blocks, and a commit block, all big-endian, with checksum v3 on a
`metadata_csum` volume and no checksums otherwise. Tags are 64-bit on a
`64bit` volume. A copy whose first word is the JBD2 magic is escaped. A
writable mount turns on revoke records and the matching checksum and
width features in the journal superblock while the log is empty, as
Linux does. It refuses a version 1 journal superblock, the CRC32 commit
checksum, async commit, fast commit, an unknown incompat bit, and a
journal with a recorded abort (`s_errno`).

**Commit order.** Data mode is `data=ordered`. A transaction goes to disk
as: file data at home; the log body; a flush; the commit block; a flush.
The operation is durable when the second flush returns, and only then
does it return. The metadata blocks then go to their home locations
without a flush. `TestCommitOrder` checks the order of the writes. The
superblock is no exception: the mount and unmount state changes and the
free counts are staged in transactions like any other metadata block, so
every home write of it has a committed copy in the live log.

**Log space.** The log is reclaimed whole. When the next transaction does
not fit, or on `Sync`, the writer flushes, so every home write is durable,
writes the journal superblock with the log start moved to the head, and
flushes again before writing over the old log. A transaction holds at most
256 metadata blocks (ADR 0224), so it always fits in the 1024-block journal
the smallest supported profile has.

**Revokes.** The writer remembers each block copied into the live log.
When a transaction frees one, the transaction carries a revoke record for
it, so replay never writes an old copy over the block's next owner. A
block staged and freed in the same transaction is neither logged nor
written home. Blocks freed in a transaction still cannot be reused before
it commits (ADR 0224).

**Replay.** Replay reads the log three times from `s_start`, like Linux:
the scan finds the last transaction with a valid commit block, the second
pass collects revoke records, the third writes each logged block that no
later revoke cancels to its home location.

The scan stops at the first block that does not continue the log: a
missing block, a sequence number out of place, a descriptor, revoke or
commit block that fails its checksum, or a descriptor without a last tag.
That block is either the torn or unwritten tail of a transaction that
never committed, or damage to one that did. The commit order decides
which. The writer writes a commit block only after a flush made the rest of
its transaction durable, and starts the next transaction only after the
commit's own flush. So the scan looks through the rest of the log: a valid
commit block of the stopped transaction, or a valid block of the next one,
proves the stopped transaction committed, and the mount fails with a
corrupt-metadata error. Without such proof the stopped transaction is the
tail and replay ends before it. A committed transaction whose data block
fails its tag checksum, or names a block outside the filesystem or inside
the journal, is damage too.

Replay only rewrites blocks, so an interrupted replay runs again from the
same log. The sequence number after a replay skips one, since blocks of the
transaction that never committed may still carry the next number. Replay
refuses what Linux refuses in a journal superblock: any `ro_compat` bit,
and checksum v2 together with v3.

**Mount states.** `Mount(device, Options{})` stays read-only and refuses,
as needing recovery, a volume with `needs_recovery`, a non-empty journal,
an unclean state or an orphan list. It never replays. A writable mount,
`Options{Writable: true, Clock: ...}`, runs:

```text
probe: superblock, features, 4 KiB blocks, has_journal, no error state
  (a superblock whose only fault is its checksum is kept as suspect)
  -> device flush must succeed, or refuse as unsupported
  -> bootstrap: journal inode and journal superblock only
  -> journal non-empty: replay, flush, mark the log empty, flush
     journal empty and superblock suspect: refuse as corrupt
  -> read everything again with every checksum: superblock, descriptors,
     bitmaps, root, journal; bitmap and descriptor counts
  -> journal superblock: live log at s_first; flush
  -> transaction: superblock with needs_recovery set, valid bit clear,
     free counts summed from the descriptors
  -> finish the orphan list, one journaled transaction per inode
  -> ReadyRW
```

Before replay, any block outside the log may hold a torn home write, so
the bootstrap reads only what finds the journal. It takes the
superblock's geometry without trusting its checksum, the journal inode's
descriptor without its checksum, and the journal inode, which GoOS never
rewrites, so every version of its inode table block holds the same bytes
for it. Nothing else is checked until replay has run and the volume is
read again. A suspect superblock passes only if replay rewrote it.

Linux discards the log of a volume whose superblock lacks
`needs_recovery`, so the order of the two superblock transactions
matters. The mounting one may be lost that way, since the next
transaction's first flush makes its home write durable before that
transaction can commit. `Unmount` releases pinned inodes that lost their
last link, reclaims the log so that it holds nothing else, commits the
clean superblock, flushes, and marks the log empty. After a failed write or flush the volume moves to
`StateFailed`, refuses further operations and unmounts with an I/O error,
leaving the journal for the next writable mount.

**Orphans.** The engine keeps Linux's original orphan list: the
superblock's `s_last_orphan` names the first inode and each inode's
`i_dtime` names the next. Removing a last link puts the inode on the list
in the same transaction that removes the name. Release then frees its
blocks in steps that may each commit and takes it off the list in the
step that frees the inode. A shrinking `Truncate` lists the inode in the
transaction that sets the new size and unlists it in the step that frees
the last block past it. `Pin` and `Unpin` let a caller keep an unlinked
inode alive: it stays listed with no links, can still be written, and is
released at the last `Unpin` or at `Unmount`. Pins live in memory, so
after a crash the writable mount releases those inodes. Cleanup truncates
a listed inode that still has links to its size and releases one without
links. This covers every `release` caller and multi-transaction truncate,
the two gaps ADR 0224 named. `orphan_file` and `orphan_present` stay
refused.

**Durability.** Each operation, or each transaction of a multi-transaction
operation, is durable in the log when it returns, so `fsync`,
`fdatasync` and a directory `fsync` have nothing further to wait for. A
service that reports a write to its client after the operation returns can
acknowledge an `fsync` at once. `Sync` only checkpoints, which shortens the
next replay. A multi-transaction write commits a prefix and reports its
length (ADR 0224). If a home write fails after the commit flush, the
operation reports an I/O error although replay will apply it: the caller
must treat an I/O error as an unknown outcome, as it would a lost reply.

| Power cut after | Next writable mount finds |
|---|---|
| file data writes, before the log flush | the old tree; the new data sits in blocks nothing references |
| the log flush, before the commit block | the old tree |
| the commit block, before its flush | the old tree or the new one; the operation had not returned |
| the second flush | the new tree |
| any home metadata write, torn or not | the new tree, rewritten from the log |
| the reclaim flush or the journal superblock write | the new tree |
| any point of replay | what an uninterrupted replay gives |
| an orphan cleanup step | the inode still listed; cleanup runs again |
| unmount, after the reclaim, at any point of the clean-superblock transaction | the tree, clean or still mounted; nothing else in the log |

**Device contract.** A write returns when the device accepted it; a flush
returns when every accepted write is durable. The block service passes
`OperationFlush` to virtio-blk, which sends `VIRTIO_BLK_T_FLUSH` and fails
the request when the device refuses it, so a device without a working
flush fails the writable mount's first flush. Writes are atomic per
512-byte sector and nothing larger: any write may tear between sectors.
The superblock's home writes are covered by the log. The journal
superblock is the one block written outside a transaction, and every
field GoOS changes in it, its checksum included, lies in its first
sector.

**Bounds.** The journal inode may map in at most 1024 extents; the revoke
table replay builds holds at most 2^20 blocks; the scan and its look-ahead together read at most one
pass over the log; the orphan walk refuses a loop, an inode outside the
inode table, a free inode, and a list longer than 2^20. The writer's
memory is one transaction's staged blocks plus the map of logged blocks,
which the log size bounds.

**What stays out.** Exposing writable ext4 through `/bootstrap/ext4`,
`ext4vol`, the VFS and PID 1 needs a writable block grant, the probe to
exclusive-writer transition ADR 0222 leaves open, and a mount option; it
is the next step and changes none of the rules above. Until then the
additional-volume grant stays read-only and a dirty ext4 volume stays
refused in GoOS. External journals, fast commits, `data=journal`,
`data=writeback`, the orphan file, and 1 or 2 KiB blocks remain refused
for writing.

## Evidence

- `make test-ext4-unit`: every mutation test now commits through the
  journal, and every volume check first recovers a copy of the mounted
  device, so each test also exercises replay. `TestPowerCutRecovery`
  records a 71-operation workload (create, write, rename, links, overwrite,
  append, a 400-extent truncate, a 1.5 MiB write over six transactions, a
  checkpoint, unlink of a large file, rename over a file, rmdir, unlink
  while pinned, writes to the unlinked file, last close, indexed-directory
  edits) with the mount and unmount around it. It sorts the events into
  flushes, file data, descriptors, log copies, revoke blocks, commit
  blocks, home metadata, superblock writes and journal superblock writes,
  fails if any class is missing, and cuts power before and after the
  first two, a middle and the last two events of each class plus a random
  sample, 150 cuts in all. `make test-ext4-stress` cuts before and after
  every event. At each cut, writes after the last flush land in a random
  subset, and any of them may tear sector by sector, the superblock's
  included. The recovered tree must equal the tree after the last returned
  operation, the one after the operation in flight, or for the large write
  a committed prefix, and the checker of ADR 0224 must accept the volume
  with an empty orphan list. Other tests cut power during replay and
  replay again; wrap the log while freeing logged extent blocks and check
  that replay skips the revoked copies; damage a committed descriptor,
  revoke block or commit block, jump a commit's sequence number and drop a
  descriptor's last tag, each with later transactions behind it, and
  expect a corrupt-metadata refusal; tear the last commit block and expect
  replay to drop only that transaction; refuse journal `ro_compat` bits
  and checksum v2 with v3; replay a superblock torn between its sectors,
  and refuse one with a bad checksum and an empty log; fail a flush in
  mid-commit; check the clean-unmount superblock state; and recover a
  pinned, unlinked file from the orphan list.
- `make test-ext4-mutations` runs the pinned e2fsprogs 1.47.2. Images
  that GoOS unmounted cleanly pass `e2fsck -fn` and match `debugfs`. Six
  images cut in the middle of the workload go to `e2fsck -fy`, which
  replays the GoOS journal and finishes the orphan list without fixing
  anything else; a second `e2fsck -fn` reports nothing, and the tree
  matches what GoOS recovered from the same image. In the other direction,
  debugfs writes journals with and without checksums, with revoke records,
  an escaped block and an uncommitted transaction, and GoOS replay writes
  the same blocks as e2fsck's replay of a copy.

## Consequences

A volume GoOS writes survives a power cut under the device contract above,
and Linux can recover it. The engine is ready for a writable volume
instance; the remaining work is the authority path that lets one reach a
GoOS client. Every operation costs two flushes, which the release
performance work will measure before batching transactions.
