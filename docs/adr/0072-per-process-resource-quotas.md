# ADR 0072: Per-process resource quotas

## Status

Accepted. Extends ADR 0007 and ADR 0040.

## Context

Capability isolation answers whether a process may use a resource. It says
nothing about how much of one it may hold, and the system had no second answer.

Every kernel resource lived in a fixed global table — sixteen channels, sixteen
streams, sixty-four tasks, one physical page allocator — and a table size bounds
the system, not a process's share of it. The gap was reachable. `StreamCreate`
requires no capability, by design: a stream is a primitive any process may need.
A single utility calling it in a loop took all sixteen slots and held them until
it exited, and every pipeline the shell tried to build after that failed. One
ordinary process could stop composition for the whole system without holding any
authority it was not given.

Memory had the same shape with no accounting at all. Nothing recorded how many
pages a process's address space owned, so `ps` could not report it and nothing
could refuse a launch whose image did not fit a share. CPU was worse: the
scheduler counted global ticks and attributed none of them, so a process that
never yielded was indistinguishable from one that never ran.

This is the specification's point that capability isolation is not enough for
denial of service. The missing mechanism is accounting.

## Decision

**Each process has a typed resource account.** Channels, streams, tasks and
pages are charged to the process that causes them to exist and released when
they cease to. A quota is not a capability: it is not transferable, a process
cannot raise its own, and nothing it holds lets it raise another's.

**An ordinary process gets a share, not the table.** Four channels, four
streams, four tasks, 192 pages. The stream limit is the one that binds today,
and four leaves twelve for the pipelines PID 1 builds. The page limit covers an
address space, a loaded image, guarded stacks and a startup page, sized against
the largest image the system has, plus a DMA region and the leaf table an image
mapping needs.

**PID 1 gets the system share, and it is still a limit.** Sixteen channels,
sixteen streams, eight tasks, 512 pages. PID 1 creates the endpoints every other
process communicates through, so an equal slice would be wrong; it is refused
past its own share like anything else. `GrantSystemQuota` refuses every process
but PID 1 and is called once, at boot, so the larger share cannot be acquired
later by a process that manages to look like an administrator.

**A charge that would exceed the quota changes nothing.** The caller gets a
refusal and has no partial reservation to unwind. Charging precedes taking the
resource, so a refused request never reaches the global table at all.

**A release that exceeds what is held reports failure and clamps to zero.**
Over-release is a kernel accounting bug. The counter has to stay truthful rather
than wrap into a value that would silently grant an unbounded quota, and the
caller has to be told rather than have the mistake hidden.

**Channels and streams are charged to their creator until the object is
released.** Both endpoints can be transferred, so a stream outlives the handles
its creator held. The registries record the owner and report it on release, and
the charge comes back then — which may be after the creator has exited, so the
release tolerates an unknown process.

**Launch memory is counted from the allocator, not threaded through the
loader.** The difference in free pages across address-space creation and image
loading is exactly what that address space owns. It is charged before the task
exists, because a runnable task cannot be withdrawn, so an image that does not
fit the page quota fails to launch rather than running over its share. Teardown
releases the whole account once the address space is destroyed.

**CPU is sampled, not measured.** The timer charges a tick to the task it
interrupted, one counter per task slot, and a task's count folds into its
process when it exits. This is a sample of who was running rather than an exact
duration, which is what a tick-driven scheduler can honestly report.

**Accounting is visible.** `ProcessInfo` carries CPU ticks and both counter
arrays, `ps` gains MEM and CPU columns, and `quota` prints every process's held
and permitted share. None of it needs a capability: the record names no kernel
object and reveals only what a process list already shows.

## Alternatives

**Refusing `StreamCreate` without a capability.** It would close the specific
hole by making streams privileged. It also makes a primitive that any process
legitimately needs into something PID 1 must broker, which is the ambient-
authority shape the architecture avoids, and it does nothing for pages or CPU.

**Growing the global tables.** Sixteen streams is a small number, and raising it
makes the hole take longer to reach. It never closes it, because the bound the
attacker exhausts is whatever the number is.

**Charging by handle rather than by object.** Counting the handles a process
holds is simpler and needs no owner in the registries. It also means a process
can create a stream, transfer both endpoints away, and create another, without
its count ever rising — the table empties and the quota never notices.

**Accounting memory inside the loader.** Threading a page count through
`LoadNative` and `CreateUserAddressSpace` would be exact rather than inferred.
It touches every allocation path for a number the allocator already knows, and
the difference in free pages is exact anyway as long as the measurement brackets
the whole launch, which it does.

**Enforcing a CPU limit rather than accounting.** Refusing to schedule a process
past a budget needs a policy about what happens next — killed, throttled,
demoted — and a scheduler that can express it. Accounting is what the
specification asks for and what a round-robin scheduler can support honestly.

## Consequences

A process that creates streams until it is refused leaves slots for everyone
else. The validation build proves it: a scratch process with an ordinary quota
drives the same charge-then-create order `StreamCreate` uses and must be refused
while the registry still has slots, printing `QUOTA_OK`.

`ps` reports real memory. PID 1 holds 159 pages, the filesystem service 139, the
shell 81, a utility about 60. `quota` shows PID 1 holding exactly its seven boot
channels and each service an ordinary four-of-each share.

The task quota of four binds before `MaxTasksPerProcess` of thirty-two does.
That is intended: the table size is a structural bound and the quota is the
policy one. Every native process has one task today, so nothing is constrained
by it yet.

`ProcessInfo` records grew from 32 bytes to 80. The native ABI is not stable
until it is declared so, and the record's layout is documented with the constant.

**The accounting exposed a defect, since fixed by ADR 0075.** The shell is charged about
95% of the ticks in a session. `ConsoleRead` answers `StatusNotFound` when no
byte has arrived and the shell immediately retries, so it busy-polls COM1 for as
long as it waits for input. Nothing breaks today — round-robin still runs
everyone and there is nothing else to run — but it defeats the idle task, it
contradicts the move away from polling in ADR 0051, and it would matter on real
hardware and under SMP. ADR 0075 makes the console interrupt-driven and its
input a waitable object, after which the shell is charged two ticks over a
comparable session.

Shared-memory objects are bounded as they were, by `shmem.MaxObjects` per handle
table and `shmem.MaxPages` per object. No native call creates one, so there is
nothing for a quota to refuse; the pages of the two objects the kernel creates
at boot are counted in PID 1's page total.
