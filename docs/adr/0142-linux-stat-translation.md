# ADR 0142: Linux stat translation

## Status

Accepted.

## Context

Linux programs ask what a name or a descriptor is through `stat`, `lstat`,
`fstat`, and `newfstatat`, and read the answer from `struct stat`. GoOS files
have a kind, a size, one or two timestamps, a few native attributes, and a
mount. They have no owner, no permission bits, no link count, and no inode
number. The answer still has to be a complete, correctly laid out structure,
and programs compare parts of it: `st_dev` and `st_ino` to learn whether two
names are one file, `st_mode` to tell a file from a directory.

## Decision

The layout is written out field by field from the Linux x86_64 UAPI header:
144 bytes, every one set on each call. Nothing derives from the structures of
the host that builds or tests the kernel. A host test reads each field back by
offsets the test repeats for itself.

| Field | Value |
| --- | --- |
| `st_mode` | `S_IFREG\|0644` for a file, `S_IFDIR\|0755` for a directory; write bits cleared when the VFS reports the read-only attribute |
| `st_uid`, `st_gid` | 1000, the identity the initial stack reports |
| `st_nlink` | 1 |
| `st_dev` | the VFS mount identity |
| `st_ino` | the VFS node identity; 1 when the node has none |
| `st_size` | the VFS size |
| `st_blksize` | 4096 |
| `st_blocks` | the size in 512-byte units, rounded up |
| `st_atime`, `st_mtime`, `st_ctime` | the modification time, else the creation time, else zero |
| `st_rdev` | 0 |

These values are presentation. No GoOS check reads them; authority stays with
capabilities. A link count of 1 for a directory tells programs that count
subdirectories from it to look instead.

**Identity.** VFS protocol version 13 returns a node's identity with a stat
and with a directory entry, in the field an open reply uses for its handle.
Two answers about one file of a mount carry one value, and two files carry
two. For an open file the VFS keeps the identity the path had at open time,
so a stat by handle and a stat by path agree on FAT32, where an open file is
known by a cookie and a path by a directory location. The values are
filesystem node numbers, never kernel addresses.

**What is stat-ed.** A path is stat-ed by name after ADR 0140 resolution; a
path that demands a directory and names a file is `ENOTDIR`. A file
descriptor is stat-ed through its VFS handle, which follows the file through
a rename. A directory descriptor is stat-ed by the path it keeps. A stream
descriptor gets a synthetic record with no VFS exchange: a FIFO with mode
0600, device 1, and an inode that tells the process's streams apart.

**Calls.** `lstat` is `stat`: GoOS has no symbolic links. `newfstatat` accepts
`AT_SYMLINK_NOFOLLOW` and `AT_NO_AUTOMOUNT`, which change nothing, and
`AT_EMPTY_PATH`, which makes an empty path name `dirfd` itself; other flags
are `EINVAL`. The buffer is checked before the VFS is asked, so a bad pointer
is `EFAULT` without an exchange.

## Consequences

Programs can tell files from directories, read sizes, and detect that two
names are one file, including two spellings of one name on case-insensitive
FAT32. Three limits follow from what the filesystems record. After an open
file is renamed, its descriptor still reports the identity of the old name
while the new path reports a new one on FAT32. A program that trusts
`st_mode` to predict access is told 0644 where a later write may still fail.
All three times are one time.

`statx` is not implemented. `getdents64` can report `d_ino` from the same
identity.
