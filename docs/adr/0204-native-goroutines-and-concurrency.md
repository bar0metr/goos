# ADR 0204: Native goroutines and library concurrency

## Status

Accepted. The goroutine runtime ships in production images, where it runs
the VFS service. The collector, native I/O adapters, failure tests and KVM
validation passed together; the
[validation report](../development/native-concurrency-validation.md) records
the evidence and its limits. The serial VFS remains available with
`VFS_RUNTIME=serial`, and `test-vfs-serial` keeps it under every boot check.

## Decision

Use a pinned TinyGo overlay for native M:N goroutines while preserving the
kernel's scheduler and custom allocator. Keep G stacks distinct from native
M stacks, identify each M through FS, and integrate G roots into the existing
stop-the-world collection. Runtime pins protect M-owned state; scheduler-aware
wait adapters release a G's execution permit while it waits. The VFS owner
orders namespace and control operations and sends bounded read plans to
workers. Production acceptance required joint native failure, GC and KVM
evidence, recorded under Service integration and acceptance.

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

The preemption path registers a native user entry. On a timer
tick the kernel changes only the calling task's saved user RIP and RSP. The
entry preserves the interrupted flags, integer registers, FPU and XMM state,
and the red zone before its kernel call and before consulting the scheduler. A pin prevents switching
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
write returns PeerDied. Utility's standard streams use them.

ThreadJoin claims a kernel reservation, polls with its token, parks on a
WaitAny interest in the thread's end and polls again for the code. The
interest's handle is the thread's ID with kcall.WaitAnyThreadEnd set; the
kernel publishes it from the thread's teardown. The reservation belongs to
the target's generation and a process-owned token, not the joining M, so a
G can migrate without changing its right to collect. Legacy native joins,
native polls and other token claims return AlreadyExists until the owner
collects or releases, including after the target ends. The adapter defers
release across wait failures and goroutine unwinding. Collection consumes
the reservation; process teardown clears any remaining claims. The kernel
never reuses a token within the process and returns ResourceExhausted if its
64-bit token sequence runs out.

WaitAnyCursor parks through its own hook. The poller always waits in the
cursor format, so each registration keeps its log cursor and every other
interest carries the zero cursor the kernel requires.

IRQWait first probes with IRQPoll, preserving the line-11-only capability
check and consuming an already pending event. IRQPoll uses a separate,
append-only opcode so older IRQWait callers can keep arbitrary values in
unused argument registers. It
then parks on the line's readiness and takes the event with IRQTake,
waiting again when another taker got it first. The disk's line 11 is visible
to WaitAny: with no IRQWait waiter its interrupt wakes the kernel loop, which
publishes the pending event. Its interrupt still wakes a native IRQWait
waiter directly. WaitAny observes the pending flag without consuming it;
IRQWait and IRQTake race to consume the event. A zero-time WaitAny unmasks the lines it
names, so the polls of busy threads do not hold a parked goroutine's
interrupt back until a thread idles. Both native and goroutine IRQWait refuse the network line, which remains
available through WaitAny and IRQTake.

Two goroutines that wait on the same log or line form an aliased aggregate,
which the poller resolves with one-tick probes rather than one kernel wait.
The [caller audit](../development/native-concurrency-callers.md) covers the
service-library ownership boundaries.

Two guest fixtures cover these waits on one permit. In validation builds PID 1
grants the system goprobe the log with Read and Write, by name, so the command
index format stays unchanged. `goprobe logwait` parks two Gs on the same log
cursor, writes a record once both have parked, and requires both to read it
while a peer G counted. Validation builds run the virtio-blk driver on the
goroutine runtime with GOMAXPROCS 1. Its first four interrupt waits that a
peer G sees registered, and during which the peer advances, print
`VIRTIO_BLK_G_IRQ_OK`. The driver keeps the disk line; no other process
receives it. Normal builds keep the serial driver.

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

