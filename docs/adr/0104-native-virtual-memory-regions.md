# ADR 0104: Native virtual-memory regions

## Status

Accepted. Replaces the bootstrap user layout of ADR 0003 and ADR 0063 and the
image-resident stack and heap of ADR 0083 and ADR 0095.

## Context

A process's address space was whatever its image and a handful of fixed
windows put there. User pointers were confined to `[1 GiB, 2 GiB)`; a new
address space had one leaf table, enough for two MiB; the heap was a 128 KiB
array the linker reserved inside the image and the stack a 32 KiB one next to
it, both allocated in full at launch; shared memory could be mapped only into a
256 KiB window. There was no way for a program to ask for more memory, to give
memory back, to change a mapping's protection, or to ask what was mapped. The
kernel had no record of a process's mappings beyond its page tables.

Linux compatibility will have to express `mmap`, `munmap`, `mprotect`, `brk`,
stack growth, and ELF segment mappings in native terms (GoOS 0.1.3
specification §4). Those native terms must exist first, and must be good
enough for native programs on their own.

## Decision

**Every process has a region map.** A region is a page-aligned range with a
protection (any combination of read, write, execute except write with execute),
a backing — anonymous, image, shared, device, or guard — the object and offset
it is backed by, and flags. The map (`kernel/mm/region`) is a bounded sorted
table of 128 regions per process, an intentional system limit: every mapping is
one region and a partial unmap or protection change may split one. Regions
never overlap. The page tables remain the record of which pages are present;
the map records what a page may become.

**Anonymous memory is demand-zero.** Mapping anonymous memory reserves address
space only. The first touch of a page raises a page fault; the kernel finds the
region, checks that the access is one its protection allows, allocates a zeroed
page, charges it to the process's page quota, maps it, and resumes the
instruction. The zero-filled tail of an image segment is supplied the same way,
so a large BSS costs nothing until it is used. A process whose quota is spent
at fault time is retired with a distinct exit code, like any other fatal user
fault. The kernel's own copies into user memory (`CopyIn`, `CopyOut`) fault in
demand-zero pages the same way, so a system call may name memory the process
has not touched yet.

**Faults are classified.** A user page fault is resolved as demand-zero only
for an absent page of an anonymous or image region whose protection allows the
access. A touch of a guard region is a guard violation; an access the
protection refuses is a permission violation; an address in no region is
invalid. All three retire the process — never the kernel — and the kernel logs
which it was. A fault in kernel mode remains fatal.

**Native calls.** `VMMap` reserves anonymous memory at a requested address or
one the kernel chooses and returns the address; `VMUnmap` removes a range,
freeing the pages it owned and uncharging them; `VMProtect` changes protection;
`VMDiscard` frees the pages of a range but keeps the reservation, so it reads
as zeros again; `VMQuery` returns the region containing an address. Addresses
and lengths are validated for alignment, wraparound, and the user range, and
W^X is enforced: no mapping is writable and executable, and anonymous memory is
never executable (a write-then-seal JIT interface is future work). Regions the
kernel creates for the process's own structure — image segments, the stack
guard, the startup page — are pinned: the native calls refuse to unmap or
reprotect them. `MemoryMap` of a shared-memory object may now name any free
address, or zero to let the kernel choose, and records a shared region.

**User address layout.** User addresses are canonical lower-half addresses in
`[1 GiB, 0x00007ffffffff000)`. The kernel keeps the first GiB identity-mapped
and supervisor-only, and its direct map in the upper half (ADR 0103). Page
tables are allocated on demand at every level and charged to the process.

| Range | Use |
| --- | --- |
| `0x40000000` | self-test probe page (validation build) |
| `0x40001000` | startup page |
| `0x40002000` and up | image segments (static, small code model, below 2 GiB) |
| `0x0000080000000000` | shared-memory window used by callers that pass a fixed address |
| `0x0000081000000000` | DMA window |
| `0x0000082000000000` | system image window |
| `0x0000083000000000` | PID 1 launch staging pages |
| `0x0000100000000000` – `0x0000700000000000` | addresses the kernel chooses (heap, anonymous maps, shared maps) |
| below `0x00007fffff000000` | stack reservation, with an unmapped guard below it |

The layout is deterministic. Every kernel-chosen address comes from one
first-fit search whose starting point is a parameter, and every fixed window is
returned to the caller by the call that maps it, so a randomized base can be
introduced without changing the ABI.

**The stack is a kernel region.** The image no longer contains a stack. The
kernel reserves the stack below `0x00007fffff000000` — by default 1 MiB, or what
the image header asks for up to 64 MiB — as anonymous memory that grows on
demand within the reservation, with at least 64 KiB of unmapped guard below it,
the gap `tools/stackframes` bounds every frame by (ADR 0088).

**The heap is anonymous memory.** The userspace runtime reserves its heap with
`VMMap` at startup instead of using an array in the image, so it is limited by
the process's page quota rather than a constant, and costs only the pages it
touches. A service returns the memory above its per-request mark with
`VMDiscard`.

## Alternatives

- **Copying Linux VMAs.** The native map carries only what native mappings
  need; Linux-specific flags belong to the compatibility layer.
- **Committing anonymous memory at map time.** Charging a whole reservation up
  front would make a large sparse heap or stack cost its full size. Charging at
  fault time, bounded by the quota, gives the same bound on memory with the
  cost paid only for what is used.
- **An unbounded, heap-allocated region list.** The kernel heap never frees
  (ADR 0065); a fixed table per process is bounded and costs nothing to free.
- **Moving the kernel to the upper half now.** It would free the low GiB for
  fixed-address executables, but the kernel is a PE image the firmware loads
  at a fixed address. It is recorded as a prerequisite of fixed-address Linux
  executables at `0x400000`; position-independent static executables do not
  need it.

## Consequences

- The native image format gains version 2, which carries a stack reservation
  instead of a stack segment. Version 1 images are refused.
- Processes start with only the pages their image and startup data need; the
  heap and stack grow into their reservations as they are touched.
- `ps`, `mem`, and `quota` report pages actually in use.
