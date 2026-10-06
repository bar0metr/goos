# Capabilities: security view

The data structures and transfer rules are described in
[docs/architecture/capabilities.md](../architecture/capabilities.md). This
document states the security properties and who holds what.

## Properties

- **No ambient authority.** Every native call that touches a kernel object
  takes a process-local handle and looks it up with an expected type and a
  required rights mask. There is no global object namespace visible to
  userspace, no root user, and no UID 0.
- **Generation-safe IDs.** A handle ID is `generation << 32 | (slot + 1)`
  (`kernel/handle/table.go`). Close clears the slot and increments the
  generation, so stale and forged IDs fail lookup after slot reuse. Zero
  index, index above 128, and generation 0 are rejected before indexing.
- **Typed lookup.** A channel handle cannot be used as a stream, device, IRQ,
  memory, or process handle. Stream endpoints additionally encode their direction.
- **Complete rights match.** `Capability.Allows` requires every requested bit.
  An empty request never succeeds.
- **Non-escalating derivation.** `Derive` returns a capability only if the
  requested mask is a non-empty subset of the source mask.
- **Explicit transfer.** Moving authority to another process requires the
  Transfer right on the source handle. IPC transfer also removes Transfer from
  the delivered handle and closes the sender's handle, so a received
  capability cannot be forwarded again.
- **Counted stream ends.** A stream end counts the handles that name it in
  every table and closes with the last (ADR 0089). Holding a copy of an end
  confers nothing beyond the end's direction: several processes can write one
  stream, but only a holder of its reading end reads it, and closing one's own
  handle cannot close the end for another holder. Counts cannot underflow and
  are bounded before any handle is installed.

## Rights

| Right | Bit | Meaning |
| --- | --- | --- |
| Read | `0x01` | receive IPC, read stream, read console and wait on it (the console object), read the log (the log object), read ports, map shared memory or the system image |
| Write | `0x02` | send IPC, write stream, write console and the log, write ports, allocate DMA, map shared memory writable, stop a process, reset the machine |
| Execute | `0x04` | launch a native image from the system image object (PID 1 only, ADR 0093) |
| Transfer | `0x08` | derive into another process (manifest or IPC attachment) |
| Wait | `0x10` | block on a channel, stream, process, or IRQ |

## Spawn manifests

Only PID 1 can reach the kernel launch path: launch requests arrive on the
lifecycle channel, whose only endpoint is in PID 1. A request names its
executable as an extent of an image object, together with the handle that
authorizes executing from it, and carries at most seven grants of
`{source handle, type, rights, name}` (ADR 0024, 0031, 0093). The kernel
resolves no path: PID 1 found the executable in its own read-only mapping of
the system image, or opened a stable VFS object and converted its sealed header
object into a file-backed source using the non-transferable loader capability
(ADR 0121). The request also names one kernel-defined resource profile; PID 1
cannot submit numeric limits (ADR 0122).

Before creating the address space the kernel:

0. requires the image handle in PID 1's table to be the system image with
   Execute and the extent to lie inside the image, or a sealed executable
   source with Execute named whole; the native image validator then decides
   whether the bytes are a program;
1. resolves each source in PID 1's table with Transfer authority;
2. checks that the requested rights derive from the source;
3. installs derived handles into the empty child table in manifest order,
   rolling back on failure;
4. verifies each installed handle has exactly the requested rights;
5. binds the handle IDs into the child's startup object by role name.

A grant marked `Consume` closes PID 1's source handle after a successful
launch. Exclusive endpoints (service channel ends, pipeline streams) are consumed, so
PID 1 does not keep a second copy. The system image is not: PID 1 keeps it with
Execute to launch from, and the filesystem service receives a Read-only copy
that cannot launch anything. The storage
driver's device authority is the exception: PID 1 keeps it to relaunch the
driver (ADR 0078). Startup role names locate handles; they do not grant
authority.

## Holders at boot

The kernel installs PID 1's initial table in a fixed order before PID 1 runs
and panics if any slot lands out of order (`kernel/main.go`). PID 1 then
derives rights into children through the manifests in
`userspace/cmd/init/services.go`. The rows below are the normal build; the
validation build also grants the console, write only, to `fault-probe`, and
uses slot 9 for a block-service probe (ADR 0058).

