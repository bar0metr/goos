# ADR 0114: Complete VFAT names and mirrored FSInfo

## Status

Accepted. Amends ADR 0110 and ADR 0112.

## Context

VFAT permits 255 UTF-16 code units, whose UTF-8 representation can exceed the
old 255-byte native component limit. The old case matcher covered selected
scripts only. The formatter wrote backup FSInfo, but mounts neither validated
nor maintained it. Valid foreign trailing-dot names fell back to an alias.

## Decision

The native component limit is 1024 bytes while `PathMax` remains 4096 bytes.
Existing attached path and name pages carry long values; IPC payloads remain
bounded and only their length fields and protocol versions change. FAT32 alone
enforces 255 UTF-16 code units.

FAT32 validates UTF-8 and UTF-16 surrogate pairs without normalization. Name
matching uses the generated standard-library Unicode simple uppercase table:
one code point maps to one code point, deterministically and without locale.
Creation and rename reject trailing space or dot. A valid foreign on-disk name
with either suffix is listed and opened only by its exact untrimmed spelling,
which prevents two distinct directory entries from collapsing to one native
name. Case-insensitive collisions, including scripts outside the old subset,
are rejected.

Primary and backup FSInfo sectors are validated and compared. The primary is
authoritative when both validate; the backup supplies advisory values when the
primary is invalid. Health reports invalid and differing copies. Mounting does
not write. When at least one copy validated, a later clean sync writes a
canonical, consistent pair. Free counts are still derived from the FAT, never
trusted from FSInfo.

FAT calendar fields remain local-time fields interpreted as UTC because GoOS
has no timezone configuration. Conversion remains inside FAT32.

## Consequences

- Every legal VFAT long name has enough native byte capacity.
- RAMFS and fixed name buffers grow; generic IPC messages do not.
- Unicode lookup behavior is broader and tied to the toolchain's generated
  Unicode tables, not a handwritten script list.
- Foreign trailing names are interoperable without adopting Win32 trimming.
- A clean sync can reconcile FSInfo copies; mount-time diagnostics preserve
  evidence of the state originally observed.
- Linux time syscalls, NTP, timezone databases, DST, partition handling and
  fsck repair policy remain out of scope.
