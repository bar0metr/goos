# ADR 0051: Event-driven multi-object waits

## Status

Accepted.

## Context

Long-running userspace services handled several independent endpoints by polling
them in turn. PID 1 had to service lifecycle and shell-exec traffic, VFS had a
control channel plus a bounded set of client sessions, and the block service had
public and filesystem endpoints. The polling interval made request latency depend
on the 100 Hz scheduler tick and increasing the timer rate did not remove the
underlying scheduling and liveness risks described by ADRs 0049 and 0050.

A wait must not consume the object state it reports. It must also close the race
between observing an object and committing the caller to the blocked state, and
it must not allow a stale task-slot identity or an unvalidated userspace handle
to become a wakeup target.

## Decision

Add native operation 22, `WaitAny`, as a bounded readiness-only primitive. One
operation contains one to sixteen fixed-size interests. Each interest contains a
process-local handle and an event mask; the result is a 16-bit mask, so all
objects found ready in one observation can be reported together. The caller
performs the typed receive, stream operation, or process wait afterwards.

The kernel resolves every interest before registration. Resolution validates the
handle generation, object type, requested event set, capability rights, endpoint
side, and live object identity. The current native boundary supports readable or
peer-closed channel endpoints, readable/writable or peer-closed stream endpoints
according to their direction, and completed process handles. Invalid, duplicate,
unsupported, or over-authorized interests fail the whole operation.

Store wait operations in a fixed table indexed by the scheduler slot, but retain
and compare the complete generation-safe `TaskID`. There is at most one operation
per task and at most 16 interests per operation. Registration, completion, and
temporary wire storage are statically bounded.

Observe readiness, register, then observe readiness again. If the second
observation succeeds, cancel the registration and return directly. Otherwise the
native-call bridge commits the caller to `BLOCKED` and rewinds the call for retry.
The current uniprocessor implementation depends on the interrupt gate keeping
the registration, final observation, and blocked-state commit in one exclusion
domain. SMP requires a different synchronization design.

Publish channel, stream, and process state changes into the table. Peer death is
readiness and wakes interested tasks. Deadline expiry completes an operation with
`Timeout`; explicit cancellation removes an operation during the register/recheck
race and task teardown. Existing one-object `IPCWaitReceive`, `StreamWait`,
`ProcessWait`, and `IRQWait` calls remain supported for compatibility.

Migrate the service loops that genuinely wait on several objects: PID 1 waits on
the lifecycle and exec channels, VFS waits on its control channel and active
sessions, and the block service waits on its public and filesystem endpoints.

## Consequences

These service loops no longer poll their endpoint sets. A state publisher marks
completed waiters runnable. If the publishing task subsequently blocks, the
native-call bridge selects another runnable task immediately; timer preemption is
still the scheduling point when a publisher continues running. There is no
`needReschedule` safe-return hook yet, so this decision does not claim complete
tick independence for nonblocking publishers.

The default timer rate remains 100 Hz. The mechanism removes polling from the
migrated loops, but the full performance matrix and the PID 1 acceptance target
of 1,000 lifecycle events while keeping process launch below 500 ms still require
QEMU validation before changing the timer rate or making broader latency claims.
