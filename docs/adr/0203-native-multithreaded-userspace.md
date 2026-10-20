# ADR 0203: Native multithreaded userspace

## Status

Accepted and implemented. The measurements are from the runs the
Measurements section names.

## Context

A native process runs one task. The kernel already schedules several tasks
of one process on several processors: Linux processes do it through `clone`
(ADR 0177, ADR 0179), and `process.Process` holds up to 32 task identities.
The native runtime does not: its system-call wrappers share scratch
descriptors in package variables, the collector (ADR 0201) scans one stack
from one recorded top, regions keep one cursor, and the heap (ADR 0202) has no
lock.

This ADR adds native threads: tasks that share one native process's address
space, handles, capabilities, ABI, and quota, each with its own stack, run in
parallel, and allocate from one heap that one stop-the-world collector
reclaims.

What the code showed before the design was fixed (base `a5a3c2b`):

- Every way a native task leaves a processor writes its user registers in one
  layout. The timer and reschedule interrupts push the fifteen general-purpose
  registers and keep the IRETQ frame; the native call gate, when a call
  blocks, retries, or yields, rewrites its entry frame into the same layout
  (`.user_park` in `kernel/arch/amd64/io.S`); a demand-zero fault that must
  reschedule does the same (`.user_fault_preempt`). `taskContexts[slot]`
  then points at fifteen registers, R15 lowest, and an IRETQ frame whose CS
  is the user selector. A task held off its processor can be read from there.
- `HoldTasks`, `QuiesceTasks`, and `ReleaseTasks` keep tasks off every
  processor and wait for them to leave; Linux fork uses them while it copies
  memory. A task stopped while it waits for a lock set gives the set up and
  retries its call (ADR 0184), so a held task holds no kernel lock.
- A new task's FPU area is the initial one (`InitialFPUArea` on reclaim), and
  the scheduler saves and restores FS base per task.
- A native call may return `ActionBlock`: the gate parks the task with its
  `INT` rewound and runs the call again on wake. `waitProcess` registers a
  waiter, checks again, and blocks; joins work the same way.
- The kernel creates the main stack itself, pinned, above a 64 KiB guard
  region (`setUpStack`). User code cannot unmap or reprotect a pinned region.
- TinyGo builds userspace with `scheduler: none`: no goroutine ever moves to
  another stack, and the stack pointer always lies in the stack its task
  started on.

## Decision

### Native thread ABI

Seven operations are appended to `kcall`; no existing number changes.

| Operation | Arguments | Contract |
|---|---|---|
| `ThreadCreate` | entry, argument, result address | Starts a task in the caller's process at `entry` with the argument in RDI and its stack slot in RSI. Writes the thread's ID and slot. |
| `ThreadExit` | code | Ends the calling thread. The process goes on while another of its threads lives. |
| `ThreadJoin` | ID, flags, result address | Waits for a thread of the caller's process to end and writes its exit code once. With `JoinPoll` it answers `Timeout` instead of waiting. |
| `ThreadSelf` | result address | Writes the caller's ID and slot. |
| `ThreadYield` | | Gives up the processor. Nothing needs it for progress: siblings are preempted. |
| `ThreadSuspend` | buffer, capacity, result address | Runtime-private: holds every other thread of the caller's process, waits until each is off its processor, and writes a root snapshot of each. |
| `ThreadResume` | | Releases what the caller's `ThreadSuspend` held. |

A thread ID is the task's `sched.TaskID`: slot and generation, so an ID of a
thread that ended never names the thread that reuses its slot. It is not a
handle and confers nothing: every operation finds the caller's process from
the calling task and refuses an ID that is not one of that process's threads.
No capability is needed to create a sibling, since a thread cannot reach past
its own process, and the task quota bounds creation. Only native processes
can make these calls; the gate checks the ABI before dispatch.

`Exit` stays process-wide. When a process with several threads calls it, the
kernel records the code as the process's exit code and terminates every
other thread. A thread that ends any way but `ThreadExit`, a fault or a kill,
ends the process the same way, with its code. The process's exit event,
which PID 1 collects once, carries the first such code, or the code of the
last thread to call `ThreadExit`.

### Stacks

