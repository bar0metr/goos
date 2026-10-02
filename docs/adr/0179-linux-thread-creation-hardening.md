# ADR 0179: Linux thread creation preserves native capacity and commits once

## Status

Accepted. Supersedes the task-capacity, clone-commit, and `set_robust_list`
parts of ADR 0177 and the `set_robust_list` decision in ADR 0176.

## Context

The Linux process reserve protects Process records. A thread-form `clone`
adds a scheduler Task to an existing Process, so Linux threads could consume
the task slots PID 1 and native services need.

Thread creation also wrote parent and child TIDs before scheduler task
creation. Later failures could leave those writes visible. The scheduler
allocated and mapped a kernel-entry stack before it installed a task, but an
installation failure did not return those pages or remove their mappings.

GoOS accepted `set_robust_list` and retained the head. Thread exit never
walked the list, marked held futexes with `FUTEX_OWNER_DIED`, or woke their
waiters. Success claimed owner-death behavior that the kernel lacked.

## Decision

`NativeTaskReserve` keeps 16 scheduler slots available to native tasks. A
Linux root launch, `fork`, `vfork`, process-form `clone`, and thread-form
`clone` may create a task only when creation leaves all 16 slots free. The
check counts unused slots and reclaimed dead slots. Native task creation does
not use this ceiling. The existing limit of eight tasks per Linux Process
remains in force.

Thread-form `clone` uses a prepare and commit sequence. The kernel validates
the stack, TLS value, and complete writable ranges for requested TID stores.
It snapshots registers, FS base, FPU state, and the caller's signal mask. It
then reserves the Process task charge and a compatibility thread slot with a
TID. A failure before scheduler installation cancels both reservations.

The scheduler returns the allocated kernel-stack pages and removes the new
stack mappings if it cannot install the task. During commit, the kernel
disables interrupts, installs the task, binds the reserved Process and thread
records, sets FS and FPU state, writes requested TIDs, and restores interrupts.
The new task cannot run with partial state.

`set_robust_list` returns `ENOSYS` for the supported 24-byte structure.
Lengths other than 24 bytes return `EINVAL`. GoOS stores no robust-list head
until thread exit implements bounded robust-futex owner-death handling.

## Consequences

Linux compatibility cannot consume the last 16 scheduler slots through any
implemented creation path. Process and task reserves protect different fixed
tables and apply together to a new Linux Process.

A failed thread clone leaves no TID store, compatibility record, Process task
charge, scheduler task, or kernel-entry stack allocation. Successful clones
publish all task-local state before the scheduler can select the new task.

Programs that require robust mutex recovery receive `ENOSYS`. The pthread and
7-Zip paths used for GoOS 0.2.3 do not require robust mutex owner-death
semantics.
