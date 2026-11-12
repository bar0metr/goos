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

PERFORMANCE

## Resource bounds

Every bound below is a constant in `userspace/ext4` unless named
otherwise. Metadata from the disk cannot raise them.

| What | Bound |
|---|---|
| Resident metadata blocks | 32 (`cacheBlocks`) |
| Metadata blocks one transaction stages | 256 (`maxTxnCredits`), 8 of them kept for inode tables and the superblock |
| File data blocks one transaction holds | 256, 1 MiB at 4 KiB blocks (`maxTxnData`) |
| Freed runs one transaction remembers | 512 (`maxTxnFrees`) |
| Batch size that commits at once | half of each of the three above (ADR 0227) |
| Commit interval | one second (`commitInterval`, `userspace/cmd/ext4`) |
| Extent tree depth | 5 (`extentMaxDepth`) |
| Hash-tree levels | 3 |
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