| PID 1 slot | Object | PID 1 rights | Derived to | Child rights | Consumed |
| --- | --- | --- | --- | --- | --- |
| 1 | lifecycle channel | R W Wait | PID 1 only | - | - |
| 2 | console device | R W T | VFS, block, virtio-blk, fat32, config, log, netd, virtio-net | W | no |
| | | | tty `console` | R W | no |
| 3 | VFS session, client end | R W Wait T | shell `vfs` | R W Wait | yes |
| 4 | VFS session, server end | R W Wait T | VFS `session` | R W Wait | yes |
| 5 | VFS control, client end | R W Wait | PID 1 only | - | - |
| 6 | VFS control, server end | R W Wait T | VFS `control` | R W Wait | yes |
| 7 | exec, server end | R W Wait | PID 1 only | - | - |
| 8 | exec, client end | R W Wait T | shell `exec` | R W Wait | yes |
| 9 | block, client end | R W Wait | PID 1 only | - | - |
| 10 | block, server end | R W Wait T | block `block` | R W Wait | yes |
| 11 | virtio port range | R W T | virtio-blk `ports` | R W | no |
| 12 | IRQ 11 | Wait T | virtio-blk `irq` | Wait | no |
| 13 | DMA domain 1 | W T | virtio-blk `dma` | W | no |
| 14 | driver, client end | R W Wait T | block `driver` | R W Wait | yes |
| 15 | driver, server end | R W Wait T | virtio-blk `service` | R W Wait | yes |
| 16 | FS-block, client end | R W Wait T | fat32 `block` (first launch) | R W Wait | yes |
| 17 | FS-block, server end | R W Wait T | block `filesystem` | R W Wait | yes |
| 18 | system control | W | PID 1 only | - | - |
| 19 | launch staging page (memory) | R W | PID 1 only; mapped by the kernel at `0x401ff000` | - | - |
| 20 | exec area (memory) | R W T | shell `execpage` | R W | no |
| 21 | system image | R X T | VFS `image` | R | no; PID 1 keeps it to launch from (ADR 0093) |
| 22 | log | R T | log `log`, shell `log` | R | no |
| 28 | virtio-net port range | R W T | virtio-net `ports` | R W | no |
| 29 | network IRQ line | Wait T | virtio-net `irq` | Wait | no |
| 30 | DMA domain 2 | W T | virtio-net `dma` | W | no |

Slots 23 to 27 hold the loader, the file cache, the two terminal consoles and
the second exec area. Without a usable network function, slots 28 to 30 hold
inert objects that no call accepts, Read on the ports and DMA slots and Wait
on the line, all without Transfer; PID 1 starts no network driver then
(ADR 0190).

Both drivers' port, IRQ, and DMA authority is granted, not consumed, so PID 1
can hand the same authority to a restarted driver; the dead driver's copies go
with its teardown (ADR 0078, ADR 0190).

Only the terminal service holds the console's read right: nothing else can read
what is typed (ADR 0082). The shell and the commands hold no console capability
at all (ADR 0089). PID 1 creates, when it starts the terminal service:

| Object | PID 1 keeps | Derived to | Child rights | Consumed |
| --- | --- | --- | --- | --- |
| terminal output stream, reading end | R Wait T | tty `output` | R Wait | no |
| terminal output stream, writing end | W Wait T | shell `stdout`; every stage's `stdout`/`stderr` that is not piped or redirected | W Wait | no |
| terminal session, server end | - | tty `session` | R W Wait | yes |
| terminal session, client end | - | shell `tty` | R W Wait | yes |
| terminal control, client end | R W Wait T | PID 1 only | - | - |
| terminal control, server end | - | tty `control` | R W Wait | yes |

PID 1 keeps both ends of the output stream so that neither ever closes: the
terminal service's reader survives the service, and no writer sees it go away.
When the terminal service or the shell restarts, PID 1 creates a new session
(and, for the shell, a new exec channel and new VFS and configuration sessions)
and hands the survivor its end as a `GTTY` session notice: over the shell's
exec channel, or over the terminal's control channel, whose other end only PID 1
holds. The validation build additionally keeps both services' process handles
for fault injection.

PID 1 also creates channels at run time with `ChannelCreate`: the block
service's `control` endpoint, over which a restarted driver's channel and a
restarted FAT32 service's block channel are delivered; the channel between
the VFS and the FAT32 service, renewed with the block channel on every restart
of that service, whose VFS end goes over the VFS control endpoint (ADR 0096); the config service's `control` endpoint; one configuration session
each for the shell (whole namespace, read and write) and the log service
(`system.log.`, read only); and one VFS session each for the log service, the
config service, its own reading of the command index, and every command whose
index entry asks for one (ADR 0073, 0076, 0078, 0080). It keeps only the
client side of each control endpoint.

