# ADR 0168: poll sleeps on native readiness

## Status

Accepted.

## Context

A shell and many utilities wait on several descriptors with `poll`. GoOS
already publishes stream readiness for the native wait-any table, which wakes
a task when any of its registered objects changes state or its deadline
passes. Polling in a loop would burn the CPU and still be late.

## Decision

`poll(fds, nfds, timeout)` copies at most 64 `pollfd` entries; more is
`EINVAL`. An unreadable array is `EFAULT`. A negative descriptor is skipped
and one that is not open reports `POLLNVAL` without failing the call.

Readiness:

- a stream read end is readable while it holds bytes, and reports `POLLHUP`
  once every writer is gone and nothing is left;
- a stream write end is writable while 4096 bytes fit, so a `PIPE_BUF`
  write cannot block, and reports `POLLERR` once every reader is gone;
- a regular file, a directory, or a device is always readable and writable.

`POLLERR` and `POLLHUP` are reported whether asked for or not. The result
counts the entries with any event.

When nothing is ready and the timeout is not zero, the task registers every
unready stream end with the wait-any table, merging two descriptors of one
end, and sleeps on its continuation. A positive timeout adds a timer deadline
from the monotonic clock, rounded up to whole ticks so the call never returns
early. Any negative timeout waits without limit, as on Linux. A wake from a
stream, the timer, or a handled signal (ADR 0167) retries the call, which
scans every descriptor again.

## Consequences

A process waiting on pipes uses no CPU until one of them changes. A read end
that still holds bytes after its writers left reports `POLLIN` without
`POLLHUP` until it is drained. `ppoll` and `select` are not implemented.
