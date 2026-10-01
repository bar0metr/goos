# ADR 0167: Handled signals interrupt waits, and SA_RESTART restarts them

## Status

Accepted.

## Context

A signal sent to a process asleep in a system call waited until that call
returned (ADR 0166). A shell blocked in `read` or `wait4` would not run its
`SIGCHLD` or `SIGINT` handler until something else woke it. Linux breaks such
a wait. The call then fails with `EINTR`, or restarts after the handler when
the handler asked for `SA_RESTART` and the call allows it. A call must never
repeat an effect that already happened.

## Decision

When a signal with a handler becomes pending and unblocked for a process, the
kernel looks at the process's task. If it sleeps in one of these waits, the
kernel cancels the wait and resumes the call's continuation with an
interrupted wake:

- a stream `read`, `readv`, `write`, or `writev` with nothing moved;
- `wait4`;
- `nanosleep`;
- `poll`.

A VFS exchange or a request queued for the family VFS context keeps waiting,
because Linux regular-file I/O does not fail with `EINTR` and programs do not
retry it. A vfork parent keeps waiting, as on Linux. A signal under a
terminating default has already ended the process (ADR 0166).

The retried call sees the interrupted wake. `wait4` first scans again and
returns a child that has ended, as Linux does; the child's exit wakes the
parent before its `SIGCHLD` is raised. A stream call and `wait4` restart when
the signal delivered next has `SA_RESTART`, or when no signal remains to
deliver. Otherwise they fail with `EINTR`. `nanosleep` fails with `EINTR` and
writes the time left to `rem`. `poll` fails with `EINTR`. Neither restarts,
whatever the handler's flags.

A restart rewinds the return to the `SYSCALL` instruction and puts the call
number back in RAX. The handler frame saves that state, so `rt_sigreturn`
resumes into the same call, which starts afresh.

## Consequences

`SA_RESTART` now changes behavior: ADR 0165 accepted it while no call could be
interrupted. A blocked stream call never moves bytes before it sleeps, so an
interrupted one has none to report and no short count arises from a signal.
File operations finish before any handler runs.