The snapshot words reach the mark twice: `markThreads` scans each record's
registers and vectors, and `markGlobals` scans the snapshot buffer, a
global. A mutation that shortens only the `markThreads` range therefore
changes nothing. `goprobe regroots` checks each register root alone. Two
validation helpers, `goos_probe_xmm_root` and `goos_probe_gpr_root`, put a
list's address in one register, XMM0 to XMM15 or one of the fifteen
general-purpose registers, zero every other register copy and spin without
calls or stack writes. Their callers keep only an inverted token. After the
stop the spinner checks the list while the returned address is still live.
Each register runs as a `thread.Start` thread, which the collector finds in
its snapshot, and as a goroutine. The goroutine runs first on every permit,
where collections hold it in user code and, for every register in each TCG
run, inside `goos_preempt_entry`. It then runs on one permit, where every
collection finds it switched out with the address only in the frame the
entry saved on its stack. Control runs that hold the address in no register
must see the list freed. A temporary mutation that zeroed the vector words of
every snapshot before marking lost all 16 XMM lists of the thread and running
cases and none of the switched-out ones; zeroing the general-purpose words
lost all 15 lists of those cases in the same way. The validation report gives
the counts.

`threadprobe region-window N ROUNDS` now exercises the native region window.
A validation-only hook holds one thread after the cursor store and before the
innermost store in nested Enter and Leave. The main thread or a peer worker
requests collections while that thread waits. Each request must record a
region-window rejection, exhaust the bounded rendezvous, and leave the
collection and accepted-suspension counts unchanged. After the worker closes
the window, the next collection must finish. The worker checks heap lists
reachable only through its stack and outer-region holders, checks inner-region
lists after Leave, then clears the inner region. The collector must reclaim
those lists while the outer lists survive. The builder hands off the lists
through atomic pointers. The inner checker returns before the reclamation
collection so LLVM's dead head spills no longer sit in a live frame. Workers
acknowledge each round before the main thread changes their roles. The boot
script runs 16 rounds
with one worker and 24 with four under validation; CPU samples check that a
peer collector observes the held worker on another processor in the multiworker
run. The hook adds no calls to normal-build Enter or Leave. Separate
allocator-window and region-window counters identify each rejection reason.
TCG with four processors passed both commands: 160 collection requests gave
up after 640 region-window rejections, then 120 collections completed with the
window closed. The workers checked 1,600 lists and reclaimed 896,000 bytes
after clearing the inner regions. The four-worker run included 15 peer
collector rounds and 26 samples of the held worker running on another CPU.
The reclamation assertion allows conservative retention and requires at least
8 KiB back per round; this run reclaimed 22,400 bytes per round. This covers
native arena transitions, not suspended goroutine stack plans. The combined
KVM run on eight processors also passed both probes: 160 requests gave up,
120 collections completed, and the four-worker probe recorded 30 cross-CPU
samples. The [validation report](../development/native-concurrency-validation.md)
records the tested revision, runtime and VFS recovery checks, measurements
and remaining evidence limits.

The first KVM run on eight processors stopped in `goprobe threads`: a
processor whose last dispatched call was ThreadSuspend spun on the process
registry for longer than the four-second lock watchdog, and the report named
no writer as holder. The report could not name the reader. Review found one
defect that fits: a waiting registry writer marks the lock pending, which
keeps new readers out, and a writer stopped by ThreadSuspend gave up without
clearing that mark. The runtime's thread calls write the registry, so a
worker can be that writer. Every later reader then waited for a writer that
no longer existed. A writer that gives up now clears its mark. Validation
stuck-lock reports now list each processor's task slot, last call and held
lock levels, and every native call records its number, including the wait
paths entered from assembly. The next KVM run passed every check. One pass
does not prove the race gone; TCG had not exposed it at all.

`goprobe gcstress N ROUNDS` checks lists during concurrent collections from
N goroutines. The boot GC suite runs N=1 and N=4; pvelab runs N=1 and N=8
through both goroutines and native-concurrency. ROUNDS defaults to eight.
Each scenario requires completed collections and checks list values before
following links:

- Preemption: oversubscribed workers walk lists in a call-free loop or keep
  lists across Gosched. The checked TinyGo output keeps the current cell in
  RDI. The fixture waits for a preemption, with a 30-second bound.
