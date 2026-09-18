# Typed IPC

The bootstrap IPC mechanism uses fixed-capacity bidirectional channels. Each
endpoint has an inbound queue of 16 messages. Queue storage is part of the
channel object, so sending cannot trigger heap allocation. A full queue returns
`ResourceExhausted` instead of blocking or growing without a bound. Payloads
larger than the inline limit use transferred shared-memory handles.

Every message names a nonzero protocol ID and version. Request and reply
messages carry a nonzero correlation ID; notifications carry none. A channel
tracks outstanding request IDs per endpoint, rejects duplicate requests, and
rejects replies that do not match an outstanding request. Payloads are limited
to 256 bytes. Bulk transfer will use shared memory rather than larger messages.

The stable wire envelope is little-endian and independent of Go structure
layout. Its 32-byte header contains `GIPC`, protocol ID, version, message kind,
correlation ID, payload size, handle count, and reserved fields. The payload is
followed by up to four receiver-local opaque handle IDs. Decoding requires the
exact encoded length and zero reserved fields.

The channel core derives capabilities from the sender's process-local table
into the receiver's table. The source handle must carry Transfer authority, and
the requested rights cannot exceed the source. A failed multi-handle transfer
closes all handles already derived for that message. Kernel-internal callers may
retain the source; the native `IPCSend` boundary applies move semantics after a
successful send and strips Transfer authority from the receiver.

Closing one endpoint prevents further sends. Its peer may drain already queued
messages and then receives `PeerDied`. The mechanism is single-CPU: callers
provide IRQ-level exclusion. Userspace accesses channels only through
capability-checked handles, never through kernel object addresses. Each channel
capability identifies one endpoint side. PID 1 may create a bounded channel pair
through the native call ABI. Userspace IPC attachments use move semantics and
strip `Transfer` from the receiver. Closing both endpoint capabilities releases
the fixed registry slot.

Unstructured byte flow is intentionally separate from typed IPC. Native streams
preserve byte order in a fixed ring buffer and expose directional capability
endpoints; see [native byte streams](streams.md). Typed IPC remains the control
plane for requests, replies, and handle transfer.

# Native call boundary

`IPCSend` and `IPCReceive` accept a process-local channel handle, a user virtual
address, and a bounded wire-buffer size. The dispatcher validates the size,
the handle table enforces endpoint rights, and `CopyIn`/`CopyOut` validate every
mapped page before data crosses the privilege boundary. Native sends may attach
up to four handles. Successful attachment consumes each sender handle; failed
sends leave the sender table unchanged.

`IPCWaitReceive` takes a fixed 24-byte descriptor containing the destination
buffer, its capacity, and a relative timeout in timer ticks. An empty channel
registers the current task in the fixed-capacity wait set and returns the
`Block` action to the interrupt bridge. The bridge converts the kernel-call
frame into the canonical scheduler frame, rewinds RIP to retry the call, and
switches to another runnable task. Message arrival and timer expiry wake the
task. The retry either consumes the queued message or returns the stored typed
timeout status.

`StreamWait` applies the same scheduler handoff to directional byte streams.
Readers sleep while the ring is empty and a writer still exists; writers sleep
while the ring is full and a reader still exists. Data movement, peer closure,
or timeout wakes the registered task, which retries the original operation.

## PID 1 lifecycle channel

The `GINITLIF` version 5 protocol uses a dedicated channel capability installed
only in PID 1. Process-exit notifications carry the PID and typed exit code.
Launch requests and results are correlated, so each pending request is retired
by exactly one reply. Launch requests also carry bounded startup data and may
consume selected PID 1 handles after successful derivation. Endpoint routing is
derived from the endpoint identity stored in each channel capability.

Calls from the interrupt bridge obey Win64's 16-byte stack alignment. This is
explicit rather than dependent on the generated Go function's current stack
frame size.
