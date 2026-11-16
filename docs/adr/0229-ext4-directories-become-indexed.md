# ADR 0229: ext4 directories become indexed

## Status

Accepted. Changes the directory rule of ADR 0224: a linear directory no
longer stays linear when it outgrows its first block.

## Context

ADR 0224 left directories GoOS creates linear, so a lookup reads every
block until it finds the name. A directory of 10000 names costs about 50
blocks per lookup that way. Linux turns a directory into a hash tree when
its first block fills, provided the volume has `dir_index`, and the
GoOS writer already inserts into hash trees and splits their leaves.

## Decision

When an insert finds no room in a linear directory of exactly one block,
on a volume with `dir_index` whose default hash the reader computes, the
writer indexes the directory first, as Linux does: it moves every name but
`.` and `..` into a new second block, rewrites block 0 as the root with
the two dot entries, root information carrying the volume's default hash,
and one index entry pointing at that leaf, sets the inode's index flag,
and then inserts through the index, which splits the full leaf. The root
and the leaves carry their checksums. The work happens in the inserting
operation's transaction, in a step of its own; that step can commit the
batch before the operation (ADR 0227) but never part of the operation.

A directory already longer than one block stays linear, and the index
still holds one level: 507 leaves at 4 KiB blocks.

**Listing reads a copy.** A hash-indexed directory lists names in hash
order, so their inodes lie all over the inode table. The adapter reads
each entry's inode while the listing runs, and those reads evicted the
leaf being listed from the 32-block cache and reused its buffer: the walk
then failed with a corrupt record, which Linux-built indexed directories
could trigger too. `ReadDir` now walks a copy of each leaf, and
`addIndexed` copies a full leaf's records before it reads the parent node.

**A larger cache.** The metadata cache holds 256 blocks, 1 MiB at 4 KiB
blocks, in 64 sets of four, so a lookup compares four entries. Listing
2000 names in hash order read 1343 blocks from the device with 32 blocks
and reads 3 now.

## Evidence

- `TestReadDirSurvivesEviction` reads twice the cache's blocks for each
  of 2000 listed names; it fails without the copy.
- `TestLinearDirectoryBecomesIndexed`: a directory stays linear at 50
  names, is indexed at 600, keeps every name after half are removed, and
  checks out after a remount.
- `make test-ext4-mutations` exports a volume with 10000 names in one new
  directory and a chain of 200 directories; the pinned `e2fsck -fn`, which
  checks hash trees, finds it clean and `debugfs` lists the same 12177
  entries.

## Consequences

A directory GoOS fills keeps lookups at one leaf plus the root, as on
Linux. A directory past 507 leaves still refuses new names with
`ENOSPC`; growing the index by a level stays out of 0.3.0.
