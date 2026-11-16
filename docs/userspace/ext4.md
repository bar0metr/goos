# ext4

GoOS reads and writes Linux ext4 volumes in userspace. The format code
lives in `userspace/ext4` (ADR 0220), `userspace/ext4vol` serves it through
the node-ID backend contract the VFS uses for FAT32 (ADR 0221), and
`/bootstrap/ext4` runs it as one process per mounted volume (ADR 0222).
Writes go through the volume's JBD2 journal (ADR 0225). The persistent
volume stays FAT32; an ext4 partition mounts below `/mnt`, read-only unless
you ask for `rw` (ADR 0226):

```text
goos> mount -t ext4 name:linux-data /mnt/linux
goos> mount -t ext4 -o rw name:linux-data /mnt/linux
```

docs/userspace/storage.md describes mounting, detection, and what happens
when an instance fails.

## What the reader accepts

The writable target profile is the one `mke2fs` builds with:

```sh
mke2fs -t ext4 -b 4096 -I 256 \
  -O none,has_journal,ext_attr,resize_inode,dir_index,filetype,extent,flex_bg,sparse_super,large_file,huge_file,dir_nlink,extra_isize,metadata_csum,64bit \
  IMAGE
```

Spell out the feature list. Recent e2fsprogs defaults add `orphan_file`,
which mounts read-only at most. Check the result with `dumpe2fs -h IMAGE`
before you rely on it.

docs/development/ext4-feature-matrix.md lists every feature bit and what
the reader does with it. Older volumes with `uninit_bg`, block-mapped
ext3-style files, and 1 or 2 KiB blocks mount read-only.

## What it refuses

The reader refuses volumes it cannot interpret: unknown incompat or
ro_compat bits, encryption, casefold, `inline_data`, `bigalloc`,
`meta_bg`, external journals, multi-mount protection, and checksum
mismatches in the superblock, group descriptors or bitmaps. A volume Linux
did not unmount cleanly, or one whose journal holds transactions, is
refused read-only as needing recovery. Mount it with `-o rw` once, which
replays the journal, or mount it on Linux or run `e2fsck -fy`, then try
again.

A read-write mount also needs 4 KiB blocks, an internal journal and a
device whose flush succeeds; 1 and 2 KiB block volumes stay read-only.

Damage found after the mount, in an inode, extent block, directory block or
hash-tree node, fails the operation that read it with a corrupt-metadata
error.

## What a client sees

- Regular files and directories, with sizes, modification times and
  creation times.
- The mode, owner, link count, inode number, allocated blocks, device
  number and access and change times each inode stores (ADR 0223). A Linux program sees them in `stat`; GoOS
  grants nothing from them.
- Names exactly as Linux stored them: up to 255 bytes, case-sensitive, not
  necessarily UTF-8.
- Hard links as one object under several names, with their link count.
- Sparse files and preallocated (unwritten) ranges read as zeros.
- Symbolic links, followed by the VFS under the caller's root and scopes,
  at most 40 in one lookup (ADR 0223). `stat` in the shell and `lstat` in a
  Linux program describe the link itself; `readlink` returns its target.
- Device nodes, FIFOs and sockets in listings and `stat`. A listing gives
  their type as unknown (`DT_UNKNOWN`); `stat` gives the exact one.
  Opening one fails: GoOS has nothing behind them.
- On a read-only mount every mutation fails with permission denied, the
  read-only backend status, including new symbolic and hard links.

## Writing

On a read-write mount you can create, write, truncate, rename, replace and
remove files and directories, and make symbolic and hard links. New files
get mode 0644 and new directories 0755, owned by user and group 0; GoOS
grants nothing from those bits (ADR 0223). Marking a file read-only clears
its write bits. Extended attributes stay as Linux left them, and GoOS
cannot change owners or other mode bits.

Operations commit through the journal in ordered-data mode, in batches
(ADR 0227). An operation that returned is durable once `fsync`, the shell's
`sync`, `umount`, `reboot` or `poweroff` returns, or after the service's
one-second commit interval; a crash before that loses it, as on Linux. An
`fsync` that returned is never lost. A file you remove while it is open keeps its data until you close
it. `umount` empties the journal and marks the volume clean, so Linux and
the read-only mount accept it. If the service or the machine dies first,
the next read-write mount, Linux, or `e2fsck -fy` replays the journal.
Do not mount one partition in GoOS and Linux at the same time.

## Tests

`make test-ext4-unit` runs the reader and adapter tests against images that
e2fsprogs 1.47.2 built (docs/development/ext4-reference-versions.md).
`make test-ext4-stress` repeats the random-damage walk 3000 times and
cuts power before and after every event of the crash workload, once with a
sync after each operation and once batched; `test-all` and
`release-check` run it.
`make test-volumes` boots a disk with the installed system and three Linux
ext4 partitions and checks mounting, the refusals, symbolic links, the
metadata a Linux program sees (`abi-probe ext4-links`), and a replaced
instance.
`make test-ext4-rw` mounts two of them read-write, writes through the
shell, kills one instance, unmounts the other, cuts the guest off, and
checks both partitions with the GoOS reader and with the pinned `e2fsck`
and `debugfs`.
`tools/ext4fixture/make-fixtures.sh` rebuilds the images; it needs Docker.
`make test-ext4-mutations` writes images through the mutation code and
checks each one with the pinned `e2fsck -fn` and `debugfs`, has `e2fsck`
replay journals GoOS left after a simulated power cut, and has GoOS replay
journals `debugfs` wrote; it needs Docker too, and `DOCKER_FLAGS` passes extra `docker run` options such as
`--dns`.
