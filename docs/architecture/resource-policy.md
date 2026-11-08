# Resource policy

This document lists every capacity a release build exposes, gives each one a
class, and records six facts about it. ADR 0215 sets the rule; this file is
the inventory the rule points at. The second half defines the production
reference profile for a 1 GiB virtual machine and lists where the code falls
short of it.

## Classes

| Class | Meaning | What justifies it |
| --- | --- | --- |
| protocol | A wire format, ABI or standard fixes the number. | The field width or the specification. |
| hardware | The processor, firmware or device fixes the number. | The architecture manual or the device. |
| policy | A security or quota decision: how much one principal may take. | The principal it protects, and who may change it. |
| profile | A memory size picked for a target machine. | The memory it costs and the profile it belongs to. A profile bound needs a configuration key or a named build profile. |
| artifact | A number that falls out of how the code is built today. | Nothing. Each one is a defect and appears under [Implementation artifacts](#implementation-artifacts). |

## The six facts

Each release-visible row records:

- **Default** and **maximum**: the value in force and the ceiling a key or
  build may raise it to. One number means the bound is a constant.
- **Owner**: the account that pays for the resource. "global" means one
  table for the whole system, with no per-principal share.
- **Memory**: what the bound costs, static or on demand.
- **At exhaustion**: what the caller gets back.
- **Status**: the status field that shows use and limit, or "none".

Protocol and hardware rows that bound a single message or field cost nothing
beyond their buffer and have no use to report; those rows give the value and
the class and leave the other columns as "n/a".

## Checking the inventory

`tools/resourcepolicy` parses every non-test Go file under `kernel/` and
`userspace/`, collects each package-level constant or variable whose name
starts with `Max`, and fails when one has no row here. A row names a bound by
package directory and name in backquotes, `kernel/ipc.MaxChannels`. A row
naming `pkg.Max*` covers every `Max` bound in that package; use it only when
all of them share one class. The tool also fails on a row naming a bound that
no longer exists.

```text
go run ./tools/resourcepolicy          # check
go run ./tools/resourcepolicy -list    # every bound with its value and file:line
```

`make test-unit` runs the check through `go test ./...`; `make
test-resource-policy` runs it alone.

## Production reference profile: 1 GiB VM

The reference machine has 1 GiB of RAM, one to four processors, a virtio
network card and a FAT32 system disk. A release build must meet each line
below without a rebuild.

| Requirement | Provided by | Holds |
| --- | --- | --- |
| 256 task slots | `kernel/sched.MaxTasks` = 256 | Yes. `process.MaxProcesses` is 256 and Linux programs may hold 224 of them, 64 per family. |
| 64 threads in one process | `kcall.ThreadSlots` = 64; large profile task quota 64 (normal 16, tiny 1) | Yes for a large process: each thread past the first holds six pages of its page quota, 378 of 16384 for 64. A Go program pays for the per-thread runtime records of the slots it runs, not for 64. |
| 1024 Linux descriptors per process | `RLIMIT_NOFILE` soft 1024, hard `linux.LinuxFDCeiling` 4096 (ADR 0212) | Per table, yes. The system pool `linux.MaxOpenDescriptions` (4096) is shared by every family, so four processes at their soft limit fill it. Regular files also need a VFS slot: `vfs.MaxOpenPerOwner` (64) per session and `vfs.MaxOpenFiles` (256) in all. Directories share 32 slots system-wide. |
| 256 concurrent TCP connections | `system.net.tcp.max-endpoints` 1024 (ceiling 4096); `netapi.MaxSessionQuota` 512 granted to `linuxsockd`; `linuxsock` family quota 512, service quota 1024; `system.net.tcp.memory-bytes` 40 MiB at 8 KiB per handshake | Yes for Linux programs and for a native session PID 1 grants 512. An ordinary native session gets 8 by policy. `netd` serves at most 46 TCP sessions, and `net status` shows how many are in use (see gaps). |
| Listen backlog of at least 128 | `system.net.tcp.listen-backlog` 128 (ceiling 4096); `linuxsock.MaxBacklog` 255 | Yes. A Linux backlog reaches `netd` up to 255, the width of the GTCP listen byte, and `netd` clamps it to the key, as Linux clamps to `somaxconn`. Raising the key past 255 reaches no listener. |
| Kernel log ring of at least 2048 records | `klog.Capacity` = `kcall.LogRingCapacity` = 2048 (416 KiB BSS) | Yes, at the floor. |
| 64 MiB of persistent log history | `system.log.segment-bytes` 16 MiB × `system.log.segments` 4 (`logpersist.DefaultSegmentBytes`, `DefaultSegments`) | As a ceiling. The count includes the live segment, so right after a rotation the files hold 48 MiB. Five segments would keep 64 MiB at all times. |
| Network progress never waits for an idle timeout | `netd` computes its wait from `stack.Deadline()`, polls while work is pending, and arms the GTCP 4 doorbell before it sleeps; `idleWait` (30 s) covers only a lost wake | Yes in `netd`. A validation build stops with `NETD_DELIVERY_STALLED` when a parked request could run before the wait (`checkSettled`). `linuxsockd` and the kernel socket bridge have not been audited for the same property. |
| No full-table TCP lookup on receive | Hashed four-tuple, port and listener indexes (ADR 0213); validation builds compare each lookup with a registry walk | Yes. `TCPStats.LookupProbes` and `MaxSegmentProbes` measure it; `OpTCPStatus` carries both and `net status` prints them as `lookups`. |

### Gaps against the profile

1. **Threads per process.** Closed: 64 thread slots, a large-profile task
   quota of 64 charged in pages, and runtime records paid by the slots in
   use (ADR 0203, ADR 0122). Programs PID 1 launches with the normal
   profile still stop at 16 threads.
2. **Processes.** Closed: `process.MaxProcesses` is 256 and the Linux
   per-process state takes pages on first use (ADR 0111). A slot keeps its
   pages after its process ends, so memory follows the peak process count.
3. **TCP sessions in `netd`.** One `WaitAny` names 64 objects, because its
   ready mask is one 64-bit word (ADR 0111); the driver, the control
   channel, eight command sessions and eight UDP sessions take 18, which
   leaves 46 TCP sessions (`netd` `tcpSessionSlots`). The stack keeps 64
   owners. Connections are not affected; native client processes are. A
   wider mask changes the `WaitAny` result ABI and every per-processor wait
   buffer, so the bound stays as policy and `net status` reports the
   sessions in use against it.
4. **Open descriptions.** `linux.MaxOpenDescriptions` (4096) is one pool
   for sockets, files, pipes and eventfds of every Linux family, with no
   per-family share and no status field.
5. **Linux side tables.** 32 directory descriptions, 64 eventfd counters
   and 8 terminals serve every Linux process together, and `readv`/`writev`
   refuse more than 16 vectors where Linux accepts 1024.
6. **Global kernel tables have no release status.** A `goos_diag` build's
   dump prints `DIAG tables` (tasks, processes, channels, streams, shared
   memory, executables) and `DIAG linux-tables` (open descriptions,
   directories, eventfds, terminals, Linux processes and the busiest
   family) as use/limit. A release build has no such call: `SystemInfo`
   refuses any length but 40 bytes and carries no version, so it still
   reports the process count without its limit. The VFS service has no
   status operation, so its open-file table stays invisible.

## Implementation artifacts

These bounds have no justification beyond the current code. Each is a
defect to fix or to reclassify with a reason.

| Bound | Value | Why it is an artifact |
| --- | --- | --- |
| `kernel/mm/region.MaxRegions` | 128 | Fixed region table per process; Linux allows 65530 maps. A native thread takes one entry for its stack and guard, so 63 threads hold 63 (ADR 0203). |
| `kernel/cpu.MaxCPUs`, `kernel/sched.MaxCPUs`, `kernel/arch/amd64.MaxCPUs` | 64 | One 64-bit mask per processor set. |
| `kernel/sched.MaxKernelTasks` | 64 | One-word mask in `Bootstrap`. |
| `kernel/boot.MaxInitramfs` | 6 MiB | Follows the 8 MiB EFI partition the build makes. |
| `kernel/compat/linux.MaxOpenDirectories` | 32 | One global table for every Linux process. |
| `kernel/compat/linux.MaxEventCounters` | 64 | One global table for every Linux process. |
| `kernel/compat/linux.MaxIOVec` | 16 | Per-CPU copy area; Linux `IOV_MAX` is 1024. |
| `kernel/compat/linux.MaxExecArgs`, `MaxExecEnv`, `MaxExecStringBytes`, `MaxExecTotalBytes` | 256, 256, 32 KiB, 64 KiB | Fixed exec scratch; Linux allows a quarter of the stack limit. |
| `kernel/compat/linux/abi.Max*` | 64 KiB stack, 256 strings | Same initial stack bound as exec. |
| `kernel/compat/linux/elf.Max*` | 64 headers, 16 load segments | Fixed loader arrays. |
| `kernel/compat/linux.MaxSocketRequests` | 8 | Fixed in-flight array per family. |
| `userspace/linuxsock.MaxFamilies` | 16 | Width of the GLNXCTL2 status table. |
| `userspace/runtime/goroutine.MaxWaiters` | 63 | One kernel `WaitAny` per waiting thread. |
| `userspace/dns.MaxTransactions` | 4 | Fixed transaction array; a burst of distinct names queues. |
| `userspace/tcpupstream.MaxStreams` | 2 | Two 64 KiB assembly buffers. |
| `userspace/dnsstub.MaxConnections`, `MaxUDPRequests` | 2, 6 | Fixed slots in the local stub. |

## Missing status fields

| Resource | Use and limit that exist | Status that should carry them |
| --- | --- | --- |
| Kernel tasks | `DIAG tables` `tasks` in `goos_diag` builds | a release call; `SystemInfo` has no room |
| Processes | count in `SystemInfo` word 1; `DIAG tables` `processes` with the limit | the limit in a release call |
| Channels, streams, shared memory, executables | `DIAG tables` in `goos_diag` builds | a release table-use call |
| VFS open files | `MaxOpenFiles`, per-owner counts | VFS service status; the service has no status operation |
| Linux open descriptions, directories, eventfds, terminals | `DIAG linux-tables` in `goos_diag` builds | a release Linux table-use call |
| Linux processes | `DIAG linux-tables` `processes` (of 224), `family-peak` (of 64) | a release Linux table-use call |
| Goroutines | `goroutine.ReadStats` `Live`, `Limit` | a per-process diagnostic a command can print |

These status fields exist:

| Resource | Status |
| --- | --- |
| TCP memory | `OpTCPStatus` `Capacity.MemoryBytes`, `MemoryBudget`, `OwnerMemoryBudget`; `net status` `memory` |
| TCP TIME_WAIT | `Capacity.TimeWait`, `TimeWaitBudget`, `OwnerTimeWaitBudget`, `TimeWaitHeld`; `net status` `time-wait` |
| TCP drops | `Capacity.DropMemory`, `DropBacklog`, `MemoryCapped`; `net status` `memory` and `dropped` |
| TCP lookup cost | `Capacity.LookupProbes`, `MaxSegmentProbes`; `net status` `lookups` |
| `netd` sessions | `Capacity.CommandSessions`, `UDPSessions`, `TCPSessions` against 8, 8 and 46; `net status` `sessions` |
| Kernel log persistence | `/state/system.log.status`, printed by `log status`: appended, overwritten, persisted, lost, failures, rotations |

`net endpoints` writes the same TCP and session figures to the log as
`tcp memory`, `tcp time-wait`, `tcp lookup cost` and `netd sessions`
records.

## Inventory

### Processes, tasks and quotas

| Bound | Default / max | Class | Owner | Memory | At exhaustion | Status |
| --- | --- | --- | --- | --- | --- | --- |
| `kernel/sched.MaxTasks` | 256 | profile (build constant) | global | static per-slot contexts, roots and FPU areas; assembly `GOOS_MAX_TASKS` | `StatusResourceExhausted` | `DIAG tables` `tasks` |
| `kernel/sched.MaxKernelTasks` | 64 | artifact | kernel | one word | boot panic | none |
| `kernel/sched.MaxCPUs` | 64 | artifact | global | one word per set | extra processors stay offline | `CPUInfo` count |
| `kernel/process.MaxProcesses` | 256 (`sched.MaxTasks`) | profile (build constant, ADR 0111) | global | a 16 KiB process record on a slot's first use; a Linux process adds a two-page state and its family a one-page VFS context on first use; a slot keeps its pages for reuse; 8 B per slot static | `LaunchResourceExhausted`; Linux `EAGAIN` | `SystemInfo` process count; `DIAG tables` `processes` with the limit |
| `kernel/process.MaxExitEvents` | 256 | profile (follows `MaxProcesses`) | PID 1 | 24 B each, static | oldest report overwritten | none |
| `kernel/process.MaxTasksPerProcess` | 64 | policy (the large profile's task quota) | process | 8 B a task in the process record | `StatusResourceExhausted`; Linux `EAGAIN` | `ProcessInfo` task count |
| `kernel/process.TaskPages` | 6 pages | hardware (kernel stack and guard, `amd64.TaskStackPages`) | process page quota, per task past the first | the stack | `StatusResourceExhausted`; Linux `EAGAIN` | `ProcessInfo` pages |
| `kernel/process.MaxWaiters` | 256 | protocol (one per task slot) | global | static | cannot fill | n/a |
| Launch quota, tiny | 2 channels, 8 streams, 1 task, 2048 pages | policy (ADR 0122) | process | counters | `StatusResourceExhausted` | `ProcessInfo` used and limits (`quota`, `ps`) |
| Launch quota, normal | 4, 16, 16 tasks, 8192 pages | policy | process | counters; each task past the first holds `TaskPages` | `StatusResourceExhausted` | `ProcessInfo` |
| Launch quota, large | 8, 32, 64 tasks, 16384 pages (64 MiB) | policy (ADR 0122) | process | counters; 64 tasks hold 378 pages | `StatusResourceExhausted`; Linux `EAGAIN` | `ProcessInfo` |
| PID 1 quota | 256 channels, 64 streams, 16 tasks, 16384 pages | policy (ADR 0072) | PID 1 | counters | `StatusResourceExhausted` | `ProcessInfo` |
| `kcall.ThreadSlots` | 64 | protocol (thread-area layout, ADR 0203) | process | 576 KiB address space per slot, pages on touch; runtime records of slots past 0 in demand-zero reservations, paid by the slots that run | `StatusResourceExhausted` from thread create | none |
| `kernel/kcall.MaxThreadSnapshots` | 63 | protocol (follows `ThreadSlots`) | process | 416 B per held thread, in a reservation | n/a | n/a |
| `kernel/kcall.MaxPreemptTicks` | 1000 | policy | task | n/a | value refused | n/a |
| `linux.NativeProcessReserve`, `LinuxGlobalMaxProcesses`, `LinuxFamilyMaxProcesses` | 32, 224, 64 | policy | global, family | process states and VFS contexts on first use (see `MaxProcesses`) | `EAGAIN` from `fork`/`clone` | `DIAG linux-tables` `processes`, `family-peak` |
| `linux.NativeTaskReserve` | 32 (`NativeProcessReserve`) | policy | global | none | `EAGAIN` | none |
| `RLIMIT_NPROC` | unlimited | policy | n/a | n/a | not enforced; the task quota, family limit and task reserve answer `EAGAIN` | `getrlimit` |

### IPC, streams, handles, waits, shared memory

| Bound | Default / max | Class | Owner | Memory | At exhaustion | Status |
| --- | --- | --- | --- | --- | --- | --- |
| `kernel/ipc.MaxChannels` | 256 | profile (build constant) | global; creator charged | three pages per live channel (about 10.5 KiB; ADR 0111) | `StatusResourceExhausted` | per process `ProcessInfo`; global `DIAG tables` `channels` |
| `kernel/ipc.MaxPayload` | 256 | protocol | n/a | n/a | `StatusInvalidArgument` | n/a |
| `kernel/ipc.MaxAttachedHandles` | 4 | protocol | n/a | n/a | `StatusInvalidArgument` | n/a |
| `ipc.QueueCapacity` | 16 messages | profile | channel | inside the live channel object | sender waits or `StatusResourceExhausted` | none |
| `kernel/ipc.MaxWaiters` | 256 | protocol (one per task) | global | static | cannot fill | n/a |
| `kernel/stream.MaxStreams` | 256 | profile (build constant) | global; creator charged | two pages per live stream (8176-byte ring plus counters) | `StatusResourceExhausted` | per process `ProcessInfo`; global `DIAG tables` `streams` |
| `kernel/stream.MaxReferences` | 65535 | protocol (counter width) | stream | n/a | refused before wrap; 256 tables of 256 handles reach 65536 | n/a |
| `kernel/handle.MaxHandles` | 256 | policy | process | 24 B a slot, 6 KiB per process record | `StatusResourceExhausted` | `HandleInfo` per slot; count none |
| `kernel/wait.MaxObjects` | 64 | protocol (ready-mask width) | wait | n/a | `StatusInvalidArgument` | n/a |
| `userspace/wait.MaxObjects` | 64 | protocol | n/a | n/a | n/a | n/a |
| `kernel/shmem.MaxObjects` | 256 | profile (build constant) | global | a page per live object | `StatusResourceExhausted` | `DIAG tables` `shmem` |
| `kernel/shmem.MaxObjectsPerProcess` | 16 | policy | process | none | `StatusResourceExhausted` | none |
| `kernel/shmem.MaxObjectsPerSupervisor` | 32 | policy | PID 1 | none | `StatusResourceExhausted` | none |
| `kernel/shmem.MaxPages` | 65536 pages (256 MiB) | profile | creator's page quota | on touch | `StatusResourceExhausted` | `ProcessInfo` pages |
| `kernel/executable.MaxObjects` | 256 (`process.MaxProcesses`) | policy (one per process) | PID 1 | a page per object; 8 B per slot static | `StatusResourceExhausted` | `DIAG tables` `executables` |
| `kernel/executable.MaxSize` | 256 MiB | profile | launcher's page quota | pages | `StatusResourceExhausted` | n/a |
| `kernel/executable.MaxWrite` | 1 MiB | protocol | n/a | n/a | short write | n/a |

### Memory, boot and processors

| Bound | Default / max | Class | Owner | Memory | At exhaustion | Status |
| --- | --- | --- | --- | --- | --- | --- |
| `kernel/mm/pmm.MaxPhysical` | 512 GiB | hardware (one PML4 slot of direct map) | global | n/a | memory above stays unmanaged | `SystemInfo` managed pages |
| `kernel/mm/pmm.MaxSegments` | 64 | hardware (firmware map) | global | static | further ranges unmanaged | `SystemInfo` managed pages |
| `kernel/mm/pagelist.MaxIndexPages` | 128 | profile | object | a page per index page | `StatusResourceExhausted` | n/a |
| `kernel/mm/pagelist.MaxFrames` | 65536 | profile | object | n/a | as above | n/a |
| `kernel/mm/region.MaxRegions` | 128 | artifact | process | fixed table in the process; one entry a native thread | `StatusResourceExhausted`, Linux `ENOMEM` | none |
| `kernel/boot.MaxRegions` | 256 | hardware (firmware map after merging) | global | static | boot fails | n/a |
| `kernel/boot.MaxInitramfs` | 6 MiB | artifact | build | n/a | build and boot refuse | n/a |
| `kernel/boot.MaxFramebufferWidth`, `kernel/boot.MaxFramebufferHeight`, `kernel/boot.MaxFramebufferSize` | 8192, 8192, 256 MiB | hardware | n/a | n/a | framebuffer refused, serial only | n/a |
| `kernel/acpi.MaxProcessors` | 256 | hardware (MADT parse bound) | global | static | extra entries ignored | n/a |
| `kernel/cpu.MaxCPUs` | 64 | artifact | global | static | processors stay offline | `CPUInfo` |
| `kernel/cpu.MaxXAPICID` | 0xfe | hardware | n/a | n/a | processor not started | n/a |
| `kernel/arch/amd64.MaxCPUs` | 64 | artifact | global | static | as `cpu.MaxCPUs` | n/a |
| `kernel/lock.MaxCPUs` | 64 | artifact (validation builds only) | n/a | static | n/a | n/a |
| `kernel/cpu/x86.MaxCaches` | `cpuinfo.MaxCaches` | hardware (CPUID leaf 4 entries) | n/a | n/a | extra levels unreported | n/a |
| `kernel/loader.MaxSegments` | 8 | protocol (native image format) | n/a | n/a | `LaunchInvalidImage` | n/a |
| `kernel/loader.MaxImageExtent` | 4 GiB - 1 | protocol (cache key) | n/a | n/a | refused | n/a |
| `kernel/loader.MaxSharedSegments`, `kernel/loader.MaxSharedPages` | 128, 1024 | profile | global | 4 MiB of shared image pages | launch takes a private copy | none |
| `kernel/loader.MaxStackReserve` | 64 MiB | policy | process | address space only | `LaunchInvalidImage` | n/a |

### Kernel calls and kernel log

| Bound | Default / max | Class | Owner | Memory | At exhaustion | Status |
| --- | --- | --- | --- | --- | --- | --- |
| `kernel/kcall.MaxConsoleWrite`, `kernel/kcall.MaxStreamIO`, `kernel/kcall.MaxHoldRange`, `kernel/kcall.MaxIPCMessage`, `kernel/kcall.MaxRandomRead`, `kernel/kcall.MaxProcessName`, `kernel/kcall.MaxStartupSize`, `kernel/kcall.MaxExecutableWrite` | 4096, 256, 4096, 320, 64, 16, 64 KiB, 1 MiB | protocol (ABI) | n/a | per-CPU copy areas | `StatusInvalidArgument` or a short transfer | n/a |
| `kernel/kcall.MaxLogRecord` | 181 | protocol | n/a | n/a | truncated | n/a |
| `kernel/kcall.MaxInstruction` | 15 | hardware (x86 instruction length) | n/a | n/a | n/a | n/a |
| `klog.Capacity`, `kcall.LogRingCapacity` | 2048 records | profile (build constant) | global | 416 KiB BSS | oldest record overwritten | `LogReadSequence` gap flag; `logd` `counters` record (`overwritten`, `lost`) |
| `kcall.LogReadMax` | 64 records | protocol | n/a | per-CPU copy area | reader continues | n/a |

### VFS and RAMFS

| Bound | Default / max | Class | Owner | Memory | At exhaustion | Status |
| --- | --- | --- | --- | --- | --- | --- |
| `kernel/vfs.MaxOpenFiles` | 256 | profile (build constant) | namespace | a few dozen bytes each, static | `StatusResourceExhausted` | none |
| `kernel/vfs.MaxOpenPerOwner` | 64 | policy | VFS session | none | `StatusResourceExhausted`, Linux `ENFILE` | none |
| `kernel/vfs.MaxMounts` | 8 | profile | namespace | static | `StatusResourceExhausted` | `mount` listing |
| `kernel/vfs.MaxCache` | 32 | profile | namespace | static | entry evicted | none |
| `kernel/vfs.MaxScopes`, `kernel/vfs.MaxAuthorities` | 8, 64 | policy | session | static | `StatusResourceExhausted` | none |
| `kernel/vfs.MaxRootPath` | 64 | protocol | n/a | n/a | refused | n/a |
| `kernel/vfs.MaxPath` | `limits.PathMax` | protocol (ADR 0110) | n/a | n/a | `StatusInvalidArgument` | n/a |
| `kernel/vfs.MaxProtocolData`, `kernel/vfs.MaxReplyData`, `kernel/vfs.MaxInlineName`, `kernel/vfs.MaxDirBatch` | message payload less headers; 4096 | protocol | n/a | n/a | n/a | n/a |
| `kernel/ramfs.MaxNodes` | 65536 | profile | instance | 64-node chunks on demand | `StatusResourceExhausted` | `df` |
| `kernel/ramfs.MaxFileSize` | about 4 GiB | protocol (index depth) | file | pages on write | `StatusResourceExhausted` | n/a |
| `kernel/ramfs.MaxName`, `kernel/ramfs.MaxVolumeName`, `kernel/ramfs.MaxInlineData`, `kernel/ramfs.MaxDirBatch`, `kernel/ramfs.MaxDirRecord` | `NameMax`, 64, payload, 4096, record | protocol | n/a | n/a | refused | n/a |
| `userspace/vfs.MaxReadWorkers` | 4 | profile | VFS service | one worker each | requests queue (`ReadQueueCapacity` 8) | none |
| `userspace/block.Max*` | payload less header; 1 MiB bulk | protocol | n/a | n/a | refused | n/a |
| `userspace/partition.Max*` | 128 partitions, 108-byte names | protocol (GPT) | n/a | n/a | table refused as unsupported | n/a |

### Linux compatibility

| Bound | Default / max | Class | Owner | Memory | At exhaustion | Status |
| --- | --- | --- | --- | --- | --- | --- |
| `RLIMIT_NOFILE` | soft 1024, hard 4096 (`LinuxFDCeiling`) | policy (ADR 0212) | process | 64 entries inline, then a 4 KiB page per 1024, charged to the process | `EMFILE`; `ENOMEM` when the table cannot grow | none (`prlimit64` reads the limit) |
| `kernel/compat/linux.MaxOpenDescriptions` | 4096 | profile (build constant) | global | static pool | `ENFILE` | `DIAG linux-tables` `descriptions` |
| `kernel/compat/linux.MaxOpenDirectories` | 32 | artifact | global | static, a path each | `ENFILE` | `DIAG linux-tables` `directories` |
| `kernel/compat/linux.MaxEventCounters` | 64 | artifact | global | static | `ENFILE` | `DIAG linux-tables` `eventfds` |
| `kernel/compat/linux.MaxTerminals` | 8 | policy (one per session PID 1 serves) | global | static | launch refused | `DIAG linux-tables` `terminals` |
| `kernel/compat/linux.MaxNetFamilies` | 16 | policy (follows `linuxsockd`) | global | static | `socket` fails, `ENFILE` | GLNXCTL2 `Families` |
| `kernel/compat/linux.MaxSockets` | 512 | protocol (GLNXCTL2 index field) | family | readiness and snapshot pages of the pool | `ENFILE` | GLNXCTL2 `FamilySockets` |
| `kernel/compat/linux.MaxSocketRequests` | 8 | artifact | family | static | caller waits for a slot | none |
| `kernel/compat/linux.MaxIOVec` | 16 | artifact | n/a | per-CPU 256 B | `EINVAL`; `EMSGSIZE` for `sendmsg` | n/a |
| `kernel/compat/linux.MaxVectorBytes`, `kernel/compat/linux.MaxDeviceTransfer` | 64 KiB | profile (scratch size) | n/a | per-CPU | short transfer | n/a |
| `kernel/compat/linux.MaxTransfer` | 0x7ffff000 | protocol (`MAX_RW_COUNT`) | n/a | n/a | short transfer | n/a |
| `kernel/compat/linux.MaxRandomBytes` | 256 | protocol (`getrandom` promise) | n/a | n/a | short read | n/a |
| `kernel/compat/linux.MaxPID` | 32767 | protocol (`pid_max`) | global | n/a | wraps | n/a |
| `kernel/compat/linux.MaxDirentSize`, `kernel/compat/linux.MaxDirentExchanges` | one record; 128 exchanges | protocol | n/a | n/a | short listing, caller continues | n/a |
| `kernel/compat/linux.MaxExecArgs`, `kernel/compat/linux.MaxExecEnv`, `kernel/compat/linux.MaxExecStringBytes`, `kernel/compat/linux.MaxExecTotalBytes` | 256, 256, 32 KiB, 64 KiB | artifact | n/a | exec scratch | `E2BIG` | n/a |
| `kernel/compat/linux/abi.Max*` | 64 KiB stack, 256 arguments, 256 variables | artifact | n/a | n/a | `E2BIG` | n/a |
| `kernel/compat/linux/elf.Max*` | 64 program headers, 16 load segments, 16 reserved ranges | artifact | n/a | n/a | `ENOEXEC` | n/a |

### Network service (`netd`)

Keys are under `system.net.` and documented in
[configuration](../userspace/configuration.md). `net status` reads the UDP
fields from `OpStatus` and the TCP fields from `OpTCPStatus`.

| Bound | Default / max | Class | Owner | Memory | At exhaustion | Status |
| --- | --- | --- | --- | --- | --- | --- |
| `userspace/network.MaxTCPEndpoints` (`tcp.max-endpoints`) | 1024 / 4096 | profile | global | 416 B control block on open | `Busy` from `Open` | `OpTCPStatus` `TCPEndpoints`, `TCPMaxEndpoints` |
| `userspace/network.MaxTCPEndpointsPerSession` (`tcp.max-endpoints-per-session`) | 8 / 8 | policy | session | none | `Busy` | `OpTCPStatus` `TCPSessionEndpoints` |
| `userspace/network.MaxTCPSessionQuota` | 512 | policy (PID 1 grant) | session | per-session tables sized to the grant | session refused | none |
| `userspace/network.MaxTCPSessions` | 64 | policy | global | per owner | session refused | none |
| `netd` `tcpSessionSlots` | 46 | policy (one `WaitAny` of 64 objects watches every session) | `netd` | static | session refused | `OpTCPStatus` `Capacity.TCPSessions`, `TCPSessionLimit` |
| `userspace/network.MaxTCPListeners` | 256 | profile | global | index entry | `Busy` from `Listen` | `OpTCPStatus` `TCPListeners`; limit none |
| `userspace/network.MaxTCPBacklog` (`tcp.listen-backlog`) | 128 / 4096 | policy | listener | children are endpoints | SYN dropped, `DropBacklog` | limit and `Capacity.DropBacklog` in `OpTCPStatus` |
| `userspace/network.MaxTCPBuffer` (`tcp.send-buffer-bytes`, `tcp.recv-buffer-bytes`) | 256 KiB / 4 MiB | profile | connection | ring from 4 KiB, doubling | write `Busy`, window closes | limits in `OpTCPStatus` |
| `userspace/network.MaxTCPMemoryBudget` (`tcp.memory-bytes`) | 40 MiB / 48 MiB | profile | global | the budget is the cost | `Busy`, SYN dropped, `DropMemory` | `OpTCPStatus` `Capacity.MemoryBytes`, `MemoryBudget`, `DropMemory`, `MemoryCapped` |
| `tcp.memory-bytes-per-session` | 32 MiB / 48 MiB | policy | session | inside the global budget | as above | `OpTCPStatus` `Capacity.OwnerMemoryBudget` |
| TIME_WAIT tombstones (`SetTCPTimeWaitBudget`) | 8192, 2048 per owner | profile | global, owner | 56 B each | endpoint kept, then `Busy` | `OpTCPStatus` `Capacity.TimeWait`, both budgets, `TimeWaitHeld` |
| `userspace/network.MaxUDPEndpoints` (`udp.max-endpoints`) | 1024 / 4096 | profile | global | per endpoint plus receive queue | `Busy` | `net status` `UDPEndpoints`, `UDPMaxEndpoints` |
| `userspace/network.MaxUDPSessionEndpoints` (`udp.max-endpoints-per-session`) | 8 / 8 | policy | session | none | `Busy` | `net status` `UDPSessionEndpoints` |
| `userspace/network.MaxUDPSessionQuota` | 512 | policy | session | none | session refused | none |
| `userspace/network.MaxUDPSessions` | 8 | profile | global | static | session refused | `OpTCPStatus` `Capacity.UDPSessions`, `UDPSessionLimit` |
| `userspace/network.MaxSessions` | 8 command sessions | profile | global | static | session refused | `OpTCPStatus` `Capacity.CommandSessions`, `CommandSessionLimit` |
| `userspace/network.MaxNeighbors`, `userspace/network.MaxResolutions` | 16, 4 | profile | global | static | the entry closest to expiry is replaced; a resolution waits | none |
| `userspace/network.MaxDNSServers` | 4 | protocol (lease option kept) | n/a | n/a | extra servers ignored | `net status` servers |
| `userspace/network.MaxEchoTimeout`, `userspace/netapi.MaxEchoTimeoutMS` | 5 s | policy | n/a | n/a | refused | n/a |
| `userspace/network.MaxFrame`, `userspace/netdev.MaxFrame` | 1514 | protocol (Ethernet) | n/a | n/a | frame dropped | n/a |
| `userspace/network.MaxPrefix`, `userspace/network.MaxIPv4Text`, `userspace/network.MaxMACText`, `userspace/netapi.MaxPrefix` | 30, 15, 17, 32 | protocol | n/a | n/a | refused | n/a |
| `userspace/netapi.MaxServers` | 4 | protocol (status reply) | n/a | n/a | n/a | n/a |
| GNETAPI1 reply | 256 bytes, version 8 | protocol (`ipc.MaxPayload`) | n/a | n/a | n/a | n/a |
| `userspace/netapi.MaxSessionQuota` | 512 | protocol (16-bit control field, version 7) | n/a | n/a | refused | n/a |
| `userspace/netdev.MaxQueueSize` | 1024 | hardware (virtio queue) | driver | queue pages | device refused | none |
| `userspace/netdev.MaxFaultReason` | enum bound | protocol | n/a | n/a | n/a | n/a |

### Native transport layouts

| Bound | Default / max | Class | Owner | Memory | At exhaustion | Status |
| --- | --- | --- | --- | --- | --- | --- |
| GTCP 3 (`tcpapi.Layout3`) | 8 TX and 8 RX slots of 16 KiB | protocol | session | 256 KiB of pool | writer waits for a slot | `OpTCPStatus` GTCP message counters |
| GTCP 4 (`tcpapi.Endpoints4`, slots) | 512 endpoints, 16 TX and 16 RX slots of 16 KiB | protocol (ADR 0214) | session | 512 KiB of pool plus service pages | writer waits | GTCP counters |
| `userspace/tcpapi.MaxCredit4` | 16 | protocol | endpoint | n/a | fill refused | n/a |
| `userspace/tcpdispatch.MaxPending` | 32 | profile | session client | static | caller waits | none |
| `userspace/udpapi.Max*` | 4096 | protocol (slot size) | n/a | n/a | datagram refused | n/a |

### Linux socket service (`linuxsockd`)

| Bound | Default / max | Class | Owner | Memory | At exhaustion | Status |
| --- | --- | --- | --- | --- | --- | --- |
| `linuxsock.DefaultSystemSockets` / `userspace/linuxsock.MaxSystemSockets` | 1024 / 4096 | policy | service | 64-socket chunks on demand | `ENFILE` from `socket`/`accept` | GLNXCTL2 `Sockets`, `SocketLimit` |
| family quota (`DefaultFamilySockets`) / `userspace/linuxsockapi.MaxSockets` | 512 / 512 | policy; protocol ceiling | family | pool pages per family | `ENFILE` | GLNXCTL2 `FamilyLimit`, `FamilySockets` |
| `DefaultFamilyReserve` | 32 per family | policy | family | none | n/a | GLNXCTL2 `FamilyReserve` |
| `userspace/linuxsock.MaxFamilies` | 16 | artifact | service | family table | family refused | GLNXCTL2 `Families` |
| `userspace/linuxsock.MaxBacklog` | 255 | protocol (GTCP listen backlog byte) | listener | none | Linux backlog clamped; `netd` clamps again to `tcp.listen-backlog` | limit in `OpTCPStatus` `TCPListenBacklog` |
| `userspace/linuxsock.MaxWarm` | 8 per protocol | profile | service | 8 endpoints of its native quota | opened on demand | none |
| `userspace/linuxsockapi.MaxDatagram` | 4096 | protocol (GUDP) | n/a | n/a | `EMSGSIZE` | n/a |

### Resolver

| Bound | Default / max | Class | Owner | Memory | At exhaustion | Status |
| --- | --- | --- | --- | --- | --- | --- |
| `userspace/dns.MaxCacheEntries` (`system.dns.cache.entries`) | 64 / 256 | profile | service | static | oldest evicted | `resolve status` `CacheUsed`, `CacheLimit` |
| `userspace/dns.MaxClients` | 24 | profile (16 sessions and 8 stub requests) | service | static | one slot per session and stub request, so it cannot fill | none |
| `userspace/dns.MaxTransactions` | 4 | artifact | service | static | lookup waits | none |
| `userspace/dns.MaxWaiters` | 8 per transaction | profile | transaction | static | new transaction | none |
| `userspace/dns.MaxServers`, `userspace/resolvapi.Max*` | 8 | protocol | n/a | n/a | n/a | `resolve status` |
| `userspace/dns.MaxAddresses`, `userspace/dns.MaxCNAMEChain`, `userspace/dns.MaxQuery`, `userspace/dns.MaxStreamMessage` | 8, 8, query size, 65535 | protocol | n/a | n/a | answer truncated or refused | n/a |
| `userspace/tcpupstream.MaxStreams` | 2 | artifact | service | 64 KiB each | transaction waits | `TCPFallbacks` counters |
| `userspace/dnsstub.MaxUDPRequests`, `userspace/dnsstub.MaxConnections`, `userspace/dnsstub.MaxRequests` | 6, 2, 8 | artifact | stub | static | datagram dropped, connection not accepted | none |
| `userspace/dnsstub.MaxStreamQuery` | EDNS payload | protocol | n/a | n/a | connection closed | n/a |
| `userspace/dnswire.Max*`, `userspace/dnstcp.Max*`, `userspace/dnsq.Max*`, `userspace/hostname.Max*` | DNS name, label, pointer and message widths; `dnsq` 60 s timeout | protocol | n/a | n/a | refused | n/a |

### Supervisor, launch and configuration

| Bound | Default / max | Class | Owner | Memory | At exhaustion | Status |
| --- | --- | --- | --- | --- | --- | --- |
| `userspace/init.MaxServices` | 32 | profile | PID 1 | a few hundred bytes each | service refused at boot | `services` |
| `userspace/init.MaxDependencies`, `userspace/init.MaxCapabilities` | 8, 10 | protocol (launch payload) | n/a | n/a | declaration refused | n/a |
| `userspace/init.MaxEvents` | 32 | profile | PID 1 | static | events wait | none |
| `userspace/init.MaxRestarts` | 3 | policy | service | none | service stays failed | `services` |
| `userspace/init.MaxName`, `userspace/init.MaxExecutable`, `userspace/init.MaxProcessName`, `userspace/init.MaxBootText` | 31, 127, 16, 64 | protocol | n/a | n/a | refused | n/a |
| `userspace/exec.MaxStages` | 4 | policy | shell job | none | pipeline refused | n/a |
| `userspace/exec.MaxPath`, `userspace/exec.MaxLaunchObject`, `userspace/exec.MaxServiceEntries`, `userspace/exec.MaxServiceIndex`, `userspace/exec.MaxServiceName` | path, slot, 11, 32, 15 | protocol | n/a | n/a | refused or paged | n/a |
| `userspace/config.MaxEntries` | 96 | profile | `configd` | static | declaration refused | none |
| `userspace/config.MaxKey`, `userspace/config.MaxText`, `userspace/config.MaxHostName` | 63, 64, 248 | protocol | n/a | n/a | `InvalidArgument` | n/a |
| `userspace/config.MaxHosts` | 64 | profile | `configd` | static | `Set` refused | `hosts` listing |
| `userspace/startup.Max*` | 64 KiB object, 64 handles | protocol | n/a | n/a | launch refused | n/a |
| `userspace/commands.Max*`, `userspace/release.Max*` | index and record widths | protocol | n/a | n/a | entry refused | n/a |

### Remote access

| Bound | Default / max | Class | Owner | Memory | At exhaustion | Status |
| --- | --- | --- | --- | --- | --- | --- |
| `userspace/remoteapi.MaxSessions` | 4 | policy | `remoted` | one worker process each | connection closed | `remote status` `sessions`, `max-sessions` |
| `userspace/remoteapi.MaxHandshakes`, `userspace/remoteapi.MaxWorkers` | 2, 6 | policy | `remoted` | one worker each | connection closed | `remote status` `pending` |
| `userspace/remoteapi.MaxKeys`, `userspace/remotepolicy.MaxAuthorizedKeys` | 64 | policy | policy page | static | extra keys refused | `remote status` `key-count` |
| `userspace/remoteapi.MaxTerm`, `userspace/remoteapi.MaxCommand`, `userspace/remoteapi.MaxUsername`, `userspace/remoteapi.MaxSFTPRoot`, `userspace/remoteapi.MaxStatus`, `userspace/remoteapi.MaxStatusParts` | 64, 4096, 32, 64, payload, 32 | protocol | n/a | n/a | refused | n/a |
| `userspace/remotepolicy.MaxAuthorizedKeysBytes`, `userspace/remotepolicy.MaxAuthorizedKeyLine`, `userspace/remotepolicy.MaxKeyComment`, `userspace/remotepolicy.MaxPath`, `userspace/remotepolicy.MaxUser` | 64 KiB, 4096, 64, 64, 32 | protocol | n/a | n/a | file or value refused | n/a |
| `userspace/sftp.MaxHandles` | 32 per session | policy | SFTP session | static | `SSH_FX_FAILURE` | none |
| `userspace/sftp.MaxPacket`, `userspace/sftp.MaxData` | 64 KiB, 32 KiB | protocol | n/a | n/a | packet refused | n/a |
| `userspace/sshserver.Max*` | 32 KiB packet, 16 KiB data | protocol (RFC 4253 minimum) | n/a | n/a | connection closed | n/a |

### Log persistence

| Bound | Default / max | Class | Owner | Memory | At exhaustion | Status |
| --- | --- | --- | --- | --- | --- | --- |
| `userspace/logpersist.MaxSegmentBytes` (`system.log.segment-bytes`) | 16 MiB / 1 GiB | profile | `logd` | disk | rotation | `log status` `rotations`, `segment-bytes` |
| `userspace/logpersist.MaxSegments` (`system.log.segments`) | 4 / 9 | profile (one-digit names) | `logd` | disk | oldest segment removed | `log status` `segments` |
| `userspace/logpersist.MaxStatus` | 512 | protocol (eleven counters of at most 20 digits) | `logd` | static | n/a | n/a |

### Runtime and terminals

| Bound | Default / max | Class | Owner | Memory | At exhaustion | Status |
| --- | --- | --- | --- | --- | --- | --- |
| `userspace/runtime/goroutine.MaxGoroutines` | 256 (`DefaultLimit`, `SetLimit`) / 1024 | profile | process | a stack per live goroutine | `go` panics: "limit of goroutines reached" | `goroutine.ReadStats` `Live`, `Peak`, `Limit` (no command prints it) |
| `userspace/runtime/goroutine.MaxWaiters` | 63 | artifact | process | none | waiter blocks | none |
| `userspace/tty.Max*`, `kernel/tty.Max*`, `kernel/keyboard.Max*`, `userspace/shell.Max*` | line, prompt, TERM and geometry widths | protocol | n/a | n/a | line split or refused | n/a |

### Applications and libraries

All bounds in these packages limit one input, one buffer or one document of
a single command. None is a shared table.

| Bound | Values | Class |
| --- | --- | --- |
| `userspace/editor.Max*`, `userspace/viewer.Max*` | 1 MiB document; 256 × 128 screen | protocol (document and terminal size) |
| `userspace/admincmd.Max*` | 64 MiB input, 8192-byte line, 256-byte pattern | protocol (input limits of text tools) |
| `userspace/fetch.Max*`, `userspace/httpwire.Max*` | 4096-byte URL, 16 headers, 5 redirects; 16 KiB of response headers | protocol (HTTP) |
| `userspace/tlsclient.Max*`, `userspace/truststore.Max*`, `userspace/truststate.Max*` | 512 KiB bundle, 256 roots, 8 certificates per chain, 64 generations | protocol (trust bundle format) |
| `userspace/filemanager.MaxEntries` | 1024 per panel, shown as truncated past it | protocol (display) |
| `userspace/utility.Max*` | bench bounds | protocol (one bench run) |