- Creation and completion: creators pass lists through go arguments;
  children check them and send new lists through result channels.
- Defers: workers return, panic/recover, or call Goexit with captured lists.
  Deferred calls collect during return and panic unwinding. A panic also
  carries a heap list through its value.
- Channels: full buffers, blocked buffered and unbuffered senders, and select
  senders hold lists. The fixture waits for blocked senders before an
  explicit collection.
- Timers: pending AfterFunc closures hold lists, then collect and check them
  in callbacks. The fixture also stops far-future timers.
- Stack transitions: workers hold lists across sleep, handle timeout waits,
  Gosched, and native yields. Native thread.Start workers exchange lists
  with goroutines and collect while holding received lists.
- Failed rendezvous: channel and select waiters keep lists while a native
  thread stops inside the validation region-transition hold. Five requests
  must give up without advancing the collection count. After release, a
  collection must finish before the waiters resume and check their lists.

Validation builds poison swept slab slots with 0xa5 bytes and the first
page of swept runs. Host tests check corruption of dead slots, survival of
live slots, zeroing on reuse, and the poisoned-byte count. Other images
compile out the poison calls. A normal `mem` ELF built before and after the
poison change compared byte-for-byte equal. Each stress run samples dropped
lists through inverted address tokens and requires at least half to change,
plus a bounded final live heap. A collector that reclaims nothing fails.

Validation-only snapshot classification counts accepted collections that
hold a thread inside goos_g_swap or goos_preempt_entry. The targeted TCG
SMP=4 suite recorded 12 and 24 preemption-entry holds for N=1 and N=4,
with zero swap holds. Both runs checked eight scenarios without a lost
list and reclaimed all 256 sampled list heads. Six final repeat runs passed
in 51 seconds, with 1 to 6 preemptions, 10 to 34 preemption-entry holds,
and 0 to 4 swap holds per run. All 224 to 256 sampled list heads changed.
The fixture waits for a preemption rather than assuming a fixed wall time
guarantees a user-time preemption. Temporary mutations that omitted parked or runnable
G stacks made gcstress fail during preemption with process fault status 13;
the mutations are not part of the runtime.

### Instruction holds

`goprobe swaphold` forces a collection at every instruction boundary of
`goos_g_swap` and `goos_preempt_entry`. Validation kernels add four native
calls, appended as operations 72 to 75: `ThreadHoldRange`,
`ThreadHoldWait`, `ThreadHoldStep` and `ThreadHoldCancel`. Other kernels
answer `Unsupported`, and the shared `userspace/runtime.o` gains only four
call stubs; the switch and the entry stay unchanged.

A hold arms an execute breakpoint in DR0 for one thread of the caller's
process at the range's first instruction. The debug exception entry saves
the thread's registers and IRETQ frame as the context a switch restores,
the same context a collection snapshots, and blocks the thread. A step sets
RFLAGS.TF. INT, INT3, INT1, SYSCALL, PUSHF and POPF instead run to a
breakpoint on the next instruction, so the kernel never sees a trap in its
own code and TF never reaches the saved flags. A step that lands outside the
range with a return address just past the stepped instruction at RSP runs to
a breakpoint there: `call goos_g_preempt` is stepped over. Any other exit
ends the hold. While a thread stands, DR0 holds its RIP and TF is clear, so
a stale wake runs it back into the same stop. A cancel while the thread runs
resumes it only from the debug exception the old hold can still raise: a
DR0 hit at the armed address, or the trap of a step in flight. Any other
debug exception ends the thread as before. A processor loads DR0 and DR7
only when it switches to a held task and clears DR7 otherwise. Preemption
delivery to a held thread stops after its first stop, and delivery clears
RF in the diverted context; QEMU TCG left RF set there, which hid the
breakpoint at the entry.

