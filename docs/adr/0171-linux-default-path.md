# ADR 0171: Linux programs search the Linux fixture directory

## Status

Accepted.

## Context

The native shell gives every command `PATH=/system/bin`, which holds native
GoOS images. A Linux shell that searched it would find native programs and
fail every `execve` with `ENOEXEC`, because Linux `execve` never loads a native
image. Linux programs live in `/system/linux`.

## Decision

When PID 1 builds the startup record of a Linux program, it replaces the
native default entry `PATH=/system/bin` with `PATH=/system/linux`. A request
without `PATH` gains `PATH=/system/linux`. Every other entry, including a
different `PATH`, passes unchanged. Native programs keep `PATH=/system/bin`.
`execve` still searches nothing; the C library or the shell does.

## Consequences

`busybox sh -c 'busybox echo hi'` finds `/system/linux/busybox` when the binary
is installed there. A native program never appears on a Linux search path.
