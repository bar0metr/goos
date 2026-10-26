# ADR 0205: Thread caches and collection pacing for native allocation

## Status

Accepted and implemented. The measurements are from the runs the
Measurements section names.

## Context

ADR 0203 gave each native thread one owned slab per size class. Allocation
and frees into that slab skip the heap lock, and a free into another
thread's slab sets a bit in the slab's mark bitmap. Under KVM on eight
processors (run `20261019T201556Z-51b3ff70`, 200000 allocations of 16 to
1000 bytes per thread, last 64 kept, medians of five runs) owned slabs beat
the heap lock alone, and total throughput still fell as threads were added:

| Threads | Heap lock ms | Owned slabs ms | Collections | Lock waits | Refills | Longest pause us |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 31.0 | 31.9 | 49 | 0 | 0 | 164 |
| 2 | 222.5 | 100.9 | 98 | 37698 | 26725 | 122 |
| 4 | 611.3 | 361.3 | 196 | 308323 | 55319 | 159 |
| 8 | 1745.8 | 816.7 | 388 | 521799 | 118464 | 410 |

The code at `ab34281` explains the curve:

- A slab of 768 to 2048 bytes holds 2 to 5 slots, so a thread with one
  slab per class refills every few allocations in those classes, and each
  refill takes the heap lock.
- The next collection starts at `max(2 × live, live + 1 MiB)`. A small live
  heap gives 1 MiB of garbage between collections however many threads
  produce it, so eight threads collect eight times as often as one.
- Every collection holds every thread, and before marking it takes every
  owned slab back, so the next allocation on each thread refills.
- No free page survives a collection by default. Retaining 256 pages cut the
  eight-thread time from 816.7 ms to 652.5 ms and slowed two threads.
- `wait.Any` collects after one thread waited 100 ms. In a process with
  several threads that proves only that the caller waits.

## Decision

### Collection reasons

Every collection carries one of four reasons. The reason decides what the
collection gives back. Root discovery, marking, and the fail-closed rules of
ADR 0203 and ADR 0204 do not depend on it.

| Reason | Started by | Thread slabs | Free pages kept |
|---|---|---|---|
| automatic | an allocation that crosses the goal | kept by threads that allocated | `retention` below |
| explicit | `runtime.GC`, `arena.Collect` | taken back | the `SetRetained` count, zero by default |
| idle | `CollectIdle` once every thread waits | taken back | none |
| pressure | an allocation that found no room | taken back | none |

A collection that completes counts one more epoch. A collection given up
before its sweep counts no epoch, leaves the goal and the allocating-thread
marks alone, and records itself under its reason in the diagnostics.

### Thread caches

A thread owns up to four slabs per class in a fixed array in its thread
record. A refill aims for 16 free slots:

```text
cacheTarget(class) = clamp(ceil(16 / slotsPerPage(class)), 1, 4)
```

That gives one slab for each class up to 256 bytes, two for 384 and 512
bytes, and four for 768, 1024, 1360, and 2048 bytes: at most 28 pages,
112 KiB, per live thread, and 896 KiB for the eight threads of a large
process. A claimed slab is an untouched page until the thread allocates from
it, so the kernel charges nothing for it before then.

The fast path tries the hinted slab and then the others of the class, at
most four, and takes no lock and no new atomic operation. Remote frees stay
invisible to it, as before. When every owned slab of a class is full, the
thread takes the heap lock and refills:

1. fold its counts;
2. fold the remote frees of every slab it owns of the class, and keep them
   all if that freed a slot;
3. otherwise give up the full slabs, which stay off the lists until a free
   or a sweep gives them a free slot;
4. take listed slabs of the class, then new ones, up to the target.

A refill that finds fewer pages than the target keeps what it found. A
refill after a pressure collection asks for one slab. A thread's exit gives
every slab back under the heap lock before `ThreadExit`, and a reused slot
starts with an empty cache. The page record stays 128 bytes.

### Kept caches across an automatic collection

