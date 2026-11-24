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
| `make -j3 test-ext4-concurrency` | read-job name ownership, serialized block transport, realistic shared-FS reads under the race detector, same-set extent and indirect traversal, serial/concurrent service logical equivalence | host |
| `make -j3 test-ext4-read-equivalence` | the serial and goroutine service loops answer the same deterministic read-only fsproto workload with equal logical replies | host |
| `make -j3 test-ext4-read-restart` | four reader processes, a synchronized active-worker fault, descriptor revocation, replacement and correct reads after recovery; guest images pass recovery and `e2fsck` | TCG, Docker |
| `make test-pve-ext4` | `fsbench` five times on a 1 GiB read-write volume with the settled page count after each, 10000 names in one directory, the volume filled to `ENOSPC` and written again, then RAMFS and FAT32 for comparison | KVM |
| `tools/ext4fixture/linux-ext4-reference.sh` | the same `fsbench` on Linux 6.12 | KVM |

`test-all` and `release-check` run `test-ext4-concurrency`, including its
`test-ext4-read-equivalence` prerequisite, alongside the pre-M3 host and TCG
gates. `release-check` also runs `test-ext4-read-restart` after `test-ext4-rw`.
Both aggregates run their gates sequentially. Lab-node measurements remain
separate from these aggregates.

## Results

The KVM and Linux results below are for commit `7afa2f63` of
`release/0.3.0`, which sets the version to 0.3.0. The release candidate
differs from it in documentation and in the host scripts of
`tools/ext4fixture`, which now retry the e2fsprogs install; guest and
ext4 code are the same. The GitHub release of `v0.3.0` names its commit. The host and TCG gates below ran on the
release-candidate tree.

| Gate | Result |
|---|---|
| `make test-ext4-unit` | pass |
| `make test-ext4-mutations` | pass |
| `make test-ext4-stress` | pass |
| `make test-ext4-rw` | pass |
| `make pve-doctor` | pass, PVE 7.4-16, node `suptests` |
| `make test-pve-ext4` | pass, run `20261119T191527Z-11bb3848`, cleanup ok |
| Linux reference | Linux 6.12.115 (Alpine 3.22 `virt`), five rounds, every round finished |

`make release-check` on the tagged commit is the release gate; its
result goes in the GitHub release, since writing it here would change the
commit it ran on.

On the lab run, the system's settled page count after a `sync` and two
idle seconds was 6256, 6264, 6267, 6229 and 6265 pages over the five
rounds, with the kernel at 1517 pages each time and the ext4 service at
371 to 382 pages. After the 10000-name round and the fill, the system
held 6367 pages and the ext4 service 483. The 10000-name round finished:
creating them took 0.93 ms each and listing them 1.44 s. A chain of 100 directories finished in every
round in 0.80 to 0.91 s. `fsbench fill` wrote 922 MiB into the 973 MiB
`df` reported free, got `ENOSPC`, saw a further write refused with
`ENOSPC` alone, freed the file, wrote 8 MiB again, and left `df` at 50.6
MiB used, where it started.

## Performance

`make test-pve-ext4` on the lab node (KVM, `cpu host`, 4 cores, 1 GiB).
The ext4 column is the median of five rounds on a 1 GiB volume with a 32
MiB journal at `7afa2f63`; "first run" is the median of three rounds at
`dca58276`, which batched commits (ADR 0227) but had none of the changes
after it: checksum tables, coalesced writes, the path cache (ADR 0228),
indexed directories and the larger cache (ADR 0229). The Linux 6.12
column is the median of five rounds of the same `fsbench` binary on the
same kind of volume under Linux 6.12.115, run by
`tools/ext4fixture/linux-ext4-reference.sh` on the same node with the
same CPU, core count and memory. RAMFS and FAT32 are one round each at
`7afa2f63`; RAMFS ran with an 8 MiB data file, since `/tmp` holds 16 MiB.
Times are per operation.

