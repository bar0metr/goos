# ADR 0220: ext4 read-only core and feature contract

## Status

Accepted for the read-only core. The writable profile below is the target
contract; this build refuses read-write mounts.

## Context

GoOS stores everything persistent on FAT32 (ADR 0096). FAT32 has no inode
identity, links, ownership or journal, and a Linux host can only exchange
plain files with it. ext4 has all of these, but its on-disk format changes
with each Linux release, and `mke2fs` defaults change with each e2fsprogs
release. A reader that accepts "ext4" without naming the features it
understands will one day mount a volume it misreads.

Filesystems run outside the kernel (ADR 0084). An ext4 service has to treat
every byte of the volume as hostile input: a crafted extent can point at
another partition's blocks, a crafted directory can loop, and a crafted
count can ask for gigabytes of memory.

## Decision

**Library.** `userspace/ext4` holds the format code as a host-testable
library with no service dependency. It reads through a `Device` with the
same shape as the FAT32 one: byte-offset reads and writes relative to the
granted block slice, `Flush`, and `Capacity`.

**Feature matrix.** `Features` lists every compat, incompat and ro_compat
bit Linux 6.12 defines (docs/development/ext4-reference-versions.md) with
one of three outcomes: reject, read-only or read-write.
`Negotiate` takes the lowest outcome among the present bits and records
the first feature that lowered it, so a refused mount names its cause.
Unknown incompat and ro_compat bits reject the volume; unknown compat bits
allow read-only. docs/development/ext4-feature-matrix.md renders the
table, and a unit test keeps document and code equal.

**Writable profile.** Read-write needs `has_journal`, `extent`, `filetype`
and `metadata_csum` on 4 KiB blocks, and allows `ext_attr`,
`resize_inode`, `dir_index`, `sparse_super`, `large_file`, `huge_file`,
`dir_nlink`, `extra_isize`, `flex_bg`, `64bit`, `metadata_csum_seed`,
`stable_inodes` and `dir_prealloc`. `uninit_bg` volumes, block-mapped
files, 1 and 2 KiB blocks, and the quota, project, verity, `large_dir`,
`ea_inode`, `orphan_file` and `fast_commit` features are read-only.

**No writes yet.** The constant `writeSupported` is false, and `Mount`
refuses `Options{Writable: true}` with `KindReadOnly`. It turns true only
together with JBD2 commit and replay, ordered data, orphan cleanup and the
flush contract across `GBLK001` and virtio-blk. A writer without those
would leave volumes that Linux has to repair.

**Mount sequence.** `Mount` reads the superblock, validates geometry and
checksums, negotiates features, then refuses a volume that needs recovery:
`needs_recovery` set, the clean bit missing from `s_state`, `orphan_present`
set, or a journal superblock with a non-zero `s_start`. The error kind is
`KindNeedsRecovery`. A dirty volume never mounts read-only as if it were
consistent, and the read-only core never replays a journal, since replay
writes. It then checks every group descriptor, its checksum, the bitmap
checksums, and that bitmaps and inode tables lie inside the volume and
clear of the descriptor table. It reads the root inode and the JBD2
superblock (big-endian, with its own checksum) before reporting
`StateReadyRO`. `State` names the full sequence, including the
recovery and writable states, so status output keeps the same names once
those states exist.

**Hostile input.** Every structure is decoded from a slice whose length
was checked first, and every derived offset is checked for overflow and
against the block count, which is checked against the partition size.
Extent walks are bounded by the five-level depth limit and require depth
to drop by one per level, so they cannot cycle. Hash-tree walks are bounded
by the tree depth and, for collision chains, by the directory's block
count. Attribute lists stop at 256 entries. Directory names with NUL or
`/` count as corruption, so no name can smuggle a path separator into the
VFS. Group descriptors are read through a 32-block cache instead of being
held whole, and the group count is capped at 2^20.

**Names.** A name is up to 255 bytes, compared byte for byte, with no case
folding and no UTF-8 requirement.

**Holes and unwritten extents.** Both read as zeros without device reads;
the extent walk reports a hole's length up to the next mapped block, so a
read in the middle of a multi-gigabyte hole costs a few metadata reads.

**Fixtures.** Linux tools, not this code, create the test volumes.
`tools/ext4fixture/make-fixtures.sh` builds them in a pinned container with
explicit feature lists and refuses images `e2fsck -fn` complains about. The
committed images are pinned by SHA-256. Tests compare the full tree,
content hashes, links and attributes against a manifest the same script
wrote from the source directory.

## Consequences

- The reader handles the canonical profile, `uninit_bg` volumes, all
  three Linux directory hashes in their signed variants, and ext3-style
  block maps on 1 KiB blocks. The unsigned hash variants follow the same
  code with a flag, but no fixture covers them because x86 `mke2fs`
  writes signed hashes.
- Mount checks every bitmap checksum, which costs two block reads per
  group. A 2^20-group volume needs two million reads at mount; volumes of
  that size will need a lazier check.
- Open-unlinked inodes, orphan lists and the inode generation used as a
  handle check are visible through `Inode`, but nothing in GoOS yet holds
  ext4 handles across a service restart.
- The service, the VFS protocol extension, symlink resolution under VFS
  authority, and the write path are separate decisions.

## Verification

`go test ./userspace/ext4` mounts every fixture and compares it with its
manifest; looks up every name of indexed directories through the hash tree
for each hash; reads a 5 GiB sparse file at its edges and in the middle of
its holes; and refuses volumes with damaged magic, checksums, descriptors,
bitmaps, inodes, extent blocks, directory blocks or hash-tree nodes, with
unknown or unsupported features, or with pending recovery. It also flips
random bytes in the metadata region and walks the tree, failing on any
panic or a walk that does not finish. `FuzzImageDamage` runs the same walk
under the Go fuzzer.
