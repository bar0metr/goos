# ADR 0147: Linux memory commit check and short transfers

## Status

Accepted.

## Context

Hardening runs of the conformance probe against hostile arguments, exhausted
resources, and a Linux reference showed three places where a correct program
could be hurt or a wrong answer given.

Anonymous mappings are lazy. A page is charged to the process's quota when it
is first touched, and a touch the quota cannot pay for ends the process. A
program that checks the result of `mmap` or `malloc` had no chance to see the
failure coming.

A read or write whose buffer ran off the end of a mapping failed whole with
`EFAULT`. Linux moves the bytes before the fault and reports them.

A mapping length too large to round to pages was `EINVAL`. Linux answers
`ENOMEM`.

## Decision

**Commit check.** `mmap` refuses with `ENOMEM` a mapping larger than the
pages the quota has left, and `brk` leaves the break where it was for growth
the quota cannot back. The check is against the quota at the time of the
call. A program that maps memory and touches it as it goes, which is how
malloc behaves, meets `ENOMEM` at the call and never the fault. A program
that maps far more than it touches, then touches all of it later, can still
be ended by a fault: GoOS does not reserve pages for untouched mappings.

**Short transfers.** `read`, `write`, `readv`, and `writev` first find how
much of the caller's buffer the process can use, page by page from its first
byte. A transfer moves that prefix and reports its length. A buffer whose
first byte is unusable is `EFAULT`. A range that leaves the user address
space or wraps is `EFAULT` whole, as `access_ok` makes it on Linux. For a
vector, the transfer ends at the first element that cannot be used in full.

**Pages still loading.** A program on a writable volume gets the pages of
its image from its file as it touches them. A call argument can lie in a page
not loaded yet: a string constant nobody read, a buffer in initialized data.
The kernel's copy asks for the page and cannot wait for it in place. The
dispatcher therefore retries a call that ended in `EFAULT` while the task has
a page of its image on the way: the task blocks until the page arrives and
executes the call again. Every call copies its arguments before it changes
anything, or writes its result after work that repeating does not change, so
the retry repeats no effect.

**Errno corrections.** A mapping length that overflows when rounded is
`ENOMEM`. A zero length stays `EINVAL`.

Anonymous executable memory stays refused, at `mmap` and at `mprotect`, with
`EINVAL`. Linux allows it; GoOS W^X does not.

## Consequences

Running out of the page quota is an error a program can handle. The hostile
mode of the probe passes unchanged on Linux and on GoOS, apart from results
the emulator of the reference run alters and the executable-memory rule. The
probe's exhaustion mode maps and touches memory until `ENOMEM`, frees it, and
allocates again.
