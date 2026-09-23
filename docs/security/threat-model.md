# Threat model

Scope: GoOS 0.1-dev on one x86_64 CPU under QEMU q35 with OVMF, booted from
the project ISO. This document states what is protected, from whom, and which
mechanisms enforce it. Mechanism details live in the linked documents.

## Assets

| Asset | Where it lives | Why it matters |
| --- | --- | --- |
| Kernel integrity | kernel image, page tables, GDT/IDT/TSS, kernel stacks | Every other guarantee depends on it |
| Physical memory | PMM bitmap, owned user pages, shared image frames, DMA window | Aliasing a page breaks isolation |
| Handle tables and registries | per-process `handle.Table`, channel, stream, process registries | They *are* the authority model |
| Resource accounts | per-process quota counters (ADR 0072) | Stop one process from starving the rest |
| PID 1 authority | PID 1 handle table | Root of every capability granted after boot |
| System image | initramfs USTAR on the ESP, held in firmware-reserved pages and mounted read-only at `/system` | Defines every program the kernel will run and the command index that decides their grants |
| Persistent data | FAT32 image on the virtio-blk disk (`/persist`, with `/home` and `/state` on the same volume) | Survives reboot; reachable only through VFS |
| System log and configuration | kernel log ring, `/state/system.log`, `/state/config` | Diagnostics and the settings components act on |
| Console | COM1 through the console device capability | Operator I/O, warning echo, and read access to the log ring |
| CPU time | scheduler slots, wait sets | Liveness of every service |

## Trust boundaries

```text
host (QEMU, OVMF, ISO, disk image file)
 └─ kernel (ring 0)                        trusted computing base
     ├─ PID 1 (ring 3)                     holds all initial authority
     │   ├─ VFS service (+ FAT32 backend)  holds VFS sessions, a block client endpoint, the system image mapping
     │   ├─ block service                  holds block endpoints, driver client endpoint
     │   ├─ virtio-blk driver              holds port, IRQ, DMA authority
     │   ├─ config service                 holds the configuration store and a VFS session
     │   ├─ log service                    holds the log (read), console write, and a VFS session
     │   ├─ terminal service               the only holder of console read; a session with the shell
     │   └─ shell                          holds console write, the terminal session, the log (read), a VFS session, exec endpoint and page, a config session
     └─ utilities (ring 3, per command)    hold only stdin/stdout/stderr and optionally one VFS session
```

| Boundary | Crossing | Enforcement |
| --- | --- | --- |
| user → kernel | `int 0x80` native calls, CPU faults | bounded `kcall` contract, handle lookup with type and rights, `CopyIn`/`CopyOut` ([attack-surface.md](attack-surface.md)) |
| process ↔ process | typed IPC, streams, handle transfer, shared pages | per-process address spaces, generation-safe handles, non-escalating transfer ([capabilities.md](capabilities.md)); shared image pages are never writable (ADR 0081) |
| PID 1 → kernel launch | lifecycle channel | only PID 1 holds the lifecycle endpoint; the kernel validates every grant against PID 1's table (ADR 0024) and resolves the executable only inside the boot archive |
| shell → PID 1 | exec channel and exec page | PID 1 copies the request out of the shared page before validating it, accepts only `/system/bin/<name>` paths, and grants a VFS session only when the command's entry in the command index asks for one (ADR 0030, 0064, 0080) |
| VFS → storage | `GBLK001` IPC | block service validates ranges and overflow; VFS never holds device authority (ADR 0033, 0036) |
| VFS → system image | `ImageMap` | read-only, non-executable, unowned mapping of the archive; the capability is held only by VFS (ADR 0070) |
| client → config service | `GCF1` session | scope and rights are fixed by PID 1 when it creates the session (ADR 0074, 0076) |
| driver → hardware | port, IRQ, DMA calls | BAR-bounded ports, IRQ 11 only, fixed DMA window (ADR 0034, 0035) |
| host → guest | firmware, ISO, disk image | trusted, except disk *contents*, which are treated as untrusted input |

