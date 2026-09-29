# ADR 0129: Safe native file replacement

## Status

Accepted.

## Context

An editor that saves by truncating the file and writing it again loses the
original whenever a write fails part way: a full volume, a dead VFS or
filesystem service, a device error. `cp` and `write` work that way today.

The obvious fix writes the new contents to a temporary file and renames it
over the original. The VFS rename cannot do that. RAMFS and FAT32 both refuse
a rename onto an existing name with `StatusAlreadyExists`, and making rename
replace silently would change `mv`, `gfm`'s move and the log service's
rotation, all of which rely on the refusal.

FAT32 has no journal. Whatever the primitive promises after a power loss has to
follow from the order in which it writes sectors and flushes the device.

## Decision

Add one operation, `Replace(source, target)`, to the VFS protocol (version 11)
and to the filesystem-service protocol `GFSPRT01` (version 11), plus an
optional `ReplaceName(id, parent, name)` backend method.

The VFS checks the request before any backend sees it:

- source and target resolve on one mount, otherwise `StatusUnsupported`;
- source and an existing target are regular files, otherwise
  `StatusIsDirectory`;
- a target that any session holds open, including a prepared executable, is
  refused with `StatusBusy`, and so is a mount root;
- a missing target makes the call an ordinary rename;
- a backend without `ReplaceName` answers `StatusUnsupported`, and the system
  image answers `StatusPermissionDenied` like its other mutations.

RAMFS unlinks the target and renames the source in one call. The VFS serves
one request at a time, so no client sees the name missing. The file keeps the
replaced file's creation time.

FAT32 keeps the target's directory entry, with its name, creation time and
attributes, and points it at the source's chain, size and modification time.
Four device flushes order the steps:

1. flush the source's data and FAT links;
2. erase the source's entry and flush;
3. rewrite the target's 32-byte short entry in place and flush;
4. free the target's old chain and flush.

Two entries never share a chain. Power lost before step 3 leaves the old file
under its name and, at worst, the new chain as lost clusters. Power lost after
step 3 leaves the new file and, at worst, the old chain as lost clusters.
`fsck-fat` reports lost clusters and repair frees them. The guarantee assumes
the device writes a sector whole or not at all. A target
with the FAT read-only attribute is refused.

Clients save through `userspace/safesave`: it writes `.<name>.~save` in the
target's directory, closes it and replaces. It removes a stale temporary file
left by an interrupted save first, and removes its own temporary file on every
failure. `utility.SaveFile` connects it to the VFS through the 64 KiB bulk
buffer.

No `fsync` operation is added. `ReplaceName` on FAT32 returns only after the
device has acknowledged its last flush, so a successful save is durable there.
RAMFS keeps nothing across a reboot.

## Consequences

- A save that fails before the replacement leaves the original byte for byte
  as it was. Host tests inject failure at create, open, write, close and
  replace, and a FAT32 test cuts the device at every write of a replacement,
  remounts and checks the target holds one whole version.
- A crash between the steps costs lost clusters, not data. The user sees the
  old or the new file.
- A file held open elsewhere cannot be replaced. This matches FAT32, which has
  no delete-while-open, and keeps open handles naming the contents they
  opened.
- Replacement stays within one filesystem. Saving across filesystems would be
  a copy.
- The atomicity is process-level on RAMFS and sector-level on FAT32. Neither
  is a journal, and GoOS claims neither is one.
