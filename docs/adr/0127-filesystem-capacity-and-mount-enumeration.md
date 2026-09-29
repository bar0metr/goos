# ADR 0127: Filesystem capacity and mount enumeration

## Status

Accepted.

## Context

Native metadata identified the mount that supplied an object, but applications
could not ask what filesystem that mount used, whether it was writable, or how
much storage it had. Inferring those properties from paths, FAT structures or
mutation failures would leak backend details and produce inconsistent answers.

## Decision

Every mounted backend may return a bounded `FilesystemInfo` record: stable
filesystem type, 64-bit total, free and available byte counts, read-only state,
an explicit capacity-availability bit, and an optional label of at most 64
bytes. VFS answers the query by canonical path or by mount ID, but refuses a
request containing both selectors.

VFS also exposes its fixed eight-slot mount table as a cursor-based sequence.
Each `MountInfo` contains only a generation-safe mount ID, canonical mount path,
filesystem type and read-only state. Capacity remains available through the
separate information query. An entry contains no backend object, endpoint or
capability. Reusing a
slot advances its generation, so an ID retained across unmount cannot describe
the replacement mount.

RAMFS reports its configured page quota and current allocated pages. The
immutable system image reports its archive byte length with zero free and
available bytes. FAT32 multiplies validated cluster geometry with checked
64-bit arithmetic and obtains the free count from the FAT on first use; the
advisory FSInfo count is never authoritative.

GoOS currently has no per-user storage reservation. RAMFS and FAT32 therefore
report `available_bytes` equal to `free_bytes`.

`GFSPRT01` and `GVF1` version 10 carry the new records. Their decoders reject
unknown filesystem types and flags, nonzero reserved bytes, impossible capacity
relations, overlong labels and successful replies without a filesystem type.

## Consequences

- Capacity above 4 GiB is represented without truncation.
- Reported read-only state comes from the backend that enforces mutations.
- Mount enumeration is deterministic and bounded by the namespace table.
- Mount IDs remain descriptive selectors and never authorize file operations.
- User-facing `df` and read-only `mount` commands consume only a VFS session;
  they receive no device or backing-service authority.