The kernel and the host (QEMU, OVMF, the ISO build) are trusted. Nothing in the
boot path verifies the kernel or the system image; see
[boot-chain.md](boot-chain.md) for the planned verified boot chain. PID 1 is
trusted to apply policy but is still subject to kernel checks. Services, the
driver, and utilities are untrusted relative to each other.

## Attacker models

### Malicious utility

A program launched from the shell, for example a compromised command image in
`/system/bin` or a command fed hostile arguments.

- Holds only `in`, `stdout`, `stderr` and, when its manifest in the command
  index asks for one, a private VFS session channel (ADR 0080). `in` is always
  a stream; `stdout` and `stderr` are streams or the console, write only. It
  cannot read what is typed except the lines the shell writes to its input
  (ADR 0082). No Transfer right on any of them.
- Cannot create channels (`ChannelCreate` is restricted to PID 1), cannot
  launch or stop processes, cannot reach block, driver, lifecycle, or
  configuration endpoints.
- Its VFS session is a dedicated channel; it cannot read replies meant for
  another process (ADR 0030, docs/userspace/vfs.md).
- A fault terminates only its task; exit closes its handles, so peers see
  `PeerDied`/EOF instead of hanging (ADR 0050).
- Residual risks: it can create up to four streams, its per-process quota,
  which leaves the rest of the registry to everyone else (ADR 0072); it can
  read the high-resolution clock (ADR 0054) and the process list with each
  process's CPU and resource use (`ProcessInfo`); it can run without yielding,
  because CPU is accounted but not limited; and it can write anything the VFS
  namespace permits, including `/persist`, `/home`, and `/state`.

### Compromised service

VFS, block service, config service, log service, or shell executing
attacker-controlled code.

- Each service holds only the endpoints listed in its PID 1 manifest, with
  Transfer removed, so it cannot forward authority it received.
- VFS compromise exposes every file in its namespace and, through its block
  client endpoint, the whole disk. It gains no device, port, IRQ, or DMA
  authority. It also serves `/system/commands`, which PID 1 reads to decide
  which commands get a VFS session; a lying VFS can only change who receives a
  session it serves itself. It cannot change what executes: the kernel resolves
  launches in the archive directly, and the archive mapping VFS holds is
  read-only.
- Block service compromise exposes the disk through the driver channel. It
  gains no hardware authority.
- Config service compromise exposes and can rewrite every setting, including
  `/state/config`, through its own VFS session.
- Log service compromise can read every retained log record and write records
  and warnings to the console; logging is bounded by record size, not rate
  (ADR 0059, 0073).
- Shell compromise can ask PID 1 to run any `/system/bin` image with the grants
  its index entry names, stop its own jobs, read and write the console, read
  and write every configuration setting through its full-scope session, and ask
  PID 1 to sync and reset the machine (`reboot`, ADR 0061). It cannot reset the
  machine itself: only PID 1 holds the system-control capability.
- The storage driver, the config service, and the log service restart on
  failure. VFS, the block service, and the shell use `RestartNever` and stay
  down after a crash; clients observe `PeerDied`, and bounded storage deadlines
  report I/O errors instead of freezing (ADR 0048, 0078).

### Compromised driver

The `virtio-blk` process programs a bus-mastering device. There is no IOMMU:
the device can DMA to any physical address the driver writes into the
virtqueue or into the legacy queue-address register inside its port range. A
malicious driver can therefore corrupt kernel memory, the boot archive, and
the shared read-only image pages every process running an image maps
(ADR 0081); sharing does not widen what it can reach. Capability scoping
(ADR 0034) contains accidental driver faults and removes ambient port access;
it does not contain a malicious driver. The driver is part of the effective
TCB until an IOMMU is used. PID 1 keeps the driver's port, IRQ, and DMA
authority so that it can grant it to a restarted driver (ADR 0078).

### Malformed storage image

The raw disk image is host-provided and persists across boots.

- The FAT32 backend checks the declared geometry against the device capacity
  before deriving any offset, walks FAT chains with bounded loops, and supports
  only a fixed, narrow geometry (ADR 0036, 0077).
