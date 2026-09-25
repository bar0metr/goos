# ADR 0098: File cache between the VFS and a filesystem service

## Status

Accepted. Amends ADR 0096.

## Context

ADR 0096 moved FAT32 into a process of its own and carried file data between
it and the VFS inside IPC messages: at most 216 bytes per write and 224 per
read. A 16 KiB read of a file on the volume took 74 exchanges, and a file read
twice cost the same the second time. The specification's target is a page
cache: the filesystem service fills memory the VFS can serve from, and cached
reads cost no exchange at all (§21, §22).

The kernel already has shared-memory objects (ADR 0013): kernel-created,
mapped whole into a fixed window, never executable. What was missing was one
the VFS and the filesystem service share, and a protocol that names places in
it instead of carrying bytes.

## Decision

**The file cache is one shared-memory object.** The kernel creates it at boot:
`fsproto.CachePages` (32) pages, 128 KiB, the largest object `shmem.MaxPages`
now allows. PID 1 holds it as handle `0x100000018` with Read, Write and
Transfer, and grants a Read and Write copy, not consumed, to the VFS and to the
filesystem service at every launch. Each maps it at the start of its shared
window, `0x40180000`.

**Messages name slots, not bytes.** Protocol `GFSPRT01` version 2 removes the
data field from requests and replies. A read or write names a slot of the
cache, a file offset, and a length that stays within one page: the bytes lie
at the offset's position within that slot. The service reads into the slot or
writes from it and answers with the count. Both ends check the slot and the
bounds before touching memory, and the VFS refuses a count larger than a page.

**The index is the VFS's alone.** The VFS keeps which slot holds which page of
which node, and serves a read of a cached page without an exchange. A miss
takes the slot used longest ago, asks the service to fill it with the whole
page, and keeps it. A page shorter than a page is the end of its file. The
service never learns what a slot means to the VFS, so nothing it writes can
make the VFS serve one file's bytes as another's; it can only change bytes of
the volume it serves anyway.

**Writes go through.** A write to a cached page updates the slot and writes
from it; a write to a page that is not cached is written from a scratch slot
and not cached. The service writes through to the block service before it
answers, as before, so a write that returns is on the service's side and a
full volume or a file past FAT32's 64 KiB limit fails the write that caused it.

**The cache is coherent for the reason the attribute cache is.** The VFS is
the service's only client and runs one request at a time, so every change
passes through the backend that owns the index. A write updates the page it
changed and drops any other short page of the file, which a write further on
may have made too short. A truncation drops the file's pages; a rename, an
unlink, a transport failure, and attaching a service, restarted or not, drop
them all, since node IDs can move and a dead service may have written half a
page.

## Alternatives

**Write-back.** Dirty pages flushed on close, sync or eviction would turn a
`cp` of 64 KiB from 300 exchanges into 16. But the error of a full volume
would reach the program after its write returned, and dirty pages die with the
VFS, which 0.1.2 is to make restartable. It can be added on top of this cache
once the VFS keeps its state across a restart.

**One object per open file, mapped into the program.** Zero-copy all the way
to the application is where the specification points, but it needs mapping
revocation and a VFS that hands out memory objects; the program-to-VFS path
still moves 224 bytes per exchange.

**Keeping the inline data path beside the slots.** Two data paths would both
need testing forever; the slot path serves every size.

## Consequences

Measured under QEMU TCG at 100 Hz, both disk images on one host volume, medians
of five: a 16 KiB read of a FAT32 file (`bench read /persist/uname.bin`,
ADR 0085) fell from 54.2 ms to 9.4 ms, the same as a read of the memory-resident
system image, since what remains is the program's 224-byte exchanges with the
VFS. `bench read` of a small persistent file fell from 574 to 488 µs. IPC
ping-pong, stream throughput, command launches and persistent writes are
unchanged within noise.

An earlier run reported persistent writes 40% slower. Its disk image was on a
different host volume from the baseline's, the error ADR 0096 describes; on
one volume the two are equal.

The kernel holds three registered shared objects at boot instead of two, and
128 KiB of memory for the cache. The filesystem service can write any slot at
any time; it holds only data of its own volume, and the VFS copies bytes out
of a slot only as file data, never as structure. A second filesystem service
would need a cache of its own.

Host tests cover a repeated read costing no exchange, writes to cached and
uncached pages, a write past the page that ended the file, truncation, a file
recreated after an unlink, eviction across two files larger than the cache
together, a service claiming an overlong page, and a VFS without a mapping.
The boot test requires the VFS to map the cache (`FILE_CACHE_OK`) and runs the
persistent-storage, restart and reboot scenarios over it.
