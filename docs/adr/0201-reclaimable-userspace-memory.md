# ADR 0201: Reclaimable userspace memory

## Status

Accepted. ADR 0202 adds the 1360-byte class, a stack scrub before each
collection bounded by the bottom of the stack, free-page retention (off by
default), and trims of free pages at idle time and before out-of-memory.
ADR 0203 extends the heap and stop-the-world collector to native sibling
threads. ADR 0204 extends root discovery to M:N goroutine stacks and full
held-thread register snapshots.

## Context

Native userspace ran on a bump allocator. `runtime.alloc` moved a pointer
forward through a 256 MiB reservation, and `runtime.free`, `runtime.GC`, and
`runtime.ReadMemStats` did nothing. Services kept memory bounded with
`arena.Current` and `arena.Reset`: take a mark once long-lived state existed,
rewind to it before each request. A request that stored anything into
long-lived state, such as a map that grew, left a pointer into memory the next
request overwrote. Commands rewound inside their own loops for the same reason.
A program with no natural request boundary leaked.

The kernel follows a different rule, and keeps it. ADR 0065 holds: the kernel
links TinyGo's leaking heap, allocates only during boot and in approved
self-tests, and its repeating paths allocate nothing. A collector in the kernel
would have to stop interrupt handlers and processors and would add pauses to
every path ADR 0065 keeps allocation-free.

## Decision

Native userspace gets a heap that reuses memory, a collector for ordinary Go
objects, and regions for scratch with an obvious lifetime. The kernel is
unchanged.

### Allocator

`userspace/runtime/arena` keeps the 256 MiB reservation and splits it into
4 KiB pages. Allocations up to 2048 bytes share slab pages of one size class:
16, 32, 48, 64, 96, 128, 192, 256, 384, 512, 768, 1024, or 2048 bytes. Larger
ones take a run of whole pages. Pages no allocation holds form free runs,
merged with free neighbours and kept in seventeen lists by length.

Every page has a 120-byte record in a second reservation: its kind, its slot,
mark, and pointer-free bitmaps for a slab, its length, owning first page, and
pointer-free flag for a run, and list links. Payload pages hold only allocation bytes, so the allocator can
discard a free page without losing its bookkeeping, and the collector resolves
any address to its allocation without trusting the bytes beside it. Metadata
pages are touched only up to the highest page the heap has handed out.

Allocation picks the first slab of the class with a free slot, or takes a page.
It never scans the heap. A page records whether it may hold old bytes; the
allocator clears a slot or run on such a page before handing it out, and skips
clearing pages the kernel supplied zeroed.

`runtime.free` releases the allocation at that exact address. A pointer inside
the heap that does not start a live allocation, including a second free, stops
the process: the metadata can no longer be trusted. The emptied slab of a class
stays cached when it is the class's only partial slab, so a loop that frees and
allocates again does not take and return a page each time.

### Collector

The collector is a per-process, stop-the-world, non-moving, conservative mark
and sweep. A native program has one thread (`scheduler: none`), so stopping the
world means the allocating call does the work. The kernel and other processes
keep running.

Roots are the mutable globals, the stack, the callee-saved registers, and the
used part of every region. Marking reads every aligned word of a root or a
marked allocation. A word inside the handed-out part of the heap marks the
allocation containing it, at its start or anywhere inside, so interior pointers
keep their allocation. Other words are ignored: the collector never follows a
value into kernel memory, shared memory, or another reservation.

Marked allocations go on a mark stack of 64 Ki entries in its own reservation.
When the stack is full, a newly marked allocation stays marked but unqueued and
the collector notes an overflow; after the stack drains it rescans every marked
allocation until a pass completes without overflow. A list a million nodes deep
needs one stack entry at a time and no recursion.

Sweep frees every unmarked allocation, clears the marks, and returns emptied
slab pages to the free runs. Every free page that may hold old bytes then goes
back to the kernel with `VMDiscard`, which keeps the reservation. Page tables
stay until the process exits. The mark stack also gives back pages beyond its
first.

A word that only looks like a pointer keeps garbage alive. That is the
trade-off of a conservative scan and the reason it is safe: nothing reachable
is swept.

