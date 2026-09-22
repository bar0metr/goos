# ADR 0060: FAT32 subdirectories

## Status

Accepted.

## Context

The FAT32 backend at `/persist` served only regular files in a one-cluster root
directory. Node IDs were the root entry index plus two, so no ID could name an
entry anywhere else, and `mkdir` failed. Users need to organise persistent data
into directories, and the VFS already resolves paths one component at a time
through `OpenName`, so only the backend was missing.

## Decision

- A node ID is `key<<16 | (index+2)`. The key is 0 for the root and otherwise
  the first cluster of the containing directory; the index is the entry
  position in that directory. ID 1 remains the root node. Root entries keep
  their existing IDs, and an ID is stable for as long as its entry exists.
- Every directory, the root included, is at most four one-sector clusters (64
  entries). A directory grows by one zeroed cluster when it has no free slot.
- `mkdir` allocates and zeroes one cluster, writes `.` and `..` (`..` is 0 for
  a root parent), sets the directory attribute, links the entry, and flushes.
- Unlink removes a directory only when it holds nothing but `.` and `..`, and
  frees its chain. Rename works within one directory; moving between
  directories returns invalid argument.
- Each operation walks a directory chain with a bound of four clusters. A chain
  that is longer, loops, reaches a free cluster, or leaves the data area is
  reported as a resource error instead of being followed. File chains are
  bounded by the file size and validated against the data area, and freeing a
  chain clears each link before following it, so a loop ends.
- Names stay 8.3; long-file-name entries are skipped, never created.

## Alternatives

- **A node table in memory.** It would allow unbounded IDs but needs
  allocation, eviction, and invalidation when entries move, and IDs would not
  survive a remount.
- **The byte offset of the entry on disk as the ID.** It is compact, but a
  stale ID could not be checked against a directory chain, so a reused cluster
  could be read as a directory.
- **Unbounded directory size.** Scans and chain walks would lose their fixed
  cost bound for little benefit on a 64 KiB-file filesystem.

## Consequences

- `mkdir`, `ls`, create, write, truncate, read, and remove work under
  `/persist` at any depth that the VFS path length allows, and the data
  survives a reboot.
- A directory holds at most 62 entries (64 in the root). Create beyond that
  returns resource exhausted.
- Moving an entry between directories is unsupported until a later change
  also rewrites `..` for moved directories.
- An ID whose entry was deleted and whose slot was reused names the new entry,
  as it did before.
