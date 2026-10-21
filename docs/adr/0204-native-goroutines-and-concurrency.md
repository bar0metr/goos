# ADR 0204: Native goroutines and library concurrency

## Status

Experimental implementation on `feature/native-userspace-concurrency`, based on
`d0c8d5f`. This branch does not meet the feature's acceptance criteria yet.
Do not enable this runtime in production services until the collector, native
I/O adapters, failure tests, and KVM validation pass together.

## Decision

Use a pinned TinyGo overlay for native M:N goroutines while preserving the
kernel's scheduler and custom allocator. Keep G stacks distinct from native
M stacks, identify each M through FS, and integrate G roots into the existing
stop-the-world collection. Runtime pins protect M-owned state; scheduler-aware
wait adapters release a G's execution permit while it waits. The VFS owner
orders namespace and control operations and sends bounded read plans to
workers. Production acceptance requires joint native failure, GC and KVM
evidence; the experimental branch status remains unchanged until then.

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
TinyGo root. It replaces the scheduling and task transport files and the
sync mutex implementation, patches channel panic cleanup, and connects timers,
GOMAXPROCS, and task counts to GoOS. It does not write the installed toolchain.
The output metadata lists input and output hashes. The goroutine target uses
`goos_goroutines`; existing native programs retain their old target.

A compiler probe exposed a recovery bug in the pinned channel implementation:
`trySend` panics on a closed channel while holding its lock. The overlay adds
unlocked panic paths for direct sends and immediate select sends. A select
whose send wakes after close panics after removing its other queued operations
and releasing the locks. The native fixture tests recovery followed by another
select, including a sender observed parked before close.

The pinned RWMutex also counted queued readers as active readers. A writer
could wait forever after the last active reader left. The replacement tracks
active and queued readers separately and wakes a waiting writer only when the
active count reaches zero.

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
use one anonymous region with a fixed guard/stack stride. Native VM mapping
validates that format; demand faults reject guard offsets before allocating a
page. The kernel rejects partial unmaps and protection changes that could split
a batch or remove its guards. The runtime discards a finished stack's physical
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

Channel and sync waits park Gs through the task adapter. A running or runnable
G can hold one pending wake credit. Preemption and dispatch preserve it; the
next park consumes it. Without that credit, preemption between waiter
publication and Pause could lose a wake. Host tests cover wake before
preemption, while runnable, after dispatch, and after park, plus concurrent
CAS transitions and a second park that must block.

The GoOS timer heap holds sleep and native wait deadlines. TinyGo retains its
time.Timer list, which the scheduler services on scheduler stacks.

Native channel creation requires PID 1, so scheduler workers cannot allocate
IPC channels to wake themselves. `ThreadWake` sets a generation-safe flag for
a thread in the caller's native process. A `WaitAnyThread` pseudo-interest
observes only the calling thread's flag. `ThreadWakeTake` consumes it. These
operations confer no handle or capability authority.

An idle worker may become the event poller and wait for registered handles,
its wake flag, and the next deadline. One-task processes must use that same
worker rather than reserve a helper task. The syscall layer exposes runtime
hooks before acquiring M-local scratch. Ordinary WaitAny and IPCWaitReceive
can park the calling G through these hooks; IPC receive retries share one
absolute deadline. Zero-time probes and scheduler-stack waits use the kernel.
Regions, 64-interest waits, and WaitAnyThread retain native M-blocking behavior.
The G poller bounds registered interests to 63, reserving the remaining kernel
interest for its own thread wake. A G with an additional runtime pin cannot
park through these adapters.

ProcessWait probes the process, then parks on its completion readiness and
probes again for the exit code. It keeps the native call's rights check,
errors and finite timeout. StreamWait keeps its native contract, which needs
only the Wait right and reports a closed peer as PeerDied. StreamWaitRead and
StreamWaitWrite wait through WaitAny instead: they need the Read or Write
right as well, and report peer closure as readiness, so the next read or
write returns PeerDied. Utility's standard streams use them. ThreadJoin,
cursor-log waits and IRQWait still block their native thread: the kernel
records a joiner by task, a cursor wait needs a cursor in the registration,
and the disk interrupt line has no readiness that WaitAny could observe. The
complete service-library wait audit remains unfinished.

## Collector integration

The collector remains conservative, nonmoving, and stop-the-world. Kernel
snapshots still identify native tasks. A runtime stack plan maps each suspended
RSP to a registered M or G stack, adds saved parked G stacks and dormant M
stacks, and refuses an incomplete plan before marking or sweeping. The plan
must not acquire locks a suspended thread could own or allocate from the heap.
Thread-owned slabs remain M-owned; migration changes which cache a G uses.
Region Enter and Leave hold both a migration pin and a separate collector
window until cursor, innermost identity and ownership agree. A pin alone did
not protect against a kernel hold between those stores. The collector resumes
threads caught in either an allocator or a region-transition window and
retries within its rendezvous bound. Kernel snapshots include XMM words;
conservative scanning must retain that coverage because LLVM can keep pointer
aggregates only in vector registers during a copy.

The current hooks are experimental. Tests must still cover transitions during
creation and completion, pending channel values, saved register roots, timer
callbacks, mixed native threads, and failed rendezvous. A passing channel or
mutex test does not establish collector correctness.

## Service integration and acceptance

The VFS build now selects the goroutine target. Its ingress owner admits at
most eight requests to two read workers. It plans canonical reads on the owner,
executes RAMFS/image backend work without a process-wide lock, commits cursors
on completion, and retires each request after reply and mapping cleanup.
Mutations, session close, namespace operations and control requests pass through
ordered barriers. Each session has at most one admitted request; generations
prevent delivery to a reused owner slot. A stream bridges Go-channel completion
hints into the owner's native readiness wait. The goroutine driver caps sessions
at 60 to leave room for control, completion, the persistent lane and the runtime
poller's own wake interest.

Persistent reads serialize the whole Remote operation, including cache use.
Workers publish transport failure; the owner detaches at a drained boundary.
An unanswered accepted exchange quarantines the shared cache. The VFS refuses
reattach and executable-slot writes against that cache; local reads continue.
Automatic recovery needs a supervisor fence proving the old writer has exited
or a coordinated fresh cache generation. Closing an endpoint alone is not that
fence. This recovery protocol and native service acceptance remain unfinished.

Host tests demonstrate actual RAMFS read overlap and check ordering, retirement
and the serial batch's quarantine fence. The service compiles with the native
goroutine target and passes frame checks. Joint VM and KVM validation must
still exercise the driver, runtime and hardened clients together.

The final branch must include the userspace API audit, compiler evidence,
complete runtime and collector tests, scheduler-aware I/O, the real service
pool, and controlled KVM measurements. No version change, release note, or
merge belongs to this implementation.
