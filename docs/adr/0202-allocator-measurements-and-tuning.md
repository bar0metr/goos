# ADR 0202: Allocator measurements and tuning

## Status

Accepted. Amends the allocator and collection policy of ADR 0201.

## Context

ADR 0201 gave native userspace a slab and page-run heap under a
conservative collector, with size classes, a slot search, a metadata layout,
and a page-return policy chosen before any GoOS program ran on them. Nothing
counted what the allocator did, so nobody could tell whether those choices fit
the system's workloads: which sizes programs ask for, how much a slot wastes,
how often a page goes back to the kernel and comes straight back.

This ADR records the instrumentation added to answer that, what the
measurements showed, and which changes they justified. Rejected changes are
listed with the reason.

All host figures are medians of five alternating runs on an Apple M2 with Go
1.25.5, from `make bench-allocator` (`build/reports/allocator-optimization.md`).
They time the heap model compiled by the host Go compiler. Guest figures come
from `make bench-allocator-guest` under QEMU TCG on the same host and compare
only with each other.

## Decision

### Measurements

The heap counts what it does in a `counters` record inside its own state,
which the collector skips, so no count can pass for a pointer. Counting
allocates nothing and costs a few increments on cold paths. The counters cover
pages taken from free runs and from the untouched reservation, pages taken
again after a discard (the kernel supplies and charges each one anew), bytes
cleared on reuse, slab transitions between full and partial, free-run searches
and the candidates they examined, discards and refused discards, and trims.
A request-size histogram in 16-byte steps costs about 1 ns, 5% of a small
allocation, so only validation images and host builds keep it.

`arena.ReadDiagnostics` copies the counters and walks the metadata for a
snapshot of slabs per class, free slots, free runs, dirty free pages, and
metadata pages. `Diagnostics.AppendProfile` writes it as `ALLOC_PROFILE`
lines of at most the caller's buffer, so a console that relays whole writes
keeps each line intact. memreclaim prints its profile, and the VFS service
prints one every 4096 requests in validation builds.

`runtime.ReadMemStats` now reports `HeapReleased` as what it means in Go: idle
bytes the kernel holds no page for. It used to report the cumulative
`Released`, which counts a page once for every discard. `arena.PauseTotal`
returns the cumulative collection time; TinyGo's `MemStats` has no field for
it.

`tools/allocreport` runs the host benchmarks against other revisions and
size-class tables through `go test -overlay`, and renders benchmark results
and guest profiles into one report. `docs/development/testing.md` describes
the targets.

### Stack scrub before a collection

The first guest run with the counters failed memreclaim's unreachable-cycle
check. Watching the three rings of the cycle showed each kept alive by
another, and the entry point moved with the last `arena.Watch` call: a
collector frame slot that the compiler reserved but had not written still held
an address that an earlier, deeper call (`alloc`, `find`, `Watch`) left at the
same stack depth. The new code only moved the frames; main had the same
exposure.

`Collect` now calls `goos_gc_scrub`, which zeroes up to 4 KiB of stack below
its frame, and runs the collection in a separate function whose frames start
out zero. Frames of the collector's callers can still hold stale words; that
is the conservative scan's known false retention. The scrub touches at most
one more stack page per process.

The scrub writes below the stack pointer, which no compiled frame does, so
its bound is argued here:

- Every image `tools/elf2native` builds leaves its stack to the kernel and
  names its size in `_goos_stack_reserve`, an absolute symbol the linker
  script sets to 1 MiB; elf2native refuses an image without it, and the
  loader accepts a page multiple up to 64 MiB. `kernel/loader` reserves the
  stack as the reserve bytes that end at `UserStackTop` and leaves a 64 KiB
  guard below them. `_start` records its entry stack pointer, inside the top
  page, in `goos_stack_top`.
- `stackBase` in `userspace/runtime/arena` rounds `goos_stack_top` minus the
  reserve up to a page, which is exactly the lowest stack address, and
  `Collect` passes it. The scrub zeroes `[max(SP - 4096, base), SP)`, where SP
  is the address of its own return address: nothing below the stack, nothing
  at or above the return address.
- Below the caller's stack pointer nothing is live. TinyGo stores Go values
  in the red zone only in functions that make no call, and `Collect` calls.
  The scrub changes rax, rcx, rdi, and the direction flag, all caller-saved,
  and leaves the flag clear as the System V convention requires.
- Were the base wrong, the writes would still stay within 4096 bytes below
  the stack pointer, less than the guard, so the first one past the stack
  would fault in the guard: the argument `tools/stackframes` makes for every
  frame.

