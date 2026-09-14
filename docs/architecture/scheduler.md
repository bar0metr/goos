# Scheduler

GoOS separates schedulable Tasks from future Processes. A Task owns execution
state and a kernel stack. A Process will own an address space, capabilities,
handles, metadata, and one or more Tasks.

The architecture-neutral bootstrap scheduler uses a fixed-capacity queue of 64
tasks and explicit Created, Runnable, Running, Blocked, and Dead states. Its
round-robin policy skips non-runnable tasks and wraps deterministically. The
fixed capacity prevents allocator dependencies during early boot.

## x86_64 context switch

PIT IRQ0 is the preemption point. The entry stub saves all 15 general-purpose
registers. The CPU-provided long-mode interrupt frame supplies RIP, CS, RFLAGS,
RSP, and SS. The complete saved RSP becomes the task context. The stub selects
the next slot, changes RSP, restores registers, acknowledges the PIC, and uses
`IRETQ` to resume the selected task.

New tasks receive four contiguous physical pages and a synthetic interrupt
frame. The frame preserves Win64 entry alignment and home space required by the
TinyGo-generated functions. The boot task is captured on the first preemption.
Two worker tasks and one idle task run independently; boot resumes only after
both workers have executed at least three times.

The current implementation is single-CPU and the IRQ stub owns run-queue
synchronization by disabling nested maskable interrupts. Guard pages, arbitrary
task creation, teardown, blocking, wakeup, priorities, per-CPU queues, and SMP
are not implemented yet.
