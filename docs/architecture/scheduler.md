# Scheduler

GoOS separates schedulable Tasks from Processes. A Task owns execution state
and a kernel stack. A Process owns an address space, handles, metadata, and one
or more Task identities. Userspace tasks record their owning process and CR3;
kernel tasks use the kernel page-table root.

The architecture-neutral bootstrap scheduler uses a fixed-capacity queue of 64
tasks and explicit Created, Runnable, Running, Blocked, and Dead states. Its
round-robin policy skips non-runnable tasks and wraps deterministically.

## x86_64 context switch

Every switch saves all 15 general-purpose registers above the interrupt frame,
records that stack pointer as the task context, saves the SSE and x87 state into
the slot's 512-byte FXSAVE area, selects the next slot, restores its FXSAVE
area, loads its CR3 when it differs from the current one, updates TSS.RSP0 for
userspace, and resumes it with `IRETQ`. A slot's FXSAVE area is reset to a clean
image, all exceptions masked, when the scheduler starts and when an exited
task's slot is reclaimed (ADR 0056). The switch points are:

- PIT IRQ0, the preemption point, once per tick;
- a native call that blocks, rewound over its `INT` so it retries on wake;
- a native call that exits or faults;
- a native call that returns while `need_reschedule` is set;
- IRQ11 when it woke the driver's waiter;
- vector 0x81, which kernel tasks use to give up the CPU without advancing
  time. It is an interrupt gate with DPL 0, so ring 3 cannot invoke it.

Selection is round-robin from the slot after the current one. Slot 3, the idle
task, is chosen only when no other task is runnable, so a runnable task never
waits behind a halted kernel task.

New kernel tasks receive four contiguous stack pages, a synthetic interrupt
frame, and two unmapped guard pages below them; the build bounds every kernel
frame by that guard (ADR 0088). The frame preserves Win64 entry alignment
and home space required by compiler-generated functions. Userspace tasks receive
the same guarded kernel stack plus a ring 3 frame naming their RX entry point
and RW+NX user stack.

The IRQ selector maintains a runnable bit mask. A task can block itself and be
woken by another task. Native Exit removes the current user task from the mask
and schedules its successor without returning through a global boot context.

`WaitAny` registers up to 16 readiness interests against the full generation-safe
Task ID. A matching channel, stream, or process event completes the operation and
sets the task's runnable bit.

## Wakeups and safe-return rescheduling

A wake only marks the task runnable and sets `need_reschedule`; it never
switches from inside the kernel code that published the event. The switch
happens at the next safe point: the return from the native call or IRQ that
performed the wake. A native call whose work is complete and that finds
`need_reschedule` set is parked with its result already in RAX and its
instruction pointer after the `INT`, exactly as if the timer had preempted it
on return, and the next runnable task is selected. Every switch clears the flag.
Delivery of a message, a stream write, a process exit, or a block-device
interrupt therefore does not wait for the next PIT tick.

## Kernel tasks

Slot 0 runs the kernel's housekeeping loop (ADR 0053). Each step runs with
interrupts disabled and ends by sleeping: it clears its runnable bit and
switches through vector 0x81 without leaving the exclusion domain, so no wake
can fall between its last check and the sleep. It is woken by every tick, which
expires deadlines and retries retained lifecycle messages; by a user task exit
or fault; and by a lifecycle request from PID 1. Slots 1 and 2, the boot-time
scheduling demonstration, sleep permanently once the demonstration completes.
Slot 3 halts and is selected only when nothing else can run.

The live userspace scenario schedules two processes with distinct CR3 roots.
One first waits out a deadline longer than its peer's, so the peer's timed-out
wait is observed before any message arrives, then remains in ring 3 long enough
to be preempted, performs native calls, and exits. The other executes an invalid opcode; the exception path terminates only
that task, after which kernel tasks and the boot sequence continue.

After exit delivery, the dead task's guarded kernel stack is returned to the
physical allocator and its architecture slot can be reused. The implementation
is single-CPU, and IRQ-level synchronization disables nested maskable
interrupts. Task IDs combine a generation with their reusable slot; IPC wakeups
and exit teardown validate both fields, so stale events cannot target a new
task. A dead slot becomes reusable only after its exit record has been delivered
and its kernel stack reclaimed, because installing a new task bumps the slot
generation: reusing a slot with an undelivered exit would orphan that record and
block every waiter on the exited process forever. Priorities, per-CPU queues, and SMP are not implemented yet.

The current register-then-recheck wait protocol also relies on the uniprocessor
interrupt gate to exclude event publication until assembly has committed the
caller to the blocked state. An SMP implementation must replace that dependency
with explicit synchronization.
