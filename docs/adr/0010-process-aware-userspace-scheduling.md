# ADR 0010: Process-aware userspace scheduling and lifecycle

Status: Accepted

## Context

The first native program entered ring 3 synchronously and returned through two
global boot-context slots. It was not a scheduler task, could not coexist with
another userspace process, and any userspace CPU exception stopped the kernel.

## Decision

Represent every runnable architecture task with a saved stack pointer, a page-
table root, and an optional task-local kernel-entry stack. Timer selection loads
the selected task's CR3 before restoring its context. Before entering a user
task it writes that task's guarded kernel-stack top to TSS.RSP0.

Create ring 3 tasks with synthetic interrupt frames and add them to the same
bounded run queue as kernel tasks. Native Exit and userspace CPU faults remove
the current task from the runnable mask and immediately select another task.
Kernel-originated faults remain fatal invariant violations.

## Alternatives

Keeping a global userspace return context permits only one synchronous process.
Sharing one RSP0 stack lets user tasks overwrite suspended interrupt frames.
Deferring CR3 changes to userspace entry prevents direct process scheduling.

## Consequences

Kernel and userspace tasks are preempted by one scheduler, two process roots can
coexist in the run queue, and a crashing user task no longer stops the kernel.
The implementation remains single-CPU and bounded to 64 tasks. Dead slots do
not yet reclaim stacks, address-space tables, or process-owned objects. Wait
semantics and ID reuse remain future work.
