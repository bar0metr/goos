# ADR 0208: Batched directory listing

## Status

Accepted and implemented. Replaces the one exchange per entry of ADR 0143. Amends ADR
0098 with a larger file cache.

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
call encodes records until the caller's buffer is full and sets `d_off` from each
record. It can run across several batches, and the next call continues from the
description's offset. A record that does not fit behaves as ADR 0143 states.
`MaxDirentExchanges` now counts batches.

### getdents64 asks for what its buffer can hold

A nonzero `Size` on `OperationReadDirBatch` limits the bytes of records, rising to
one longest record, and the limit reaches the filesystem service. getdents64 asks for
three times its free space, because a record is at most three times its dirent. The
limit therefore holds every entry that fits. A call also ends without another
exchange once the buffer has no room for the shortest dirent. The kernel keeps no
listing state per descriptor: the cursor stays the only state, and the few records
past the buffer's end are read again by the next call.

### Listing output

A stream write wakes a blocked reader, so a command printing one line per write made
the `write` stage of a redirection run after almost every line. `utility.GatherLines`
lets finished lines wait and leave together in one stream write of at most 256 bytes.
Gathered lines that fit within one native stream write remain atomic. Longer lines
preserve the existing multi-write behavior. The process-local output lock preserves
ordering between stdout and stderr records produced by the same command, because any
other record sends the gathered lines first. `Exit`, `Flush` and `ReadDirBatch` send
them too, so a listing appears batch by batch. The fatal path tries them without
waiting; if stdout refuses them, they are lost and the diagnostic is still attempted.

A command opts in when it prints a burst and waits for nothing but the filesystem.
`ls` does, and any other such command can.

### File cache

The file cache grows from 32 to 128 pages (ADR 0098). A command on `/system` faults
in its code through this cache, one exchange per page. `cat`, `ls` and `write` touch
33 to 40 pages each, more than the 31 slots available, so with least-recently-used
eviction a launch evicted its own pages and read them again from the disk. Every file
read shares this gain. The cache costs 384 KiB more, fixed at boot.

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

Each boot used TCG, one processor, 512 MiB, a disk built with
`LINUX_APPS_DIR=/Volumes/Data/linux_static`, and 119 entries in `/apps`. The tables
list the second and later runs of a boot; the first pays for loading and scope walks.
Host load varied between boots, so differences below about 10 ms mean nothing.
`make DIAG=1 LINUX_APPS_DIR=dir LATENCY_DIRECTORY=/apps test-latency` repeats them.

### Enumeration

| Measure | One exchange per entry | Batched |
| --- | --- | --- |
| `time ls /apps > /tmp/apps-list`, same build otherwise | 389, 415, 381 ms | 217, 218, 232 ms |
| Client to VFS requests | 120 | 3 |
| FAT32 listing exchanges | 120 | 3 |
| Single-entry exchanges from scope walks | 3 to 6 | 3 |
| Scope checks | 372 to 376 | 135 |
| Entries returned | 119 | 119 |

### Where the command's time goes

`ls /apps > /tmp/latency-out`, median of five, in milliseconds.

| Part | Measured as | DIAG, before | Normal build, after |
| --- | --- | --- | --- |
| Launch and exit of `ls`, with its stat | `ls` of an empty directory | 105 | 50 |
| `VFS_LISTING` serial line, DIAG only | `bench list` of an empty directory | 33 | 0 |
| `write` stage: launch, create, truncate, open, close | `ls DIR > FILE` less `ls DIR` | 85 | 48 |
| Enumeration: three batches | `bench list /apps` | 6 | 6 |
| Output: streams and file writes | the remainder | 24 | 16 |
| Total | `time` | 253 | 120 |

With DIAG on, the same total went from 253 ms to about 165 ms. Mapping and unmapping
the batch object costs about 0.2 ms per batch: an empty `bench list` takes 0.85 ms
against 0.64 ms for `bench vfs`, in a normal build. That is 0.6 ms for `/apps`.

In the kernel, `spawnExecutable` takes under 0.8 ms per launch. Launch time lies in
the page faults and in code TCG translates afresh for each process's new frames.

File-backed page faults, in a DIAG build with the 32-page cache: `uname` makes 12 per
launch at about 1.6 ms each, and `cat /system/motd` makes 33 at about 6 ms each. With
128 pages, the average wait per fault over the whole benchmark falls from about 7 ms
to 1.4 ms.

| Command, DIAG, median | Before | After |
| --- | --- | --- |
| `cat /system/motd` | 90 ms | 49 ms |
| `echo x > /tmp/latency-out` | 134 ms | 78 ms |
| `ls /tmp/latency-empty > /tmp/latency-out` | 222 ms | 133 ms |
| `ls /apps > /tmp/latency-out` | 253 ms | 165 ms |
| `uname`, which fit the old cache | 18 ms | 20 ms |

Printing 119 short lines to a file on `/tmp` took a median of 83 ms gathered and
99 ms with one stream write per line, including both launches, over 13 alternating
runs of `bench lines` in one normal-build boot. Making the same writes directly, 119
VFS writes of 16 bytes take about 68 ms.

### getdents64 with a small buffer

Counters over one listing of `/apps`, DIAG. The boot without the limit ran under
heavy host load, so these counters are the comparison and not its times. In a normal
build the listing takes 99 ms with a 512-byte buffer and 92 ms with 4096 bytes.

| Buffer | Measure | Without limit | With limit |
| --- | --- | --- | --- |
| 512 bytes | Client to VFS requests | 8 | 8 |
| 512 bytes | Entries the VFS returned | 317 | 139 |
| 512 bytes | Scope checks | 337 | 159 |
| 4096 bytes | Client to VFS requests | 4 | 4 |
| 4096 bytes | Entries the VFS returned | 119 | 119 |

## Consequences

Directory enumeration now scales with batches rather than entries. In the measured
119-entry `/apps` workload, the listing requires three client-to-VFS and three
VFS-to-FAT32 batch exchanges instead of about 120 of each.

Authority remains entry-based: every returned node is checked independently, and
protected entries never reach client-owned memory. Scope traversal remains separate
from directory batching because its access pattern is recursive and normally consumes
one entry at a time.

Linux `getdents64` consumes directory batches and may cross several VFS batches
within one syscall. Listing position remains the cursor of the last committed entry,
so short user buffers, partial results and retries preserve normal directory-stream
semantics without losing or duplicating entries. The boot test checks that one call
returns 70 entries across batches, and that a 128-byte buffer lists the same 70.

Each batch is bounded to 4096 bytes and uses existing shared memory. The FAT32, VFS
and kernel batch hot paths remain allocation-free. Gathered output adds 256 bytes to
every native command's data and allocates nothing.

The remaining command latency is accounted for by the launch and output benchmarks
rather than by per-entry directory exchanges. In a normal build, launching the two
processes takes about 98 of the 120 ms for `ls /apps > /tmp/latency-out`. Numbers
from TCG overstate that share, since real hardware does not translate code.