The collector reads one piece of type information: whether an allocation can
hold a pointer at all. TinyGo passes `runtime.alloc` the layout its precise
collector reads (`runtime/gc_precise.go`, `internal/gclayout`). An odd value
with no bit set above bit 6 is an inline bitmap with no pointer word. The
allocator records such an object as pointer-free, and marking keeps it alive
without reading it. Byte buffers and strings take this path. Everything else,
including the nil layout the runtime passes for maps, channels, and C
`malloc`, is scanned word by word.

Skipping those objects matters beyond speed. Reading a page the program never
touched makes the kernel supply and charge it, so a conservative scan of a
large, mostly unused buffer would commit all of it. The first validation run
showed this: a process holding untouched 1 MiB buffers ran out of page quota
during a collection. The encoding is internal to the pinned TinyGo, so
`memreclaim` checks it on every validation boot: byte buffers must come out
pointer-free, and a slice of pointers and a struct with a pointer must not.
Precise scanning of the remaining objects can use the same layouts later.

### Root discovery

TinyGo 0.42's `gc.custom` contract says a custom collector must call
`runtime.markStack`, but the pinned toolchain builds that function only for
the conservative, precise, and Boehm collectors and for WebAssembly. For
`gc.custom` on amd64 it does not exist, and `tinygo_scanCurrentStack` is not in
this target's objects. GoOS therefore finds its roots itself and uses no
TinyGo internals:

- `_start` stores the stack pointer it calls the program with in
  `goos_stack_top`, the upper end of every frame.
- `goos_gc_registers` stores `rbx`, `rbp`, and `r12` to `r15` and returns its
  caller's stack pointer. The collector scans those six words and every word
  from that stack pointer to the top.
- `userspace/runtime/amd64.ld` brackets `.data` and `.bss` with
  `_goos_gc_globals_start` and `_goos_gc_globals_end`. Both sections land in
  one output section, so the range covers every mutable global and no code or
  read-only data. The collector skips its own heap state inside that range;
  the heap base it holds would otherwise keep the first allocation alive.

TinyGo calls `runtime.markRoots` only from its own stack scanners, which this
target does not build. The hook marks a range if a collection is running.

Nothing outside those roots is scanned: page-pool pages, shared-memory
windows, device and DMA memory, and other reservations hold no roots. A Go
pointer stored only there keeps nothing alive. Programs keep numeric
references in such memory; the RAMFS node table already does. No call
registers another root range, so the rule has no exception.

### Collection policy

`runtime.GC` collects synchronously: sweep and discard are done when it
returns. An allocation collects first when live bytes reached the threshold
the last collection set, `max(2 × live, live + 1 MiB)`. An allocation that does
not fit collects and retries, and only a failed retry calls `arena.OnExhausted`.
Nothing runs in the background.

A service that waits is idle. `userspace/wait.Any` with a timeout longer than
100 ms first waits 100 ms; if nothing became ready and the caller allocated
since its last collection, it collects and waits out the rest. The shell, PID 1,
VFS, configd, block, tty, FAT32, netd, resolved, virtio-net, remoted, and
remoted-worker all wait there, so each holds about its live heap while idle
instead of whatever garbage it reached before the next threshold.

The boot test's leak rounds used to require identical page counts after
repeated workloads. Idle heaps still move by a page or two as slabs fill at
different points, so `mem -p` now also reports the pages processes hold and
the pages the kernel holds for itself. The kernel's count must stay identical
across rounds; the total may move within eight pages but must not rise every
round. Seven measured rounds kept the kernel at 1099 pages and the total
within one page. Three rounds in a band cannot show a slow leak in a process,
so `make test-memory` adds a twenty-round run of a mixed workload that
compares the kernel's pages exactly and process pages between the first and
second half.

A service starts with `arena.InitService`. A program started from a file loads
each image page the first time it touches it (ADR 0121), through the VFS,
FAT32, and block services, and the boot test restarts those services on
purpose. A service that first runs a page of code inside that window faults.
With the collector the pages a service touches changed: first a log service
faulted on the collector's own code, then netd on a send path the new layout
put on a fresh page. The boot logs before this change show the same kind of
fault in another process during network tests. `InitService` therefore reads
one byte of every page of the service's image, code, constants, and globals,
and runs one collection over two throwaway allocations. Commands stay lazy:
they start often and rarely outlive a storage restart.

### Regions

