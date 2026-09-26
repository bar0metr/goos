# ADR 0107: Bulk I/O through shared memory

## Status

Accepted. Extends ADR 0013 (shared memory), ADR 0033 (block protocol), and
ADR 0098 (file cache).

## Context

Every byte a program read from or wrote to a file travelled inline in an IPC
message: at most 224 bytes per VFS reply and 32 bytes per VFS write request.
Behind the VFS, the FAT32 service moved one 4 KiB page per exchange, and every
block request carried at most 232 bytes, which the driver moved with a
read-modify-write of whole sectors through a 1 KiB bounce buffer. A 1 MiB read
from the persistent volume cost about 4700 exchanges with the VFS and 4500
with the block service; copying a 3 MiB file took several seconds; launching a
program from a file meant PID 1 relaying it 224 bytes at a time
(GoOS 0.1.3 specification §5). Only the kernel could create shared memory,
four objects of at most 32 pages, and nothing freed them.

## Decision

**Processes create shared memory.** `MemoryCreate(pages)` makes an object of
zeroed pages charged to the caller's page quota and returns a handle with
Read, Write, and Transfer. Objects keep their frames in page lists (ADR 0106),
up to 256 MiB each; the table holds 64 objects and a process may pay for 16,
both intentional limits. Every handle and every mapping of an object is a
reference: the last one gone frees the pages and returns the charge. A shared
mapping is removed whole or not at all.

**Handles can be narrowed.** `HandleDuplicate(handle, rights)` installs a
second handle to a memory object with a subset of the original's rights. A
client keeps its own handle and mapping and attaches to each request a copy
that carries only what the server needs — Read and Write for a read into the
buffer, Read for a write from it — plus Transfer to send it. The kernel strips
Transfer when a handle moves, so the server cannot pass it on.

**Bulk operations carry an object, not data.** The VFS protocol (GVF1
version 4) adds `ReadInto` and `WriteFrom`, which move up to the object's size
between an open file and the attached object at a given offset, and
`FillExecutable`, which reads a file into an attached executable object until
it seals. The VFS maps the object for the one request and closes the handle
when it answers; objects over 64 MiB are refused, since the VFS pays for the
page tables. PID 1 now fills an executable object with one `FillExecutable`
request instead of relaying the program itself.

**The storage path moves runs, not pages.** The filesystem-service protocol
(version 4) lets a read or write span consecutive slots of the file cache, and
the VFS reads and writes up to 16 pages per exchange, caching the pages it
wholly moved. The block protocol (version 2) adds `ReadBulk` and `WriteBulk`
with the same attached-object shape: the FAT32 service moves up to 128 KiB per
request through a buffer of its own; the block service, which cannot pass a
handle it received on, copies between the client's object and a buffer it
shares with the driver; the driver moves a whole run per virtio request
through a 128 KiB DMA data area. `DMAAllocate` takes the size the driver asks
for, up to 256 KiB.

**FAT writes are gathered per sector.** FAT changes are made in the resident
sector and written, to every FAT copy in use, a sector at a time before any
directory entry that could name the changed chain and at every sync. This
keeps the ordering of ADR 0102 while a chain of many clusters costs a write per
sector instead of one per entry per copy.

**Small requests stay inline.** Metadata operations and small reads keep the
inline protocols, which need no mapping.

## Alternatives

- **Larger IPC messages.** Every message would cost its maximum in every
  queue, and a few KiB would still leave large transfers thousands of
  exchanges long.
- **Letting the block service forward the FAT32 service's handle.** It would
  save a copy, but only by letting received handles be passed on, which the
  capability model deliberately forbids.
- **A persistent buffer negotiated once per session.** Stateless per-request
  attachment needs no setup protocol and no cleanup when a client dies; the
  cost, a duplicate and a map per request of up to 64 KiB, is small against
  the transfer.

## Consequences

- One exchange with the VFS moves up to 64 KiB for `cp` and `bench io`, where
  it moved 32 bytes (writes) or 224 bytes (reads). In QEMU (TCG), a 1 MiB file
  in `/tmp` moves at about 100 MiB/s instead of 1.4 MiB/s; on the FAT32 volume
  reads reach about 11 MiB/s and writes about 8 MiB/s, instead of 1.2 MiB/s and
  2.5 MiB/s. `make test-microbench` records 4 KiB to 8 MiB transfers.
- Launching a 3 MiB program from the volume takes about 0.2 s instead of 3.5 s.
- A process that creates shared memory pays for it, and one that maps another's
  object pays for the page tables.
- The copy count of a volume read is: device to DMA area, DMA area to the
  block service's buffer, that buffer to the FAT32 service's buffer, that
  buffer to the file cache, the file cache to the client's buffer. The copies
  are memory copies of whole runs, and each hop is an isolation boundary.
