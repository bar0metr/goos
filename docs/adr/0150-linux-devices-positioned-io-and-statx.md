# ADR 0150: Kernel-served devices, positioned I/O, and statx

## Status

Accepted. Extends ADR 0141 and ADR 0142; amends the transfer rule of
ADR 0147.

## Context

Shell scripts and build tools open `/dev/null` and `/dev/zero` without asking
whether they exist. Libraries read files with `pread64` to leave the file
position alone. Newer C libraries call `statx` first and fall back to `stat`
only on `ENOSYS`. 0.2.0 had none of the three.

## Decision

**Devices.** After a path resolves, the kernel compares the canonical result
with three names before it asks the VFS:

| Path | Open | Read | Write | `st_rdev` |
| --- | --- | --- | --- | --- |
| `/dev/null` | a descriptor | end of file | discarded, all reported written | 1, 3 |
| `/dev/zero` | a descriptor | zero bytes, at most 64 KiB per call | discarded | 1, 5 |
| `/dev/tty` | `ENXIO` | | | 5, 0 |

A device descriptor has the synthetic kind the descriptor table already
reserved. It holds the device and the access mode and costs no VFS exchange.
`stat`, `statx`, and `fstat` report a character device with mode 0666, and
`access` answers from that mode. `lseek` returns zero. `ioctl` is `ENOTTY`.
`/dev/tty` reports `ENXIO`, which is what Linux tells a process with no
controlling terminal; a GoOS Linux process has none.

The VFS could have carried these as a mount. It was not chosen: a VFS node is
a file or a directory, so the mount would show native programs regular files
named `null` and `zero`, and every discarded write would cost an IPC round
trip.

The names are fixed. `unlink` and `rename` of one are `EACCES`, `mkdir` is
`EEXIST`, `rmdir` and `chdir` are `ENOTDIR`, and a path beneath one is
`ENOTDIR`. `/dev` itself is no directory: a `stat` of it is `ENOENT` and no
listing shows the three names. Native programs do not see them.

**Positioned I/O.** The VFS keeps one position per open file and reads and
writes there. `pread64` and `pwrite64` therefore run three exchanges: seek to
the offset, transfer, seek back to the position the open-file description
holds. A failed transfer still runs the third. The description's position
never changes. A process has one task, and no other process shares its open
files, so nothing can use the position between the exchanges.

A descriptor opened with `O_APPEND` writes at the end of the file whatever
offset `pwrite64` names, after one more exchange for the file length. Linux
does the same and documents it as a bug. A negative offset is `EINVAL` before
the descriptor is looked at, a stream is `ESPIPE`, a directory `EISDIR` for a
read and `EBADF` for a write.

**statx.** `statx` runs the same stat as `newfstatat` and writes the answer as
the 256-byte `struct statx`, laid out by hand from the Linux header.
`stx_mask` names the basic fields, and `STATX_BTIME` when the filesystem
recorded a creation time. The two halves of the device number are split the
way the C library's `major` and `minor` split `st_dev`, so `makedev` of them
gives the `st_dev` of the same file. `AT_STATX_FORCE_SYNC` and
`AT_STATX_DONT_SYNC` change nothing; both together, a reserved mask bit, and
an unknown flag are `EINVAL`. A null path with `AT_EMPTY_PATH` names the
descriptor, as on Linux 6.11 and later.

**Range rule.** Linux checks the whole range a `read` or `write` names before
it does anything else: a length that carries the address past the user half
is `EFAULT`, even when the first bytes are mapped. GoOS now makes the same
check on the length the program asked for. ADR 0147 clamped the length first
and reported a short transfer. The run on a real Linux kernel (ADR 0152)
showed the difference.

## Consequences

BusyBox `sh` redirects to `/dev/null`, and `dd if=/dev/zero` fills a file. A
positioned transfer costs three VFS round trips where a plain one costs one;
a program that reads a large file only with `pread64` pays for it. A process
killed between the exchanges leaves the VFS position moved, and the file
closes with the process. A program that lists `/dev` to find its devices
finds no directory.