| Workload | ext4 first run | ext4 | ext4, 10000 names | Linux 6.12 | RAMFS | FAT32 |
|---|---|---|---|---|---|---|
| create, 2000 names in one directory | 2.29 ms | 0.93 ms | 0.93 ms | 19.8 µs | 0.28 ms | 70.6 ms |
| stat | 1.18 ms | 0.15 ms | 0.18 ms | 1.7 µs | 0.10 ms | 13.8 ms |
| readdir of the whole directory | 59.7 ms | 13.8 ms | 1.44 s | 0.50 ms | 49.9 ms | 39.6 ms |
| unlink | 1.01 ms | 0.56 ms | 0.59 ms | 12.2 µs | 0.15 ms | 28.4 ms |
| 100 nested directories, file, stat, remove | | 0.83 s | 0.82 s | 4.0 ms | 0.09 s | 3.70 s |
| sequential write, 64 MiB, then fsync | 18.9 MiB/s | 96.9 MiB/s | | 225 MiB/s | 370 MiB/s (8 MiB) | 38.5 MiB/s |
| sequential read | 106.8 MiB/s | 109.4 MiB/s | | 6900 MiB/s (page cache) | 482 MiB/s | 106.6 MiB/s |
| random 4 KiB write, one fsync at the end | 0.61 ms | 0.49 ms | 0.48 ms | 13.6 µs | 0.15 ms | 29.3 ms |
| random 4 KiB read | 0.62 ms | 0.53 ms | 0.49 ms | 1.0 µs | 0.20 ms | 10.1 ms |
| 4 KiB write and fsync | 1.82 ms | 1.49 ms | 1.41 ms | 0.29 ms | 0.22 ms | 0.91 ms |
| write, fsync, rename over a target | 6.55 ms | 3.64 ms | 3.62 ms | 0.49 ms | 0.54 ms | 6.28 ms |
| 1 process: create, write 4 KiB, unlink | 258 ops/s | 551 ops/s | 542 ops/s | 23300 ops/s | 2175 ops/s | 176 ops/s |
| 8 processes at once | 288 ops/s | 522 ops/s | 526 ops/s | 85600 ops/s | 2026 ops/s | 157 ops/s |

A sparse write at 5 GiB allocated 8 blocks and read back on every ext4
round and every Linux round. At `dca58276` RAMFS took 4.57 ms per create
and 2.66 ms per `stat` in the same scenario; the name lookup change of
ADR 0228 removed most of that.

What the numbers show:

- One ext4 service handles one request at a time, so 2, 4 and 8
  processes reach the throughput of one. Linux scales from 23300 to 85600
  operations a second over the same range. 0.3.0 accepts this limit.
- `fsync` costs two device flushes on ext4, as the JBD2 commit needs, and
  one on FAT32, which has no journal. Linux pays 0.29 ms for the same
  commit and GoOS 1.49 ms; this run does not split the GoOS time between
  the services a request crosses.
- An ext4 `stat` costs about 1.5 times one on RAMFS: the request to the
  ext4 service and its lookup. A create costs about three times: every
  namespace change empties the VFS path cache, so each create resolves its
  directory again (ADR 0228), and it stages its inode, bitmaps and
  directory leaf.
- Linux answers metadata calls from its dentry and inode caches inside
  the kernel, 45 to 90 times faster than a GoOS request that crosses
  two process boundaries. Its sequential read after the write comes from
  the page cache; GoOS keeps no file data cache and reads from the device.
- Listing 10000 names takes 1.4 s: each listed entry reads its inode,
  and in hash order those reads spread over 10000 inodes, more than the
  1 MiB cache holds.

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

## M1 pathname-cache results

The M1 component cache (ADR 0230) replaced the resolved-path cache. Its
KVM results below are five-round `fsbench` medians extracted from the
run's `serial.log` (the guest's own output), not from a report summary.

| Metric | 0.3.0 | pre-FIX3 M1 | FIX3 |
|---|---:|---:|---:|
| ext4 deep 100 | ~830 ms | 324.747 ms | 92.7 ms |
| ext4 stat | ~150 µs | 196.0 µs | 163.7 µs |
| ext4 readdir 2k | 13.8 ms | 16.406 ms | 14.5 ms |
| ext4 create | 930 µs | 887.7 µs | 807.2 µs |
| ext4 1 proc | 551 ops/s | 608.5 ops/s | 680.4 ops/s |
| RAMFS deep 100 | ~90 ms | 268.770 ms | 29.3 ms |
| RAMFS stat | ~100 µs | 142.8 µs | 95.7 µs |
| RAMFS 1 proc | ~2175 ops/s | 1655.7 ops/s | 2424.0 ops/s |

