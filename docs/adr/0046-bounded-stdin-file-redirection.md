# ADR 0046: Bounded stdin file redirection

## Status

Accepted.

## Context

GoOS already composes native pipelines from capability-protected streams and
gives filesystem utilities private VFS sessions. Input redirection must preserve
those boundaries instead of moving file paths or filesystem policy into PID 1,
the exec protocol, or the kernel.

## Decision

Accept one unquoted, unescaped `<` operator with exactly one path target. The
shell prepends the existing `/system/bin/cat` utility to the requested pipeline.
That producer receives a private VFS session, reads the resolved path, and sends
the bytes through its stdout stream to the first requested stage's read-only
`in` role.

Input and output redirection may be combined when the producer, requested
stages, and optional output consumer fit the existing four-stage bound. PID 1
continues to launch an ordinary version 4 pipeline and remains unaware of file
semantics. The kernel, startup ABI, exec IPC, VFS protocol, and capability model
remain unchanged.

## Consequences

Input redirection consumes one pipeline stage. The existing downstream-first
launch and endpoint cleanup behavior applies unchanged. Host tests cover syntax
and stage bounds; the QEMU gate copies file input through `readline` into a
second file and reads it back through VFS.