The mark bitmap of an owned slab holds remote frees between collections and
marks during one. ADR 0203 took every slab back before marking to clear the
bitmaps. A collection holds every other thread before it touches their
caches, so no remote free can land while it works, and it can fold the
remote frees in place:

1. For each live thread that allocated since the last collection, fold the
   remote frees of every slab it owns and keep the slabs owned. Every other
   thread gives its slabs back.
2. Fold every thread's counts.
3. Fold the remote frees left in unowned slabs.
4. Mark: every mark bitmap is clear.

The sweep frees dead slots in an owned slab as in any other, and leaves the
slab with its owner, off the lists, even when it empties. Afterwards
`checkCache` stops the process if any cached page is no longer a slab of its
class owned by that thread. A collection given up after step 2 leaves
consistent state: folded frees and counts are already part of the heap, and
the slabs stay owned.

### Collection goal

A thread counts as allocating when it allocated from the heap since the last
completed collection. Folding counts with allocations sets the mark, and so
does a run allocation, under the heap lock it already holds. A collection
counts the marks of live threads after folding, clears them once it
completes, and keeps them when it gives up. Region and zero-byte
allocations set nothing. A single-thread process always counts one.

The heap reads its page quota once, at start, with `ProcessInfo` into a
global. After each sweep:

```text
floor = min(allocators × 1 MiB, max(1 MiB, quota / 16))
next  = min(reservation, live + max(live, floor))
```

| Profile | Page quota | Largest floor |
|---|---:|---:|
| tiny | 8 MiB | 1 MiB |
| normal | 32 MiB | 2 MiB |
| large | 64 MiB | 4 MiB |

A heap that cannot read its quota keeps the 1 MiB floor. A large live heap
keeps `2 × live`. Both the small-allocation refill and the run-allocation
path fold the caller's counts before comparing with the goal. Other threads'
unfolded counts can overshoot it by at most what their caches hold.

The 1/16 share bounds how much garbage the floor allows. It reserves
nothing, and images, page tables, stacks, and regions share the same quota.
An eighth was measured first: eight threads then collected every 8 MiB,
and each pause swept, and in validation images poisoned, 8 MiB of garbage,
up to 1.9 ms. A sixteenth kept eight threads under 100 collections and the
longest pause near half a millisecond.

### Retention

| Collection | Free pages kept |
|---|---|
| automatic, one allocating thread | the `SetRetained` count, zero by default |
| automatic, several, no `SetRetained` call | `min(32 × allocators, 256)` |
| automatic, after `SetRetained(n)` | `n` |
| explicit | the `SetRetained` count, zero by default |
| idle, pressure | none |

Kept pages stay dirty. Allocation clears only the slot or run it hands out.

The benchmark ran 16, 32, and 64 pages per allocating thread (`r16`, the
default, `r64`). Between two runs of one image the four-thread time for 16
pages moved from 142 ms to 203 ms, and 32 pages passed the targets in every
run, so the default stays at 32.

With several threads the collector discards free pages after it lets the
other threads go, still holding the heap lock: a free run changes only under
that lock, and the threads run on their own slabs meanwhile. At an 8 MiB
floor the discards had taken half of each pause.

### Idle collection

Each thread record has an idle mark. `wait.Any` sets it for any wait with a
timeout, the goroutine scheduler sets it around an M's idle wait and a
pinned thread's wait for its goroutine, and the scheduler clears it whenever
it runs a goroutine: a goroutine that parks in `wait.Any` leaves its M
marked until then.

`CollectIdle` in a process with several threads collects only when every
other live thread is marked. The collector looks again after it holds the
threads and gives up the idle collection if one has left its wait. A wrong
mark costs work only: an idle collection holds every thread and finds every
root like any other. `IdlePending` and `CollectIdle` also count slabs that
threads own, so an idle process gives back its caches.

### Lock hierarchy

Unchanged from ADR 0203: the collection lock, then the heap lock, then the
runtime's thread-table and scheduler locks, then kernel locks inside one
call. The fast path takes no lock, and no class has a lock of its own.

### Diagnostics

`ALLOC_PROFILE` is at version 3. After the `threads` line come four lines:

