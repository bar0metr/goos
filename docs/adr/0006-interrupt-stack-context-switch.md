# ADR 0006: Interrupt-stack context switching

Status: Accepted

## Context

The scheduler must be timer-preemptive from its first executable version. A
cooperative-only task ABI would establish the wrong execution model and hide
register preservation defects.

## Decision

Use the IRQ0 stack frame as the x86_64 task context. Save every general-purpose
register, store the resulting RSP in a bounded context table, select the next
slot round-robin, load its RSP, restore registers, and return with `IRETQ`.
Construct new-task stacks as synthetic long-mode interrupt frames containing
RIP, CS, RFLAGS, RSP, and SS. Reserve an unmapped guard page below each task
stack. Maintain an IRQ-visible runnable mask with atomic block and wake
operations under disabled interrupts.

## Alternatives

Cooperative function switching does not test timer preemption. Calling the Go
scheduler directly from IRQ assembly requires a frozen interrupt calling
convention, stack alignment contract, and reentrancy policy that do not yet
exist. Using TinyGo's internal task scheduler would couple kernel scheduling to
an unsupported runtime implementation.

## Consequences

Kernel tasks are genuinely preempted by hardware timer interrupts, execute on
independent guarded stacks, and can block and resume without losing context.
Selection remains a small assembly bootstrap path rather than the
architecture-neutral Scheduler object. The next scheduler revision must define
a stable assembly/Go dispatch ABI, general wait queues, teardown, and stack
reclamation before this mechanism can support arbitrary kernel work.