`tools/stackframes` had not seen the scrub at all. `go tool objdump` prints
the displacement -4096 unsigned, as `0xfffff000(SP)`, which the red-zone rule
did not parse, and the stores go through DI. The tool now reads such
displacements as the signed values the instruction uses, and admits
`goos_gc_scrub` only as the exact instruction sequence reviewed here; the
first, unbounded version is rejected. memreclaim's stack check collects with
its frames 16, 8, and 3 KiB above the bottom of the stack. At 3 KiB the
unbounded version would have written a KiB into the guard and stopped the
process. The check then drops objects that only dead frames 6 to 100 frames
below it held, at 48 depths, and requires at least nine in ten collected. In
the measured builds one or two depths kept their object: the address lay in
the frames that call the collector, which the scrub leaves alone, and which
depths those are moves with the compiler's frame layout.

### Size classes

The guest profile showed where requests fall. 59% of the VFS service's
requests ask for 1025 to 1152 bytes, and every one took a 2048-byte slot, half
a page. The 1360-byte class fits three in a page. Fitted to the service's
measured requests, slot bytes the requests did not ask for drop from 42.9% to
19.5% and slab pages per thousand requests from 342 to 244. On the host the
representative benchmark, drawn from that mix, is 17.7% faster and peaks
28.2% lower in pages, and allocation at 1025 to 1360 bytes is 3% faster than
without the class. The service collects 31% less often, 323
against 469 times in 9216 requests, because more of its objects fit under the
same threshold.

Rejected:

- Widening 384 to 400 and 768 to 816 keeps the slots per page and fits a few
  more sizes, none of which the services asked for. Larger slots cost more to
  scan and to clear: the scanned-collection benchmarks ran 12% to 19% slower.
- A 336-byte class (12 per page) for the 29% of VFS requests at 321 to 336
  bytes cut peak pages by a further 1.4 points and cost the mixed and
  long-and-short-lived benchmarks the 2% to 5% the other changes had won.
- 1280, 1536, and 1792: 1536 and 1792 fit two per page, the same as 2048, so
  they save no page, and 1280 fits three like 1360 while serving fewer sizes.
  Fitted to the VFS requests the three give the same 244 pages per thousand
  as 1360 alone.

### Small-allocation fast path

- A table indexed by the size in 16-byte steps replaces the search of the
  class sizes, and a table of slots per class replaces a division.
- `find` divides the 32-bit offset in its page by the slot size, which on
  amd64 is the cheaper division, and rejects a slot that ends past the page;
  it used to divide the 64-bit offset and divide again for the slots per
  page.
- A slab records the first slot not handed out since its page last read as
  zeroes. Slots from there on are not cleared: a slab on a page the kernel
  just supplied used to clear every slot it handed out after the first,
  because the dirty mark is per page.
- A slab records the first bitmap word that may hold a free slot. A free
  lowers it, a sweep resets it, and the test invariant checks that every slot
  before it is in use.

On the host a small allocation got 30% faster, the short-lived-garbage
benchmark 33% faster (18% from the tables and the clearing, 18% of the rest
from the hint), allocation at the class boundaries 8% to 11% faster, and the
mixed, long-and-short-lived, and free-and-reallocate benchmarks 2% to 4%
faster. Collection times stayed within 3% of main. The hint costs one-word
classes about 2%. In the guest the mixed workload ran in 570 ms against
1108 ms and cleared 2.6 MB on reuse against 73 MB.

Not adopted:

- A multiply by `ceil(2^32 / size)` and a shift in place of the division in
  `find`. The lookup benchmark ran 12% slower than main with it and 2% faster
  without it; collections did not change either way.
- A separate cache of free slots. The bitmap is the one record of which slots
  are in use, the hint already skips full words, and a cache would need
  invalidation on every free, sweep, and reuse.

### Metadata

Each page keeps its 120-byte record. A layout with a 24-byte page descriptor
and slab bitmaps in their own array indexed by page saves 96 bytes for every
page that is not a slab. A long-running service, though, places slabs all over
its heap over time, and once touched a page of bitmaps stays charged. The
diagnostics count the bitmap pages every slab so far would have touched: the
VFS service touched all 7 for its 271 pages, so that layout would cost it 9
pages against 8. The mixed workload: 10 against 9. The layout stays.

### Free-page retention

The heap can now keep a bounded number of dirty free pages after a collection
or an explicit free for the allocations that follow, instead of giving every
one back. `arena.SetRetained` sets the bound, at most 256 pages. The trims
give the longest runs back first and a run from its end, so what stays is in
the short runs a slab is taken from.

