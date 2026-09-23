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

## Rights

| Right | Bit | Meaning |
| --- | --- | --- |
| Read | `0x01` | receive IPC, read stream, read console and wait on it (the console object), read the log (the log object), read ports, map shared memory or the system image |
| Write | `0x02` | send IPC, write stream, write console and the log, write ports, allocate DMA, map shared memory writable, stop a process, reset the machine |
| Execute | `0x04` | defined; no native call uses it yet |
| Transfer | `0x08` | derive into another process (manifest or IPC attachment) |
| Wait | `0x10` | block on a channel, stream, process, or IRQ |

## Spawn manifests

Only PID 1 can reach the kernel launch path: launch requests arrive on the
lifecycle channel, whose only endpoint is in PID 1. A request carries at most
seven grants of `{source handle, type, rights, name}` (ADR 0024, 0031).

Before creating the address space the kernel:

1. resolves each source in PID 1's table with Transfer authority;
2. checks that the requested rights derive from the source;
3. installs derived handles into the empty child table in manifest order,
   rolling back on failure;
4. verifies each installed handle has exactly the requested rights;
5. binds the handle IDs into the child's startup block by role name.

A grant marked `Consume` closes PID 1's source handle after a successful
launch. Exclusive endpoints (service channel ends, the system image, pipeline
streams) are consumed, so PID 1 does not keep a second copy. The storage
driver's device authority is the exception: PID 1 keeps it to relaunch the
driver (ADR 0078). Startup role names locate handles; they do not grant
authority.

## Holders at boot

The kernel installs PID 1's initial table in a fixed order before PID 1 runs
and panics if any slot lands out of order (`kernel/main.go`). PID 1 then
derives rights into children through the manifests in
`userspace/cmd/init/main.go`. The rows below are the normal build; the
validation build also grants the console, write only, to `fault-probe`, and
uses slot 9 for a block-service probe (ADR 0058).

| PID 1 slot | Object | PID 1 rights | Derived to | Child rights | Consumed |
| --- | --- | --- | --- | --- | --- |
| 1 | lifecycle channel | R W Wait | PID 1 only | - | - |
| 2 | console device | R W T | VFS, block, virtio-blk, config, log, shell | W | no |
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
| 16 | FS-block, client end | R W Wait T | VFS `block` | R W Wait | yes |
| 17 | FS-block, server end | R W Wait T | block `filesystem` | R W Wait | yes |
| 18 | system control | W | PID 1 only | - | - |
| 19 | launch staging page (memory) | R W | PID 1 only; mapped by the kernel at `0x401ff000` | - | - |
| 20 | exec page (memory) | R W T | shell `execpage` | R W | no |
| 21 | system image | R T | VFS `image` | R | yes |
| 22 | log | R T | log `log`, shell `log` | R | no |

The driver's port, IRQ, and DMA authority is granted, not consumed, so PID 1
can hand the same authority to a restarted driver; the dead driver's copies go
with its teardown (ADR 0078).

Only the terminal service holds the console's read right: nothing else can read
what is typed (ADR 0082). PID 1 also creates the terminal session channel, whose
server end goes to the terminal service as `session` and client end to the shell
as `tty`.

PID 1 also creates channels at run time with `ChannelCreate`: the block
service's `control` endpoint, over which a restarted driver's channel is
delivered; the config service's `control` endpoint; one configuration session
each for the shell (whole namespace, read and write) and the log service
(`system.log.`, read only); and one VFS session each for the log service, the
config service, its own reading of the command index, and every command whose
index entry asks for one (ADR 0073, 0076, 0078, 0080). It keeps only the
client side of each control endpoint.

No child receives Transfer. After boot, PID 1 still holds slots 1, 2, 5, 7, 9,
11, 12, 13, 18, 19, and 20, the client sides of the block and config control
endpoints, and a Wait + Write process handle for each running job stage; the
Write right is what `ProcessKill` requires (ADR 0067).

### Utilities

For each pipeline stage PID 1 grants:

| Role | Object | Rights |
| --- | --- | --- |
| `in` | stream reader: the pipeline's upstream stage, or a stream of its own that the shell feeds with typed lines (foreground) or that is at end of input (background) | R Wait |
| `stdout` | stream writer, or console device | stream: W Wait; console: W |
| `stderr` | stream writer, or console device | stream: W Wait; console: W |
| `vfs` | new private VFS session channel, only when the command's entry in `/system/commands` asks for one | R W Wait |

The VFS session is a fresh channel from `ChannelCreate`. PID 1 sends the
server end to VFS as an IPC attachment on the control channel and consumes the
client end into the child manifest (docs/userspace/vfs.md). Which commands
get one is decided by the manifest each command declares in its source, which
the build turns into the command index in the immutable system image
(ADR 0080). A command without an index entry gets the standard streams only.

## Device authority

The kernel discovers the legacy virtio-blk function and mints three separately
typed objects held first by PID 1 (ADR 0034, 0035):

- **Ports**: a device object encoding the discovered I/O BAR base and size.
  `PortRead`/`PortWrite` require Read/Write, widths 1, 2, or 4, and
  `offset + width <= size`. There is no other port-I/O path from ring 3.
- **IRQ**: an IRQ object for line 11 only. `IRQWait` requires Wait. The kernel
  masks the level-triggered route in the handler and rearms it on the next
  wait.
- **DMA**: a DMA-domain device object. `DMAAllocate` requires Write and maps
  four zeroed contiguous pages RW+NX at one fixed user address, returning their
  physical address to the caller only.

When a process holding DMA authority exits, teardown resets the device and
masks IRQ 11 before its owned pages are reclaimed. Without an IOMMU these
checks bound what the driver can do through the CPU, not what the device can
do on its behalf; see [threat-model.md](threat-model.md).

Four further device objects are not tied to hardware discovery:

- **Console**: COM1. Write authorizes `ConsoleWrite` and `LogWrite`; Read
  authorizes `ConsoleRead`, `ConsoleInterrupt`, and waiting on console input
  with `WaitAny` (ADR 0059, 0075). Only the terminal service is given Read
  (ADR 0082).
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
| `StartupRead`, `SystemInfo`, `StreamCreate`, `ProcessInfo` | caller must be a registered process |
| `HandleInfo` | caller must be a registered process; describes only its own table |
| `ChannelCreate` | caller must be PID 1 |
| `DebugWrite` | caller must be PID 1 or bootstrap process A (root comparison) |

`StreamCreate` is open because the new endpoints grant nothing beyond
themselves (ADR 0040); the per-process stream quota bounds how many one
process can hold (ADR 0072). `ProcessInfo` names no kernel object and reveals
only process names, states, and accounting. The full list is in
[attack-surface.md](attack-surface.md).

## Tests

`make test-security` runs host tests for wrong types, missing rights, stale
and forged IDs, unauthorized transfer, rights escalation, capacity exhaustion,
manifest rollback, stream direction, and the shared-memory and image mapping
checks (`internal/capability`, `kernel/handle`, `kernel/ipc`, `kernel/stream`,
`kernel/shmem`, `kernel/loader`, `userspace/init`, and others). Quota charging
and release are tested in `kernel/process`.
