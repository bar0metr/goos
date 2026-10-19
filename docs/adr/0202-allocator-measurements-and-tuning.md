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

`Collect` now calls `goos_gc_scrub`, which zeroes the 4 KiB of stack below
its frame, and runs the collection in a separate function whose frames start
out zero. Frames of the collector's callers can still hold stale words; that
is the conservative scan's known false retention. The scrub touches at most
one more stack page per process.

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
classes about 2%. In the guest the mixed workload ran in 527 ms against
1108 ms, cleared 2.6 MB on reuse against 73 MB, and its longest collection
took 24% less time.

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
the short runs a slab is taken from. An idle wait gives back every free page,
retained or not, even with nothing allocated since the last collection:
`arena.IdlePending` also reports free pages that wait. A failed allocation
gives them back before its retry, and so does the path that reports
exhaustion. A refused discard leaves its pages dirty, so they are cleared
before reuse, and a later trim tries again.

Retention defaults to zero, which is ADR 0201's behaviour. The guest workload
ran slower the more it retained:

| Retained pages | Time | Bytes cleared on reuse | Pages at the end |
|---:|---:|---:|---:|
| 0 | 538 ms | 2.6 MB | 115 |
| 16 | 598 ms | 15 MB | 125 |
| 64 | 769 ms | 55 MB | 163 |
| 256 | 976 ms | 205 MB | 317 |

A retained page holds old bytes and must be cleared before it is reused. A
discarded page comes back zeroed, and the untouched-slot record above skips
clearing its slots. A process cannot find its own page quota without a new
kernel call, since `ProcessInfo` has no entry for the caller, so retention
also could not back off before the quota; at zero it never has to.

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

## Consequences

- An allocator change comes with numbers: `make bench-allocator` for the
  host, `make bench-allocator-guest LABEL=...` for one guest, and
  `make allocator-report` for both.
- Programs allocate faster and the VFS service collects less often. After
  idle time the processes of the shell rounds hold 3276 to 3278 pages
  against 3300.
- The kernel's pages in those rounds read 1071 against 1069, steady from
  round to round. The kernel did not change; the difference has not been
  traced.
- The command index is at 4086 of its 4096 bytes after memreclaim's usage
  line grew; the next command or usage change must make room.
- Validation images carry the request histogram; other images do not.
