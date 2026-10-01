# ADR 0163: Linux signals become pending and act at syscall return

## Status

Accepted.

## Context

GoOS recorded signal dispositions and the blocked mask but never generated or
delivered a signal (ADR 0151). A shell needs `SIGCHLD` when a child ends, and a
pipeline stage needs `SIGPIPE` when its reader is gone. The kernel can change
a Linux task's user state at one point it already controls: the return from a
system call, after the call's effects are complete.

## Decision

Each Linux process keeps a 64-bit pending set beside its mask, with one
`siginfo` record per standard signal. Standard signals do not queue: a second
instance of a pending signal is merged and the first record kept. Fork gives
the child an empty pending set. Image replacement keeps pending signals, as
Linux does.

GoOS generates and delivers `SIGINT`, `SIGQUIT`, `SIGKILL`, `SIGUSR1`,
`SIGUSR2`, `SIGPIPE`, `SIGTERM`, and `SIGCHLD`. A CPU fault still ends the
process at once with the signal it maps to (ADR 0160) and cannot be caught.

Generation discards a signal that is ignored and not blocked: one whose
handler is `SIG_IGN`, or `SIGCHLD` under its default. `SIGKILL` is never
ignored, blocked, or caught. Every other signal becomes pending.

The kernel generates two signals in this release. `SIGCHLD` goes to a Linux
parent when its child becomes a zombie, with `CLD_EXITED` and the status or
`CLD_KILLED` and the signal, the child's PID, and UID 1000. `SIGPIPE` goes to
a process whose `write` or `writev` failed with `EPIPE`, recorded as sent by
itself, and the call still returns `EPIPE`.

When a system call returns, the kernel takes pending signals the mask does
not block: `SIGKILL` first, then the lowest number. A discarded signal is
skipped. A default action ends the process with that signal as its
termination status. A handler is entered with a frame (ADR 0164), and the
rest wait for the next return. A system call that blocks, exits, or replaces
the image delivers nothing at that point.

## Consequences

A handler runs before the call it interrupted returns to the program, which
is when Linux runs it for a signal pending at that return. A process that
makes no system calls receives nothing until it makes one, and a blocked call
is not interrupted yet. `SIGPIPE` now ends a writer whose reader is gone,
unless the writer ignores or handles it.
