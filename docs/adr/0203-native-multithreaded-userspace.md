# ADR 0203: Native multithreaded userspace

## Status

Accepted as the design the implementation follows. Sections marked as
measured are filled in from the runs they name.

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
5. The runtime checks every snapshot's fast-path flag. A thread interrupted
   inside the allocation fast path is released alone, and the suspension is
   taken again once its flag clears; it finishes the one operation and then
   sees the GC flag. Three rounds that keep finding such a thread fail the
   collection.
6. Marking scans the globals, the collector's own stack and registers, each
   snapshot's registers and its stack from 128 bytes below its RSP (the red
   zone) to its stack top, the thread descriptors, and the used part of every
   region. Sweep and trim run as before.
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

The correctness step puts the heap behind the heap lock: every allocation,
free, collection, statistic, and diagnostic takes it.

The finished allocator gives each thread bounded slabs of its own. Each
thread descriptor holds, per size class, one owned slab. An owned slab's page
record names its owner slot; only the owner allocates from it, without the
heap lock, by setting a bit in the slab's bitmap. The owner announces the
fast path in its descriptor before it reads the GC flag and clears the
announcement after the bitmap write, both with sequentially consistent
atomics, so the collector, which raises the flag before it suspends and reads
the announcements after, sees either the finished write or the announcement.

A free of a slot in a slab another thread owns sets the slot's bit in the
slab's remote-free bitmap with an atomic OR. The owner folds that bitmap into
its allocation bitmap when its slab runs out, and every collection folds it
first. The bitmap is the queue: it is bounded by the slab, needs no memory,
and a second free of one slot finds its bit already set, which is a double
free. Large runs stay under the heap lock.

A collection takes every owned slab back: it folds remote frees, sweeps,
clears each owner field, and empties each descriptor's cache, so no cache
outlives the epoch that might discard its pages. A thread's exit returns its
slabs under the heap lock before `ThreadExit`. With one owned slab per class
and thread, a thread holds at most 14 pages it does not use, 56 KiB; eight
threads hold at most 448 KiB, which an idle collection gives back with the
rest. Statistics count per thread and are summed under the heap lock.

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