Numbers above: 0.3.0 from the table above this section; pre-FIX3 M1 from
`make test-pve-ext4` run `20261122T194609Z-acfb74b5` (commit `8eccc53f`,
node `suptests`, `cpu=host`, 4 cores, 1024 MiB); FIX3 from run
`20261122T214955Z-e59b9c3c` (commit `8a3edcc6`, same node and shape),
`build/pve/runs/20261122T214955Z-e59b9c3c/serial.log`.

The first set-associative geometry, 32 sets × 4 ways with a raw FNV-1a
low-bit hash, failed the deep target: ext4 inode numbers
(`group*InodesPerGroup+index`) vary little in the low bytes, so the
deep path's same-named components collapsed into 13 sets of exactly 8
keys, overfilling the 4 ways and re-walking the prefix through
filesystem IPC (deep ~9 s). The fmix64 avalanche, the 64 × 8 geometry,
and two hot-path fixes (no 896-byte set-array copy per lookup, and the
1 KiB `ramfs.Name` built only on a cache miss) close the target:
deep 92.7 ms is ~9x the 0.3.0 baseline, and RAMFS deep 29.3 ms is
faster than 0.3.0's 90 ms.

## M2 readdir-prefetch results

The M2 inode prefetch (ADR 0231) replaces the inode-at-a-time walk of a
large directory listing with a block-grouped batch. KVM numbers below are
five-round `fsbench` medians from run `20261123T095006Z-ad12b999` (commit
`ea788082`, node `suptests`, `cpu=host`, 4 cores, 1024 MiB),
`build/pve/runs/20261123T095006Z-ad12b999/serial.log`.

| Metric | 0.3.0 | M2 |
|---|---:|---:|
| ext4 readdir 10k | 1.44 s | 193 ms |
| ext4 readdir 2k | 13.8 ms | 40.1 ms |

The 10000-name listing is 7.5x faster, above the 2x target. The guest
logged 1256 prefetch device reads over the whole benchmark: the win comes
from reading each inode-table block once instead of once per entry, not
from noise. The 2000-name listing regressed from 13.8 ms to 40.1 ms: the
batch machinery costs more per entry on the guest than it saves when the
inode table already fits the metadata cache, and the 10000-name win dwarfs
that cost. The read-count regression proof is `TestPrefetchListingReads`
(6331 interleaved reads against 675 prefetch reads on the 10000-name
fixture).

The coherence fixes (staged blocks override the prefetch, a generation-bound
decoded batch, and no raw alias on listing-only inodes) changed neither
bound nor the guest numbers. Post-FIX run `20261123T113227Z-9fe47d6c`
(commit `0edf9260`, same node and shape): 10k readdir 184 ms, 2k readdir
median 39.8 ms, 1256 prefetch reads and 24664 fetches. The 10k win holds
well above the 2x target.

The second review fixed two further defects without moving the numbers: a
reused `Inode` destination is reset before `decodeInodeInto`, and the
mutation epoch now advances on every successful `fold()` so a partial
multi-transaction write that later fails still invalidates decoded batches.
Run `20261123T123647Z-ba28f0c7` (commit `36f9ed78`): 10k readdir 193 ms, 2k
readdir median 36.0 ms, 1256 prefetch reads and 24652 fetches.

## M3 bounded parallel reads

Branch: `feat/ext4-read-concurrency`. Reviewed acceptance record:
`8d44b8452ccd1b26068c4edf467a887577b93d31` (documentation-only successor to
tested implementation `b85957ed3b3efb403fc2598e7962f7f90fc5d8a7`).

### Serialized-transport measurement before FIX3

KVM run `20261123T200415Z-6046a6ae` records commit
`26f7a44177caa82e9a243a93294b72e04e65eee9` with a clean worktree in
`build/pve/runs/20261123T200415Z-6046a6ae/report.json`. This run predates
reviewed HEAD; it does not certify the later commits. Node `suptests`, KVM,
`cpu=host`, one socket, 4 vCPUs, 1024 MiB. Scenario PASS; cleanup PASS
(`cleanup=ok`).

| Read-only clients | ops/s |
|---:|---:|
| 1 | 885.3 |
| 2 | 1043.7 |
| 4 | 1186.4 |
| 8 | 1262.3 |

