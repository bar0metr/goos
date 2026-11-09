# ext4

GoOS reads Linux ext4 volumes in userspace. The format code lives in
`userspace/ext4` (ADR 0220), `userspace/ext4vol` serves it through the
node-ID backend contract the VFS uses for FAT32 (ADR 0221), and
`/bootstrap/ext4` runs it as one process per mounted volume (ADR 0222).
Nothing writes ext4 yet. The persistent volume stays FAT32; an ext4
partition mounts read-only below `/mnt`:

```text
goos> mount -t ext4 name:linux-data /mnt/linux
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
refused as needing recovery. Mount it on Linux once, or run
`e2fsck -fy`, then try again.

Damage found after the mount, in an inode, extent block, directory block or
hash-tree node, fails the operation that read it with a corrupt-metadata
error.

## What a client sees

- Regular files and directories, with sizes, modification times and
  creation times.
- The mode, owner, link count, device number and access and change times
  each inode stores (ADR 0223). A Linux program sees them in `stat`; GoOS
  grants nothing from them.
- Names exactly as Linux stored them: up to 255 bytes, case-sensitive, not
  necessarily UTF-8.
- Hard links as one object under several names, with their link count.
- Sparse files and preallocated (unwritten) ranges read as zeros.
- Symbolic links, followed by the VFS under the caller's root and scopes,
  at most 40 in one lookup (ADR 0223). `stat` in the shell and `lstat` in a
  Linux program describe the link itself; `readlink` returns its target.
- Device nodes, FIFOs and sockets in listings and `stat`. Opening one fails:
  GoOS has nothing behind them.
- Every mutation fails with permission denied, the read-only backend
  status, including new symbolic and hard links.

## Tests

`make test-ext4-unit` runs the reader and adapter tests against images that
e2fsprogs 1.47.2 built (docs/development/ext4-reference-versions.md).
`make test-ext4-stress` repeats the random-damage walk 3000 times.
`make test-volumes` boots a disk with the installed system and three Linux
ext4 partitions and checks mounting, the refusals, symbolic links, the
metadata a Linux program sees (`abi-probe ext4-links`), and a replaced
instance.
`tools/ext4fixture/make-fixtures.sh` rebuilds the images; it needs Docker.
