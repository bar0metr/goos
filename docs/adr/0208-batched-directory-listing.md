# ADR 0208: Batched directory listing

## Status

Accepted and implemented. Replaces the one exchange per entry of ADR 0143.

## Context

ADR 0207 removed the repeated scope walks from `ls /apps`. The listing still cost
about 120 client requests and 120 FAT32 exchanges for 119 entries, and about 0.4 s
under TCG.

## Decision

### One record format at both boundaries

A directory batch is a run of variable-length records, `ramfs.DirRecord`: a 64-byte
header with the node ID, kind, size, timestamps, attributes, mount ID and next
cursor, then the name, padded to eight bytes. Each record carries its own cursor,
because a client or a getdents64 buffer can stop in the middle of a batch.

A batch holds at most `MaxDirBatch`, 4096 bytes. The longest name fits, and about 50
short names do. Messages carry the batch's byte count, record count, next cursor and
end flag; the records travel in memory the two sides already share:

- From the filesystem service to the VFS, the name page of the file cache. One
  exchange is in flight at a time, so the page is free.
- From the VFS to a client, the object the request attaches with `NameReturn`. A
  native client lends its path page and the Linux bridge its I/O buffer. A path too
  long for the message arrives in the same object, and the VFS copies it out before
  writing records over it.

Long names need no second exchange now, on either boundary.

`OperationReadDirBatch` is new in both protocols (fsproto version 12, VFS protocol
version 14). `OperationReadDir` stays. Scope walks use it, because a walk descends
after each directory it meets and would discard most of a batch.

### Authority stays per entry, in the VFS

`ReadDirBatchFor` checks each entry's own node ID with `nodeScopes`, as ADR 0207
requires, and writes a record to the client's object only after `permits` allows it.
A hidden name never reaches client memory and takes no room there. The VFS keeps
scanning until the object is full, the listing ends, or it has examined
`maxScopeWalk` entries.

With no visible entry by that bound, the call answers `StatusPermissionDenied`, as
`ReadDirFor` does for such a run. With visible entries, it returns them and the next
call gets a fresh bound. A backend or scope-check failure behaves the same way: the
records gathered so far go out, and the next call reports the failure.

An empty directory answers `StatusOK` with no records and the end flag, so a client
no longer needs a stat to tell it from a missing one.

### The service's memory is checked after copying

The filesystem service can write the name page at any time. `Remote.ReadDirBatch`
copies the bytes to `Namespace.dirBatch`, then requires that the count of records is
exact, each record is well formed, and cursors rise to the reply's cursor. A failure
is `StatusIOError`, and the namespace reports a backend error. Scope checks can run
exchanges of their own during a batch; they use the name page and leave the private
copy alone.

### Mounts and other backends

The mount phase is unchanged: after the backend's end, the scan walks the mount table
with the same cursors above `mountCursor`, filtered by the same check. A backend
without `BatchBackend`, such as the volatile root and the boot archive, and a remote
backend with no cache attached, answer one entry at a time inside the same loop.
Clients see batches from every directory.

### FAT32

`FileSystem.ReadDirBatch` makes one pass over the directory under the rules of
`ReadDir`. Its cursor stays just past the last short entry written, so a record that
does not fit is scanned again from the start of its long chain. Cursors name places
on the volume, so a restarted service continues a listing from the same cursor.

### Clients

`utility.ReadDir`, `ls`, the directory walk in `stat` and getdents64 use batches. A getdents64
call encodes records until the caller's buffer is full, sets `d_off` from each
record, and discards the rest of the batch; the next call asks for it again from the
description's offset. A record that does not fit behaves as ADR 0143 states.
`MaxDirentExchanges` now counts batches.

### Allocation

On a FAT32 listing, `tinygo -print-allocs` reports no heap allocation on the batch
path in the VFS, the filesystem service or the kernel. `mountedEntry` now fills
`Namespace.dirEntry` in place for the same reason. The one-entry path for other
backends still copies a `ramfs.Entry` per entry, as it did before. `utility.ReadDir`
allocates one 4096-byte buffer per listing, because a visitor may start another
listing.

### Diagnostics

The `DIAG=1` listing line gains four counters:

```text
VFS_LISTING requests=3 scope_checks=135 scope_contains=1 scope_openname=1 scope_backend_readdir=3 fat32_readdir=3 batch_requests=3 batch_entries=119 fat32_batch_requests=3 fat32_batch_entries=119
```

`fat32_readdir` now counts only the single-entry exchanges of scope walks.

## Measurements

One boot of each tree, DIAG build, TCG, one processor, 512 MiB, a disk built with
`LINUX_APPS_DIR=/Volumes/Data/linux_static`, and 119 entries in `/apps`. Times are the
second, third and fourth `time ls /apps > /tmp/apps-list` of the boot. The first run
of each boot pays for loading `ls` and walking scopes: 802 ms before, 601 ms after.

| Measure | Before | After |
| --- | --- | --- |
| `time ls /apps > /tmp/apps-list` | 389, 415, 381 ms | 217, 218, 232 ms |
| Client to VFS requests | 120 | 3 |
| FAT32 listing exchanges | 120 | 3 |
| Single-entry exchanges from scope walks | 3 to 6 | 3 |
| Scope checks | 372 to 376 | 135 |
| Entries returned | 119 | 119 |

## Consequences

The remaining 217 ms is not listing exchanges. Launching `ls` and printing 119 lines
to a file on `/tmp` are the next things to measure.

A getdents64 caller with a small buffer requests discarded records again, up to one
batch per call.

A batch costs more per request than a single entry: the VFS maps and unmaps the
client's object for each one. That cost is three mappings for `/apps`, against 120
pairs of exchanges before.
