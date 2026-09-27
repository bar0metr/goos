# ADR 0119: Stable filesystem open objects

## Status

Accepted. Completes GoOS 0.1.4 Phase 6 and amends ADR 0096.

## Context

The VFS previously stored the FAT32 node returned by pathname lookup in every
open handle. That node encoded the directory cluster and entry slot. Moving a
file, especially across directories, changed that location and invalidated an
already-open handle.

## Decision

Backends may implement the VFS `ObjectBackend` extension. After pathname
resolution and type checking, `Open` acquires an opaque stable cookie and the
VFS stores that cookie in the open-file entry. `Close` and session teardown
release it. The filesystem protocol version 8 carries the acquire and release
operations. Release returns the object's current pathname location so the VFS
service can rebind coherent attribute and page-cache entries.

FAT32 keeps a fixed table of 256 live objects. A cookie contains a table slot
and generation and never exposes a directory-entry offset as VFS identity.
Rename updates every live object that refers to the moved entry. Generation
checks reject cookies after their final release, and mounting or resetting a
volume invalidates the complete table.

GoOS does not implement Unix delete-while-open semantics in 0.1.4. Unlink of a
file with one or more live objects is refused with `StatusPermissionDenied`.
After the last handle closes, unlink proceeds normally. This avoids orphaned
cluster ownership without adding an on-disk or in-memory deferred deletion
model.

Stable cookies belong to one FAT32 service lifetime. If that service dies,
handles it issued become invalid; after reattachment callers reopen the path.
The earlier ADR 0096 promise that FAT32 handles survive a service restart is
therefore withdrawn. Namespace mounts and pathname access still recover.

## Consequences

- Open handles continue to read and write the same file across rename and
  moves between directories.
- Multiple handles remain independently valid until each is closed; live
  object state is reclaimed on the last close.
- Open-file capacity is bounded explicitly and exhaustion is reported.
- Removing an open file has native, documented refusal semantics rather than
  partial Unix semantics.
- Opening and closing a persistent file each add one filesystem-service
  exchange.
