# ADR 0182: Per-processor run queues and a kernel lock replace interrupt exclusion

## Status

Accepted.

## Context

Until now one rule kept the kernel consistent: it ran on one processor, and
native calls, Linux calls, fault resolution, and housekeeping ran with
interrupts disabled (ADR 0053). With a second processor that rule protects
nothing shared. The scheduler lived in assembly as global masks and one
`current` index, the wait protocols relied on no wake arriving between a
task's last check and its sleep, and the kernel's call paths share static
buffers on the assumption that only one runs at a time.

## Decision

The run state moves into `kernel/sched.Core`, a pure structure with host
tests that model interleavings across processors. It keeps the 64-slot task
table and generation-safe IDs, gives each processor a run queue and a current
slot, and marks a task running from the moment a processor picks it until
that processor has left its stack. A switch has two halves: one on the
outgoing stack picks the next task, one on the incoming stack releases the
outgoing task and publishes its exit if it ended. No processor can pick a
task whose registers are still being saved, and housekeeping never frees a
stack in use.

Placement is simple. A woken task stays on the processor it last ran on when
that one idles, then goes to an idle processor, then to the least loaded. An
idle processor steals one runnable task from another queue. A preempted task
moves only to an idle processor, so no task bounces between busy ones. The
kernel's own tasks are pinned to the bootstrap processor, which keeps
deadline expiry, exit delivery, and lifecycle retries in one place. Each
processor has its own local APIC timer; only the bootstrap processor's tick
advances kernel time.

A wake records a pending wake, and a task that blocks with one pending stays
runnable. That takes over from interrupt exclusion in every wait protocol. A
task is installed not runnable and started by its creator once its process
binding, thread record, TLS, FPU state, and TID writes exist. Waking a remote
processor is a flag plus a reschedule interrupt, and a pending flag covers
later wakes.

Shared state takes explicit locks, in increasing level order with interrupts
disabled: the kernel lock, the page allocator's lock, the scheduler lock, and
the console lock. The kernel lock serializes the kernel's object layer: every
native call, Linux system call, user fault resolution, and housekeeping step
runs under it. Tasks run in parallel in user mode and their calls run one at
a time in the kernel. The scheduler, the timers, IPIs, the TLB shootdown, and
the page allocator do not depend on it.

A processor can stop a task another processor runs. Terminate marks it
killed and interrupts its processor; hold keeps it off every processor until
released. A task waiting for the kernel lock gives up when it is killed or
held, and its call is retried if it runs again, so a processor that holds the
lock while it waits for the task cannot deadlock. Exec waits for the old
image's other threads to leave their processors before it destroys the old
address space; a Linux fork holds the caller's other threads while it copies
memory.

Kernel console output is collected per processor and emitted a line at a
time under the console lock.

Validation builds record each lock's holder and check the level order,
recursion, and after every switch the scheduler's invariants, among them that
no task is current on two processors.

## Consequences

Programs with several threads compute in parallel, which is what workloads
such as 7-Zip spend their time on. Kernel calls do not scale with
processors: the kernel lock serializes them. Splitting it, by process and by
object type, is the next step and needs no change to the scheduler,
timers, or TLB code. A lost wake no longer depends on interrupt timing, and
the start-to-run window for new tasks is closed for one processor as for
many.