One permit runs the probe's goroutines, so every switch of the region runs
on the main thread; a `thread.Start` controller arms the hold and calls
`runtime.GC()` at each stop. In the swap region two goroutines yield in a
loop holding a list each. Passes run until both directions stopped at all 15
boundaries: the outgoing goroutine's saves and the restore of the goroutine
switched to. In the preemption region one goroutine spins without calls on a
list held only in registers. Each pass covers all 47 reachable boundaries
of the entry, through the kernel call, the stepped-over runtime call and the
restore to `RET 128`. The UD2 after a failed take is never reached. Only a
stop whose collection completed covers its boundary. Every boundary, and for
the switch every boundary in each direction, must be covered, and each list
must survive. `tools/swaphold` compares the covered offsets with the
boundaries `go tool objdump` lists for the ELF; the KVM check counts them
from the same offset lists.

A TCG SMP=4 run held 45 swap stops, covering 15 of 15 distinct RIPs in two
outgoing and one incoming pass, and 94 preemption stops, covering 47 of 47
in two passes. A joint eight-vCPU KVM run matched it with 30 swap stops. All 139 collections completed at their stops, with no retry,
suspension failure or lost list. Two temporary stack-plan mutations failed
the swap region. One counted the goroutine a held thread's M runs as
scanned whenever the thread's stack pointer lay on the scheduler stack; it
lost both lists. The other dropped the stack of every held thread whose
stack pointer lay on a goroutine stack; it lost one. A third mutation
cleared the held threads' snapshot registers before marking, and both
regions still passed: another root kept each list, so the probe does not
isolate register roots; `goprobe regroots`, described under collector
integration, isolates each XMM and general-purpose register root.

These tests cover root lifetimes, every context-save instruction of one
thread and each register root alone. They do not force collections at each
instruction of creation, completion or the scheduler's own G/M bookkeeping
around the switch. A hold stops one thread at a time on one permit; it does
not combine a held switch with other threads switching at once. Compiler
spills and conservative aliases can retain a list through another root. The
tests do not cover all timer cancellation races or exercise kernel-induced
rendezvous timeouts. The rejected region window
covers bounded rendezvous failure with parked Gs. A later eight-vCPU KVM
run passed this stress fixture at one and eight collectors after the fixture
reserved task slots for its native threads and required three completed
final collections. An earlier KVM retry reported an impossible `Live` count;
the [validation report](../development/native-concurrency-validation.md)
records both results. A passing channel or mutex test alone does not
establish collector correctness.

## Service integration and acceptance

`VFS_RUNTIME` selects the VFS build. Normal and validation builds default to
`goroutines`, the read pool below. `serial` builds the VFS with the ordinary
native target, which compiles `driver_serial.go`: every request runs on one
thread with the same barriers and recovery fence. `make test-vfs-serial` runs
every boot check against that build, and `test-all` and `release-check` run it
beside the goroutine suites. A non-default runtime builds into its own variant
tree. The disk driver keeps the serial target in normal builds; validation
builds run it on goroutines to prove interrupt waits.

The goroutine VFS ingress owner admits at most eight requests to two read
workers. It plans canonical reads on the owner,
executes RAMFS/image backend work without a process-wide lock, commits cursors
on completion, and retires each request after reply and mapping cleanup.
Mutations, session close, namespace operations and control requests pass through
ordered barriers. Each session has at most one admitted request; generations
prevent delivery to a reused owner slot. A stream bridges Go-channel completion
hints into the owner's native readiness wait. The goroutine driver caps sessions
at 59 to leave room for control, recovery, completion, the persistent lane and
the runtime poller's own wake interest.

