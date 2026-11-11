# ADR 0224: ext4 mutation engine and transaction staging

## Status

Accepted. Extends ADR 0220 (read-only core and feature contract), ADR 0221
(adapter) and ADR 0222 (volume instances). Real mounts stay read-only:
`writeSupported` is false, `Mount` refuses `Options{Writable: true}`, and
additional volumes still get read-only block slices.

## Context

Read-write ext4 needs three things in order: code that changes the on-disk
structures correctly, a JBD2 journal that makes those changes atomic, and a
writable mount that lets requests reach them. The journal can only be sized
and tested against real mutation code, and the mutation code can only be
trusted once Linux's own checker accepts what it writes. So the mutation
code comes first, runs on throwaway images, and never reaches a GoOS
volume.

## Decision

**One engine in the existing package.** `userspace/ext4` gains the writers
next to the parsers they reuse: `txn.go` (staging), `alloc.go` (bitmaps,
descriptors, counters), `inodew.go` (inode encoding and the live inode
set), `extentw.go` (extent trees), `dirw.go` (linear and hash-indexed
directories) and `mutate.go` (the operations). There is no second parser.

**Operations.** `Create` (regular files, FIFOs, sockets), `Mkdir`,
`Symlink` (inline below 60 bytes as Linux does, one data block otherwise),
`Link`, `Unlink`, `Rmdir`, `Rename` (within and across directories, over a
file or an empty directory, with the moved directory's `..` and both
parents' link counts updated), `WriteFile` (holes allocated, unwritten
extents converted in place, overwrite, append, sparse offsets past 4 GiB)
and `Truncate` (grow as a hole, shrink across extents and tree nodes). Each
one takes the caller's `*Inode` and refuses it with `KindStale` when the
inode's generation changed, so a handle never reaches a reused inode.

**Gate.** Only `mountScratch` sets `FS.mutable`. It is unexported, nothing
outside the package's tests calls it, and it requires the writable profile
of the feature matrix, 4 KiB blocks, and superblock free counts that match
the descriptors. On a volume from `Mount`, every operation returns
`KindReadOnly`. The ext4 service and adapter do not call the operations.

**Staging.** An operation runs in a transaction: a map from block number to
a private copy. `blockCache.get` and `readBlocks` read staged copies first,
so an operation sees its own changes. Abort drops the map and the live
inode set; the device and the read cache never saw anything. Commit stages
dirty inodes and the primary superblock counters, writes file data blocks,
flushes, writes metadata blocks in ascending order, flushes, and refreshes
any resident cache blocks. A failed write or flush moves the volume to
`StateFailed` and stops mutation. Commit has no journal, so a power cut
between metadata writes damages the volume; that is why only scratch
devices see it.

**Allocation.** Blocks come from the goal's group onward, one run per call,
never crossing a group, never taking the superuser reserve. A group marked
`BLOCK_UNINIT` gets its bitmap built from its own superblock copy,
descriptor tables, reserved descriptor blocks and any group's bitmaps and
inode table inside it, and the build must agree with the descriptor's free
count. Inodes for directories go to a group with at least average free
inodes and the most free blocks; other inodes start in the parent's group.
`itable_unused` shrinks as inodes are taken from the unused tail. A new
inode's generation is one past the slot's previous value. Blocks freed in a
transaction cannot be reused before it commits, which journal revoke will
depend on. Freeing a block or inode whose bit is clear is damage.

**Extent trees.** An edit stages the path from the root to one leaf,
rewrites that leaf, merges neighbours that continue each other, and splits
upward. A split appending past a leaf's last record leaves the full leaf
alone; any other split halves it. A full root moves its records into a new
child and the tree grows a level, up to depth 5. Index keys always equal
the first block their child maps, which e2fsck checks. Truncate frees one
extent per step from the end, frees nodes that empty, and pulls a lone
child back into the root while its records fit there. Block-mapped,
`huge_file`-flagged, verity and attribute-value inodes are refused, not
converted.

**Directories.** Linear directories grow by a block and stay linear. In a
hash-indexed directory a name goes to the leaf its hash selects; a full
leaf is sorted by hash and split at the middle, with the low bit of the new
index entry set when the split falls inside a run of equal hashes, as
Linux does. A full index node returns `KindNoSpace`: growing the index by a
level is not implemented, which bounds an indexed directory at 507 leaves
at 4 KiB blocks. Names stay byte sequences. Removing a name merges its
record into the one before it, or marks the first record of a block
unused. Every leaf and index node is resealed after a change.

