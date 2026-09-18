# ADR 0039: Bounded standard-input consumer

## Status

Accepted.

## Context

ADR 0038 gives each external utility an explicit read-only `in` capability, but
no utility consumes it. Building redirection or pipes on that unexercised role
would leave the startup boundary validated only structurally.

## Decision

Install `readline` as a hardlink to the bounded native utility image. It reads
one line from the inherited `in` handle, accepts at most 128 bytes, stops on CR
or LF, and writes the accepted line to `stdout`. Missing read authority, device
failure, and oversized input produce a non-zero exit through `stderr`.

The command deliberately consumes one bounded record instead of waiting for an
EOF that the current serial endpoint cannot represent. It adds no ambient
console access: the kernel validates the process-local device handle and its
`Read` right on every byte.

## Consequences

The boot test now proves that an isolated child, rather than the shell, consumes
serial input through its named startup capability. The current implementation
still uses the console device operation, so it is not itself a pipe abstraction.
A later iteration must add an alternate capability-protected stream endpoint
and route standard I/O through that endpoint before implementing redirection or
pipelines.
