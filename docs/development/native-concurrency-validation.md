# Native concurrency validation

## Tested revision

The combined KVM run tested `da1da8ffb92f72dc513b3371b7f1e32d0f41dffb`
with no tracked changes. Run ID: `20261021T211333Z-6c7304d6`.

Configuration: TinyGo 0.42.0, LLVM 22.1.4, Go 1.25.5, optimization level 2,
100 Hz, `VALIDATION=1 VFS_PROBE=1`. The guest used Q35, OVMF, KVM host CPU,
one socket, eight vCPUs and 1,024 MiB RAM. The host reported an Intel Xeon
Gold 6240 at 2.60 GHz. The test did not pin vCPUs to host cores or isolate
host load.

ISO SHA-256:
`73aedd10c4ce8a2e021ddcd08de0843516b8eb1970eea51dacec271bceb8dfda`.
Initial disk SHA-256:
`a8bde2c76afdcc38fd329c7e873d3d9e068d0e184b5d83b78f7a648a0b489d87`.

Reproduce the combined scenario with:

```sh
make test-pve-native-concurrency
```

The run directory contains the report, serial log, host metadata, artifact
hashes and goroutine benchmark samples. Pvelab reported `pass` and cleanup
`ok`; it destroyed the owned VM and removed its uploaded ISO.

## Joint native checks

The scenario checks the VFS boot fixture, runs the goroutine tests and three
benchmark samples, runs two region-window probes, then stalls fat32 and
checks persistent I/O after replacement. These checks ran in one guest with
one kernel/userspace build.

| Check | Observed result |
|---|---|
| Compiler transport | `GOCOMPILER_OK symbols=7 hooks=6 tls=fs overflow=recursive` on the same source. |
| Real VFS read pool | Local workers overlapped; local reads completed while a persistent read waited; write/control barriers and dead-client cleanup completed; 4,096 reads completed. The overlap gate counts simultaneous workers and rejects timeout release. |
| Goroutine runtime | Pool, smoke at 2/8, channels/select, sync, timers, fairness, panic/recover, limits, I/O, waitsets, native threads and GC at 1/8 passed. |
| Preemption | The fairness probe reported 50 progress events and 51 preemptions over about one second. |
| Native join | Waitsets included a G joining a native thread while other Gs progressed. Kernel token tests cover competing native/G joiners and migration. |
| Region window, one worker | 16 rounds; 64 rejected GC requests; 256 region-window retries; 48 subsequent successful collections; 640 list checks; 358,400 bytes reclaimed. |
| Region window, four workers | 24 rounds; 96 rejected GC requests; 384 region-window retries; 72 subsequent successful collections; 960 list checks; 537,600 bytes reclaimed. The fixture recorded 15 peer-collector rounds and 30 cross-CPU overlaps. |
| Persistent recovery | `FAT32_STALLED`, `VFS_CONTROL_REPLACED_OK`, `FAT32_STALL_REPLACING`, `FILE_CACHE_RECOVERED_OK`, `FAT32_RESTARTED_OK`, `SYSTEM_REATTACHED_OK`. The existing file survived; a new write read back correctly; VFS itself did not restart. |

The region fixture holds nested Enter and Leave between cursor and innermost
stores. It checks that the collector rejects those snapshots before marking
or sweeping, then accepts a later rendezvous. It verifies outer-region and
stack roots, clears the inner region, retires frames containing its original
list heads and observes reclamation. This is a deterministic native test of
those transitions, not all possible collector roots or lifecycle positions.

The persistent recovery fixture leaves fat32 alive while it retains the
cache mapping. PID 1 replaces ordinary control through the independent
recovery endpoint, kills the old process, waits for its reported exit and
then permits fenced reattachment. Host tests additionally cover a control
request timing out behind the persistent lane, discarded unread attachments
without replay, exit-before-notice ordering, rejected unfenced attachment
and the restart bound. The combined KVM scenario performs one recovery; it
does not exercise the terminal restart bound in the guest.

## Host and TCG checks

On the integrated source:

- `go test ./...`: 3,232 tests in 153 packages passed.
- `go vet ./...`: passed.
- `go test -race ./userspace/...`: 2,159 tests in 70 packages passed.
- The separate rand and TLS probe tags passed 2 and 4 race tests. The probes
  no longer replace process-global entropy.
- Save ownership, configuration retry and editor tests passed 1,960 cases
  across 20 race runs. Each save reserves its own temporary pathname;
  ambiguous Replace failures do not delete a pathname another save may own.
