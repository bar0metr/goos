# ADR 0012: Versioned typed IPC channels

Status: Accepted

## Context

Userspace services require communication without global object identifiers,
unbounded allocation, string commands, or compiler-dependent structure layout.
The existing handle transfer primitive had no message transport or request
lifecycle.

## Decision

Introduce bidirectional channels with fixed queues of 16 messages. Messages
carry an explicit protocol ID, version, kind, correlation ID, a payload bounded
to 256 bytes, and at most four transferred handles. Define a strict little-
endian wire header with magic and reserved fields.

Track outstanding correlations per endpoint. Reject duplicates and orphan
replies. Derive transferred capabilities into the receiver's local handle table
without rights escalation, rolling back partial transfers on failure. Preserve
queued messages after peer close, then return the typed `PeerDied` status.

## Alternatives

String commands provide no stable schema. Unbounded queues allow trivial memory
exhaustion. Passing global IDs bypasses process-local authority. Depending on Go
structure layout silently couples protocols to compiler and target details.

## Consequences

The kernel has allocation-free typed request/reply and notification primitives,
controlled handle passing, deterministic backpressure, peer-death reporting,
host security tests, a boot self-test, and a ping-pong benchmark. Blocking wait,
timeout/cancellation, userspace kernel-call bindings, and shared memory remain
separate iterations.
