# ADR 0041: Native two-stage pipelines

## Status

Accepted.

## Context

ADR 0040 defines directional byte-stream capabilities, but standard utility I/O
still targets the console and stream backpressure originally required polling.
A useful composition boundary requires two isolated processes to run
concurrently with no shared console authority between their connected roles.

## Decision

Support exactly one unquoted, unescaped `|` in the bounded shell grammar. The
version 3 exec request carries two already parsed invocation vectors. PID 1
copies both specifications out of the shared IPC buffer before launching either
stage, creates a private stream, launches the producer and consumer before
waiting, and substitutes writer and reader endpoints for `stdout` and `in`.
`stderr` remains on the console.

Stream endpoints include `Wait` authority. `StreamWait` blocks the current task
in a fixed-capacity scheduler wait set and wakes it on data, space, peer close,
or timeout. Utility I/O selects stream operations for stream handles and retains
console operations for device handles.

PID 1 grants VFS sessions only to filesystem commands. Pure stream and system
utilities no longer receive ambient VFS authority. The VFS AddSession control
protocol is version 2 and carries an explicit operation byte.

## Consequences

`echo value | readline` executes as two concurrent native processes connected
only by directional capabilities. The boot gate verifies the real serial
command, transferred data, child exits, later VFS commands, and persistence.

The grammar deliberately rejects multiple pipes. File-backed redirection and
arbitrary-length pipelines require further bounded planning and cleanup rules.