No child receives Transfer. After boot, PID 1 still holds slots 1, 2, 5, 7, 9,
11, 12, 13, 18, 19, 20, 28, 29, and 30, the client sides of the block,
config, network and terminal control endpoints, both ends of the terminal output stream, and a Wait + Write
process handle for each running job stage; the Write right is what
`ProcessKill` requires (ADR 0067). Slot 7, the exec server end, is replaced by a
fresh channel's when the shell restarts.

### Network

PID 1 creates the network's channels and pools at run time (ADR 0191):

| Object | PID 1 keeps | Derived to | Child rights | Consumed |
| --- | --- | --- | --- | --- |
| `netd` control, client end | R W Wait | PID 1 only | - | - |
| `netd` control, server end | - | netd `control` | R W Wait | yes |
| configuration session, `system.net.` read only | - | netd `config` | R W Wait | yes |
| driver channel, driver end | - | virtio-net `service` | R W Wait | yes |
| driver channel, service end | - | moved to `netd` in `SetDevice` | R W Wait | yes |
| receive pool, 2 pages | - | virtio-net `rx`; a copy moved to `netd` | R W; R | yes |
| transmit pool, 2 pages | - | virtio-net `tx`; a copy moved to `netd` | R; R W | yes |
| command session, service end | - | moved to `netd` in `AddSession` | R W Wait | yes |
| command session, client end | - | the command's `net` | R W Wait | yes |

A new driver channel and a new pair of pools are made for every driver
launch, and PID 1 keeps nothing of them once the driver starts. It narrows
its copies of the pools with `HandleDuplicate` before it moves them, and
`netd` checks through `HandleInfo` that each handle it receives carries
exactly the rights above, so it never holds write access to the receive pool.
A command session's rights class, ping and status or status and reload, is
recorded by `netd` against its end when PID 1 opens it; the command cannot
change it or pass the session on.

### Utilities

For each pipeline stage PID 1 grants:

| Role | Object | Rights |
| --- | --- | --- |
| `in` | stream reader: the pipeline's upstream stage, or a stream of its own that the shell feeds with typed lines (foreground) or that is at end of input (background) | R Wait |
| `stdout` | stream writer: the next stage's pipe, a redirection sink's, or a copy of the terminal's writing end | W Wait |
| `stderr` | stream writer: a `2>` sink's, or a copy of the terminal's writing end | W Wait |
| `vfs` | new private VFS session channel, only when the command's entry in `/system/commands` asks for one | R W Wait |
| `net` | new private network session channel, only when the entry asks for `GrantNetworkPing` or `GrantNetworkAdmin` and `netd` accepted the session | R W Wait |

The VFS session is a fresh channel from `ChannelCreate`. PID 1 sends the
server end to VFS as an IPC attachment on the control channel and consumes the
client end into the child manifest (docs/userspace/vfs.md). Which commands
get one is decided by the manifest each command declares in its source, which
the build turns into the command index in the immutable system image
(ADR 0080). A command without an index entry gets the standard streams only.

## Device authority

The kernel discovers the legacy virtio-blk function and the legacy
virtio-net function, and mints three separately typed objects for each, held
first by PID 1 (ADR 0034, 0035, 0190):

- **Ports**: a device object encoding the discovered I/O BAR base and size.
  `PortRead`/`PortWrite` require Read/Write, widths 1, 2, or 4, and
  `offset + width <= size`. There is no other port-I/O path from ring 3.
- **IRQ**: an IRQ object for line 11, the disk's, or for the network
  function's routed line. `IRQWait` requires Wait and serves line 11 only;
  a `WaitAny` interest and `IRQTake` require Wait and serve the network line
  only. The kernel masks the level-triggered route in the handler and
  unmasks it on the next wait.
- **DMA**: a DMA-domain device object, domain 1 for the disk and 2 for the
  network function. `DMAAllocate` requires Write, refuses a domain whose
  device was not discovered, and maps up to 64 zeroed contiguous pages RW+NX
  at one fixed user address, returning their physical address to the caller
  only.

When a process holding DMA authority exits, teardown masks that domain's
interrupt route and resets that domain's device before its owned pages are
reclaimed; a network driver's exit leaves the disk alone. Without an IOMMU
these checks bound what a driver can do through the CPU, not what its device
can do on its behalf, so both drivers are part of the effective trusted
computing base; see [threat-model.md](threat-model.md).

Four further device objects are not tied to hardware discovery:

- **Console**: COM1. Write authorizes `ConsoleWrite`, `LogWrite` and, in a
  `goos_diag` build, `Diagnostics`; Read authorizes `ConsoleRead`,
  `ConsoleInterrupt`, the read-only `ConsoleInfo`, and waiting on console input with `WaitAny` (ADR 0059,
  0075). Only the terminal service is given Read (ADR 0082). Write goes to the
  terminal service and to the system services that start before it and report
  through the log; the shell and commands print through the terminal's output
  stream instead (ADR 0089).