Persistent reads serialize the whole Remote operation, including cache use.
Workers publish transport failure; the owner detaches at a drained boundary.
An unanswered accepted exchange quarantines the shared cache. The VFS detaches
the filesystem service, refuses executable-slot writes against that cache and
sends PID 1 a notification naming the attachment generation over an independent
recovery endpoint; local reads continue. An ordinary control may already have
timed out behind the persistent lane and quarantined its endpoint. PID 1 uses
its existing authority to create a fresh control channel and sends the server
end over the recovery endpoint. At a drained barrier the VFS releases handles
from unread old controls, closes the old endpoint and acknowledges the handoff.
PID 1 closes its old client and uses the new ID only after that acknowledgement.
Neither side replays ambiguous old controls. Earlier admitted controls finish
before the handoff; AddSession clients abandoned by PID 1 close, so the VFS
also reclaims their orphaned sessions. A filesystem restart renews a quarantined
control endpoint before attempting SetFilesystem even if its notice is late. Closing an endpoint does not stop a stalled service that still
maps the cache, so PID 1 kills that instance and waits for the kernel to report
its exit. The kernel reports it after the last task of the process has ended.
The supervisor restarts the service only after that exit, and PID 1 then sends
SetFilesystem with a new generation and a fence flag stating that every earlier
instance has exited. While the cache is quarantined the VFS accepts only a
fenced attach; open files are reacquired as after any restart, and the
page index starts empty, so nothing the old writer left in the cache is served.
A notice for a superseded generation is ignored. PID 1 makes at most
`supervisor.MaxRestarts` such replacements per boot, because each follows a
six-second request deadline and the supervisor's restart window alone would
not stop a service that stalls every time. The VFS also bounds fenced recovery
admissions by the same limit: a service may exit before PID 1 handles its
notice, and that ordering must not bypass the bound. Past the bound the cache stays quarantined until the VFS itself
is replaced.

The cache object is not replaced. The kernel copies executable pages from the
last slot of the boot cache object, so a fresh cache generation would need the
kernel to follow it. The exit fence makes reuse safe without one.

Host tests demonstrate actual RAMFS read overlap and check ordering, retirement
and the serial batch's quarantine fence. `make test-vfs-goroutine-acceptance`
builds a separate validation image whose PID 1 drives the real VFS through six
sessions before the first prompt. It checks that a RAMFS and an image read
occupy both workers at once, that local reads complete while a held persistent
read runs, that a write and a sync wait behind that read, that a client which
dies with a ReadInto pending leaves no mapping, and that 4,096 further reads
leave the VFS heap on a plateau. The VFS reports its events to PID 1 as
control notifications, since the kernel accepts a reply only for a pending
request. Ordinary validation images leave the fixture out. `make
test-pve-goroutines VALIDATION=1 VFS_PROBE=1` boots that image under KVM on
eight processors; the run on 2026-10-21 passed the fixture and every goprobe
check, with an eight-thread compute speedup of 2.9 to 4.4 over one thread.

Building the fixture found a separate gap. The standard native target keeps
TinyGo's UEFI `putchar`, which dereferences the absent firmware table, so a
runtime panic in such a program recurses through its own panic report until
the stack guard stops the process. The process still dies, but without the
panic text. Validation kernels now log the code addresses found just above a
stack guard fault, which names a recursion's functions.

The goroutine target had a second panic-reporting recursion: its `putchar`
decoded the GST4 startup object on every byte, allocating inside an
allocator panic. It now reads the mapped startup handle table and writes to
stderr without a Go allocation. This exposed `arena: slab count does not
match its bitmap` during eight-collector KVM stress. A subsequent TCG boot
again faulted in VFS `ramfs.MakeName` with a null source register. Both
failures had one cause. `goos_preempt_entry` made its kernel call before it
saved the SSE state, and the native call path keeps only the general-purpose
registers, so a preemption could change the interrupted code's XMM
registers. TinyGo built the `MakeName` slice header from XMM0, and the
collector's SSE2 popcount kept its masks in XMM3 to XMM6 across the sweep.
The entry now saves the SSE state before the call.

After the fix, ten consecutive joint KVM runs on eight vCPUs, 20 TCG panic
boots and 20 TCG `gcstress 8` runs passed with no lost vector state, slab
mismatch or null-source fault. A complete `test-all` then passed on `main`
at `39c7036`, including `test-boot` with the goroutine VFS and
`test-vfs-serial`. On that evidence the goroutine VFS became the production
default. The remaining evidence limits in the validation report still apply.
`goprobe swaphold` later covered every context-save instruction of one held
thread (Instruction holds above), and `goprobe regroots` isolated each XMM
and general-purpose register root.