**Attributes.** In-inode attributes and every byte the decoder does not
interpret stay as read, since an inode is re-encoded over its own raw
bytes. Releasing an inode drops one reference on its attribute block and
frees the block with the last one. Writing attributes is not implemented.

**Credits.** Every step reserves the metadata blocks it may stage, and
staging more is `KindInternal`, so the estimates below are checked on each
test run rather than trusted. A transaction holds at most 256 metadata
blocks (a quarter of the smallest supported journal, less 8 kept for the
inode table blocks and superblock commit adds), 256 data blocks and 512
freed runs; a step that does not fit commits the transaction and opens the
next.

| Step | Credits | What it covers |
|---|---|---|
| allocate a run | 2 | bitmap and descriptor; a run stays in one group |
| extent edit | 23 | 5 path nodes, and at each of 6 levels a new node with its allocation |
| directory insert | 28 | leaf, index node, a new leaf with its allocation and extent edit |
| create | 31 | inode bitmap, block bitmap of a group initialized on first use, descriptor, insert |
| mkdir, symlink | 34 | create plus the first block and its allocation |
| link | 28 | insert |
| remove a name | 1 | the leaf |
| rename | 31 | insert, removal, replaced record, moved directory's first block |
| write step | 25 | one run allocated and mapped; overwrites stage data only |
| truncate step | 39 | one extent freed over two groups, the path, emptied nodes, root collapse |
| free an inode | 5 | inode bitmap, descriptor, attribute block with its bitmap and descriptor |

Inode table blocks and the superblock are added at commit, one block per
dirty inode plus one.

**Mutation graph.** A write maps a hole (allocate run, edit extents,
inode), then stages data. Create allocates an inode, inserts a name and
dirties the parent. Unlink removes a name, drops a link, and on the last
one runs truncate steps to zero, drops the attribute block and frees the
inode. Rename inserts or retargets the new name, finds the old one again
(an insert may have repacked its block), removes it, fixes `..` and link
counts, and releases a replaced inode. Each arrow is a step above; a
journal transaction must contain whole steps.

**Locking.** One ext4 service process owns one `FS` and handles one request
at a time, so the engine takes no locks and holds none while it waits on
block IPC. `begin` refuses a nested transaction. A reader in the same
process runs between operations and sees only committed blocks. When
writes reach the VFS, the VFS keeps its namespace order (path resolution,
then the mount's backend) and must drop path, attribute and page cache
entries for every node an operation reports changed; an operation reports
its result only after commit.

**Gaps a writable mount must close first.** There is no orphan list: an
unlink frees the inode at once, and an unlink or truncate that spans
transactions leaves an unreachable, allocated inode if it stops between
them. A write that spans transactions commits a prefix and reports it.
Neither is safe without the journal and orphan handling.

## Evidence

- `make test-ext4-unit` runs the mutation tests: every operation and
  refusal, sparse and multi-transaction writes, unwritten conversion,
  truncate across a depth-1 Linux tree, a tree grown to depth 2 by 1500
  random-order extents and shrunk back to depth 0, 2500 names added to and
  half removed from Linux hash trees (TEA and legacy), block and inode
  exhaustion with counts restored after cleanup, a fault on each device
  read of six operations in turn, a failed commit write, and four seeded
  random runs of 400 operations against a content model with remounts.
  After each, `checkVolume` cross-checks bitmaps against claimed blocks,
  every free and directory count, extent shape, keys and checksums, sector
  counts, `..` entries, link counts, lookups through the index, and
  reachability.
- `make test-ext4-mutations` exports four mutated images and runs the
  pinned e2fsprogs 1.47.2: `e2fsck -fn` reports nothing on any of them, and
  the tree `debugfs rdump` extracts matches the GoOS reader's manifest,
  content hashes included. It needs Docker.

## Consequences

The journal work starts from mutation code whose output Linux accepts and
whose credits are measured. Nothing changes for GoOS volumes: the service
binary contains the engine but no path calls it. Index growth past one
level, attribute writes, orphan handling, `huge_file` inodes and 1 or 2 KiB
blocks remain refused.
