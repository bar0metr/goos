# ADR 0119: Stable filesystem open objects

## Status

Accepted. Amends ADR 0096. ADR 0178 reverses its restart rule: the VFS
acquires new objects for open files from a restarted service.

## Context

The VFS previously stored the FAT32 node returned by pathname lookup in every
open handle. That node encoded the directory cluster and entry slot. Moving a
file, especially across directories, changed that location and invalidated an
already-open handle.

## Decision

Backends may implement the VFS `ObjectBackend` extension. After pathname
resolution and type checking, `Open` acquires an opaque stable cookie and
stores it in the open-file entry. `Close` and session teardown release the
cookie. Filesystem protocol version 8 carries both operations. Release returns
the object's current directory location, which the VFS service uses to rebind
attribute and page-cache entries.

FAT32 keeps a fixed table of 256 live objects. Each cookie contains a table
slot and generation, without exposing a directory-entry offset as VFS
identity. Rename updates the live object that refers to the moved entry.
Reference counts let concurrent handles share one object. The last release
increments its generation and frees the slot. Mounting or resetting a volume
invalidates the table.

GoOS does not implement Unix delete-while-open semantics in 0.1.4. FAT32
returns `StatusPermissionDenied` when a caller tries to unlink a file with a
live object. Unlink succeeds after the last handle closes. This rule prevents
orphaned cluster ownership without a deferred deletion model.

Stable cookies belong to one FAT32 service lifetime. A service crash
invalidates its cookies. Callers reopen the path after PID 1 attaches the new
service. ADR 0119 withdraws the ADR 0096 promise that FAT32 handles survive a
service restart. PID 1 still restores the namespace mounts and pathname
access.

## Consequences

- Open handles continue to read and write the same file across rename and
  moves between directories.
- Multiple handles remain valid until each caller closes its handle. FAT32
  reclaims the live object on the last close.
- VFS and FAT32 enforce fixed open-file limits and report exhaustion.
- Removing an open file has native, documented refusal semantics rather than
  partial Unix semantics.
- Opening and closing a persistent file each add one filesystem-service
  exchange.
