# ADR 0161: wait4 reaps family zombie records

## Status

Accepted.

## Context

A shell runs a command by forking and then waiting for the child with `wait4`.
The family registry already keeps a zombie record with the exit status, the
terminating signal, and the CPU ticks after the child's Process is gone
(ADR 0154, ADR 0160). The parent needs a call that consumes that record, one
that returns at once when nothing has exited, and a wait that sleeps until a
child ends without polling.

## Decision

`wait4(pid, status, options, rusage)` accepts a positive PID for one child and
-1 for any child. Process-group targets, 0 and below -1, are `EINVAL` until
GoOS has process groups. `WNOHANG` is supported. `__WALL` and `__WNOTHREAD`
change nothing for a single-task process whose children end with `SIGCHLD`,
so they are accepted. `WUNTRACED` and `WCONTINUED` ask for stopped and
continued children, which GoOS never produces, and are `EINVAL`, as is any
other option bit.

The call looks only at records whose parent is the caller. With no match it
returns `ECHILD`. A matching zombie is reaped: its PID is freed and the caller
gets the PID, the wait status, and a `struct rusage`. With only running
matches, `WNOHANG` returns 0. Otherwise the task blocks on its syscall
continuation and the process records the blocked task. A child's exit wakes
that task once, and the retried call scans again, so a parent waiting for one
PID sleeps again when another child ends. A wake from anything else leaves the
task blocked.

The kernel checks that the status and rusage destinations are writable before
it consumes a zombie. A bad pointer is `EFAULT` and the child stays waitable.
Linux reaps the child before it reports the fault.

`struct rusage` carries the child's CPU ticks as user time. GoOS samples which
task each timer tick interrupted and cannot tell user time from system time,
so system time and every other field are zero. The time covers the child
itself, not the descendants it reaped.

## Consequences

A parent reaps each child exactly once, and a reaped PID can be reused. A
blocked `wait4` costs no CPU. Signals cannot interrupt the wait yet; a killed
parent loses its continuation, and a later child exit finds no task to wake.
