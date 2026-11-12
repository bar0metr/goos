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
| `make test-ext4-stress` | 3000 damaged images; a power cut before and after every event of the crash workload | host |
| `make test-ext4-mutations` | images the mutation code wrote pass the pinned `e2fsck -fn` and `debugfs`; `e2fsck` replays journals GoOS left after six cuts and finds the tree GoOS recovery finds; GoOS replays journals `debugfs` wrote | host, Docker |
| `make test-volumes` | read-only additional volumes beside the persistent one: configured and shell mounts, refusals, links, metadata a Linux program sees, a replaced instance | TCG |
| `make test-ext4-rw` | read-write volumes: writes, renames, refusals, `sync`, a commit made by the commit interval, a killed instance recovered from its journal, `abi-probe ext4-rw`, the VFS, block service and driver restarted under both volumes with held descriptors revoked, a clean unmount, a guest cut off with a volume mounted; the GoOS reader, GoOS recovery and the pinned `e2fsck` on what the guest left | TCG, Docker |
| `make test-pve-ext4` | `fsbench` three times on a 1 GiB read-write volume | KVM |
| `tools/ext4fixture/linux-ext4-reference.sh` | the same `fsbench` on Linux 6.12 | KVM |

`test-all` and `release-check` run every gate above except the KVM ones,
which need the lab.

## Results

RESULTS

## Performance

`make test-pve-ext4` on the lab node (KVM, `cpu host`, 4 cores, 1 GiB),
three rounds of `fsbench` on a 1 GiB volume with a 32 MiB journal; the
table gives the median round. "Before" is commit `dca58276`, which already
batched commits (ADR 0227); "after" is `1e2a345a`, with the checksum,
write coalescing, path cache and RAMFS changes of ADR 0227 and ADR 0228.
Times are per operation.

| Workload | ext4 before | ext4 after | RAMFS after | FAT32 after |
|---|---|---|---|---|
| create (2000 names in one directory) | 2.29 ms | 1.28 ms | 0.27 ms | 68.7 ms |
| stat | 1.18 ms | 0.25 ms | 0.10 ms | 14.1 ms |
| readdir of 2000 entries | 59.7 ms | 53.1 ms | 92.1 ms | 48.7 ms |
| unlink | 1.01 ms | 0.61 ms | 0.14 ms | 28.5 ms |
| sequential write, 64 MiB, then fsync | 18.9 MiB/s | 91.2 MiB/s | (RAMFS full) | 37.1 MiB/s |
| sequential read | 106.8 MiB/s | 112.4 MiB/s | | 109.7 MiB/s |
| random 4 KiB write, one fsync at the end | 0.61 ms | 0.50 ms | | 28.6 ms |
| random 4 KiB read | 0.62 ms | 0.52 ms | | 10.0 ms |
| 4 KiB write and fsync | 1.82 ms | 1.51 ms | | 0.86 ms |
| write, fsync, rename over a target | 6.55 ms | 3.63 ms | | 6.44 ms |
| 1 process, create, write, unlink | 258 ops/s | 552 ops/s | | 175 ops/s |
| 8 processes at once | 288 ops/s | 543 ops/s | | 161 ops/s |

A sparse write at 5 GiB allocated 8 blocks and read back on every round.
Before the RAMFS and VFS changes, RAMFS took 4.57 ms per create and 2.66
ms per `stat` in the same run.

What the numbers show:

- One ext4 service handles one request at a time, so 2, 4 and 8
  processes reach the throughput of one.
- `fsync` costs two device flushes on ext4 and one on FAT32, which has no
  journal.
- On ext4 a `stat` costs about 2.5 times one on RAMFS; the difference is
  the request to the ext4 service and its lookup. A create costs
  about 1.3 ms. Part of that is the walk from the mount: every namespace
  change empties the VFS path cache, so each create resolves its
  directory again (ADR 0228).

The same `fsbench` on Linux 6.12 comes from
`tools/ext4fixture/linux-ext4-reference.sh` on the same node; its numbers
belong in this table once that run is recorded for the release commit.

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
