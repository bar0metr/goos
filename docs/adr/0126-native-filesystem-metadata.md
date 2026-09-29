# ADR 0126: Native filesystem metadata

## Status

Accepted.

## Context

The VFS backend contract and both filesystem protocols returned only object
kind and size. FAT32 already decoded creation and modification times and the
read-only, hidden, system and archive bits, but those values stopped at the
filesystem implementation. RAMFS and the system image had no explicit way to
say that a timestamp was unavailable. Applications therefore could not show
metadata without learning filesystem-specific formats or inventing POSIX
ownership and mode bits.

## Decision

`ramfs.Stat` is the internal native metadata record shared by all VFS
backends. It contains kind, 64-bit size, optional UTC creation and modification
timestamps, native attributes and an explicit attribute-availability bit.
Each timestamp is signed Unix seconds plus nanoseconds and has its own
availability bit. An unavailable value is all zero; Unix epoch remains a valid
available value.

The native attributes are directory, read-only, hidden, system and archive.
They describe stable GoOS semantics. Raw FAT attribute bytes, Unix uid/gid and
Unix mode bits are not part of the API.

The filesystem-service protocol `GFSPRT01` version 9 carries the fixed-width
metadata between FAT32 and VFS. The application VFS protocol `GVF1` version 9
carries the same metadata plus a non-authorizing mount ID assigned by the VFS
namespace. Both decoders reject unknown flags, reserved bytes, invalid
nanoseconds and attribute bits outside the defined set.

FAT32 converts its directory-entry timestamps and attributes at its boundary.
RAMFS timestamps mutations from the native realtime clock; tests or early
startup without a clock report time as unavailable. The immutable system image
reports read-only attributes and unavailable timestamps because its current
archive reader does not expose a validated timestamp. Missing data is never
replaced with the Unix epoch.

Directory listings carry the same metadata as `stat`. The remote-backend cache
retains size needed for EOF checks but marks modification time unavailable
after a write until an authoritative filesystem reply refreshes it.

## Consequences

- `stat` reports modification time, native attributes and mount identity.
- Long names keep using the existing attached-name transports; the larger
  fixed headers only reduce the inline-name threshold.
- No metadata operation allocates according to filesystem-provided lengths.
- Protocol version 8 peers are rejected deterministically.
- Mount IDs describe namespace placement and grant no handle or capability.
- Filesystem capacity and mount enumeration build on this record in ADR 0127
  without changing its meaning.
