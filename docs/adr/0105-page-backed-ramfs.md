# ADR 0105: Page-backed RAMFS

## Status

Accepted. Supersedes the storage model of ADR 0017.

## Context

RAMFS held 32 nodes in a fixed array, each with an inline 4 KiB buffer, so `/tmp`
could hold at most 31 files of at most 4 KiB each, and the 128 KiB was reserved
whether or not anything was stored. The VFS process that serves it resets its
heap after every request (ADR 0095), so the obvious fix — allocating buffers
from the heap — would have handed out memory the next request reuses. GoOS
0.1.3 (§2) requires `/tmp` to hold multi-megabyte files within bounded,
reclaimable memory.

## Decision

**Files are page-indexed.** A node holds twelve direct page references, a
single-indirect index page, and a double-indirect one: a little over 4 GiB of
file. Pages are allocated on first write; unwritten ranges are holes that read
as zeros. Truncation frees every page past the new end and every index page
left empty, and zeroes the tail of the last kept page; removal frees them all.

**Memory comes from a store.** `ramfs.Store` hands out zeroed pages by
reference, takes them back, and supplies node-table chunks. References rather
than pointers keep index pages plain bytes. The node table grows 64 nodes at a
time up to its limit and never shrinks.

**The VFS store is a page pool outside the arena.** `userspace/runtime/pages`
reserves anonymous address space with `VMMap` (ADR 0104) — the pages and a
stack of the numbers given back — and returns each page it is given back to the
kernel with `VMDiscard`. Node chunks are permanent allocations from the pool's
never-used pages. The pool is the only place the VFS keeps memory across
requests, and `unsafe` stays in the runtime package.

**Every instance is bounded twice.** An instance has a node limit and a page
limit, set when it is configured, and the VFS process's page quota bounds both
in the end. A write is checked against the page limit before anything is
allocated — data pages and index pages both — so a write that does not fit is
refused whole with `no space`.

**Directories keep ordered child lists.** Lookup walks one directory, not the
whole table; a listing cursor is the slot after the last entry returned, which
stays valid across removals.

## Alternatives

- **A fixed pool of larger slots.** Any fixed size is either too small for
  real files or too large to reserve up front.
- **Contiguous per-file buffers that grow by copying.** Every growth would copy
  the file and fragment the pool; the index keeps growth and shrinkage at page
  granularity.
- **Keeping freed pages in the pool without discarding them.** The process
  would hold the high-water mark of `/tmp` forever; discarding returns it.

## Consequences

- `/tmp` holds files up to the VFS's page limit (16 MiB by default) and 8192
  nodes; both are VFS configuration, bounded by its page quota.
- Memory used by `/tmp` shows in the VFS's page count and falls when files are
  removed or shrunk.
- A crash of the VFS still loses `/tmp`, as before (ADR 0099).