Four clients / one client = 1.34x. The 2.0x target was not met. The accepted
performance fallback retains singular mutation/JBD2 ownership and records the
remaining serial boundaries: VFS persistent-read serialization and the serialized
block endpoint, block service and driver paths. These measurements do not isolate
the cost of each boundary.

Readdir-2k median: 44.218 ms across five rounds. Readdir-10k: 217.259 ms
(217 ms rounded). This returns near the earlier M3 pre-FIX level, not the M2
post-FIX result of 184 ms: the rounded M3 result costs about 17.9% more time.

The read-only single-client result falls from about 953.8 ops/s before M3 to
885.3 ops/s, about 7.2%. Treat that loss as a regression when assessing the
"no material single-client regression" goal. The adaptive dispatch path checks
`requestPending()` before choosing inline execution; earlier investigation named
that admission overhead. This record has no isolated profile that assigns the
7.2% loss to that check alone. It also has no profile that assigns the remaining
readdir cost to cache synchronization or directory ownership copies; those causes
remain unconfirmed.

### FIX3 host results (2026-11-24)

The FIX3 worktree passed `make -j3 lint`, `docs`, `test-concurrency-audit`,
`test-resource-policy`, `test-ext4-unit` and `test-ext4-stress`, one gate at a
time. Stress completed in 322.760 s. `test-ext4-concurrency` passed both full
race-instrumented packages: `userspace/ext4` in 519.532 s and
`userspace/cmd/ext4` in 1.267 s. The standalone equivalence gate passed the
46-reply read-only workload and the host adapter tests. Restoring nested cache
borrows through a temporary Go overlay made both traversal regression tests fail
at their completion bounds; the production source kept the fixed release points.

`make -j3 test-ext4-rw` passed the normal VM crash/replay workload, the GoOS
image checks and pinned `e2fsck`. `make -j3 test-ext4-read-restart` then passed
with 4 vCPUs and 512 MiB under TCG. For each fault (`volume`, `block`, `driver`),
the service marker preceded injection and all four reader processes reported
old-descriptor revocation and correct data after fresh opens. The full script,
GoOS clean/recovery checks and `e2fsck` also passed. The active log is
`build/logs/ext4-read-restart.log`; the clean and dirty images are under
`build/ext4-read-restart`.

The final rerun also passed `test-linux-elf`, the normal VM gate, lint/docs,
both policy gates, units and equivalence. Full ext4 race tests took 599.728 s;
command-package race tests took 1.310 s. The implementation commit below
contains the tested source; the later evidence commit changes documentation only.

### Final FIX3 KVM measurement (2026-11-24)

`make -j3 test-pve-ext4` passed on clean implementation commit
`b85957ed3b3efb403fc2598e7962f7f90fc5d8a7`. Run
`20261124T071705Z-2668fdcb` used node `suptests`, PVE 7.4-16, q35/OVMF,
`cpu=host`, one socket, four cores and 1024 MiB. The report records scenario
`pass`, tool status 0 and cleanup `ok`; the outer Make command returned 0.
The harness destroyed VM 154 and removed the uploaded ISO.

Evidence: `build/pve/runs/20261124T071705Z-2668fdcb/report.json` and
`build/pve/runs/20261124T071705Z-2668fdcb/serial.log`. The ISO SHA-256 was
`d2f168be7d90a46000cc10ebed071f1a9314df13f6fa6377a2b2f91acd79306d`.

| Read-only clients | ops/s |
|---:|---:|
| 1 | 882.3 |
| 2 | 1060.9 |
| 4 | 1226.0 |
| 8 | 1350.6 |

Four clients / one client = 1.39x. The final run misses the 2.0x target.
The accepted fallback retains the read pool and serialized block transport;
FIX3 adds correctness gates without changing those boundaries. Single-client
throughput remains about 7.5% below the pre-M3 953.8 ops/s result. It differs
by -0.34% from the earlier serialized-transport run, which does not prove a
statistically significant FIX3 throughput change.

Readdir-2k rounds: 56.303, 44.181, 45.299, 47.971 and 45.330 ms;
median 45.330 ms. Readdir-10k: 223.555 ms. The latter costs about 21.5% more
than the M2 post-FIX 184 ms result and 2.9% more than the earlier M3
serialized-transport 217.259 ms result. Do not call this an M2 baseline recovery.
The final run supplies no isolated profile to assign these regressions to
`requestPending()`, cache locks or directory copies.

