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
UID, GID, link count, inode generation, device number in Linux's 32-bit
encoding, access time and change time, and an availability flag. ext4
fills it from the inode. FAT32, RAMFS and the boot archive leave it
unavailable and zero rather than invent owners. The VFS grants nothing
from these numbers; rights, scopes and roots stay the authority.

**Wire.** `GFSPRT01` version 13 widens the reply header by a 48-byte Unix
record (`ramfs.EncodeUnix`) and adds `ReadLink`, `Symlink` and `Link`. A
link target travels in the file-cache slot the request names, the way
write data does, so a 4095-byte target fits. A service without link
support answers `StatusUnsupported`. `GVF1` version 15 carries the Unix
record in the data area of a stat reply, adds the same three operations,
the `NoFollow` right for open, stat, link and metadata changes, and the
statuses `StatusLoop` and `StatusCrossDevice`. A long `ReadLink` answer
comes back in the object the request attaches, as a long listed name does.
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

**Follow rules.** Open, stat, truncate, listing, filesystem information,
metadata changes and the source of a hard link follow the last component
unless the request carries `NoFollow`. Create, mkdir, unlink, rename,
replace, read-link and symlink act on the last component itself. Create
does not make the target of a dangling link; that differs from Linux
`open(O_CREAT)` and closes the classic `/tmp` link attack.

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
`link`, `linkat` and `O_NOFOLLOW` map onto the operations above. With
Unix metadata a stat reports the stored mode, owner, link count, device
number and times; without it the old presentation stays (0644, 0755, one
link, UID 1000). `access` still answers from the owner bits, as for the
file's owner, and enforces nothing. `getdents64` reports `DT_LNK` for links
and `DT_UNKNOWN` for special files. unlink and rename stat their source
without following it. `utimensat` with `AT_SYMLINK_NOFOLLOW` still
follows the link.

## Consequences

- Every path request now costs one more walk on mounts that can hold
  links. The walk's results land in the path cache, and the remote
  backend answers the per-node kind check from its attribute cache, so an
  ext4 lookup takes no extra exchange.
- The namespace allocates three `PathMax` buffers the first time a mount
  that can hold links appears.
- Paths through links follow the lexical `..` rule, which differs from
  Linux's physical one for `link/..`.
- A crashed ext4 instance still revokes every open handle (ADR 0222);
  links add no recovery path.

## Verification

`go test ./kernel/ramfs ./kernel/vfs ./userspace/fsproto ./userspace/vfs
./userspace/ext4vol ./kernel/compat/linux` cover the wire records, RAMFS
links, and the namespace rules against the `links` fixture that e2fsprogs
1.47.2 builds: chains of 40 and 41 links, loops, dangling and absolute
targets, a `..` escape, rooted sessions, scopes, `/system` reached through
a link, cross-volume links and a restarted instance. `make test-volumes`
mounts the fixture in a guest and runs `abi-probe ext4-links`;
`make test-linux-vfs` runs `abi-probe links` on RAMFS.

The same two probe modes pass on Linux: `userspace/linux/run-on-linux.sh
abi-probe links` on Alpine's tmpfs, where `link` makes a second name, and
`abi-probe ext4-links` in a privileged Alpine container with `links.img`
loop-mounted read-only, where Linux reports `DT_FIFO` for the FIFO GoOS
lists as `DT_UNKNOWN`.
