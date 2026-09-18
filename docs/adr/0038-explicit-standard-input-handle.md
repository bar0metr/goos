# ADR 0038: Explicit standard input handle

## Status

Accepted.

## Context

External utilities have separate output roles but no input role. A future pipe
or input redirection cannot be represented safely if programs implicitly read
the shell console. A fourth full-length startup role would exceed the 128-byte
GST2 envelope for real commands with structured arguments and `PATH`.

## Decision

PID 1 installs the compact named `in` role as a read-only standard-input device
capability in every external utility process, alongside write-only `stdout` and
`stderr` and its private VFS session. `in` currently derives from the serial
console. Utilities validate the role at startup but do not consume it until a
command needs input.

The compact role keeps representative commands within the existing 128-byte
GST2 envelope. The serialization format, process storage, and lifecycle launch
protocol remain unchanged. The lifecycle IPC envelope continues to validate the
combined executable path, startup data, and capability manifest against the
256-byte IPC payload.

## Consequences

Every external utility now has explicit standard-input, `stdout`, and `stderr`
authority with least-privilege directions. Existing behavior is unchanged.
ADR 0039 subsequently adds the first bounded stdin-consuming utility. Alternate
stream endpoints, redirection, and pipes remain future Priority 3 work.
