# Threat model

Scope: GoOS 0.1-dev on one x86_64 CPU under QEMU q35 with OVMF, booted from
the project ISO. This document states what is protected, from whom, and which
mechanisms enforce it. Mechanism details live in the linked documents.

## Assets

| Asset | Where it lives | Why it matters |
| --- | --- | --- |
| Kernel integrity | kernel image, page tables, GDT/IDT/TSS, kernel stacks | Every other guarantee depends on it |
| Physical memory | PMM bitmap, owned user pages, DMA window | Aliasing a page breaks isolation |
| Handle tables and registries | per-process `handle.Table`, channel, stream, process registries | They *are* the authority model |
| PID 1 authority | PID 1 handle table | Root of every capability granted after boot |
| Persistent data | FAT32 image on the virtio-blk disk (`/persist`) | Survives reboot; reachable only through VFS |
| Boot images | initramfs USTAR on the ESP | Defines every program the kernel will run |
| Console | COM1 through the console device capability | Operator I/O and the only log channel |
| CPU time | scheduler slots, wait sets | Liveness of every service |

## Trust boundaries

```text
host (QEMU, OVMF, ISO, disk image file)
 └─ kernel (ring 0)                        trusted computing base
     ├─ PID 1 (ring 3)                     holds all initial authority
     │   ├─ VFS service (+ FAT32 backend)  holds VFS sessions, a block client endpoint
     │   ├─ block service                  holds block endpoints, driver client endpoint
     │   ├─ virtio-blk driver              holds port, IRQ, DMA authority
     │   └─ shell                          holds console, a VFS session, exec client endpoint
     └─ utilities (ring 3, per command)    hold only stdin/stdout/stderr and optionally one VFS session
```

| Boundary | Crossing | Enforcement |
| --- | --- | --- |
| user → kernel | `int 0x80` native calls, CPU faults | bounded `kcall` contract, handle lookup with type and rights, `CopyIn`/`CopyOut` ([attack-surface.md](attack-surface.md)) |
| process ↔ process | typed IPC, streams, handle transfer | per-process address spaces, generation-safe handles, non-escalating transfer ([capabilities.md](capabilities.md)) |
| PID 1 → kernel launch | lifecycle channel | only PID 1 holds the lifecycle endpoint; the kernel validates every grant against PID 1's table (ADR 0024) |
| shell → PID 1 | exec channel | PID 1 accepts only `/system/bin/<name>` paths and builds a fixed minimal manifest (ADR 0030) |
| VFS → storage | `GBLK001` IPC | block service validates ranges and overflow; VFS never holds device authority (ADR 0033, 0036) |
| driver → hardware | port, IRQ, DMA calls | BAR-bounded ports, IRQ 11 only, fixed DMA window (ADR 0034, 0035) |
| host → guest | firmware, ISO, disk image | trusted, except disk *contents*, which are treated as untrusted input |

The kernel and the host (QEMU, OVMF, the ISO build) are trusted. PID 1 is
trusted to apply policy but is still subject to kernel checks. Services,
the driver, and utilities are untrusted relative to each other.

## Attacker models

### Malicious utility

A program launched from the shell, for example a compromised command image in
`/system/bin` or a command fed hostile arguments.

- Holds only `in`, `stdout`, `stderr` (stream endpoints or the console device
  with a single right) and, when its manifest in the command index asks for
  one, a private VFS session channel (ADR 0080).
  No Transfer right on any of them.
- Cannot create channels (`ChannelCreate` is restricted to PID 1), cannot
  launch processes, cannot reach block, driver, or lifecycle endpoints.
- Its VFS session is a dedicated channel; it cannot read replies meant for
  another process (ADR 0030, docs/architecture/vfs.md).
- A fault terminates only its task; exit closes its handles, so peers see
  `PeerDied`/EOF instead of hanging (ADR 0050).
- Residual risks: it can create streams until the global stream registry
  (16 objects) is full, it can read the high-resolution clock (ADR 0054),
  and it can write anything the VFS namespace permits, including `/persist`.

### Compromised service

VFS, block service, or shell executing attacker-controlled code.

- Each service holds only the endpoints listed in its PID 1 manifest, with
  Transfer removed, so it cannot forward authority it received.
- VFS compromise exposes every file in its namespace and, through its block
  client endpoint, the whole disk. It gains no device, port, IRQ, or DMA
  authority.
