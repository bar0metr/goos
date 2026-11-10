# ADR 0223: Symbolic links, hard links and Unix metadata

## Status

Accepted. Extends ADR 0126 (native metadata), ADR 0096 (filesystem
service protocol), ADR 0221 (ext4 adapter) and ADR 0222 (volume
instances). Writes to ext4 are still refused.

## Context

ext4 stores symbolic links, hard links, device nodes, FIFOs and sockets,
and keeps a mode, an owner, a link count and three times for every inode.
The ext4 adapter hid all of it: listings skipped links and special files,
and a stat reported a size and two times.

A symbolic link changes the authority model. Its target is a path, and a
path decides which mount, which root and which protected directory a
request reaches. A filesystem service sees node IDs and single names; it
cannot know the caller's root or scopes, so it must never follow a link.

## Decision

**Kinds.** `ramfs.Kind` gains `KindSymlink` and `KindSpecial`. A special
node is listed and described, and opening one is `StatusUnsupported`
(Linux `ENXIO`). Its exact type lives in the Unix mode.

**Unix metadata.** `ramfs.Stat` gains a `Unix` block: mode with type bits,
UID, GID, link count, inode number and generation, allocated 512-byte
blocks, device number in Linux's 32-bit encoding, access time and change
time, and an availability flag. ext4 fills it from the inode and reports a
directory's `i_size` as its size. FAT32, RAMFS and the boot archive leave
it unavailable and zero rather than invent owners; Linux then sees the
synthetic presentation below. The VFS grants nothing from these numbers;
rights, scopes and roots stay the authority.

**Node ID and inode number.** The VFS node ID identifies an object for the
VFS: open files, scopes and stale-handle checks compare it. For ext4 it is
`generation << 32 | inode`, so a handle that outlives its inode cannot
reach a file that later reuses the number. Linux `st_ino` is the inode
number alone, taken from `Unix.Inode`, and directory records carry the same
number for `d_ino`. Two volumes stay apart through `st_dev`, the mount
identity. Once ext4 writes, a freed and reused inode keeps its `st_ino` and
gets a new node ID; nothing in the VFS may use `st_ino` as identity.

**Wire.** `GFSPRT01` version 13 widens the reply header by a 64-byte Unix
record (`ramfs.EncodeUnix`) and adds `ReadLink`, `Symlink` and `Link`. A
link target travels in the file-cache slot the request names, the way
write data does, so a 4095-byte target fits. A service without link
support answers `StatusUnsupported`. `GVF1` version 15 carries the Unix
record in the data area of a stat reply, adds the same three operations,
the `NoFollow` right for open, stat, link and metadata changes, and the
statuses `StatusLoop` and `StatusCrossDevice`. A directory-batch record
carries the 32-bit inode number in bytes once reserved. A long `ReadLink` answer
comes back in the object the request attaches, as a long listed name does.
`GVF1` version 16 adds bounded open-intent flags to `OperationOpen` in the
request-flags word. Transport flags and open flags are validated separately;
open flags on any other operation and unknown flags are refused.
Peers of different versions refuse each other's messages.

**Resolution.** The VFS resolves links before it authorizes or performs a
request. `Namespace.Expand` walks the canonical path; on a mount whose
filesystem can hold links (RAMFS and ext4) it checks each node's kind.
When it meets a link it reads the target and splices it in:

- a relative target continues from the directory holding the link;
- an absolute target starts at the caller's root, so a session rooted at
  `/home/u` that meets `/docs` reaches `/home/u/docs`;
- `..` applies to the spelled path, as it always has in this namespace,
  so a `..` after a link never climbs through the link's target;
- after 40 expansions the request fails with `StatusLoop` (`ELOOP`).

The caller must be allowed to read every link it follows, and every
intermediate path must lie inside its root; a rooted session gets
`StatusPermissionDenied` before it can learn what a link outside its root
points to. The expanded path then goes through the existing checks:
roots, scopes, reserved mount points and the node-based protection of
`/system`. What the VFS authorizes is what it changes.

A walk that reaches a link it was not told to expand stops with
`StatusNotDirectory`, as a backend would answer, so a code path that
skips expansion fails closed.

**Follow rules.** Open, stat, truncate, listing, sync, filesystem
information, metadata changes and the source of a hard link follow the
last component unless the request carries `NoFollow`. Create follows it
too, so `open(O_CREAT)` through a dangling link makes the target, as on
Linux; the target faces the caller's root and scopes, and a rooted
session cannot create outside its root that way. `O_EXCL` and `O_NOFOLLOW`
leave the final link unexpanded, so the atomic open sees the name itself.
An existing link ends `O_EXCL` with `EEXIST` and `O_NOFOLLOW` with `ELOOP`,
and its target is never made. Mkdir, unlink, rename, replace, read-link and
symlink act on the last component itself.

**Atomic Linux open.** Linux file open is one serialized VFS namespace
transaction. Path expansion, authority checks, lookup, optional creation,
optional truncation, stable-object acquisition and VFS handle installation
occur without another namespace mutation being admitted between them. The
Linux bridge reserves its descriptor and sends one `OperationOpen` carrying
`OpenCreate`, `OpenExclusive`, `OpenTruncate`, `OpenDirectory` and trailing-
slash intent; it no longer composes `Stat`, `Create` or `Truncate` requests.
The standalone native `OperationCreate` and `OperationTruncate` remain valid.
`O_CREAT` requires mutation authority only when the name is missing; opening
an existing file requires the requested descriptor rights. `O_TRUNC` requires
mutation authority.