- `cache`: `hits`, `misses`, `batches`, `batch_slabs`, `disowned_full`,
  `cached`, `peak_cached`, `bound`, `preserved`, `flushed`, `unfolded`;
- `gc`: completed `automatic`, `explicit`, `idle`, `pressure`; the
  `abandoned_` count of each; `allocators`, `max_allocators`, `floor`,
  `live`, `goal`, `page_limit`, `retain_target`; `idle_requests`,
  `idle_skipped`, `idle_abandoned`;
- `pause_hist` and `rendezvous_hist`: collections by pause and by the time
  holding the threads took, in buckets up to 50 us, 100 us, 250 us, 500 us,
  1 ms, 2 ms, 5 ms, and more.

`unfolded` counts threads whose counts the totals lack. `tools/allocreport`
reads versions 1 to 3 and rejects a version 3 report that lacks any of these
counters.

### Testing

Host tests (`make test-thread-allocator` runs them with and without `-race`):

- `TestCachedSlabsReuseEveryPosition`, `TestBatchRefillTakesListedThenFreeThenFreshPages`,
  `TestRemoteFreesReachEveryCachedSlab`: frees into each cache position,
  the hint wrapping around, refills from each page source and from a heap
  too small for the target, untouched claimed pages, stale owners, and
  duplicate remote frees.
- `TestAutomaticCollectionKeepsCaches`, `TestAbandonedCollectionLeavesCachesUsable`,
  `TestBatchedSlabsRace`: kept slabs across collections, empty owned slabs,
  given-up collections, and a race over the large classes with collections
  between rounds.
- `TestGoalScalesWithAllocatingThreads`, `TestRetentionDependsOnReason`,
  `TestIdleCollectionTrimsEveryPage`, `TestIdleQuorum`,
  `TestFoldRecordsAllocatingThreads`.
- `TestWindowHoldAtEveryPoint` now also holds a thread whose allocation moves
  to its next slab, with a collector that keeps the caches and one that
  takes them back.

Guest checks (`threadprobe`, in `test-thread-allocator` under TCG and
`test-pve-threads` under KVM): `bench … big` on the classes of 512 to 2048
bytes; `remote` with one and two ways of frees between paired threads and a
tag in every object; `live` with most of the heap kept; `idle`, where every
thread waits after each burst and the process's pages must not climb; and
`quota`, where threads hold a third of the free quota, drop it, and take it
again.

### Measurements

Filled in from the reference runs below.

## Consequences

- Threads that allocate collect less often, up to eight times less in a
  large process, and keep their slabs across those collections.
- A large process can hold up to 4 MiB of garbage between collections
  instead of 1 MiB, and 896 KiB of slabs in thread caches; an idle wait on
  every thread gives both back.
- The heap's frontier grows with the floor, and the page records up to the
  frontier stay charged: 128 bytes per heap page, one page of records per
  32 heap pages. After the idle rounds of eight threads a process held about
  135 pages where 0.2.8 held 104, steady from round to round.
- A process whose threads block outside `wait.Any` and the goroutine
  scheduler never counts as idle, and gets no idle collection.
- A pressure collection runs when the 256 MiB reservation is full. The
  kernel stops a process that touches a page past its quota before that, so
  the goal's quota share, not the pressure path, keeps a process inside its
  quota.

## Rejected alternatives

- A global byte counter on every allocation: it would share a cache line
  across every thread on the path this ADR keeps local. Fixed cache bounds
  already bound what the goal misses.
- Locks per class: they add lock order and collector rendezvous cases, and
  refills and collections, not the lock itself, dominated the measurements.
- A separate remote-free bitmap: it would grow the page record by 32 bytes,
  and holding the threads already lets the collector fold remote frees
  before marking.
- Caches per goroutine: memory would grow with goroutines, and allocation
  already runs pinned to the thread.
- Caches per processor: GoOS has no restartable-sequence primitive, and a
  process runs at most eight threads.
- Concurrent or incremental collection: it needs write barriers. Fewer
  collections addressed the measured cost first.
- Larger quotas: the work has to fit tiny, normal, and large as they are.
