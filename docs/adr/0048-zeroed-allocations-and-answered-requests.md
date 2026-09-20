# ADR 0048: Zeroed allocations and answered storage requests

## Status

Accepted.

## Context

Reading a file from the persistent FAT32 mount could freeze the shell
indefinitely, and persistent-storage commands were slow.

The freeze had one root cause. Every freestanding userspace image replaces
`runtime.alloc` with an arena bump allocator that never cleared the memory it
handed out. Go guarantees zeroed allocations, and the compiler relies on that
guarantee when it skips clearing a large composite literal. Because each service
resets its arena on every request, a fresh `ipc.Message` could therefore inherit
bytes from an earlier one. A block request encoded that way carried a non-zero
reserved byte, the block service correctly rejected it as malformed, and then
dropped it in silence. The VFS service waited for a reply that no longer
existed, the utility waited on the VFS service, and PID 1 and the shell waited
behind them. With client deadlines set to 100000 ticks of a 100 Hz timer, the
stall lasted about sixteen minutes and looked permanent.

The slowness had a separate cause: the FAT32 backend issued one device request
per metadata field, so a directory scan cost one round trip through the VFS,
block service, and driver for each 32-byte entry.

## Decision

Clear every allocation in the freestanding arena allocators. This restores the
language guarantee the compiler depends on and removes the whole class of stale
data leaking between values.

Encode block requests into an explicitly cleared payload, as replies already
were, so the reserved-byte invariant holds locally instead of depending on
allocator behaviour.

Require services to answer requests they cannot parse. The block service, the
virtio-blk driver, and the VFS service now reply with an invalid-argument status
whenever the correlation and operation are recoverable. Silence is reserved for
frames that cannot be attributed to a caller at all.

Bound every storage wait. One block exchange and one VFS call are limited to a
few seconds of timer ticks and report an I/O error on expiry, so a stalled peer
degrades into a reported failure rather than an apparent hang.

Give the FAT32 backend a small write-through resident-sector window. Directory
entries and FAT links are packed many to a sector, so the window removes
repeated device traffic for metadata scans while writes still reach the device
immediately and keep the resident copy coherent.

## Alternatives

Only fixing the block protocol's reserved bytes would have left every other
allocation in every service exposed to stale memory. Only adding deadlines would
have converted the hang into a slow failure without addressing the corruption.
A read-back cache without write-through would have risked serving superseded
bytes from a filesystem whose whole purpose is persistence.

## Consequences

Allocation now costs a clear proportional to the allocation size; allocations in
these services are small and bounded, and correctness outweighs that cost.
Storage errors surface as bounded failures rather than freezes. The resident
window is four sectors, chosen to hold the boot, FAT, directory, and data
sectors that a metadata operation touches.

Command latency is still dominated by roughly half a second of process-launch
cost, which measurement attributes to the 100 Hz timer granularity rather than
to storage. That is recorded as the next performance item and is not addressed
here.
