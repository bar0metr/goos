# ADR 0231: ext4 readdir inode prefetch

## Status

Accepted. Adds a bounded listing prefetch to `userspace/ext4` and routes the
batched directory listing of `userspace/ext4vol` through it.

## Context

Listing a large indexed ext4 directory cost about 1.44 s for 10000 names in
0.3.0. The batched directory protocol (ADR 0208) already carried many entries
per exchange, but the volume adapter still fetched each listed entry's inode
one at a time, and hash-order traversal scatters those reads across the inode
table. The 256-block metadata cache (ADR 0229) cannot hold a large directory's
inode table, so each inode-table block is read repeatedly: a host measurement
of the 10000-name fixture counts 6331 device reads for the interleaved walk,
about 0.63 per entry, against the ~626 blocks that actually hold the inodes.

## Decision

**Bounded listing prefetch.** `userspace/ext4` gains a per-mount
`ListPrefetch` of `MaxReaddirPrefetchBlocks` (1024) 4 KiB buffers, 4 MiB, plus
a per-batch order array. `PrefetchDir` collects up to
`MaxReaddirPrefetchEntries` (256) directory entries in cursor order, checks
each named inode is allocated, orders the batch by inode-table block, reads
each distinct block once through the prefetch (a per-buffer LRU evicts when
full), and decodes each inode while its block is resident. The entries and
their decoded, checksum-verified inodes come back in cursor order, so the
listing order and cursor semantics are unchanged. The prefetch persists
across `ReadDirBatch` calls: the volume adapter keeps the decoded batch and
continues consuming it when the next call echoes the expected cursor, and
refills otherwise.

**Validation.** Every check the per-entry path runs still runs: the inode
bitmap allocation check happens in cursor order before any decode, so a stale
entry stops the listing exactly as before; the inode checksum, the
mode/links live check and the hostile-layout check (`checkInode`) run per
decoded inode. Hard-link identity, special-file device numbers and inode
generations ride in the decoded inode as before.

**Coherence.** A committed transaction may rewrite inode-table or bitmap
blocks the prefetch copied, so `commitPending` invalidates the prefetch, the
same moment it updates the metadata cache. Listing requests never run
concurrently with a transaction: the service serves one request at a time.

**Bounds.** `MaxReaddirPrefetchEntries = 256` and
`MaxReaddirPrefetchBlocks = 1024` are package constants, classified in the
resource-policy inventory. Nothing grows with the directory: a larger
directory refills the batch more often and evicts prefetch blocks by LRU.

## Evidence

- `TestPrefetchMatchesPerEntryListing` lists the 10000-name fixture both ways
  and requires identical entries in the same order: names, inode numbers,
  sizes, modes, link counts and device numbers.
- `TestPrefetchListingReads` counts device reads on the 10000-name fixture:
  6331 for the interleaved walk against 675 for the prefetch, an
  9.4x reduction; the assertion requires at least 4x.
- `TestPrefetchPreservesHardLinksAndSpecialFiles` and the adapter tests
  `TestReadDirBatchManyDirectory`, `TestReadDirBatchInterleavedStreams` and
  `TestReadDirBatchMatchesSingleEntry` cover order, continuation across byte
  budgets, interleaved listing streams, hard-link identity and special-file
  metadata.
- `TestFixturesMatchManifest` covers the new `many` fixture (10000 files in
  one indexed directory, 16384 inodes, built by the pinned mke2fs 1.47.2 and
  checked with `e2fsck -fn`).
- The KVM `fsbench` 10000-name readdir measurement is recorded in
  docs/development/ext4-validation.md.

## Consequences

A large directory's inode table costs one device read per block per listing
instead of one scattered read per entry. The prefetch adds 4 MiB of static
buffers and a 256-entry batch to the ext4 service, both bounded and
independent of the directory size. The per-entry `ReadDir` path stays as it
was, so nothing outside the batched listing changes.
