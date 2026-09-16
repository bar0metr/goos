# ADR 0029: Process handles and native wait

## Status

Accepted

## Context

PID 1 previously waited for short-lived applications by consuming global
lifecycle notifications and matching numeric process IDs. This mixed service
supervision traffic with parent-child synchronization and provided no explicit
authority to observe a process.

## Decision

Every successful launch installs a typed process handle in PID 1 with only the
`Wait` right and returns that local handle in lifecycle protocol version 5.
Process identity remains opaque behind the handle table.

Native kernel call 12 accepts a process handle, an eight-byte result address,
and a bounded timer deadline. It returns immediately for retained completions,
blocks through a fixed-capacity wait set for running processes, and resumes on
exit or timeout using the restartable interrupt frame. The exit code is copied
to validated writable user memory. Closing the handle invalidates later waits.

PID 1 closes handles for supervised services because their policy still uses
lifecycle notifications. Shell-launched applications are awaited through the
new kernel call, after which PID 1 closes the handle.

## Consequences

Waiting requires explicit capability authority, stale local handles are
rejected, already-exited processes remain waitable through bounded completion
records, and application waits no longer consume unrelated lifecycle events.
Multiple independent waiters and process-control rights remain future work.
