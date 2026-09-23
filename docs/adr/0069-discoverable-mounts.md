# ADR 0069: Discoverable mounts

## Status

Accepted. Extends ADR 0018.

## Context

`Namespace.Mount` recorded a backend and a root in a mount table, and
`Resolve` picked the longest matching mount path. Every operation that named a
mounted path therefore worked. Nothing else did.

`ReadDir` forwarded straight to the backend owning the directory, and a backend
knows nothing about what is mounted inside it. `/persist` was mounted on the
root, whose backend is RAMFS, and RAMFS has no `persist` entry, so `ls /`
printed `system` and `tmp` and stopped. The persistent volume was reachable
only by typing its path. A namespace that cannot be walked is not a namespace;
it is a set of paths the user has to be told about out of band, which is what
the specification's discoverability goal rules out.

The same gap had a sharper edge. `Unlink` resolved the parent and called the
backend, and a backend cannot refuse on behalf of a mount it cannot see.
Removing an empty directory that a mount points at would leave the mount root
naming storage the backend had freed. The FAT32 volume makes this reachable by
an ordinary command, because a directory on it can be named both through its
own mount and through the whole-volume mount.

## Decision

**A directory listing has two phases.** Cursors below `mountCursor`, which is
`1 << 30`, belong to the backend that owns the directory and mean exactly what
they meant before. When the backend reports that it is exhausted, the scan
continues at `mountCursor` through the mount table and reports the mounts whose
path names a direct child of the directory being listed. Cursors stay opaque to
clients, the reply carries the same `uint32`, and no client changed.

Splitting the cursor space rather than interleaving the two phases keeps the
backend's own cursor semantics untouched. `1 << 30` is above every cursor a
backend can return — RAMFS stops at `MaxNodes`, FAT32 at a directory entry
index — and below the `uint32` the GVF1 reply carries.

**A mount is listed as the directory it is.** Its entry takes its name from the
last component of the mount path and its stat from the mount root, so `ls` marks
it with a trailing slash like any other directory and `stat` on it answers from
the mounted filesystem.

**A name the backend already listed is not listed twice.** Before reporting a
mount, the phase asks the owning backend whether it has an entry of that name.
If it does, the mount is skipped: resolution already prefers the mount, so the
name belongs to one entry, and listing it twice would say otherwise.

**A mount point needs no placeholder.** Nothing has to create a directory in
the parent filesystem for a mount to be visible, which is why `/system` can be
the mount of an image the root filesystem knows nothing about.

**Removal that would orphan a mount is refused.** `Unlink` refuses a path when
a mount sits at or below it, and also when the path resolves to a node some
mount uses as its root. The second check is what covers aliasing: `/persist/home`
and `/home` are the same directory under two names, and only a node comparison
sees that. The answer is permission denied, because the namespace, not the
backend, is what forbids it.

## Alternatives

**Materialising mount points in the parent filesystem.** Creating a real
`persist` directory in RAMFS would make the listing work with no VFS change,
but it puts a writable, removable, renameable stand-in in front of every mount
and makes the parent filesystem's contents depend on mount order. It also does
not work for a read-only parent.

**A separate "list mounts" operation.** A new GVF1 operation would avoid
touching `ReadDir`, but then `ls` has to know about mounts to produce a correct
listing, and so does every other directory walker. The namespace is the only
component that should need that knowledge.

**Interleaving mounts with backend entries.** Reporting mounts first, or
merging them in name order, needs a cursor encoding that carries both positions
and a stable ordering the backends do not promise. Two phases need one bit.

**Leaving removal to the backends.** A backend cannot see mounts, so each would
need the mount table passed in. Enforcing the namespace invariant in the
namespace is one check instead of one per backend.

## Consequences

`ls /` names every mount, so the layout of the system is discoverable by
walking it. `ls /system` shows the boot image and `ls /persist` shows the
volume, without either filesystem being taught about the other.

A directory listing costs one backend lookup per candidate mount in its final
phase, bounded by `MaxMounts`, which is eight. Nothing on the read path
allocates, which a test asserts alongside the existing one for `Resolve`.

`rm` on a mount point reports "read-only filesystem" rather than a generic
failure, because the utilities now distinguish permission denied; see ADR 0070.

Rename is not guarded the same way. Renaming a directory another mount points
at keeps the node ID in both backends, so the mount stays valid and only the
path it is also reachable under changes. That is confusing rather than unsafe,
and it is left for a later change that gives mounts a name of their own.