What the policy guarantees:

- After a collection, and after an explicit free that finds more than 1 MiB
  of free pages, at most the retained number of dirty free pages stay
  undiscarded, plus any the kernel refused to discard.
- An idle wait, `wait.Any` with nothing ready for 100 ms, gives back every
  free page, retained or not, even with nothing allocated since the last
  collection: `arena.IdlePending` also reports free pages that wait.
- An allocation that does not fit collects, gives back every free page, and
  retries; the path that reports exhaustion gives them back first too.
- A refused discard leaves its pages dirty, so they are cleared before
  reuse, and a later trim tries again. Every allocation reads as zero,
  whichever path its memory took.

What it does not guarantee: anything at the page quota. The kernel charges a
page when it is first touched, after the allocator has returned the memory,
and stops the process at the touch that would pass the quota; no trim runs
then. Retained pages stay charged while kept, so they take up to the retained
number of pages of headroom. A process could watch its headroom only by
reading its own `ProcessInfo` record, which reports each process's pages and
page limit; but the call indexes processes, has no entry for the caller, and
the record in state running is the caller only while one processor runs
processes. Under SMP another process can be running at the same moment. No
quota-aware retention is built on that, and `SetRetained` stays an opt-in
tuning call, zero by default. `memreclaim quota` checks reuse and the idle
trim at 256 retained pages and then grows a live heap past the quota, which
the kernel must stop without disturbing anything else.

Retention defaults to zero, which is ADR 0201's behaviour. The guest workload
ran slower the more it retained:

| Retained pages | Time | Bytes cleared on reuse | Pages at the end |
|---:|---:|---:|---:|
| 0 | 573 ms | 2.6 MB | 115 |
| 16 | 638 ms | 15 MB | 125 |
| 64 | 819 ms | 55 MB | 163 |
| 256 | 1024 ms | 205 MB | 317 |

A retained page holds old bytes and must be cleared before it is reused. A
discarded page comes back zeroed, and the untouched-slot record above skips
clearing its slots.

### Memory gate

`memreclaim service` used to band the VFS service's pages on samples taken
while requests ran. Those samples land anywhere in a collection cycle, in
which the service's pages swing by up to 200 as garbage grows to the next
threshold. With fewer collections the samples fell at other points of the
cycle and the band failed; main passed it on the points its own cycle
happened to put them. The check now samples the service after it has been idle
long enough to collect and give its pages back: all ten samples read 105
pages. `tests/boot/memory.sh` lists the pages from the service's own reports
and bands only their live heap. `memreclaim burst` and a check of pointer
slices of every class size were added.

The leak run used to put several commands on one `;` line and take the next
prompt as success. The GoOS shell has no `&&` and runs every command of such
a line, so a failed copy or a missing Linux program still produced a prompt
and a page sample. `tests/boot/leak_round.sh` now runs each step on its own
line, rejects any diagnostic a stage prints, checks what the step made (a
copy's size with `stat -b`, the benchmark's result lines and file, a listed
name, the bytes a pipeline wrote, the Linux program's output), and requires
each file gone after its removal, all before the round's pages count. A host
test feeds it canned output with a fault injected into each step in turn. The
leak criterion itself is unchanged.

## Consequences

- An allocator change comes with numbers: `make bench-allocator` for the
  host, `make bench-allocator-guest LABEL=...` for one guest, and
  `make allocator-report` for both.
- Programs allocate faster and the VFS service collects less often. After
  idle time the processes of the shell rounds hold 3275 to 3276 pages
  against 3300.
- Unresolved: the kernel's pages in those rounds read 1071 or 1072 against
  1069, steady from round to round, and 1073 to 1075 in a later 20-round
  run. The kernel did not change and the cause has not been reproduced.
  `mem -p` sums records it reads one at a time while the last command's
  processes may still be torn down, so a page can land on either side of the
  count; the memory gate accepts a dip for that reason and fails only a rise
  in the second half.
- `make test-memory` passed on the measured tree with 3276 to 3279 process
  pages and 1073 to 1075 kernel pages over 20 rounds, the VFS service at 105
  pages in every idle sample, and a burst of 33, 197, and 50 pages before, at
  its peak, and after idle time. These are an observed baseline, not
  thresholds: the gate checks trends and bands, and the counts move with
  images and workloads.
- The command index is at 4086 of its 4096 bytes after memreclaim's usage
  line grew; the next command or usage change must make room.
- Validation images carry the request histogram; other images do not.
