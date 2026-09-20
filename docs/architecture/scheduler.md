# Scheduler

GoOS separates schedulable Tasks from Processes. A Task owns execution state
and a kernel stack. A Process owns an address space, handles, metadata, and one
or more Task identities. Userspace tasks record their owning process and CR3;
kernel tasks use the kernel page-table root.

The architecture-neutral bootstrap scheduler uses a fixed-capacity queue of 64
tasks and explicit Created, Runnable, Running, Blocked, and Dead states. Its
round-robin policy skips non-runnable tasks and wraps deterministically.

## x86_64 context switch

PIT IRQ0 is the preemption point. The entry stub saves all 15 general-purpose
registers. A privilege-changing interrupt frame also supplies user RSP and SS.
The complete saved RSP becomes the task context. The stub selects the next slot,
loads its CR3, updates TSS.RSP0 for userspace, restores registers, acknowledges
the PIC, and uses `IRETQ` to resume the selected task.

New kernel tasks receive four contiguous stack pages, a synthetic interrupt
frame, and an unmapped guard page. The frame preserves Win64 entry alignment
and home space required by compiler-generated functions. Userspace tasks receive
the same guarded kernel stack plus a ring 3 frame naming their RX entry point
and RW+NX user stack.

The IRQ selector maintains a runnable bit mask. A task can block itself and be
woken by another task. Native Exit removes the current user task from the mask
and schedules its successor without returning through a global boot context.

`WaitAny` registers up to 16 readiness interests against the full generation-safe
Task ID. A matching channel, stream, or process event completes the operation and
sets the task's runnable bit. The native-call bridge switches immediately when
the publishing task subsequently blocks; a publisher that continues running is
not preempted until the timer interrupt. There is no `needReschedule` safe-return
hook yet, so event publication alone is not a general immediate-reschedule point.

The live userspace scenario schedules two processes with distinct CR3 roots.
One remains in ring 3 long enough to be preempted, performs native calls, and
exits. The other executes an invalid opcode; the exception path terminates only
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
