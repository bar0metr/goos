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
  or process handle. Stream endpoints additionally encode their direction.
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
| Read | `0x01` | receive IPC, read stream, read console, read ports |
| Write | `0x02` | send IPC, write stream, write console, write ports, allocate DMA |
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
launch. Exclusive endpoints (service channel ends, device authority, pipeline
streams) are consumed, so PID 1 does not keep a second copy. Startup role
names locate handles; they do not grant authority.

## Holders at boot

The kernel installs PID 1's initial table in a fixed order before PID 1 runs
(`kernel/main.go`). PID 1 then derives rights into children through the
manifests in `userspace/cmd/init/main.go`.

| PID 1 slot | Object | PID 1 rights | Derived to | Child rights | Consumed |
| --- | --- | --- | --- | --- | --- |
| 1 | lifecycle channel | R W Wait | PID 1 only | - | - |
| 2 | console device | R W T | fault-probe, VFS, block, virtio-blk | W | no |
| | | | shell | R W | no |
| 3 | VFS session, client end | R W Wait T | shell `vfs` | R W Wait | yes |
| 4 | VFS session, server end | R W Wait T | VFS `session` | R W Wait | yes |
| 5 | VFS control, client end | R W Wait | PID 1 only | - | - |
| 6 | VFS control, server end | R W Wait T | VFS `control` | R W Wait | yes |
| 7 | exec, server end | R W Wait | PID 1 only | - | - |
| 8 | exec, client end | R W Wait T | shell `exec` | R W Wait | yes |
| 9 | block, client end | R W Wait | PID 1 probe only | - | - |
| 10 | block, server end | R W Wait T | block `block` | R W Wait | yes |
| 11 | virtio port range | R W T | virtio-blk `ports` | R W | yes |
| 12 | IRQ 11 | Wait T | virtio-blk `irq` | Wait | yes |
| 13 | DMA domain 1 | W T | virtio-blk `dma` | W | yes |
| 14 | driver, client end | R W Wait T | block `driver` | R W Wait | yes |
| 15 | driver, server end | R W Wait T | virtio-blk `service` | R W Wait | yes |
| 16 | FS-block, client end | R W Wait T | VFS `block` | R W Wait | yes |
| 17 | FS-block, server end | R W Wait T | block `filesystem` | R W Wait | yes |

No child receives Transfer. After boot, PID 1 still holds slots 1, 2, 5, 7, 9,
and one Wait-only process handle per running application.

### Utilities

For each pipeline stage PID 1 grants:

| Role | Object | Rights |
| --- | --- | --- |
| `in` | stream reader, or console device | stream: R Wait; console: R |
| `stdout` | stream writer, or console device | stream: W Wait; console: W |
| `stderr` | stream writer, or console device | stream: W Wait; console: W |
| `vfs` | new private VFS session channel (file commands only) | R W Wait |

The VFS session is a fresh channel from `ChannelCreate`. PID 1 sends the
server end to VFS as an IPC attachment on the control channel and consumes the
client end into the child manifest (docs/architecture/vfs.md).

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

## Operations without a capability

Some calls authorize by caller identity or need nothing:

| Operation | Authorization |
| --- | --- |
| `Exit`, `ClockRead`, `ClockReadNanos` | none |
| `StartupRead`, `SystemInfo`, `StreamCreate` | caller must be a registered process |
| `ChannelCreate` | caller must be PID 1 |
| `DebugWrite` | caller must be PID 1 or bootstrap process A (root comparison) |

`StreamCreate` is open because the new endpoints grant nothing beyond
themselves (ADR 0040). The full list is in
[attack-surface.md](attack-surface.md).

## Tests

`make test-security` runs host tests for wrong types, missing rights, stale
and forged IDs, unauthorized transfer, rights escalation, capacity exhaustion,
manifest rollback, and stream direction (`internal/capability`,
`kernel/handle`, `kernel/ipc`, `kernel/stream`, `userspace/init`, and others).