- Block service compromise exposes the disk through the driver channel. It
  gains no hardware authority.
- Shell compromise can ask PID 1 to run any `/system/bin` image, with the
  fixed utility manifest, and can read and write the console.
- Services launched with `RestartNever` stay down after a crash; clients
  observe `PeerDied` and bounded storage deadlines report I/O errors instead of
  freezing (ADR 0048).

### Compromised driver

The `virtio-blk` process programs a bus-mastering device. There is no IOMMU:
the device can DMA to any physical address the driver writes into the
virtqueue or into the legacy queue-address register inside its port range. A
malicious driver can therefore corrupt kernel memory. Capability scoping
(ADR 0034) contains accidental driver faults and removes ambient port access;
it does not contain a malicious driver. The driver is part of the effective
TCB until an IOMMU is used.

### Malformed storage image

The raw disk image is host-provided and persists across boots.

- The FAT32 backend validates boot parameters before use, walks FAT chains
  with bounded loops, and supports only a fixed, narrow geometry (ADR 0036).
- Parsing runs inside the VFS process, never in the kernel. A parser defect is
  contained to VFS and its authority above.
- The block service rejects out-of-range and overflowing requests before they
  reach the driver.

### Malformed IPC

- The kernel copies at most 320 bytes of wire data, validates the header,
  payload size, and attached-handle count before queueing, and checks Transfer
  authority on every attached handle (docs/architecture/ipc.md).
- Protocol decoders (`GVF1`, `GRF1`, `GBLK001`, lifecycle, exec) check
  protocol ID, version, sizes, and require reserved fields to be zero.
- Services answer unparsable but attributable requests with an
  invalid-argument reply instead of dropping them (ADR 0048).

### Malicious native image

Images come only from the initramfs built into the ISO. A tampered image is
still loaded through the bounded native loader: fixed header, at most eight
page-aligned segments and 128 pages, user range only, no W+X, no overlaps, entry
in an executable segment, stack in a writable NX segment
(docs/architecture/native-images.md). A valid but hostile image runs with
exactly the manifest PID 1 grants it. Images are not signed or measured, so
whoever controls the ISO controls the system.

## Containment summary

| Failure | Contained by |
| --- | --- |
| user CPU fault | per-task fault path; process teardown releases handles (ADR 0008, 0050) |
| wild user pointer | `CopyIn`/`CopyOut` page-walk validation; kernel never dereferences user VAs |
| forged or stale handle | generation + type + rights check in `handle.Table.Lookup` |
| authority escalation | `Derive` refuses added rights; IPC transfer strips Transfer |
| peer death | channel and stream close on teardown; waiters woken with `PeerDied` |
| resource exhaustion | fixed-size tables everywhere, plus per-process quotas on channels, streams, tasks and pages (ADR 0072); failure is a status, not a panic |
| one process starving the rest | each process holds a share of each table, not the table; a refused charge reaches no global structure |
| crash loop | bounded restart count and backoff in the PID 1 supervisor (ADR 0019) |
| stalled storage peer | bounded storage deadlines (ADR 0048) |
| driver exit with DMA armed | teardown resets the device and masks IRQ 11 before freeing pages (ADR 0035) |

## Out of scope for 0.1

- SMP. The single exclusion domain (interrupts disabled, ADR 0053) is correct
  only on one CPU.
- IOMMU. DMA is not confined (see compromised driver).
- ASLR. Kernel and user layouts are fixed (kernel image at `0x400000`, user
  code at `0x40002000`).
- Signed or measured images, Secure Boot, and initramfs integrity.
- Per-process resource quotas. Registries are bounded globally, not per
  process.
- Side channels, including timing through `ClockReadNanos`.
- Physical attacks, malicious firmware, and a malicious hypervisor.
- Confidentiality of `/persist` against anyone with host access to the image.
- Users, logins, and multi-user separation. There is no UID model.

## Known weaknesses

- PID 1 is a single point of authority. Compromising it grants every
  capability it still holds, including the console with Transfer, the
  lifecycle channel, and the VFS control endpoint.
- `DebugWrite` is authorized by comparing address-space roots against
  bootstrap processes, not by a capability.
- The `goos_diag` build prints other processes' handle tables and physical
  addresses. It is for diagnosis only (ADR 0052).
- User stacks have no guard page below them; see
  [memory-safety.md](memory-safety.md).
