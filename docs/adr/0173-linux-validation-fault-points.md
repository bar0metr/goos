# ADR 0173: Validation builds fail fork and execve on purpose

## Status

Accepted.

## Context

`fork` and `execve` promise that a failure leaves nothing behind (ADR 0157,
ADR 0158). The ordinary tests reach the early refusals: a full budget, a
missing file, a bad pointer. They do not reach a failure after the kernel has
built most of a child, or the VFS dying while `execve` prepares an image. Those
paths run only under memory pressure or a service crash, which a test cannot
produce on demand.

## Decision

The validation build carries two deterministic fault points. The normal build
has neither: both sit behind the validation constant and compile away.

A fork whose caller's working directory is `/tmp/.validation-fork-fault`
fails after four pages of the child's copy. Before it registers the child, the
kernel records the process registry count, the family and global
compatibility counts, the open-file descriptions and their references, the
family VFS context's users, and the scheduler's task count. After the
rollback it compares them, checks that the PID, the compatibility record, and
the process slot are free, and that the free page count is what it was before
the child's address space. It reports `LINUX_FORK_ROLLBACK_OK` or
`LINUX_FORK_ROLLBACK_FAILED`. The call returns `ENOMEM`.

So that this failure unwinds all of the child, fork now installs the child's
descriptors, VFS context reference, compatibility state, and registry entry
before it copies pages. Nothing can observe the child before the call returns.

The validation VFS exits when a session asks it to prepare an executable it
opened as `/tmp/.validation-vfs-exec-crash`, before it replies. The caller's
`execve` is then between opening its image and committing the replacement. It
fails with `EIO`, the documented error of a dead session (ADR 0138), and the
caller goes on in its old image.

`abi-probe fork-fault` and `abi-probe exec-vfs-loss` drive both points, and
`make test-linux-boot` requires their results.

## Consequences

The rollback of a fork that fails late and of an exec that loses its VFS is
tested in every validation boot. The exec test ends the VFS, so it runs last
in the Linux boot test, which then waits for PID 1 to restart the service.