The first attempt, `20261124T071349Z-82b10b27`, used the same clean commit and
ISO but ended with `infra-error` after the SSH serial console disconnected:
`Can't assign requested address` / `Broken pipe`. Its report records tool
status 3 and cleanup `ok`; outer Make returned 2. It supplies no final benchmark
result. The second attempt above passed after `pve-doctor` passed again.

### Existing mixed workload

The same final KVM run completed the existing `bench parallel` workload:
each process creates, writes 4 KiB, closes and unlinks files in its own
directory, for 200 iterations per process at scale 1. The five ext4 rounds
reported these aggregate rates; M3 records this workload without a scaling
requirement.

| Processes | Round 1 ops/s | Round 2 | Round 3 | Round 4 | Round 5 | Median ops/s |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 169.1 | 174.2 | 168.8 | 167.9 | 175.9 | 169.1 |
| 2 | 167.5 | 168.2 | 169.6 | 160.9 | 171.2 | 168.2 |
| 4 | 160.9 | 172.3 | 177.0 | 159.4 | 178.4 | 172.3 |
| 8 | 160.4 | 167.7 | 170.6 | 166.2 | 168.4 | 167.7 |

Source: `serial.log` from `20261124T071705Z-2668fdcb`, lines 62–65,
107–110, 152–155, 197–200 and 242–245. Each round ended with `bench ok`.
The four-process / one-process median ratio is 1.02x; this mixed workload
shows no material scaling. The later scale-5 ext4 round recorded
175.3 / 168.6 / 177.8 / 169.1 ops/s at 1/2/4/8 processes (lines 294–297).
These results come from the existing log; no new benchmark run supplied them.

### FIX3 validation procedure

Run all Make gates with three build jobs:

```sh
make -j3 lint
make -j3 docs
make -j3 test-concurrency-audit
make -j3 test-resource-policy
make -j3 test-ext4-unit
make -j3 test-ext4-stress
make -j3 test-ext4-concurrency
make -j3 test-ext4-read-equivalence
make -j3 test-ext4-rw
make -j3 test-ext4-read-restart
make -j3 test-pve-ext4
```

`test-ext4-concurrency` runs both ext4 packages with `-race -count=1`, including
the read-job ownership and block transport tests. Synthetic traversal tests use
same-set metadata blocks and a two-second completion bound for each call to
`mapExtent` and `mapIndirect`. They cover cold and cached paths, including double
and triple indirect maps.

The equivalence gate compiles the production serial and goroutine service loops
as separate host executables. It retains the loop statements, `answer`,
`sendReply` and admission name ownership; host adapters replace capability IPC,
waits and wake streams. Both builds run the production ext4 volume and engine
against the same fixture. The comparison retains decoded reply fields and bytes,
sorts by request identity and ignores completion order and timing. Native startup,
syscalls and scheduling remain outside this host gate.

The restart gate uses a separate validation build variant; normal builds omit its
worker hold hook. Four reader processes own independent descriptors and perform
checked reads and stats. The VFS persistent-read lane admits one ext4 read and
queues the other readers. A cold read at 1 MiB reaches a service worker, which
prints `EXT4_RESTART_READER_ACTIVE` and holds the request until the injected fault.
The host waits for that service-side marker before killing the instance. This
proves one active ext4 worker at the fault, not four simultaneous endpoint reads.
Each reader uses a private file with a distinct payload marker, rejects its old
descriptor and verifies metadata and four full pages through a fresh open.
The sleeping `abi-probe hold` case remains a descriptor-revocation test and does
not satisfy this gate. Active-reader faults target the ext4 instance, block
service and driver while the Linux process keeps its VFS session. The harness
also retains the existing VFS restart check and launches new processes afterward.
ADR 0138 makes a running Linux process's VFS session fail permanently on VFS
death; requiring that process to regain files would contradict the current
capability model.

An extra exploratory active-VFS case confirmed four read revocations and service
replacement, but its fresh-open expectation failed. The readers later faulted on
an unmapped code address; this run does not establish that fault's cause. FIX3
does not certify active VFS-session renewal or change that separate lifecycle
contract.
