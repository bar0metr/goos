# ADR 0032: Native argv, environment, and base utilities

## Status

Accepted

## Context

The first startup ABI carried one opaque argument tail. Applications had to
parse it independently, launch replies lost the child's exit code, and the
native base system exposed only filesystem readers.

## Decision

`GST2` carries separately bounded, length-prefixed argument and environment
vectors. A process receives at most eight non-empty arguments and four
environment entries. The shell supports spaces, single and double quotes, and
backslash escaping, then passes `argv[0]` as the command name. Environment names
use uppercase ASCII, digits, and underscore; the initial shell environment
contains only `PATH=/system/bin`.

The shell-to-init exec protocol is version 2. Successful replies contain the
process ID and final exit code. Launch failure remains separate from child
failure, so a program returning non-zero is not reported as an image-launch
error.

The multicall native base image now exposes `echo`, `mkdir`, `touch`, `rm`,
`cp`, `mv`, `stat`, `ps`, `mem`, `uptime`, `uname`, and `sleep` in addition to
`pwd`, `ls`, and `cat`. Filesystem utilities use only the VFS protocol. A new
read-only `SystemInfo` native call returns the caller PID, live process count,
managed/free page counts, and monotonic ticks for `ps` and `mem`; it grants no
new authority and copies a fixed 40-byte record to validated user memory.

## Consequences

Native programs receive deterministic structured startup data without POSIX
file descriptors or ambient configuration. Quoting is resolved once by the
shell. File utilities remain confined to the VFS session supplied in their
capability manifest. `sleep` currently waits on the monotonic clock while the
preemptive scheduler continues to run other tasks; a future timer-wait native
primitive can replace that polling implementation.