- **Log**: Read authorizes `LogRead` and `LogBounds` and nothing else, so the
  log service and the shell read the log without the console's input
  (ADR 0082).
- **System control**: `SystemReset` requires Write on it. Only PID 1 holds it,
  without Transfer (ADR 0061).
- **System image**: `ImageMap` requires Read on it and maps the boot archive
  read-only and non-executable. PID 1 consumes it into the VFS manifest
  (ADR 0070).

Memory handles name the two shared pages the kernel creates for PID 1 at boot.
`MemoryMap` requires Read, and Write for a writable mapping, and maps only
inside the shared window (ADR 0063).

## Operations without a capability

Some calls authorize by caller identity or need nothing:

| Operation | Authorization |
| --- | --- |
| `Exit`, `ClockRead`, `ClockReadNanos` | none |
| `StartupRead`, `SystemInfo`, `StreamCreate`, `ProcessInfo`, `CPUInfo`, `CPUEntry`, `CPUCacheInfo`, `CPURuntime` | caller must be a registered process |
| `TaskAffinity` | the calling task with handle zero; another process through a process handle, Wait to read and Write to set |
| `HandleInfo` | caller must be a registered process; describes only its own table |
| `ChannelCreate` | caller must be PID 1 |

`StreamCreate` is open because the new endpoints grant nothing beyond
themselves (ADR 0040); the per-process stream quota bounds how many one
process can hold (ADR 0072). `ProcessInfo` names no kernel object and reveals
only process names, states, and accounting. The CPU information calls expose
only immutable normalized boot records; they grant no CPUID, MSR, APIC, or CPU
startup access. The full list is in
[attack-surface.md](attack-surface.md).

## Linux processes

A Linux process is a GoOS process with a different call ABI and the same
authority model (ADR 0134, ADR 0136). The kernel accepts a Linux launch only
with exactly four grants: `in` with Read and Wait, `stdout` and `stderr` with
Write and Wait, and one `vfs` channel with Read, Write, and Wait. An extra
grant, a device, or a smaller resource profile refuses the launch.

| Claim | Proof |
| --- | --- |
| a Linux process cannot use the native call gate | `abi-probe isolation` executes `int 0x80` with all 128 call numbers and gets one refusal; the kernel self-test does the same from a synthetic task |
| a Linux descriptor is no native handle | descriptors index a compatibility table; handle-shaped values are `EBADF` |
| a guessed native handle is useless | with the native gate closed, no call takes one |
| file access needs the VFS session | a session without its handle or rights issues no request; after the VFS dies every file call is `EIO` |
| no device authority | the launch takes four stream and channel grants and refuses a fifth; an I/O port instruction faults and ends the process |
| no framebuffer, DMA, IRQ, or kernel log | each needs a native call or a device handle, and a Linux process has neither |
| no PID 1 launch authority | no launch channel is among the four grants |
| fork and exec create no authority | a forked child gets the parent's VFS channel capability and nothing else; `execve` loads only a static Linux image, never a native image or manifest (ADR 0157, ADR 0158) |
| a forked child cannot reach the native gate | `abi-probe hardening` runs the isolation check in a child |
| `kill` cannot cross families | the target must be the caller or a descendant in the family registry; another family or a native process is `ESRCH` (ADR 0166) |
| a fork bomb stays bounded | 16 processes per family, 48 for all families, 16 native slots reserved; `abi-probe limits` stops at 15 children (ADR 0155) |
| a forged signal frame cannot escalate | `rt_sigreturn` refuses a return address or stack outside the user half, and every return filters RFLAGS (ADR 0164) |

Linux user and group numbers, mode bits, and PIDs are presentation. No GoOS
check reads them.

## Tests

`make test-security` runs host tests for wrong types, missing rights, stale
and forged IDs, unauthorized transfer, rights escalation, capacity exhaustion,
manifest rollback, stream direction, stream reference counts (shared ends
held open by every holder, moves that keep the count, copies that add to it,
double closes and forged endpoints refused, the bound), and the shared-memory
and image mapping checks (`internal/capability`, `kernel/handle`, `kernel/ipc`, `kernel/stream`,
`kernel/shmem`, `kernel/loader`, `userspace/init`, and others). Quota charging
and release are tested in `kernel/process`. `make test-linux-boot` runs the
Linux proofs in the table above in a booted system.
