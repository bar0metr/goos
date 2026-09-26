# ADR 0111: Bounded dynamic registries

## Status

Accepted. Revisits the fixed tables of ADR 0065 (kernel heap discipline) and
the shares of ADR 0089 (quotas).

## Context

Every kernel registry was a static array of whole entries, sized so that the
worst case fitted in static memory: 24 IPC channels (259 KiB), 64 streams
(264 KiB), 32 process records, 64 shared-memory objects, 16 executable
objects. The sizes were the prototype's needs, not decisions: PID 1 held
fourteen of the 24 channels at rest, so a dozen commands with a filesystem
session exhausted the system. In userspace the shell had four exec slots, the
VFS served twelve sessions because `WaitAny` named at most sixteen objects,
and a session could hold sixteen files (GoOS 0.1.3 specification §9). The
kernel heap never frees, so a registry could not simply allocate.

## Decision

**A registry is a table of pointers; its entries come from pages.** The
`objects.Storage` interface allocates and frees a registry's entries. The
kernel installs page storage at boot: `vm.NewObject` takes whole, zeroed,
physically contiguous pages below the identity-map limit, where a kernel
pointer may name them, and `vm.FreeObject` returns them. A registry keeps a
fixed table of pointers sized by its system-wide limit and takes an entry's
pages when the entry is created. Memory in use is what is live; the limit is a
decision about how much may be live. Host tests use the Go heap; the kernel
build has no heap fallback, so a registry used before its storage is installed
refuses rather than leaking.

**Identity survives growth.** An entry never moves while it lives, so a
pointer taken during a call stays valid. Channel and stream IDs carry their
slot in their low nine bits and a creation count above, so lookup is one
comparison and an ID is never reused: a stale ID finds nothing, even when its
slot holds a new entry.

**What is freed and what is kept.** Channels, streams, shared-memory objects,
and executable objects are named by ID wherever they are used and return their
pages when released. Process records are named by pointer from several
kernel structures, so a slot keeps its record once it has one and reuses it
for later processes; the table still costs only the slots ever used.

**The limits.**

| Registry | Before | Now | Entry |
| --- | --- | --- | --- |
| IPC channels | 24, static | 256 | 3 pages, on creation |
| streams | 64, static | 256 | 1 page (ring of 4080 bytes) |
| processes | 32, static | 64 (`sched.MaxTasks`) | 4 pages, on first use of the slot |
| handles per process | 128 | 256 | in the process record |
| shared-memory objects | 64, static | 256, 16 per process | 1 page |
| executable objects | 16, static | 64 | 1 page |
| `WaitAny` objects | 16 | 64 (64-bit ready mask) | per task slot |
| wait registrations | 64 | `sched.MaxTasks` | per task slot |
| exec slots (shell) | 4 | 8 | 64 KiB of the exec area |
| VFS sessions | 12 | 63 (`WaitAny` − 1) | |
| VFS open files | 64, 16 per session | 256, 64 per session | |
| supervised services | 16 | 32 | |

**Quotas scale with them.** An ordinary process may hold 16 streams; PID 1
may hold every channel, since only it creates them, and 64 streams, four for
each job slot and the terminal's. Every charge is still returned on release,
and teardown releases a process's whole account.

**Tasks stay at 64, deliberately.** The scheduler's run, dead, and
exit-pending sets are single 64-bit masks the assembly scans with one
instruction, and its per-slot tables are sized by them. Processes follow, since
each needs a task. Raising it means multi-word masks in the scheduler, which
the SMP scheduler will need anyway; it is recorded as that work's, not hidden.

## Alternatives

- **Larger static arrays.** 256 channels would be 2.7 MiB of kernel memory
  whether used or not.
- **Freeing process records.** It needs every holder of a process pointer to
  let go first; reusing records gives the same bound without that risk.
- **A kernel heap with a free list.** The TinyGo runtime's heap never frees;
  pages from the page allocator already have an owner and an accounting.

## Consequences

- A running system uses kernel memory for the channels and streams it has,
  not for the maximum: the boot test runs seven background pipelines at once,
  past each of the old limits.
- Iterating a table means a slice, not an array value: ranging over a fixed
  array copies it onto the kernel stack, which a 256-entry pointer table
  turned into a 4 KiB frame and a double fault at boot.
- A failed page allocation is a refused create with `ResourceExhausted`, like
  a full table.
- `vm.NewObject` measures its type by element addresses, not with
  `unsafe.Sizeof` on a value: TinyGo loads a type parameter's value to size
  it, and a 12 KiB process record on the boot stack overflowed it.