- The validation VFS suite passed 68 race tests; validation kernel locks
  passed 12 tests.
- The variable inventory covers 789 package-level variables. The bounded
  [caller audit](native-concurrency-callers.md) covers 63 library packages;
  inventory coverage alone does not establish runtime correctness.
- Integrated SMP=4 TCG passed the real VFS fixture, 500 I/O rounds and
  waitsets, including join progress. Compiler/frame checks passed.
- `make -j8 test-boot VALIDATION=1 VFS_PROBE=1` passed 468 checks, including
  persistence, VFS/service/block restarts, recovery fsck, partition tables,
  stack guards, poweroff and a separate FAT32 stall boot.

The first broad boot run exposed three harness assumptions: a command anchor
required a prompt that asynchronous output had split from the command; a
fixed delay sent `stat -b` while the previous `stat` still ran; the added
stall spent a fourth FAT32 restart beyond the supervisor's existing limit.
The harness now waits for stat completion and tests the stall in a separate
guest on a disk copy. The supervisor's restart policy has not changed.

The full run preceded the final standalone-fixture seed change in `4dffdfc`.
After that change, the standalone guest passed on a fresh installed disk:
it wrote and read its own control file before the stall, recovered, and read
the same bytes afterward. The shell regression also passed prompt/no-prompt
cases and rejected missing command output. To repeat only that guest check:

```sh
sh tests/boot/qemu_boot.sh --fat32-stall-only ISO OVMF_CODE TEST_DISK
```

The test copies `TEST_DISK`; it does not modify the supplied disk.

## Benchmark samples

These are short runtime smoke measurements, not performance acceptance for
all services. The [benchmark implementation](../../userspace/bin/goprobe/main.go#L1163)
times 2,000 goroutine creations/exits, 5,000 channel round trips, 5,000
four-channel selects, 10,000 mutex-protected increments and fifty 1 ms
sleeps. CPU scaling divides a fixed `1 << 22` recurrence workload across
1/2/4/8 workers, retains its result through an atomic sink and compares each
sample to its own one-worker time.

Three sequential samples ran in the same guest. Each table cell is one
sample's mean cost per operation; it is not an individual-operation percentile.

| Runtime operation | Sample 1 | Sample 2 | Sample 3 |
|---|---:|---:|---:|
| Create and exit, ns | 10,064 | 13,158 | 12,790 |
| Channel round trip, ns | 641 | 643 | 666 |
| Four-channel select, ns | 111 | 113 | 111 |
| Mutex increment, ns | 98 | 111 | 102 |
| Mean sleep lateness, ns | 8,921,093 | 8,996,965 | 8,944,222 |

| GOMAXPROCS | Sample 1 elapsed ns | Sample 2 elapsed ns | Sample 3 elapsed ns |
|---:|---:|---:|---:|
| 1 | 1,343,883 | 1,352,045 | 1,365,757 |
| 2 | 701,254 | 698,089 | 705,229 |
| 4 | 401,576 | 435,187 | 449,445 |
| 8 | 347,832 | 460,942 | 416,863 |

The reported eight-worker speedups range from 2.93 to 3.86. The 100 Hz timer
explains the roughly 9 ms lateness of a requested 1 ms sleep; these samples
do not establish sub-tick timer latency. The workload is short, host load
was not controlled, and three samples do not support a tail-latency claim.
There is no measured concurrent VFS throughput or latency comparison here,
and no Linux or stock Go baseline.

## Remaining evidence limits

Goprobe has no log-reader or IRQ capability. Cursor registration, alias
fallback and IRQ adapter races have host tests and source review; the guest
exercises cursor-format aggregate waits and the real disk's legacy interrupt
path. This run does not independently demonstrate a G receiving a log event
or disk IRQ. Pins, regions, maximum-sized waits and scheduler-stack calls
retain their documented native fallback.

One earlier isolated TCG run faulted in VFS memmove with a null source before
waitsets printed output. Its cause remains unknown. The isolated retry and
the final integrated TCG/KVM runs passed. They do not prove that the fault
cannot recur. Another early region test retained inner heads in LLVM spills;
the fixture now retires those frames before measuring reclamation.

The branch remains experimental as described in
[ADR 0204](../adr/0204-native-goroutines-and-concurrency.md). These results do
not establish complete GC/preemption correctness or production readiness.
