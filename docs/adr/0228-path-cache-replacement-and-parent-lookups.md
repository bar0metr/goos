# ADR 0228: path cache replacement and parent lookups

## Status

Accepted. Changes the resolved-path cache of `kernel/vfs` and the name
lookup of `kernel/ramfs`. Authority, scopes and invalidation stay as they
were.

## Context

`fsbench` (ADR 0227) under KVM stats 2000 files in one directory. On
RAMFS a `stat` took 2.66 ms and a create 4.57 ms; on a read-write ext4
volume a `stat` took 1.17 ms. Host profiles of the same pattern through
the VFS service found three costs that grow with the directory or with
the number of paths:

- The namespace caches 32 resolved paths and only fills free entries.
  Once 32 paths were cached, every later path missed after comparing
  against all 32 and was never cached.
- A miss walks every component from the mount root, so each name in a
  directory on a filesystem service costs one remote lookup for each
  directory above it as well.
- RAMFS compared a name against each child of a directory while passing
  the 256-byte name by value, copying it once per child.

## Decision

**Replacement.** A full path cache replaces entries with a clock: an
entry a lookup used since the clock last passed it is spared once. A
path already cached is not added twice.

**Parent directories.** A walk that passes the parent directory of the
path it resolves caches that directory too. A later path whose parent is
cached on the same mount looks up only its last component. A parent
cached as a symbolic link, or a path whose last component is a mount
point, takes the full walk as before. Any change to the namespace still
empties the whole cache, so a cached parent never outlives a rename or
an unmount above it.

**RAMFS names.** The lookup passes the name by pointer.

## Evidence

- `TestResolveRemembersTheParentDirectory`: after the first name of a
  directory, each of 96 more names costs one backend lookup, with the
  cache full; a mount point named by a cached directory still resolves
  to its own mount. `TestResolveDoesNotAllocate` still passes.
- Under KVM, before and after (docs/development/ext4-validation.md):
  RAMFS `stat` 2.66 ms to 0.18 ms and create 4.57 ms to 0.33 ms, from the
  name lookup change and ADR 0227's checksum changes together.

## Consequences

A directory with many names costs one lookup per name instead of one per
name and directory above it, and a working set larger than 32 paths keeps
the paths in use. Mutations still drop the whole cache, so a create in a
large directory still walks the path from the mount.