If descriptor installation fails after the VFS has opened a file, the bridge
waits for a protocol close and returns the original error on success. On timeout,
Linux closes the family VFS channel and returns the allocation error; the VFS
session teardown closes its handles and releases backend objects. A failed close
returns a VFS error. Release of external backend references after VFS service
death remains unverified. A created pathname may remain: resource cleanup does
not roll back namespace mutations.

The VFS may issue separate synchronous backend `OpenName`, `CreateName`,
`AcquireObject` and `Truncate` calls, but they execute inside the one serial
dispatch. A created node is acquired directly from the identity returned by
`CreateName`; an existing file is acquired before truncation, and that same
object is installed in the handle. A failed acquisition is not followed by a
second pathname lookup, and a failed truncation releases the acquired object
without installing a handle. M3 may replace these backend calls with
transactional ext4 mutation primitives without changing the Linux/VFS
contract. ext4 remains read-only in M2.

Directory descriptors remain pathname-backed rather than stable VFS directory
objects. This decision does not provide Linux directory-FD rename semantics.

**Cross-filesystem moves.** Rename and replace between two backends are
`StatusCrossDevice` (`EXDEV`). Two mounts of one backend, `/home` and
`/persist/home`, are one filesystem: a rename between them works, and the
open-file and held-file checks compare backends, not mount slots.

**Sync.** A sync names an open file by handle or a file or directory by
path, and the VFS asks only the backend that owns it, through
`SyncBackend`. A remote backend sends a flush to its own service: FAT32
writes its device, and a read-only ext4 volume, which has nothing pending,
answers at once. RAMFS and the boot archive succeed, since nothing they
hold outlives the VFS or changes. A backend without `SyncBackend` answers
`StatusUnsupported`, and Linux `fsync` returns `EINVAL`. ext4 gets a real
file and directory sync with its journal.

**Hard links.** A hard link must stay inside one filesystem instance: two
mounts of one backend qualify (`/home` and `/persist/home`), two backends
do not (`StatusCrossDevice`, `EXDEV`). A directory cannot be linked. A node
inside the protected system directory cannot gain a name outside it,
because protection follows the walk and a new name would escape it. No
backend in this build makes hard links: RAMFS and FAT32 answer
`StatusUnsupported` (`EPERM`) and ext4 is read-only (`EROFS`). Linux-made
hard links on ext4 share one node ID and report their link count.

**RAMFS.** RAMFS stores a symbolic link in one page. It is the writable
backend that lets `/tmp` hold links, which the Linux tests use.

**Linux ABI.** `lstat`, `newfstatat` and `statx` with
`AT_SYMLINK_NOFOLLOW`, `readlink`, `readlinkat`, `symlink`, `symlinkat`,
`link`, `linkat` (with and without `AT_SYMLINK_FOLLOW`), `O_NOFOLLOW` and
`utimensat` with `AT_SYMLINK_NOFOLLOW`, which changes the link itself, map
onto the operations above. `fsync` and `fdatasync` on a file or a
directory reach the owning backend. With Unix metadata a stat reports the
stored mode, owner, link count, inode number, allocated blocks, device
number and times; without it the old presentation stays (0644, 0755, one
link, UID 1000, blocks from the size). `access` still answers from the
owner bits, as for the file's owner, and enforces nothing. unlink and
rename stat their source without following it.

`getdents64` reports `DT_LNK` for links and `DT_UNKNOWN` for device nodes,
FIFOs and sockets, where Linux reports `DT_CHR`, `DT_BLK`, `DT_FIFO` and
`DT_SOCK`. Linux allows `DT_UNKNOWN`; `lstat` gives the exact type. Until
the native kinds name each special type, listings are not field-for-field
equal to Linux.

## Consequences

- Every path request now costs one more walk on mounts that can hold
  links. The walk's results land in the path cache, and the remote
  backend answers the per-node kind check from its attribute cache, so an
  ext4 lookup takes no extra exchange.
- The namespace allocates three `PathMax` buffers the first time a mount
  that can hold links appears.
- Paths through links follow the lexical `..` rule, which differs from
  Linux's physical one for `link/..`.
- `DT_UNKNOWN` for special files, above.
- A crashed ext4 instance still revokes every open handle (ADR 0222);
  links add no recovery path.
- A created file may remain if stable-object acquisition fails after the
  backend created it. Transactional mutation and rollback belong to M3/M4.

## Verification

`go test ./kernel/ramfs ./kernel/vfs ./userspace/fsproto ./userspace/vfs
./userspace/ext4vol ./kernel/compat/linux` cover the wire records, RAMFS
links, and the namespace rules against the `links` fixture that e2fsprogs
1.47.2 builds: chains of 40 and 41 links, loops, dangling and absolute
targets, a `..` escape, rooted sessions, scopes, `/system` reached through
a link, cross-volume links and renames, `NoFollow` metadata changes,
create through a dangling link, sync traffic per backend, and a restarted
instance. The fixture gives `file.txt` and `dir` nonzero generations and
holds a sparse file; `links.inodes.tsv` records what debugfs reads back.
`make test-volumes` mounts the fixture in a guest, runs
`abi-probe ext4-links`, and replaces the links instance while
`abi-probe hold` keeps a descriptor open through a link: the other volumes
keep serving, the old descriptor fails, and a new open works after PID 1
checked the replacement's identity. `make test-linux-vfs` runs
`abi-probe links` and `abi-probe open-intent` on RAMFS. The latter checks
existing-file creation intent, exclusive creation, truncation, read-only
creation and creation through a dangling link.

The probe modes can also run on Linux: `userspace/linux/run-on-linux.sh
abi-probe links` and `abi-probe open-intent` on Alpine's tmpfs, and
`abi-probe ext4-links` in a privileged Alpine container with `links.img`
loop-mounted read-only, where Linux reports `DT_FIFO` for the FIFO GoOS
lists as `DT_UNKNOWN`.
