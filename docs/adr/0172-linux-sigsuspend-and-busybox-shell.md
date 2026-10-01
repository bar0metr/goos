# ADR 0172: rt_sigsuspend and pause complete the non-interactive shell

## Status

Accepted.

## Context

BusyBox 1.35.0 `sh` starts external programs, pipelines, and scripts with
`fork`, `execve`, `pipe`, `dup2`, `wait4`, and a `SIGCHLD` handler. Its
`wait` builtin blocks every signal and then waits with `rt_sigsuspend` until
the `SIGCHLD` handler has run. While `rt_sigsuspend` answered `ENOSYS`, the
loop never let the handler run and `sh -c 'cmd & wait'` spun forever.
`pause` is the older form of the same wait.

## Decision

`rt_sigsuspend(mask, 8)` installs `mask` as a temporary mask, as `ppoll` does
(ADR 0169), and sleeps on its continuation until a signal with a handler is
deliverable. It always fails with `EINTR`. The handler runs under the
temporary mask and its return restores the old one. A signal pending at entry
that the temporary mask lets through ends the call at once. A set size other
than 8 is `EINVAL` and an unreadable set `EFAULT`. `pause` is the same wait
under the current mask.

A signal whose default action ends the process ends it in the sleep
(ADR 0166). A handled signal sent during the sleep wakes it (ADR 0167).

The interoperability gate for BusyBox `sh`, run by `make test-linux-busybox`
and its alias `make test-linux-shell`, requires a program started by path and
through `PATH`, a child's status in `$?` and the shell's own exit status, two-
and three-stage pipelines, a redirection read back by a child, a script run by
name, and a background job collected by `wait`. No interactive behavior is
part of the gate.

## Consequences

`wait` in BusyBox `sh` sleeps without spinning. The shell has no job control,
no controlling terminal, and no terminal-generated signals; Ctrl-C in the
native shell ends the whole Linux job (ADR 0170).
