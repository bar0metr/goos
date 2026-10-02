# ADR 0159: vfork suspends the parent over a private child

## Status

Accepted. The restriction on thread-form `clone` described below is
superseded by ADR 0177: the thread flags with a stack create a thread.

## Context

The pinned BusyBox 1.35.0 shell starts every external command with `fork`.
A trace of `sh -c` with a pipeline, `||`, and a background job shows `fork`,
`wait4`, `rt_sigaction`, `rt_sigprocmask`, `rt_sigreturn`, and `dup2`, and no
`vfork`, `clone`, or `clone3`. Other static programs still reach `vfork`
through musl's `vfork()`, and musl's `posix_spawn` calls
`clone(CLONE_VM|CLONE_VFORK|SIGCHLD)` with a stack it allocated.

Linux `vfork` lends the parent's memory to the child until the child calls
`execve` or exits. Sharing that memory would need a second task on one address
space, which the single-task Linux process model does not have.

## Decision

`vfork` and `clone(CLONE_VM|CLONE_VFORK|SIGCHLD)` create the same private child
as `fork` (ADR 0158) and then suspend the caller. The child records the parent
task. A successful image replacement or the child's exit resumes the parent
once, with the child's PID. A parent killed while suspended loses its
continuation, so a later resume finds no matching task and wakes nothing. A
wake from any other source leaves the parent suspended.

`clone(SIGCHLD)` is `fork`. A nonzero stack argument becomes the child's stack
pointer in both accepted forms and must be a user address, or the call is
`EINVAL`. Every other flag set is `EINVAL`: `CLONE_THREAD`, `CLONE_SIGHAND`,
`CLONE_SETTLS`, the TID flags, the namespace flags, `CLONE_VM` without
`CLONE_VFORK`, and a termination signal other than `SIGCHLD`. `clone3` stays
`ENOSYS`.

## Consequences

A vfork child cannot change the parent's memory. A program that relies on the
child's writes reaching the parent, beyond the documented `execve` and `_exit`
pattern, sees the parent's values unchanged. musl's `posix_spawn` fits this model:
its child reports failure through a pipe, not through shared memory. The copy costs what a `fork` costs.
