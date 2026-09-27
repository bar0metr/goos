# ADR 0116: Dynamic filesystem mount and clean unmount

## Status

Accepted. Extends ADR 0096 and ADR 0115.

## Context

The VFS could attach the persistent FAT32 service only once. Losing the service
left its mounts in place for restart, which is correct failure handling, but
there was no separate clean-unmount operation. Mounting the three aliases was
also not atomic, and no primitive could prove that open files no longer named a
backend before its block slice and service authority were released.

## Decision

`Namespace.UnmountBackend` treats every mount of one backend as a single mount
set. It first scans all open files and returns `StatusBusy` if any refer to that
set; otherwise it removes the set and invalidates the resolver cache in one
operation. Initial attachment rolls back the whole set if any alias fails.

The VFS service clean-unmount path flushes the filesystem before changing the
namespace. A failed flush or busy reference leaves the transport and every
mount intact. Success detaches `/persist`, `/home` and `/state`, clears backend
caches and releases the filesystem transport. Service death remains a distinct
detach-for-restart path: mounts stay present and return `StatusPeerDied` until
the supervised replacement proves it serves the same root.

The VFS control protocol version 6 adds `Unmount`. Only PID 1 owns this control
endpoint, making mount, reattach and unmount authority explicit; ordinary VFS
sessions cannot alter the mount table. Closing the filesystem endpoint after a
successful unmount causes the filesystem process to leave its serving loop and
release its block-slice endpoint through normal handle teardown.

The partition layer additionally walks one MBR extended partition's EBR chain.
Logical partitions remain bounded by the extended container, are checked for
cycles, malformed links and overlap, and use indices starting at five. The
filesystem still receives only the selected block slice and never parses MBR.

## Consequences

- A clean unmount either leaves the complete mount set unchanged or removes it
  after durability and reference checks.
- Failure recovery retains its stable-mount behavior and is not mistaken for
  administrative unmount.
- Logical FAT32 partitions inside an extended MBR partition can be selected by
  index and by the automatic FAT32 policy.
- Forced unmount remains out of scope because it requires explicit
  stale-handle semantics.
