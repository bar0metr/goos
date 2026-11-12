# ext4 validation

This page records what tests the ext4 support of GoOS 0.3.0, on which
commit each result was taken, and the limits a reviewer should know. A
result counts only for the commit it names. Host results come from `go
test` on the build machine, TCG results from QEMU on that machine, KVM
results from the Proxmox lab node of docs/development/proxmox-lab.md, and
Linux results from Linux 6.12.115 (the Alpine 3.22 `virt` kernel) under
KVM on the same node.

## Gates

| Gate | What it checks | Where |
|---|---|---|
| `make test-ext4-unit` | the reader against every pinned fixture and its manifest, the feature matrix, 60 damaged images, mutation, journal, recovery, power cuts, the adapter, detection, volume policy, disk composition | host |
| `make test-ext4-stress` | 3000 damaged images; a power cut before and after every event of the crash workload, synced after each operation and batched (ADR 0227) | host |
| `make test-ext4-mutations` | five images the mutation code wrote pass the pinned `e2fsck -fn` and `debugfs`, among them a volume filled to `ENOSPC` and one with 10000 names in an indexed directory and a chain of 200 directories; `e2fsck` replays journals GoOS left after six cuts and finds the tree GoOS recovery finds; GoOS replays journals `debugfs` wrote | host, Docker |
| `make test-volumes` | read-only additional volumes beside the persistent one: configured and shell mounts, refusals, links, metadata a Linux program sees, a replaced instance | TCG |
| `make test-ext4-rw` | read-write volumes: writes, renames, refusals, `sync`, a commit made by the commit interval, a killed instance recovered from its journal, `abi-probe ext4-rw`, the VFS, block service and driver restarted under both volumes with held descriptors revoked, a clean unmount, a guest cut off with a volume mounted; the GoOS reader, GoOS recovery and the pinned `e2fsck` on what the guest left | TCG, Docker |
| `make test-pve-ext4` | `fsbench` five times on a 1 GiB read-write volume with the settled page count after each, 10000 names in one directory, the volume filled to `ENOSPC` and written again, then RAMFS and FAT32 for comparison | KVM |
| `tools/ext4fixture/linux-ext4-reference.sh` | the same `fsbench` on Linux 6.12 | KVM |

`test-all` and `release-check` run every gate above except the two on the
lab node.

## Results

Every result below is for commit `7f2fa2c8` of `feat/ext4-release-hardening`
unless it names another; that commit changes no guest or ext4 code from
`537e90fb`, on which `test-ext4-stress` and `test-volumes` ran.

| Gate | Result |
|---|---|
| `make test-ext4-unit` | pass |
| `make test-partitions` | pass |
| `make test-ext4-mutations` | pass: 5 images clean in `e2fsck -fn` and equal in `debugfs`, 6 cuts replayed by `e2fsck` to the GoOS tree, 3 `debugfs` journals replayed by GoOS to the blocks `e2fsck` writes |
| `make test-ext4-stress` (`537e90fb`) | pass, 265 s: 3000 damaged images; a cut at each of the 1535 points of the synced crash workload and the 847 of the batched one |
| `make test-volumes` (`537e90fb`) | pass |
| `make test-ext4-rw` | pass; the GoOS reader, GoOS recovery and `e2fsck` agree on both written partitions |
| `make test-pve-ext4` | pass, run `20261112T211726Z-b2b675fb` |
| `make release-check` | not run on this branch yet |
| Linux reference | not recorded yet |

On the lab run, the system's settled page count after a `sync` and two
idle seconds was 6263, 6226, 6265, 6234 and 6268 pages over the five
rounds, with the kernel at 1510 pages each time and the ext4 service at
380 to 387 pages. The 10000-name round finished, and so did a chain of
100 directories in every round. `fsbench fill` wrote 922 MiB into the 973
MiB `df` reported free, got `ENOSPC`, saw a further write refused with
`ENOSPC` alone, freed the file, wrote 8 MiB again, and left `df` where it
started.

## Performance

`make test-pve-ext4` on the lab node (KVM, `cpu host`, 4 cores, 1 GiB).
The ext4 column is the median of five rounds on a 1 GiB volume with a 32
MiB journal at `7f2fa2c8`; "first run" is the median of three rounds at
`dca58276`, which batched commits (ADR 0227) but had none of the changes
after it: checksum tables, coalesced writes, the path cache (ADR 0228),
indexed directories and the larger cache (ADR 0229). RAMFS and FAT32 are
one round each at `7f2fa2c8`; RAMFS ran with an 8 MiB data file, since
`/tmp` holds 16 MiB. Times are per operation.