The kernel makes every thread stack, as it makes the main one. A process has
16 thread slots. Slot 0 is the main task and its stack. Slot k from 1 to 15
owns a fixed stack of 512 KiB ending at
`ThreadAreaTop - (k-1) * 576 KiB`, with a 64 KiB guard region below it, both
pinned. `ThreadAreaTop` lies 65 MiB below `vm.UserStackTop`, past the
largest main stack an image may ask for (`MaxStackReserve`, 64 MiB) and its
guard. `ThreadCreate` inserts the two regions, `ThreadExit`'s teardown removes
them and frees their pages once the task is off its processor. A slot whose
range overlaps another region cannot be used, and creation fails cleanly.

The entry must lie in a region that is readable and executable and not
writable: a thread never starts in data. The initial stack pointer is the
stack top, 16-byte aligned; the runtime's trampoline calls into Go from there
and calls `ThreadExit` when the thread function returns, so a thread never
runs `_start` and never sets up the heap again.

### Thread-local state

The runtime finds the current thread from the stack pointer: above
`ThreadAreaTop` it is the main thread, below it the slot is
`(ThreadAreaTop - 1 - SP) / 576 KiB + 1`. That costs a subtraction and a
division by a constant, needs no system call, no FS base, and no change to
TinyGo's code generation, and holds because a task's stack pointer never
leaves its stack under `scheduler: none`. Thread descriptors are a
`[16]`-element array indexed by slot. A descriptor holds the thread's ID,
stack bounds, its system-call scratch, its region cursor and depth, its
allocator caches, and its fast-path flag. The FS base stays unused: TinyGo on
this target emits no FS-relative access, and a process may keep it for
anything else.

### Thread lifecycle and linearization points

| Event | Linearization point | Lock |
|---|---|---|
| Creation reserved | task charge and slot claimed in the thread group | registry write |
| Thread published | `StartTask` after regions, task, owner, and group record exist | registry write |
| GC epoch begins | `ThreadSuspend` has held and quiesced every task the group lists | registry write |
| Thread exit | the scheduler publishes the task as exited | scheduler |
| Exit observed | `deliverTaskExit` marks the record exited and wakes the joiner | exit locks |
| Join claimed | `ThreadJoin` reads an exited record and frees it | registry write |
| Stack reclaimed | `deliverTaskExit`, after the task is off every processor | exit locks |

Creation fails before `StartTask` at every step and undoes what it did:
regions are removed, the task is abandoned, the charge and slot returned.
Since creation and `ThreadSuspend` both take the registry for writing, a
thread is either listed when an epoch begins or created after it ends; the
only thread that runs during an epoch is the one that began it, and it does
not create threads while it collects.

One joiner per thread. A second `ThreadJoin` on a thread with a joiner
answers `AlreadyExists`; a join of a thread already joined, or of an ID that
is not a thread of the caller's process, answers `NotFound`. A thread cannot
join itself.

### Stop-the-world collection

The collector stays per process, nonmoving, conservative, and
stop-the-world. With one thread it runs as before. With several:

1. The collecting thread takes the GC lock, the only owner of an epoch.
   Another thread that wants a collection while one runs waits for the lock
   and then returns when an epoch completed in the meantime.
2. It takes the heap lock and raises the GC flag. From here no thread starts
   a heap operation: the slow path takes the heap lock, and the fast path
   checks the flag after announcing itself.
3. It calls `ThreadSuspend`. The kernel lists the process's other tasks,
   holds each, kicks the processors that run them, and waits until each is
   off its processor. A task running user code is interrupted where it is; a
   task in a call leaves at the call's end or parks; a task waiting for a lock
   set gives the set up.
4. The kernel reads each held task's saved context. It accepts only a context
   whose CS is the user selector, and copies RSP, RIP, the fifteen
   general-purpose registers, and the sixteen XMM registers from the task's
   FPU area into the snapshot. A task whose context is anything else, or that
   does not leave its processor within the bound, fails the call: the kernel
   releases everything it held and the collection does not run.
5. The runtime checks every snapshot's fast-path flag. When a thread was
   stopped inside the allocation or free fast path, the runtime releases
   every held thread, yields, and suspends again: the thread finishes the one
   operation and then sees the GC flag. Four rounds that keep finding such a
   thread fail the collection.
6. The collector takes every thread's owned slabs back and folds their
   counts and the remote frees left in slabs no thread owns (Allocator).
   Marking then scans the globals, the collector's own stack and registers,
   each snapshot's registers and its stack from 128 bytes below its RSP (the
   red zone) to its stack top, and the used part of every region, up to the
   live cursor of a thread inside it. Sweep and trim run as before. An
   allocation from here to the end of the collection stops the process: it
   would wait for the heap lock the collector holds.
