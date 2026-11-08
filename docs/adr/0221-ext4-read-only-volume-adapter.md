# ADR 0221: ext4 read-only volume adapter

## Status

Accepted. Builds on ADR 0220. The ext4 service process and the mount
policy that would start it remain open.

## Context

The VFS talks to filesystem services through the node-ID backend contract
(ADR 0096): a service answers `GFSPRT01` requests by node ID, and
`fsproto.Serve` turns each request into a call on a `Volume`. FAT32 derives
node IDs from directory entry positions. ext4 has real inodes, and Linux
reuses an inode number once the inode is freed, so a raw inode number held
by a VFS handle can end up naming a different file.

The native VFS knows two object kinds, files and directories. ext4 also
stores symbolic links, device nodes, FIFOs and sockets. Following a
symbolic link changes the VFS authority model: the link has to be resolved
under the caller's namespace and root, never by the filesystem service.

## Decision

**Package.** `userspace/ext4vol` adapts a mounted `ext4.FS` to
`fsproto.Volume`, plus the optional batch listing, object, and
filesystem-information interfaces. The format library stays free of
`ramfs` types.

**Node IDs.** A node ID holds the inode number in its low 32 bits and the
inode generation in its high 32. Every call reloads the inode, checks that
the bitmap still marks it in use and that it has links, and compares the
generation. A mismatch, a free inode, or a number past the inode table
returns `StatusNotFound`. A hard link reaches the same node ID through
either name.

**Hidden kinds.** Listings skip symbolic links and device, FIFO and socket
inodes, and opening one by name returns `StatusUnsupported`. They become
visible once the VFS resolves symbolic links itself with bounded expansion
and authority checks on the target.

**Metadata.** `Stat` reports size, modification time and creation time
(from `crtime`, when the inode has room for it). It claims no FAT
attributes. Mode, ownership and link counts wait for a versioned metadata
extension of the protocol.

**Read-only.** Every mutation returns `StatusPermissionDenied`, the status
the contract reserves for read-only backends. `FilesystemInfo` reports the
new `ramfs.FilesystemExt4` type, read-only, with capacity from the
superblock counters, and `fsdisplay` names it `ext4`.

**Errors.** Damage maps to `StatusCorrupt`, device failure to
`StatusIOError`, and refused features or pending recovery to
`StatusUnsupported`. A refused volume answers `Mount` with the status of
its refusal and every other request with `StatusIOError`, as FAT32 does.
A directory entry that names a free inode counts as damage.

**Cursors.** A listing cursor is the byte position of the next directory
record. The read-only volume cannot change under a listing, so cursors stay
valid for the life of the mount.

## Consequences

- PID 1 still starts one filesystem service, FAT32, for the persistent
  volume. Mounting ext4 needs an explicit per-volume filesystem kind in the
  mount configuration and a service manifest; that change touches the boot
  and recovery path and gets its own decision.
- Directories larger than 4 GiB are refused, because the protocol carries
  32-bit cursors.

## Verification

`go test ./userspace/ext4vol` mounts the pinned canonical image, opens a
file and its hard link, reads it, checks its attributes, rejects stale
generations and out-of-range numbers, confirms that names compare as exact
bytes (non-UTF-8 included), pages a 2000-entry directory with small and
large batch budgets through `ramfs.ValidDirBatch`, refuses every mutation,
and serves `Mount`, `Open` and `FilesystemInfo` through `fsproto.Serve`.
