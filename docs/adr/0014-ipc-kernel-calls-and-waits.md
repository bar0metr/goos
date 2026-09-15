# ADR 0014: IPC kernel calls and bounded waits

## Status

Accepted

## Decision

Native processes send and receive the existing `GIPC` wire format through
capability-checked channel handles. Send requires `Write`; receive requires
`Read`. Kernel channel identities live in a separate high-bit namespace and
resolve through a fixed-capacity registry, so a forged handle object cannot
alias physical memory or shared-memory objects.

The native call ABI carries three arguments: channel handle, user buffer, and
buffer length. Buffers are bounded to the maximum wire message and copied
through the validated user-memory helpers. Attached handles remain rejected at
this boundary until the ABI can describe requested derived rights explicitly.

IPC waits use a fixed-capacity wait set. Registration and scheduler blocking
are transactional. Message arrival, peer death, and timer deadline expiration
produce typed wake reasons which transition blocked tasks back to runnable.
Local interrupt exclusion remains the synchronization boundary for the current
single-CPU kernel.

## Consequences

IPC never trusts user pointers or raw object identifiers. Queue and waiter
memory consumption is statically bounded. A receive buffer must reserve the
maximum wire size, which prevents destructive dequeue on a short buffer. SMP
requires replacing local interrupt exclusion with per-channel and wait-set
locking.