7. The runtime clears the snapshots, calls `ThreadResume`, lowers the GC flag,
   and releases the heap and GC locks.

A failed suspension leaves the heap untouched: the collector never sweeps on
incomplete roots. Allocation that cannot proceed without a collection then
reports exhaustion, and an explicit or idle collection is skipped and counted.

How each case the specification names is handled:

| Case | Why the epoch neither deadlocks nor loses a root |
|---|---|
| (a) preempted in the allocation fast path | Its flag is set in its descriptor; step 5 lets it finish before the epoch begins. |
| (b) waiting for the heap lock | It spins in user code with `ThreadYield`; it holds nothing and is held where it spins. |
| (c) blocked in `WaitAny` or IPC | The gate parked it with its user registers in the context; a pointer it passed is in those registers or on its stack. |
| (d) in a copy into user memory | The copy finishes before the task reaches a scheduling point; the kernel never leaves a task mid-copy. |
| (e) creating or exiting | Creation is one call under the registry lock that `ThreadSuspend` takes. An exited task is not alive: hold skips it and its roots no longer matter. |
| (f) a loop that makes no call | The reschedule interrupt the hold sends stops it at its next instruction. |

### Lock hierarchy

| Order | Lock | Kind | May be held across a system call | May be held while blocking | May be held into a collection |
|---|---|---|---|---|---|
| 1 | GC lock | user, one per process | yes, `ThreadSuspend` and `VMDiscard` | no | it is the collection |
| 2 | heap lock | user, one per process | yes, `VMDiscard` only | no | yes, by the collector |
| 3 | thread-table lock | user | no | no | no |
| 4 | kernel lock sets | kernel (ADR 0184) | inside one call only | given up when stopped | no |
| 5 | scheduler lock | kernel | inside one call only | no | no |

A thread takes them only in this order. The allocation slow path never
starts a collection while it holds the heap lock: it releases the lock,
collects, and starts over. A thread that the kernel holds holds no kernel
lock (step 3). The only user locks a held thread can own are the heap lock
and the thread-table lock: the collector owns the heap lock before it
suspends anyone, and the thread-table lock is never held across a heap
operation or a system call.

### Allocator

A process that never starts a thread takes no lock and runs the allocator of
ADR 0202 unchanged. Once a second thread exists, every heap operation either
takes the heap lock or runs on a slab the calling thread owns.

Each thread owns at most one slab per size class. A slab's page record names
its owner as slot plus one, read and written with atomics; ownership changes
only under the heap lock. The owner allocates from its slab and frees into it
without any lock. The fast path stores the thread's fast-path flag, reads the
GC flag, takes the slot, and clears the flag, all with sequentially
consistent atomics, so the collector, which raises the GC flag before it
suspends and reads the flags after, sees either the finished bitmap write or
the flag. When the slab is full the thread takes the heap lock, folds its
counts into the heap's, folds the slab's remote frees, and keeps the slab if
that freed a slot; otherwise it gives the full slab up and takes the first
listed slab of the class, or a new one.

The record's mark bitmap holds remote frees. A collection clears every mark
bit when it sweeps, so between collections the bitmap is free; there a thread
that frees a slot of a slab another thread owns sets the slot's bit with a
compare-and-swap. The bit already set is a double free. The owner folds the
bits when its slab runs out, and the collection folds them before it marks.
A page record grew from 120 to 128 bytes for the owner field; remote frees
take no memory.

A freeing thread reads the owner without the lock, so its bit can land after
the owner gave the slab up. The slab then keeps the bit until the next owner
or the next collection folds it. A lost or late bit is never unsafe: the slot
stays allocated, and a collection frees it once nothing points to it.

A collection takes every owned slab back: it folds remote frees, clears each
owner, lists or frees each slab, and folds each thread's counts, so no cache
outlives the epoch that might discard its pages. A thread's exit returns its
slabs and counts under the heap lock before `ThreadExit`. A thread holds at
most 14 pages it does not use, 56 KiB; eight threads hold at most 448 KiB.
Large runs stay under the heap lock.

A thread's counts are its own. It changes them only inside the fast-path
window or under the heap lock, and they reach the heap's counters only when
it folds them under the heap lock: at every refill, at its exit, and when a
collection holds it. Statistics and diagnostics read only folded counts, so
between collections they can miss a thread's allocations since its last
refill. A thread sets an atomic flag when it has counts to fold, and an idle
collection reads the flags to tell whether anything was allocated.

