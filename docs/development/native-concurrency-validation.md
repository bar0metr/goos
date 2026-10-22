# Native concurrency validation

## Historical KVM revision

The combined KVM run below tested `da1da8ffb92f72dc513b3371b7f1e32d0f41dffb`
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
all services. The [benchmark implementation](../../userspace/bin/goprobe/main.go#L1175)
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

## Follow-up validation on 2026-10-22

The follow-up integrates log/IRQ guest fixtures, collector stress, read-copy
checks and the serial VFS fallback. The implementation revision is
`7e6f78875483d43efe0989a1f152a4f86ae8b69e`. The historical KVM run above
does not cover these changes. A later KVM run on `00d66e69c0606ce93c7169076919753ce3395490`
failed during `goprobe panic`; see the fault analysis below. It is not a
passing joint validation result.

On the combined source, `go test ./...`, `go test -race ./userspace/...`,
`go vet ./...` and validation-tagged vet passed. Validation-tagged race tests
also passed for VFS, arena, goroutine, shell and pvelab. The inventory covers
805 package-level variables. Guest-only packages do not run in the host suite.
Compiler/frame checks reported
`GOCOMPILER_OK symbols=7 hooks=6 tls=fs overflow=recursive`;
the largest goprobe frame used 2,872 of 65,400 bytes.

One SMP=4 TCG guest passed pool, channels, sync, timers, fairness,
panic/recover, mixed native threads, 500 I/O rounds, waitsets, logwait,
GC at 1/4 and gcstress at 1/4. It used `VALIDATION=1 VFS_PROBE=1`.

| Follow-up check | Observed result |
|---|---|
| Log cursor | Two Gs waited on one cursor, read the token-bearing record at sequence 122, and observed peer progress of 314. Registrations retired. |
| Disk IRQ | The driver reported `VIRTIO_BLK_G_IRQ_OK` before and after replacement: four proved waits each, with peer progress of 8,897 and 7,148. The driver retained IRQ authority. |
| GC stress, one collector | 1,275 collections; 256 sampled dropped lists changed; eight scenarios reported `lost=0`; one context-swap range hold and 19 preemption-range holds. |
| GC stress, four collectors | 1,977 collections; 192 sampled dropped lists changed; eight scenarios reported `lost=0`; zero context-swap range holds and 27 preemption-range holds. |
| Production VFS | The normal serial target compiled. Its ELF did not contain `VFS_COPY_INVALID`. |

GC stress covers preemption/local roots, G creation and completion, deferred
return/panic/Goexit cleanup, pending channel values, timer captures,
G/M stack transitions, mixed native threads and a rejected region-window
rendezvous followed by collection. Validation sweep poisoning gives a missed
root a corruption signal; sampled dropped lists and a heap bound reject a
collector that reclaims nothing. A bounded wait for scheduler retirement
precedes final heap measurements, because WaitGroup completion alone does not
mean another M has finished retiring its G.

The context-save counters cover instruction ranges, including restore. These
runs do not establish per-instruction coverage or isolate every XMM root.
Conservative aliases can retain an object through another root. The rejected
rendezvous uses a held region transition, not a kernel-induced timeout.

Normal images now ship serial VFS; validation images default to the goroutine
VFS. `VFS_RUNTIME` can select either. Makefile changes invalidate the VFS ELF,
including an old normal build tree that still contains a goroutine image.
`test-all` and `release-check` now include the serial boot gate, compiler,
goroutine runtime, collector and scheduler-aware I/O suites. Wiring a target
into those gates does not mean the complete gates passed in this follow-up.
The panic-report suite is also wired into both gates. Its direct guest
`panicreport` check printed the panic text and exited; the earlier full
fault suite then timed out on its separate deadlock scenario. A later
`test-goroutine-panic` attempt did not reach a prompt because the VFS fault
above stopped the service during boot. The new suite therefore has no
complete passing target run yet.

The serial boot gate passed on 2026-10-22: `make test-vfs-serial` reported
`PASS qemu-boot (471 checks)`, including FAT32 stall, stack overflow and
poweroff. Earlier attempts exposed lost terminal
input, a bulk-I/O timeout and an installed-service scan failure after FAT32
reattachment. The last failure entered recovery with
`stage=services path=/system/services/virtio-net reason=unreadable`.
Its cause was not established. The completed serial gate passed through the
previous failure point with `SYSTEM_CHECK_OK` and `SYSTEM_REATTACHED_OK`.
Follow-up diagnostics record the failed installed-file phase, session,
correlation, elapsed ticks, VFS status and raw transaction failure before
quarantine. They also report a failed Close, which could otherwise make the
next Stat appear to fail on the wrong path. A targeted serial/TCG boot with
fresh init and shell over copied baseline storage binaries also completed
FAT32 reattachment and the installed-system check without a failure record.
Neither pass explains the earlier scan failure.

The harness now uses a validation-only shell acknowledgement after foreground
completion rather than a child's stdout or a redrawn prompt. Host regressions
cover stale tokens, missing output and bounded failure without sending the
next command.

## Remaining evidence limits

The historical KVM run did not give goprobe a log capability or run the disk
driver on Gs. The follow-up TCG fixture now does both: validation PID 1 grants
goprobe log Read/Write, while the disk driver keeps its IRQ and runs IRQWait
through the G adapter. Normal builds keep the serial disk driver. Pins,
regions, maximum-sized waits and scheduler-stack calls retain their documented
native fallback. The follow-up KVM run `20261022T185211Z-4c209fe6`
passed log/IRQ delivery, collector stress at one and eight collectors, and
the joint VFS recovery scenario on eight vCPUs. Its report records a dirty
worktree based on `00d66e6`, so it does not identify a clean commit.

The first diagnostic KVM retry, `20261022T184035Z-122c720f`, failed when
`gcstress` had started eight scheduler workers and then requested two more
native threads under the process's eight-task quota. The fixture now reserves
two task slots before it creates workers. The next retry,
`20261022T184411Z-aea417a0`, completed all eight root scenarios with
`lost=0`, but its final `Live` value was `0xffffffff00006af0`. It had not
checked whether its three final GC requests completed; an abandoned
rendezvous can leave per-thread counts temporarily unfolded. The fixture now
requires three completed collections with a deadline. One subsequent KVM
run passed with `final_collections=3`, `reclaimed=256/256` and normal `Live`
at both collector counts. The earlier counter anomaly is not proven solved
until the same load is repeated and the accounting path is understood.

A clean-HEAD KVM retry at `3eb909e` failed in `gcstress 8` with a G stack
guard fault while reporting an allocator panic. The goroutine runtime's
`putchar` called `startup.Read` for every byte, allocating while the
allocator was already panicking. The native panic writer now reads the GST4
handle table directly and writes through an assembly syscall without a Go
allocation. The next KVM run, `20261022T193025Z-33147d12`, exposed the
original panic: `arena: slab count does not match its bitmap` in the channel
scenario of `gcstress 8`. `gcstress 1` passed all eight scenarios in that
boot. The panic writer is fixed; the allocator or its rendezvous still has
an unresolved concurrency failure. This run used a dirty worktree. A fatal
slab check now prints its page, class, owner, bitmap count, recorded count
and collection number before the panic to narrow the next reproduction.

One earlier isolated TCG run faulted in VFS memmove with a null source before
waitsets printed output. Its cause remains unknown. The isolated retry and
the historical integrated TCG/KVM runs passed. They do not prove that the fault
cannot recur. The saved build manifest names `4200d8c`, an ancestor of
`343df2e`. Rebuilding it reproduces the recorded `vfs.elf` SHA-256
`11db0c5cf6397bd2da830e17c246aaf51642d16f9d8b50bb24d02e29d5e5e7be`.
The original ELF in the wip build directory has since changed. The matching
ELF places RIP `0x400024a5` on memmove's forward source load
`movb (%rsi,%rcx), %dil`. The zero fault address supports a null source;
the log lacks arguments and a return address, so it cannot identify the
caller or establish a torn slice header as the cause. Safe Go cannot create
a slice with a null array and a nonzero length.

The follow-up KVM run `20261022T175206Z-c7ff3f23` reproduced a null-source
memmove fault in the VFS process. Its ELF maps return address `0x4000f36f` to
`ramfs.MakeName`, after the copy from the input name into a new `Name`. The
fault had RSI=0, RDX=1024 and RCX=0. In this compiled function, `validName`
reads the input through R12 before the copy; no ordinary call lies between
its last read and the move from R12 into RSI. The evidence therefore points
to a changed register or control state during asynchronous execution. It
does not yet prove which preemption, context-switch or fault path caused the
change. The run stopped before the GC stress and recovery checks. Validation
fault reports now include RBX and R12-R15 so a repeat can distinguish a
changed saved pointer from a changed argument register. The cause is open.
Validation builds now check read-pool buffers and persistent-cache copies. The pool checks
targets at dispatch, in the worker, at collection and at delivery; the remote
backend checks each cached copy; the driver checks a delivered read against
its session, generation and buffer before the reply. A violation prints one
`VFS_COPY_INVALID` line naming the check, addresses, length, session,
generation, plan ticket, plan state and worker, and the service exits without
the copy. Persistent workers carry the captured session and ticket into
the cache check. Drained serial reads carry the owner's session generation.
A fatal user fault also logs RAX, RDI, RSI, RDX, RCX, RSP and RBP, the task,
its FS base, TLS slot and goroutine, and up to eight frame-pointer return
addresses when the program retains frame pointers. The kernel checks the
native TLS self pointer and magic before reading its slot and goroutine.

Another TCG boot on the uncommitted panic-writer tree reproduced the VFS
fault before the first prompt. RSI and R12 were both zero, RDX was 1024,
and the return address again identified `ramfs.MakeName`. No
`VFS_COPY_INVALID` record preceded it. The fault is reproducible under the
current validation load; its cause remains unidentified.

The diagnostic branch passed host race tests with and without
`goos_validation`, `go vet ./...` and `make test-concurrency-audit`.
`make test-goroutine-io` passed on TCG with four processors, including
`goprobe waitsets`. Two extra fault-suite boots printed the new register and
TLS fields after the expected overflow; both then timed out at the next
deadlock scenario. Follow-up checks passed host race tests and TinyGo
validation VFS/kernel compilation without another guest boot. These runs do
not reproduce the original VFS fault. RAMFS/image internals and IPC/name
copies still rely on the kernel fault report to identify their copy caller.

Another early region test retained inner heads in LLVM spills; the fixture
now retires those frames before measuring reclamation.

The branch remains experimental as described in
[ADR 0204](../adr/0204-native-goroutines-and-concurrency.md). These results do
not establish complete GC/preemption correctness or production readiness.
