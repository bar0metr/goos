# ADR 0204: Native goroutines and library concurrency

## Status

Experimental implementation on `feature/native-userspace-concurrency`, based on
`d0c8d5f`. This branch does not meet the feature's acceptance criteria yet.
Do not enable this runtime in production services until the collector, native
I/O adapters, failure tests, and KVM validation pass together.

## Toolchain findings

The build uses TinyGo 0.42.0 with LLVM 22.1.4 and Go 1.25.5. The native
userspace target previously selected `scheduler: none` and `gc: custom`.
The kernel keeps that scheduler selection and its separate target.

TinyGo provides a parallel `cores` scheduler, but its amd64 task backend only
builds for `scheduler.tasks`. Selecting `cores` for an unmodified native target
fails on missing `calleeSavedRegs`, `state.archInit`, `state.pause`, and
`state.resume`. The existing cores scheduler also uses hardware CPU identity,
one global run queue, platform interrupt hooks, and its own GC rendezvous.
Those assumptions do not describe native threads scheduled by GoOS.

The compiler lowers a go statement through `internal/task.start(fn, args,
stackSize)`. It evaluates the arguments before creating the task and passes a
compiler-generated wrapper and argument bundle. Channels use runtime
`chanMake`, `chanSend`, `chanRecv`, `chanClose`, and `chanSelect`.
`internal/task.Task` holds wait data and a defer-frame pointer. The native
assembly supplies TinyGo's amd64 `tinygo_longjmp`: saved SP followed by the
continuation PC. A goroutine must keep its own Task across migration so
recover finds that goroutine's defer chain.

`tools/goroutineroot` checks source SHA-256 pins and constructs a separate
TinyGo root. It replaces the scheduling and task transport files, retains
TinyGo's channel/select and sync implementations, and patches the timer,
GOMAXPROCS, and task-count adapters. It does not write the installed toolchain.
The output metadata lists input and output hashes. The goroutine target uses
`goos_goroutines`; existing native programs retain their old target.

A compiler probe exposed a recovery bug in the pinned channel implementation:
`trySend` panics on a closed channel while holding its lock. The overlay adds
an unlocked panic path for a direct send. Recovery from a closed send inside
select still needs a separate implementation and test before acceptance.

## Thread identity and stacks

The runtime entry initializes an FS-based control block before calling Go.
The task switcher already saves and restores user FS base. Native
`ThreadSetBase` changes only the calling task's base. Linux TLS remains on its
existing ABI. The TCB records its own address, native slot, magic, M pointer,
pin depth, current G pointer, task ID, and TinyGo Task pointer, in that order,
as eight 64-bit words. `tls` tests pin these offsets.

`task.Current` reads the Task pointer with one FS-relative instruction. Static
tasks cover early main startup, scheduler stacks, and `thread.Start` entry.
This avoids recursively starting the scheduler while a syscall wrapper builds
a defer frame. Existing stack-relative `tls.SlotOf` remains only a layout
helper; `tls.Slot` no longer derives identity from RSP.

Each G has a fixed 256 KiB stack and a 64 KiB guard below it. Scheduler stacks
reserve 64 KiB. The allocator charges physical pages on demand, as for other
anonymous mappings. The first implementation allocated two VM regions per G;
100 sleeping Gs exhausted the process's 128-entry region table. Stack batches
will instead use one anonymous region with a fixed guard/stack stride. Native
VM mapping validates that format; demand faults reject guard offsets before
allocating a page. Partial unmaps and protection changes must not split a
batch or remove its guards. The runtime discards a finished stack's physical
pages and reuses its slot. Batch reservations count in runtime diagnostics.

`goos_g_swap` preserves the six System V callee-saved integer registers and
switches stacks. A new G starts through a wrapper in `amd64.S`. The
`stackframes` tool checks the reviewed switch instruction sequence rather
than accepting arbitrary dynamic SP changes.

## Scheduling and preemption

A G runs, waits, becomes runnable, or completes. Each M has a bounded local
queue; idle workers steal work, and overflow goes through a global queue.
Execution permits bound the workers running Gs according to GOMAXPROCS. The
main thread participates as a worker. Task-quota refusal reduces the available
workers; it must not discard runnable Gs. A fixed descriptor table bounds Gs;
creation past the configured limit must panic in the creator.

The experimental preemption path registers a native user entry. On a timer
tick the kernel changes only the calling task's saved user RIP and RSP. The
entry preserves the interrupted flags, integer registers, FPU and XMM state,
and the red zone before consulting the scheduler. A pin prevents switching
while syscall scratch, allocator state, or runtime locks belong to an M.

This path requires further review: the complete entry/exit nesting protocol,
register-root coverage, pin release boundaries, migration, and guard faults
must pass targeted tests and KVM before it provides the required tight-loop
fairness guarantee. TCG smoke overlap alone does not prove that guarantee.

## Waiting and timers

Channel and sync waits park Gs through the task adapter. The GoOS timer heap
holds sleep and native wait deadlines. TinyGo retains its time.Timer list,
which the scheduler services on scheduler stacks.

Native channel creation requires PID 1, so scheduler workers cannot allocate
IPC channels to wake themselves. `ThreadWake` sets a generation-safe flag for
a thread in the caller's native process. A `WaitAnyThread` pseudo-interest
observes only the calling thread's flag. `ThreadWakeTake` consumes it. These
operations confer no handle or capability authority.

An idle worker may become the event poller and wait for registered handles,
its wake flag, and the next deadline. One-task processes must use that same
worker rather than reserve a helper task. Legacy native calls remain
M-blocking unless a scheduler-aware adapter intercepts them. Wiring these
adapters into the service libraries remains unfinished.

## Collector integration

The collector remains conservative, nonmoving, and stop-the-world. Kernel
snapshots still identify native tasks. A runtime stack plan maps each suspended
RSP to a registered M or G stack, adds saved parked G stacks and dormant M
stacks, and refuses an incomplete plan before marking or sweeping. The plan
must not acquire locks a suspended thread could own or allocate from the heap.
Thread-owned slabs remain M-owned; migration changes which cache a G uses.

The current hooks are experimental. Tests must still cover transitions during
creation and completion, pending channel values, saved register roots, timer
callbacks, mixed native threads, and failed rendezvous. A passing channel or
mutex test does not establish collector correctness.

## Service integration and acceptance

The current VFS ingress loop owns namespace mutations, open-file cursors,
filesystem transport scratch, and reply correlation. Its Dispatch method uses
shared read scratch; persistent reads share one backend exchange and cache.
Workers cannot call that method concurrently without separating those owners.
The read-side pool therefore remains disabled. Its design must isolate useful
read-only work, retain an ordering barrier for mutations and control requests,
and preserve per-request authority and reply identity.

The final branch must include the userspace API audit, compiler evidence,
complete runtime and collector tests, scheduler-aware I/O, the real service
pool, and controlled KVM measurements. No version change, release note, or
merge belongs to this implementation.