`arena.UseThreadCaches(false)`, called before the second thread starts,
turns owned slabs off, so the benchmark can compare them with the heap lock
alone.

### Diagnostics

`ALLOC_PROFILE` is at version 2: the first line opens with `version=2`, and a
`threads` line follows the size histogram with `live`, `peak`,
`local_allocs`, `local_frees`, `remote_frees`, `refills`, `drained`,
`returned`, `heap_locks`, `heap_contended`, `gc_locks`, `gc_contended`,
`suspensions`, `suspend_failures`, `rendezvous_last`, `rendezvous_max`,
`rendezvous_total`, `pause_last`, `pause_max`, and `pause_total`, times in
nanoseconds. `tools/allocreport` reads both versions: a report without the
field is version 1, and a version 2 report without every thread counter is
incomplete.

### Testing

`threadprobe`, built into validation images, runs every guest check:

| Target | What it runs |
|---|---|
| `test-native-threads` | `smoke` (threads of one process on several processors at once), `lifecycle` (IDs, joins, exit codes, the task quota, slot reuse), `fault` and `overflow` (a worker's fault or stack overflow into its guard ends the process, after which threads start again) |
| `test-thread-gc` | `gc` (threads share and drop lists across collections), `roots` (lists held only by a thread that loops without calls, one that sleeps in a call, and one inside a region), `storm` (every thread asks for collections at once) |
| `test-thread-allocator` | the host model and race tests of `cache.go`, then `bench` with the heap lock and with owned slabs, checking the profile's counters, and `gc` and `storm` over owned slabs |
| `bench-thread-allocator` | the benchmark only, on 1, 2, 4, and 8 threads, under TCG |
| `test-pve-threads` | the checks and the benchmark under KVM on eight processors |

The three test targets boot four processors and are part of `test-all` and
`release-check`.

### Measurements

`make test-pve-threads`, run `20261019T201556Z-51b3ff70` at `865a235` plus the
benchmark's retained mode: one VM with eight processors under KVM, `cpu=host`.
Every thread check passed; `smoke 7` saw eight threads of the process on
eight processors at once. Each thread allocates 200000 objects of 16 to 1000
bytes and keeps the last 64; the times are medians of five runs.

| Threads | Heap lock ms | Owned slabs ms | Speedup | Collections | Lock waits (heap lock) | Refills (owned slabs) | Longest pause us | Owned slabs, 256 pages retained, ms |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 31.0 | 31.9 | 0.97x | 49 | 0 | 0 | 164 | 37.6 |
| 2 | 222.5 | 100.9 | 2.21x | 98 | 37698 | 26725 | 122 | 110.0 |
| 4 | 611.3 | 361.3 | 1.69x | 196 | 308323 | 55319 | 159 | 263.9 |
| 8 | 1745.8 | 816.7 | 2.14x | 388 | 521799 | 118464 | 410 | 652.5 |

With one thread both modes run the single-thread allocator. From two
threads on, owned slabs halve the time of the heap lock alone. Total
throughput still falls as threads are added: every thread allocates from one
heap whose collection threshold the threads reach together, so collections
grow with the work, and each stops every thread. A refill takes the heap lock
once per 13 allocations on this mix, mostly for the large classes, whose
slabs hold 4 to 8 slots. Retaining 256 free pages cut the eight-thread time
by a fifth: part of the cost is pages discarded after each collection and
faulted in again. Under TCG (`make bench-thread-allocator`) the same run
shows a speedup of 1.25x at four and eight threads.

## Consequences

- Native programs may create up to 15 threads, within their task quota:
  4 tasks in the normal profile, 8 in the large one.
- `Exit` kills sibling threads, and a fault in any thread ends the process.
- Existing single-thread programs keep their behaviour: they never call the
  new operations, the heap lock is uncontended, and the collector skips
  suspension when the process has one thread.
- TinyGo goroutines, `go` statements, and `sync` stay unsupported. Threads
  are started with `thread.Start` and joined with `thread.Join`.
- A collection pauses every thread of the process. Suspension cost grows with
  the number of threads; the pauses are reported.
- Only `sys` keeps its scratch per thread. `utility` and the other packages
  with package-level buffers (`utility`'s message wire and path buffers,
  `pages`) are not safe from two threads at once: a threaded program prints
  and calls services from one thread.
