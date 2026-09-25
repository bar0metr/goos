# ADR 0102: Scalable FAT32

## Status

Accepted. Supersedes the geometry and size limits of ADR 0036, ADR 0060 and
ADR 0066; keeps their on-disk choices (8.3 entries for lower-case short names,
VFAT chains otherwise, ".." as cluster 0 for the root).

## Context

The FAT32 service (ADR 0096) served a deliberately small subset of the format.
It refused every volume that did not have 512-byte sectors, one sector per
cluster, and one FAT; it capped files at 64 KiB and directories at four
clusters; it walked every directory chain into a fixed array; truncation could
only shrink, and shrinking to a size above zero left the tail clusters
allocated; rename stayed within one directory; and each allocation scanned the
FAT from cluster 3. None of these were properties of FAT32. They were the limits
of a prototype, and each of them had to go before the native system could hold
the files a Linux compatibility layer would need (GoOS 0.1.3 specification §1).

Linux `fs/fat` was taken as the behavioural reference for what a correct
implementation accepts and does, and nothing more: none of its VFS, buffer, or
page-cache infrastructure applies to a userspace service speaking a block
protocol.

## Decision

**Geometry is what FAT32 declares, validated at mount.** Sectors must be 512
bytes (other sizes are refused as unsupported). Sectors per cluster is any power
of two up to 128, so clusters run from 512 bytes to 64 KiB. One or two FATs are
accepted. A volume must look like FAT32 — no fixed root directory, no 16-bit FAT
size — and fit its device. As in Linux, a FAT too small for the data region
limits the volume to the clusters the FAT can describe instead of refusing it,
and a volume may not number more clusters than FAT32 can address. The root
cluster must be inside the data area.

**Every FAT copy in use is written.** A change to an entry is written to each
copy, keeping the four reserved top bits of each entry as they were. When the
extended flags disable mirroring, the one active FAT named there is read and
written and the others are left alone.

**Chains are followed one checked link at a time.** A link to a free, reserved,
bad (`0x0ffffff7`), or out-of-range cluster is corruption; any value from
`0x0ffffff8` ends the chain. No walk runs longer than the volume has clusters,
so a looped chain ends in a typed `corrupt` status instead of a hang. A small
table remembers the last position reached in each of eight chains, the role
Linux's per-inode FAT cache plays, so sequential reads and appends cost one
link per cluster rather than a walk from the first cluster per request.

**Allocation searches from a hint.** The search starts at a next-free hint and
wraps once. The hint comes from a valid FSInfo sector when the volume has one
(both signatures checked, the value range-checked) and is otherwise cluster 2;
the free count FSInfo records is never trusted, and the first change replaces it
with "unknown", which every checker accepts and recomputes. A write claims every
cluster it needs before writing a byte, so a full volume refuses the whole
request with `no space` and changes nothing; a claim that fails part way frees
what it took.

**Files are limited only by FAT32.** The size field is 32 bits, so a file may
reach 4 GiB − 1; beyond that a write or truncation is refused as a resource
limit. Data moves directly between the device and the caller's buffer, one
device request per run of physically consecutive clusters; the sector window
holds only metadata, since the VFS caches file data (ADR 0098).

**No stale bytes are exposed.** Growing a file — by truncation or by a write
past its end — writes zeros over the range between the old end and the new
data before the new size is recorded. A cluster being reused, or the tail of
the last cluster a truncation kept, is therefore never read back. Directory
clusters are zeroed before they are linked.

**Truncation frees exactly what it no longer needs.** Shrinking records the
new size in the entry first, then ends the chain after the last cluster still
required and frees the rest; shrinking to zero clears the entry's first cluster
and frees the whole chain. This is the order Linux uses: an interruption loses
clusters instead of leaving an entry pointing at freed ones.

**Directories are chains of any valid length.** A directory may grow to the
65536 entries FAT allows, one zeroed cluster at a time; past that the create is
refused with `no space`. Lookup and free-slot search share one pass. A listing
resumes at its cursor, which always points just past a short entry, instead of
rescanning from the start. A node ID keeps the directory key above bit 17 and
the entry index plus two below it; a subdirectory key is accepted only if the
directory begins with a "." entry naming that same cluster, so an ID that
outlived its directory, whose cluster may now hold file data, is not found
rather than misread.

**Names follow VFAT.** Long names keep their chains; the alias is the first six
characters with `~1` to `~9`, then, as Linux and Windows do, two characters,
four hexadecimal digits of a hash of the long name, and a tail, so a directory
of many similar names does not run out of aliases. A name stored as one 8.3
entry is lower case, and byte 12 now carries the Windows NT lower-case flags so
other systems show it that way too.

**Rename moves between directories.** The new entry is written first, a moved
directory's ".." is repointed next, and the old entry is erased last, the order
Linux uses: an interruption can leave the file under both names but never under
neither. A directory cannot be moved into its own subtree (found by following
".." links, bounded by the cluster count), and an existing target is refused.
A change of case renames an entry onto itself. Within one directory the short
entry keeps its index whenever the new chain fits, which keeps the node ID.

**Unlink erases the entry, then frees the chain.**

**New volumes use a normal layout.** `fat32.Format` writes a boot sector and a
backup, an FSInfo sector, and FATs sized exactly for the remaining clusters.
`tools/mkfat32` uses it: the development and test disks are 512 MiB with 4 KiB
clusters and two FATs. An existing image of any geometry is left in place.

## Alternatives

- **Raising the constants.** Larger fixed arrays would have kept every
  structural limit and only moved it.
- **Trusting FSInfo's free count.** It is advisory, often stale on media
  written by other systems, and wrong counts are exactly what a malicious image
  would supply.
- **Refusing a short FAT, as before.** Linux clamps; clamping is equally safe,
  since no cluster the FAT cannot describe is ever allocated or followed.
- **Treating the bad-cluster value as end of chain, as Linux does.** A bad
  cluster inside a chain means the chain is wrong; reporting it is more useful
  than silently truncating the file.
- **Caching file data in the service.** The VFS already does, and streaming
  data through a small window would evict the metadata it exists for.

## Consequences

- `ramfs.Status` gains `NoSpace` and `Corrupt`, mapped through the VFS to
  "no space left on device" and "filesystem is corrupt"; device failures are
  reported as I/O errors instead of resource exhaustion.
- A node ID names an entry's position, so a file open while it is moved to
  another directory keeps an ID that no longer resolves; later operations on
  that open file report not found. The VFS drops its caches after every rename
  and unlink as before.
- There is still no journal. Every change is ordered so that an interruption
  leaves lost clusters, a chain longer than its file, or an entry under two
  names — states `fsck` repairs — and never a chain shared by two files or an
  entry pointing at freed clusters. Timestamps are not maintained.
- The host tests include a consistency checker, a model-based random workload,
  device-failure injection at every write of a sequence, and a run of the host's
  own FAT checker (`fsck_msdos` or `fsck.fat`) over volumes this code built.
  `make test-storage-stress` runs the random workload at a larger scale.
