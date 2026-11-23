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
`ListPrefetch` of `MaxReaddirPrefetchBlocks` (1024) 4 KiB buffers, 4 MiB, in
256 sets of 4 ways like the metadata cache, plus a per-batch order array.
`PrefetchDir` collects up to `MaxReaddirPrefetchEntries` (256) directory
entries in cursor order, checks each named inode is allocated, orders the
batch by inode-table block, reads each distinct block once through the
prefetch (a per-set LRU evicts when full), and decodes each inode while its
block is resident. The entries and their decoded, checksum-verified inodes
come back in cursor order, so the listing order and cursor semantics are
unchanged. The volume adapter keeps the decoded batch across `ReadDirBatch`
calls and seeks it to the cursor the caller echoes. The caller may stop
mid-batch (ADR 0208), so the cursor can name an entry inside the batch; the
adapter resumes there instead of refilling. A cursor the batch does not
cover refills it.

**Validation.** Every check the per-entry path runs still runs: the inode
bitmap allocation check happens in cursor order before any decode, so a stale
entry stops the listing exactly as before; the inode checksum, the
mode/links live check and the hostile-layout check (`checkInode`) run per
decoded inode. Hard-link identity, special-file device numbers and inode
generations ride in the decoded inode as before. A listing-only inode carries
every scalar field `stat` needs and no raw alias: the raw bytes belong to a
cache slot the batch may overwrite, so `PrefetchDir` decodes with `keepRaw`
false. Operations that need in-inode xattrs resolve the inode through the
owned-inode path.

**Coherence.** A transaction-staged inode-table block overrides any resident
prefetch copy: `prefetchBlock` asks `cache.staged` before it probes the
prefetch, so the prefetch cannot hide newer metadata the running or pending
transaction holds. A committed transaction may rewrite inode-table or bitmap
blocks the prefetch copied, so `commitPending` invalidates the prefetch as a
defensive boundary beside the metadata-cache update. The decoded `ReadDirBatch`
is generation-bound: `FS.mutationEpoch` advances when a mutation folds into
the pending transaction, before journal commit, and the adapter drops its
decoded batch when the epoch moves. Listing requests never run concurrently
with a transaction: the service serves one request at a time.

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
  `TestReadDirBatchManyDirectory`, `TestReadDirBatchInterleavedStreams`,
  `TestReadDirBatchLaggingCursor` and `TestReadDirBatchMatchesSingleEntry`
  cover order, continuation across byte budgets, a client that stops
  mid-batch, interleaved listing streams, hard-link identity and special-file
  metadata.
- `TestFixturesMatchManifest` covers the new `many` fixture (10000 files in
  one indexed directory, 16384 inodes, built by the pinned mke2fs 1.47.2 and
  checked with `e2fsck -fn`).
- `TestPrefetchSeesPendingInodeMutation`, `TestPrefetchSeesPendingCreatedInode`
  and `TestPrefetchedInodeDoesNotRetainEvictableRawAlias` cover the coherence
  fixes: a pending truncate and a pending create must win over a resident
  prefetch copy, and a prefetched inode must keep no raw alias after a set
  eviction. `TestReadDirBatchInvalidatedByUnlink`,
  `TestReadDirBatchInvalidatedByMetadataMutation` and
  `TestReadDirBatchInvalidatedByCreate` cover the generation-bound adapter.
- The KVM `fsbench` run `20261123T095006Z-ad12b999` (commit `ea788082`)
  lists 10000 names in 193 ms against the 0.3.0 baseline of about 1.44 s,
  7.5x faster. The guest logged 1256 prefetch device reads for the whole
  benchmark, so the win comes from fewer inode-table reads, not from noise.
  The 2000-name listing measured 40 ms against a 14.5 ms baseline: the batch
  machinery adds per-entry work on the guest that the 10000-name win
  dominates. The numbers are recorded in docs/development/ext4-validation.md.

## Consequences

A large directory's inode table costs one device read per block per listing
instead of one scattered read per entry. The prefetch adds 4 MiB of static
buffers and a 256-entry batch to the ext4 service, both bounded and
independent of the directory size. The per-entry `ReadDir` path stays as it
was, so nothing outside the batched listing changes.