`arena.Region` stays for scratch whose lifetime is one packet, request, or key
exchange; the SSH backend uses three. A region's memory is its own `VMReserve`,
not part of the heap. While a region is entered, every allocation is a zeroed
bump allocation in it and none reaches the heap. A region that runs out of room
calls `OnExhausted`; it never spills into the heap.

No collection runs while any region is entered. A region value may hold the
only pointer to a heap object, so the collector scans the used part of every
region, entered or not, up to its current position. Bytes past that position,
left from a previous generation, are never scanned. `Clear` resets the
position and discards the region's pages once it had used more than 64 KiB. A
region lives until `Release`, which unmaps its reservation and removes it from
the roots; a program creates regions per repeating lifetime and clears them
between uses rather than creating one per request.

### Statistics and finalizers

`runtime.ReadMemStats` reports:

| Field | Meaning |
|---|---|
| `Alloc`, `HeapAlloc` | bytes in live allocations, counted as whole slots and runs; between collections this includes unreachable ones |
| `TotalAlloc` | cumulative bytes handed out |
| `Mallocs`, `Frees` | cumulative allocations, and allocations freed explicitly or by a sweep |
| `HeapObjects` | live allocations |
| `HeapSys` | the heap reservation, 256 MiB |
| `HeapInuse` | bytes of pages holding at least one allocation |
| `HeapIdle` | `HeapSys` minus `HeapInuse` |
| `HeapReleased` | bytes of `HeapIdle` the kernel holds no page for: given back with `VMDiscard` or never touched |
| `GCSys` | metadata in use plus the mark stack reservation |
| `Sys` | `HeapSys` plus `GCSys` |
| `NumGC` | completed collections |

`arena.HeapStats` returns the same state with held and peak figures. `Held`
counts pages the heap has handed out or not yet discarded; a page handed to an
allocation may never have been touched, so `Held` bounds the heap's resident
pages from above and is not a measure of RAM. The kernel's per-process page
count is, and `memreclaim`, `VFS_MEMORY`, and `mem -p` report that.
`arena.PointerFree` and `arena.PointerFreeString` report how an allocation
was recorded. `memreclaim` checks pointers at an offset, in nested structs,
arrays, the tail of a page run, interfaces, and string slices, and keeps
objects alive only through such containers, maps, channels, and closures
across collections that reuse every freed page.
`arena.Pauses` returns the last and longest collection time. `arena.Watch` names an
allocation by an inverted address, which keeps nothing alive, and
`arena.Watched` reports the root phase and word that first marked it in later
collections. `memreclaim` prints them if its dropped global object survives.

`runtime.SetFinalizer` records nothing, and no finalizer ever runs.

### Services

Every `Current`/`Reset` loop now leaves request garbage to the collector:
shell, VFS, configd, logd, block, FAT32, tty, netd, virtio-net, resolved,
remoted, remoted-worker, PID 1, and the commands and libraries that rewound
per chunk. Their request state is either persistent, which a rewind used to
corrupt when it grew, or small and short-lived, which the threshold policy
collects within a megabyte of allocation. None has a lifetime that needs a
region for speed, and a region sized for the largest request would only move
the bound. `Current`, `Reset`, `Mark`, and `Touched` are gone.

## Consequences

- A service may keep state built during a request without arranging for it to
  outlive a rewind.
- Allocation is slower than a pointer bump. On the development host a small
  allocation costs about 13 ns, an allocate and free about 28 ns, and a
  collection of 20,000 live 64-byte objects about 0.26 ms; with 2% of them live
  it takes 7.5 µs. These are host measurements of the model, not a gate.
- A collection pauses only the allocating process, for time proportional to
  its globals, stack, regions, live objects, and handed-out pages.
- A process's committed heap after a collection is its live heap, any free
  pages the kernel refused to discard, the slab cache, metadata up to the heap's
  high-water mark, and page tables.
- False retention is possible and bounded by what the stray words point to.
- The collector reads every mutable global, so a program's whole `.data` and
  `.bss` is resident after its first collection, including BSS pages the
  program never touched. Native services have 140 to 200 KiB of globals.
- `make test-memory` gates the heap: host tests of the model, including a
  randomized run against a reference model, and one guest that runs
  `memreclaim` and a long VFS run. It is part of `test-all` and
  `release-check`.
