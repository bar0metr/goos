# ADR 0146: Linux descriptor duplication, fcntl, and no terminal

## Status

Accepted.

## Context

Shells and utilities rearrange descriptors with `dup`, `dup2`, `dup3`, and
`fcntl`, and libc asks whether a descriptor is a terminal with `ioctl`. The
descriptor table already separates descriptors from open-file descriptions
(ADR 0135). A Linux process's standard streams are GoOS byte streams, whether
the other end is a file writer, another program, or the terminal service.

## Decision

**Duplication.** `dup`, `dup2`, `dup3`, `fcntl(F_DUPFD)`, and
`fcntl(F_DUPFD_CLOEXEC)` add a descriptor for an existing description. The
duplicates share the file position and the status flags; each descriptor has
its own close-on-exec flag. `dup2` and `dup3` close an open target first.
When the target was the last descriptor of a VFS file, that close is an
exchange the call waits for before it installs the duplicate. `dup2` of a
descriptor onto itself returns it; `dup3` of one onto itself is `EINVAL`, as
is any `dup3` flag but `O_CLOEXEC`.

**fcntl.** `F_GETFD` and `F_SETFD` read and write the close-on-exec flag.
The flag is recorded although no `execve` exists, so the table needs no
change when one arrives. `F_GETFL` reports the access mode, `O_APPEND`, and
`O_LARGEFILE` for a file, `O_DIRECTORY` for a directory, and the direction
alone for a stream. `F_SETFL` changes `O_APPEND` on a regular file. It
refuses `O_NONBLOCK`, `O_ASYNC`, `O_DIRECT`, and `O_NOATIME` with `EINVAL`,
because GoOS has no behavior behind them, and ignores every other bit, as
Linux does. Locks, owners, leases, and seals are `EINVAL`.

**No terminal.** `ioctl` on an open descriptor returns `ENOTTY` for every
request, and `EBADF` on a closed one. No descriptor of a Linux process is a
terminal or a device in this release: `fstat` reports the standard streams
as FIFOs, and `ioctl` agrees. musl's `isatty` is false, stdio buffers fully,
and a program that asks for the window size falls back to its default. The
kernel does not guess from the descriptor number, and PID 1 passes no
terminal provenance. A process that needs a real terminal is outside this
release.

## Consequences

A utility that redirects its own descriptors works. Output of a Linux
program at the GoOS terminal is block-buffered by libc and appears when the
buffer fills or the program exits, which interactive programs would notice;
they need termios, signals, and a terminal descriptor, none of which exists
yet. `ENOTTY` replaces the `ENOSYS` the call returned before, which is the
answer Linux gives for a pipe.