| Workload | ext4 first run | ext4 | ext4, 10000 names | RAMFS | FAT32 |
|---|---|---|---|---|---|
| create, 2000 names in one directory | 2.29 ms | 0.94 ms | 0.93 ms | 0.29 ms | 69.8 ms |
| stat | 1.18 ms | 0.15 ms | 0.17 ms | 0.11 ms | 14.1 ms |
| readdir of the whole directory | 59.7 ms | 14.1 ms | 1.38 s | 48.8 ms | 38.8 ms |
| unlink | 1.01 ms | 0.58 ms | 0.59 ms | 0.14 ms | 27.8 ms |
| 100 nested directories, file, stat, remove | | 0.83 s | 0.83 s | 0.08 s | 3.68 s |
| sequential write, 64 MiB, then fsync | 18.9 MiB/s | 92.1 MiB/s | | 327 MiB/s (8 MiB) | 38.7 MiB/s |
| sequential read | 106.8 MiB/s | 110.8 MiB/s | | 420 MiB/s | 106.2 MiB/s |
| random 4 KiB write, one fsync at the end | 0.61 ms | 0.51 ms | 0.48 ms | 0.15 ms | 28.9 ms |
| random 4 KiB read | 0.62 ms | 0.52 ms | 0.47 ms | 0.20 ms | 9.9 ms |
| 4 KiB write and fsync | 1.82 ms | 1.49 ms | 1.49 ms | 0.21 ms | 0.82 ms |
| write, fsync, rename over a target | 6.55 ms | 3.59 ms | 3.52 ms | 0.56 ms | 6.18 ms |
| 1 process: create, write 4 KiB, unlink | 258 ops/s | 518 ops/s | 496 ops/s | 2081 ops/s | 179 ops/s |
| 8 processes at once | 288 ops/s | 524 ops/s | 518 ops/s | 2062 ops/s | 157 ops/s |

A sparse write at 5 GiB allocated 8 blocks and read back on every ext4
round. At `dca58276` RAMFS took 4.57 ms per create and 2.66 ms per `stat`
in the same scenario; the name lookup change of ADR 0228 removed most of
that.

What the numbers show:

- One ext4 service handles one request at a time, so 2, 4 and 8
  processes reach the throughput of one. That is the design of 0.3.0, not
  a fault.
- `fsync` costs two device flushes on ext4, as the JBD2 commit needs, and
  one on FAT32, which has no journal.
- An ext4 `stat` costs about 1.4 times one on RAMFS: the request to the
  ext4 service and its lookup. A create costs about three times: every
  namespace change empties the VFS path cache, so each create resolves its
  directory again (ADR 0228), and it stages its inode, bitmaps and
  directory leaf.
- Listing 10000 names takes 1.4 s: each listed entry reads its inode,
  and in hash order those reads spread over 10000 inodes, more than the
  1 MiB cache holds.

The same `fsbench` on Linux 6.12 comes from
`tools/ext4fixture/linux-ext4-reference.sh` on the same node; its column
belongs in this table once that run is recorded.

## Resource bounds

Every bound below is a constant in `userspace/ext4` unless named
otherwise. Metadata from the disk cannot raise them.

| What | Bound |
|---|---|
| Resident metadata blocks | 256 in 64 sets of 4, 1 MiB at 4 KiB (`cacheBlocks`, ADR 0229) |
| Metadata blocks one transaction stages | 256 (`maxTxnCredits`), 8 of them kept for inode tables and the superblock |
| File data blocks one transaction holds | 256, 1 MiB at 4 KiB blocks (`maxTxnData`) |
| Freed runs one transaction remembers | 512 (`maxTxnFrees`) |
| Batch size that commits at once | half of each of the three above (ADR 0227) |
| Commit interval | one second (`commitInterval`, `userspace/cmd/ext4`) |
| Extent tree depth | 5 (`extentMaxDepth`) |
| Hash-tree levels | 3 read; one index level written, 507 leaves at 4 KiB (ADR 0229) |
| Directory size written | 2 GiB; directories above 4 GiB are refused on read |
| Symbolic link target | 4095 bytes; 40 links in one VFS lookup (ADR 0223) |
| Journal extents mapped | 1024 (`maxLogRuns`) |
| Revoke records replay keeps | 2^20 (`maxRevokes`) |
| Orphan list walk | 2^20 inodes (`maxOrphanWalk`) |
| Ancestors a rename checks | 2048 (`maxAncestors`) |
| Extended attribute entries read | 256 per inode or block |
| Block groups | 2^20 (`maxGroups`) |
| Block size | 4 KiB at most; 1, 2 and 4 KiB read, 4 KiB written |
| Additional volumes | 4 (`MaxVolumes`, `kernel/vfs`), one disk |

## Limits

- A power cut can tear the superblock's home write, which lies in two
  512-byte sectors. GoOS recovery finds the journal without the
  superblock checksum and replays the committed copy
  (`TestTornSuperblockReplayed`). `e2fsck` 1.47.2 refuses to open such a
  volume until you name a backup superblock with `-b`; Linux ext4 writes
  its superblock the same way.
- Operations are durable after `fsync`, `sync`, `umount`, `reboot`,
  `poweroff` or the one-second commit interval, not when they return
  (ADR 0227).
- Identity checks on restart use the GPT or MBR identity and the ext4
  UUID. A byte-for-byte clone of the disk passes them; the journal, not the
  identity, decides what the volume holds (ADR 0222, ADR 0226).
- A mounted partition must not be open in Linux or another GoOS instance
  at the same time.
