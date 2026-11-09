# ext4 feature matrix

`userspace/ext4/features.go` holds this table as `Features`, and
`TestFeatureMatrixDocument` fails when the two differ.
`GOOS_UPDATE_MATRIX=1 go test -run TestFeatureMatrixDocument ./userspace/ext4`
rewrites the table below from the code.

A mount takes the lowest support any present feature allows:

- `reject`: the volume does not mount.
- `read-only`: the volume mounts read-only and never read-write.
- `read-write`: the feature belongs to the writable profile.

Bits the table does not list count as unknown. An unknown `incompat` or
`ro_compat` bit rejects the volume. An unknown `compat` bit allows a
read-only mount only, because some compat features (`orphan_file`,
`fast_commit`) change what a writer must maintain.

A read-write mount also needs all of `has_journal`, `extent`, `filetype`
and `metadata_csum`. This build refuses every read-write mount whatever the
features say: writes stay disabled until journal replay, ordered commits and
orphan cleanup exist (ADR 0220).

`needs_recovery` describes the mount state, so negotiation ignores it. The
mount sequence refuses a volume that has it, or a non-empty journal, with a
"needs recovery" error.

<!-- matrix:begin -->
| Feature | Word | Bit | Support | Constraint |
|---|---|---|---|---|
| `dir_prealloc` | compat | `0x00000001` | read-write | allocation hint; never acted on |
| `imagic_inodes` | compat | `0x00000002` | read-only | AFS inodes are not maintained |
| `has_journal` | compat | `0x00000004` | read-write | internal journal only; required for read-write |
| `ext_attr` | compat | `0x00000008` | read-write | existing attributes are preserved |
| `resize_inode` | compat | `0x00000010` | read-write | reserved descriptor blocks are left untouched |
| `dir_index` | compat | `0x00000020` | read-write | hash-indexed directories |
| `lazy_bg` | compat | `0x00000040` | read-only | obsolete lazy group initialization |
| `exclude_inode` | compat | `0x00000080` | reject | snapshot exclusion is not implemented |
| `exclude_bitmap` | compat | `0x00000100` | reject | snapshot exclusion is not implemented |
| `sparse_super2` | compat | `0x00000200` | read-only | backup superblock placement is not maintained |
| `fast_commit` | compat | `0x00000400` | read-only | clean volumes only; fast commits are never replayed |
| `stable_inodes` | compat | `0x00000800` | read-write | inodes are never renumbered |
| `orphan_file` | compat | `0x00001000` | read-only | clean volumes only; the orphan file is not maintained |
| `sparse_super` | ro_compat | `0x00000001` | read-write | backup superblocks in groups 0, 1 and powers of 3, 5, 7 |
| `large_file` | ro_compat | `0x00000002` | read-write | 64-bit file sizes |
| `btree_dir` | ro_compat | `0x00000004` | reject | never implemented by Linux |
| `huge_file` | ro_compat | `0x00000008` | read-write | block counts in filesystem-block units when flagged |
| `uninit_bg` | ro_compat | `0x00000010` | read-only | CRC16 descriptor checksums are verified, not written |
| `dir_nlink` | ro_compat | `0x00000020` | read-write | directory link counts saturate at 1 |
| `extra_isize` | ro_compat | `0x00000040` | read-write | extended inode fields |
| `snapshot` | ro_compat | `0x00000080` | reject | snapshots are not implemented |
| `quota` | ro_compat | `0x00000100` | read-only | quota files are not updated |
| `bigalloc` | ro_compat | `0x00000200` | reject | cluster allocation is not implemented |
| `metadata_csum` | ro_compat | `0x00000400` | read-write | CRC32C metadata checksums |
| `replica` | ro_compat | `0x00000800` | reject | replicas are not implemented |
| `read-only` | ro_compat | `0x00001000` | read-only | the volume asks to be mounted read-only |
| `project` | ro_compat | `0x00002000` | read-only | project quota is not updated |
| `shared_blocks` | ro_compat | `0x00004000` | read-only | shared blocks cannot be written in place |
| `verity` | ro_compat | `0x00008000` | read-only | file data is read; verity metadata is not checked |
| `orphan_present` | ro_compat | `0x00010000` | reject | orphan file cleanup is not implemented |
| `compression` | incompat | `0x00000001` | reject | never implemented by Linux |
| `filetype` | incompat | `0x00000002` | read-write | file type in directory entries; required for read-write |
| `needs_recovery` | incompat | `0x00000004` | read-only | mount state, not a format feature: replay first |
| `journal_dev` | incompat | `0x00000008` | reject | external journal devices are not supported |
| `meta_bg` | incompat | `0x00000010` | reject | meta_bg descriptor placement is not implemented |
| `extent` | incompat | `0x00000040` | read-write | extent-mapped files; required for read-write |
| `64bit` | incompat | `0x00000080` | read-write | 64-bit block numbers and wide descriptors |
| `mmp` | incompat | `0x00000100` | reject | multi-mount protection is not implemented |
| `flex_bg` | incompat | `0x00000200` | read-write | bitmaps and inode tables may sit in other groups |
| `ea_inode` | incompat | `0x00000400` | read-only | large attribute values in inodes are not written |
| `dirdata` | incompat | `0x00001000` | reject | directory entry data is not implemented |
| `metadata_csum_seed` | incompat | `0x00002000` | read-write | checksum seed stored in the superblock |
| `large_dir` | incompat | `0x00004000` | read-only | three-level hash trees are read, not built |
| `inline_data` | incompat | `0x00008000` | reject | inline data is not implemented |
| `encrypt` | incompat | `0x00010000` | reject | encryption is not implemented |
| `casefold` | incompat | `0x00020000` | reject | case-insensitive names are not implemented |
<!-- matrix:end -->

## Volume conditions outside the feature words

| Condition | Behaviour |
|---|---|
| Block size other than 1, 2 or 4 KiB | reject |
| More than 2^20 block groups | reject |
| `s_state` without the clean bit | refuse as needing recovery |
| `needs_recovery` set, or a journal whose `s_start` is not zero | refuse as needing recovery |
| External journal (`s_journal_dev` or `journal_dev`) | reject |
| Superblock, descriptor, bitmap, inode, extent, directory, hash-tree or attribute checksum mismatch | refuse the volume at mount, or fail the operation that read the structure |
| Inode with `inline_data`, encryption or casefold flags | the operation on that inode fails as unsupported |
| Block-mapped (non-extent) file | readable; the writable profile requires extents |
