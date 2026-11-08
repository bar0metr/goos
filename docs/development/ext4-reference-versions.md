# ext4 reference versions

The ext4 reader follows the on-disk format as one Linux release and one
e2fsprogs release define it. Features Linux added after these commits are
unsupported until someone moves the pins and reviews the difference.

| Reference | Version | Commit |
|---|---|---|
| Linux kernel (`fs/ext4`, `fs/jbd2`, `Documentation/filesystems/ext4`) | v6.12 | `adc218676eef25575469234709c2d87185ca223a` |
| e2fsprogs (`mke2fs`, `e2fsck`, `debugfs`, `dumpe2fs`, `tune2fs`) | v1.47.2 | `c3cce4a07efefc62bc7fc57a678cb870af27d0f2` |
| Alpine package that runs the e2fsprogs tools | e2fsprogs 1.47.2 on Alpine 3.22.6 | image `alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8` |

The Linux 6.12 pin matches the kernel `make test-linux-kernel-reference`
boots, so format questions and behaviour comparisons use the same release.

## How the code uses the references

`userspace/ext4` is an independent implementation. It takes on-disk
constants, field offsets and checksum coverage from the kernel documentation
and checks them against the pinned source. It copies no Linux or e2fsprogs
code. Linux ext4 and JBD2 are GPL-2.0-only and GoOS is GPL-3.0-only; adapting
nontrivial source from either project needs a provenance and license review
first.

## Fixtures

`tools/ext4fixture/make-fixtures.sh` runs the pinned Alpine image and builds
every test image with an explicit `mke2fs -O none,<features>` list, so a
change in distribution defaults (`orphan_file`, for example) cannot reach a
fixture. The script refuses to write images when `e2fsck -fn` reports
anything or when the e2fsprogs version differs from the pin.

`mke2fs -d` stamps inode change times and the superblock write time with the
real clock, so two runs produce images that differ in those fields. The
committed images are the reference; `userspace/ext4/testdata/SHA256SUMS`
pins them and the tests refuse an image that does not match.

| Image | Profile | Contents |
|---|---|---|
| `canonical` | writable profile, 4 KiB blocks, `metadata_csum`, `64bit` | files, a 2000-entry indexed directory, hard and symbolic links, a sparse file past 5 GiB, a 400-extent file, extended attributes, an unwritten extent, a non-UTF-8 name |
| `legacy` | `uninit_bg` instead of `metadata_csum`, 32-byte descriptors | the canonical tree |
| `hash-tea`, `hash-legacy` | writable profile, directory hash changed with `tune2fs -E hash_alg` | 1200 names, half with bytes above 0x7f |
| `blockmap-1k` | ext3-style: no extents, no `flex_bg`, 1 KiB blocks | a file reaching triple-indirect blocks, a 300-entry indexed directory |
