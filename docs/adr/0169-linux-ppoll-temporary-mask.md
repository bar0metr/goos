# ADR 0169: ppoll swaps the signal mask for the length of the call

## Status

Accepted.

## Context

A program that waits for descriptors and for a signal at once blocks the signal
everywhere else and lets it through only while it waits. Unblocking before
`poll` and blocking after it would lose a signal that arrives between the two
calls. `ppoll` closes the gap by installing a mask for the call alone. Linux
gives the old mask back when the call returns, except when a handler
interrupts the call: that handler runs under the call's mask, and its return
restores the old one.

## Decision

`ppoll(fds, nfds, timeout, sigmask, sigsetsize)` is `poll` (ADR 0168) with two
changes.

The timeout is a `timespec`. A null pointer waits without limit. A negative or
out-of-range field is `EINVAL`. On return the kernel writes the time left to
the `timespec`, zero after a timeout, as Linux does for the raw call.

A non-null `sigmask` needs `sigsetsize` 8, or the call is `EINVAL`. The
process keeps its mask aside and installs `sigmask` without `SIGKILL` and
`SIGSTOP` before it scans. The temporary mask decides which signals interrupt
the scan and the sleep. A handled signal it lets through, pending at entry or
sent during the sleep, ends the call with `EINTR` unless a descriptor is
ready; a ready descriptor wins. On `EINTR` the handler runs under the
temporary mask and its frame saves the old mask, so `rt_sigreturn` restores
it. On any other return the old mask comes back before signals are delivered,
so a signal only the temporary mask let through stays pending.

`poll` checks the same condition before it sleeps: a pending handled signal
ends it with `EINTR` when nothing is ready.

## Consequences

A program can wait for a pipe and a signal without the race. A retried `ppoll`
keeps the first saved mask, so a wake cannot replace it with the temporary one.
`pselect6` and `sigsuspend` are not implemented.
