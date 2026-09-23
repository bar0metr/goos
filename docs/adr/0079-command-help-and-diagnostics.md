# ADR 0079: Command help and diagnostics

## Status

Accepted. Extends ADR 0032, ADR 0043, ADR 0061 and ADR 0069.

## Context

The shell's surface was complete for 0.1.0 and hard to use. `help` printed two
lines naming the builtins and `/system/bin`, and no command described its
arguments. A mistyped command answered `exec: launch failed: 2`, which is the
number of `LaunchNotFound`, and a builtin given the wrong arguments — `services
now`, `help ls` — did not match its case in the builtin switch, fell through to
the launch path and failed the same way, as though it did not exist.

Utilities said `path required`, `create failed` or `VFS error` without naming
themselves or the path, and `rm` of a non-empty directory said the same as `rm`
of a path that does not exist: the VFS protocol folded not-a-directory,
is-a-directory and not-empty into `InvalidArgument`, and a directory a mount
depended on into `PermissionDenied`, which the utilities reported as a read-only
filesystem. `uptime` printed timer ticks and `mem` page counts.

## Decision

**One table documents every command.** `userspace/shell` holds each command's
name, synopsis, summary, whether it is a builtin, and how many arguments it
takes. The shell prints `help` and `help NAME` from it and decides builtins with
it; each utility checks its own arguments against it and answers `-h` and
`--help` from it. The three cannot disagree, and a command that is not in the
table is not a builtin.

**A malformed invocation is answered with its synopsis.** A builtin with the
wrong number of arguments prints `usage: ...` and launches nothing. A utility
does the same on stderr and exits with status 2, before it opens anything.

**Diagnostics name the command, the path and the reason.** Utilities report
`cmd: path: reason`, built into one buffer and written once so that concurrent
output cannot split it. A reason is derived from the VFS status in one place, and
a refused mutation of a read-only mount is still named as a read-only
filesystem.

**The VFS protocol keeps the reason.** `NotDirectory`, `IsDirectory`,
`NotEmpty` and `Busy` are added to its status set after the existing values, so
none of those change, and `GVF1` moves to version 2. Removing a path a mount
depends on answers `Busy` rather than `PermissionDenied`.

**A launch failure names the stage that failed.** The exec reply gains the index
of the stage PID 1 could not start, and the exec protocol moves to version 7.
The shell reads that stage's name back from the request still in the job's exec
page slot and prints `missing-stage: command not found`, or `exec: cannot start
NAME: out of resources` for a failure that is not a property of the executable.
Pipelines are launched downstream first, so the failed stage is not in general
the first one typed; naming it is what makes the message useful there.

**Readings are given in the units a person uses.** `uptime` prints `up
[D days, ]H:MM:SS`. `mem` prints `total`, `used` and `free` in KiB, MiB or GiB,
where total is the memory the page allocator manages; the page size it
multiplies by is published beside `SystemInfo` and tested against the allocator.

**End of input is not an error.** `readline` treats a closed input as the end of
input and exits with status 1 when it read nothing, instead of reporting a
failure. The consumers of a pipeline whose producer never started therefore stay
silent, and the launch failure is the only message.

## Alternatives

**Checking for the executable in the shell before launching.** A `Stat` of
`/system/bin/NAME` would let the shell say `command not found` without PID 1.
It would also mean a missing stage is never launched, and the boot test's
repeated failing pipelines exist to exercise PID 1's cleanup of a pipeline that
is partly started. The executable can also disappear between the check and the
launch, so PID 1's answer is the one that is true.

**Help text inside each utility.** Keeping the usage strings in the utility
would put them where they are used. The shell's `help` would then have to launch
every utility to list them, or keep its own copy that drifts.

**Mapping the new VFS statuses onto existing ones in the utilities.** The
utilities cannot tell a non-empty directory from an invalid name once the
service has merged them, so the distinction has to survive the protocol.

## Consequences

`help`, `help NAME` and `NAME --help` work for every command, and the boot test
checks each of them, a usage error from a builtin and from a utility, a missing
file, a busy mount point, a non-empty directory, and the `uptime` and `mem`
formats. The pipeline stress test now counts `missing-stage: command not found`.

The latency harness measures `help cd` as its launch-free baseline: `help`
prints the whole table, which would time the serial line rather than the shell.

The first version of the utilities' argument check added about 1.3 ms to every
utility launch, on a `uname` of 5 ms, although it executes a few hundred
instructions. Under QEMU's TCG a process is loaded into freshly written pages,
so every block of code it runs is translated again on every launch: what a
utility's startup costs is how much distinct code it touches, not how many
instructions it retires. Reading the argument count from the vector's count byte
instead of decoding the vector, decoding it only when a lone argument might be a
help flag, and indexing the table rather than ranging over a copy of it brought
the launch back to within measurement noise of the previous baseline. Code on
the startup path of every utility is therefore worth keeping short even where it
is cheap to execute.

A new utility has to be added to the table, or it reports that it is not a
utility in this image. That is the same list the capability manifests in PID 1
still keep by name, which a manifest carried in the image would replace.

The shell still prints `exit status: N` after a failing command, including one
that has already explained itself. Removing it is a decision about how exit
codes are shown, left for when the shell has a way to query them.