- A volume that fails the checks is refused, not fatal: the system boots with
  `/persist`, `/home`, and `/state` absent, and the log and config services
  continue without their files (ADR 0077).
- Parsing runs inside the VFS process, never in the kernel. A parser defect is
  contained to VFS and its authority above.
- The block service rejects out-of-range and overflowing requests before they
  reach the driver.
- Crash consistency is not addressed: an interrupted write can leave a volume
  that mounts and reads inconsistently.

### Malformed IPC

- The kernel copies at most 320 bytes of wire data, validates the header,
  payload size, and attached-handle count before queueing, and checks Transfer
  authority on every attached handle (docs/architecture/ipc.md).
- Protocol decoders (`GVF1`, `GRF1`, `GBLK001`, lifecycle, exec and its
  control protocol, `GCF1` and its control protocol) check protocol ID,
  version, sizes, and require reserved fields to be zero.
- Services answer unparsable but attributable requests with an
  invalid-argument reply instead of dropping them (ADR 0048). A request whose
  peer has died is dropped without stopping the service (ADR 0078).
- Requests that travel through a shared page (launch staging, exec page) are
  copied into private memory once, before validation, so the sender cannot
  change them afterwards (ADR 0062, 0064).

### Malicious native image

Images come only from the initramfs built into the ISO. A tampered image is
still loaded through the bounded native loader: fixed header, at most eight
page-aligned segments and 256 pages, user range only, no W+X, no overlaps, entry
in an executable segment, stack in a writable NX segment with an unmapped
guard page below it (docs/kernel/native-images.md). A valid but hostile image runs with
exactly the manifest PID 1 grants it. Its read-only segments are shared with
every other process running the same image, but never writable by any of them
(ADR 0081). Images are not signed or measured, so whoever controls the ISO
controls the system ([boot-chain.md](boot-chain.md)).

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
| runaway command | Ctrl-C or `kill` makes PID 1 stop the job through `ProcessKill`, which runs the ordinary teardown (ADR 0067) |
| crash loop | bounded restart count and backoff in the PID 1 supervisor (ADR 0019) |
| stalled storage peer | bounded storage deadlines (ADR 0048) |
| storage driver crash | PID 1 restarts the driver on a fresh channel and hands the block service the new endpoint (ADR 0078) |
| corrupt persistent volume | mount refused, system runs without persistent storage (ADR 0077) |
| driver exit with DMA armed | teardown resets the device and masks IRQ 11 before freeing pages (ADR 0035) |

## Out of scope for 0.1

- SMP. The single exclusion domain (interrupts disabled, ADR 0053) is correct
  only on one CPU.
- IOMMU. DMA is not confined (see compromised driver).
- ASLR. Kernel and user layouts are fixed (kernel image at `0x400000`, user
  code at `0x40002000`). What would have to change is described in
  [memory.md](../architecture/memory.md#aslr-readiness).
- Signed or measured images, Secure Boot, and initramfs integrity. The planned
  chain is described in [boot-chain.md](boot-chain.md).
- CPU limits. CPU time is accounted per process but never refused (ADR 0072).
- Side channels, including timing through `ClockReadNanos` and cache timing
  between processes that share an image's read-only pages (ADR 0081).
- Physical attacks, malicious firmware, and a malicious hypervisor.
- Confidentiality of `/persist` against anyone with host access to the image.
- Users, logins, and multi-user separation. There is no UID model.

## Known weaknesses

- PID 1 is a single point of authority. Compromising it grants every
  capability it still holds, including the console with Transfer, the
  lifecycle channel, the VFS, block, and configuration control endpoints, the
  system-control capability, the driver's port, IRQ, and DMA authority, and a
  stop right over every running job.
- `DebugWrite` is authorized by comparing address-space roots against
  bootstrap processes, not by a capability. In a normal build only PID 1
  passes the check.
- The `goos_diag` build prints other processes' handle tables and physical
  addresses. It is for diagnosis only (ADR 0052).
