# Architecture overview

GoOS follows a pragmatic microkernel/hybrid design. The privileged kernel will
own CPU state, address spaces, scheduling, interrupts, timers, IPC primitives,
handles, capability enforcement, and a dedicated Linux x86_64 ABI
translation subsystem. Linux applications enter through a separate SYSCALL
gate; their supported syscalls are translated in the kernel into native
mechanisms and, where needed, capability-scoped requests to userspace services.
The compatibility subsystem is separate from the native ABI, not a separate
ring-3 service (ADR 0134). Filesystems, TTY, storage, networking,
configuration, logging policy, and complex drivers belong in isolated
userspace services.

Authority is explicit. Kernel objects are named by process-local opaque handles
and operations require capabilities carrying a rights mask. Derived
capabilities may remove rights but never add them. IPC protocols will carry an
explicit protocol identifier and version; Go structure layout is not an ABI.

The current kernel leaves UEFI boot services, owns its physical allocator and
page tables, installs descriptor and interrupt tables, receives timer IRQs, and
preempts independent kernel tasks. It constructs distinct process address
spaces, preemptively schedules multiple ring 3 tasks with process-specific CR3
roots and guarded kernel-entry stacks, and dispatches bounded native calls with
validated user-memory copying. A versioned native loader validates and maps
executable, data, and stack segments. A userspace CPU fault terminates only its
task. Bounded typed IPC supports versioned request/reply, notifications,
capability-derived handle passing, peer-death reporting, and shared pages for
bulk data. Restartable IPC waits, the first service protocol cores, bounded init
supervision policy, UEFI initramfs discovery, ring-3 PID 1 launch, and bounded
process-death delivery are implemented. PID 1 receives lifecycle events over a
capability-protected channel and controls validated service restarts and native
application launches. The supervised VFS and block-service processes use
versioned capability-bounded IPC. The block service reaches a persistent QEMU
disk through an isolated userspace virtio-blk driver with bounded port-I/O,
DMA, and IRQ authority. A bounded FAT32 filesystem service, a process of its
own that the VFS mounts at `/persist`, reaches that block service through a
dedicated capability-scoped endpoint (ADR 0096). Every process after PID 1 is
launched from an extent of the read-only boot archive that PID 1 names under
an image capability with Execute, or, for every other program, the installed
system on the disk included, from a file-backed executable source tied to a
stable VFS open object; the kernel resolves no application path (ADR 0093,
ADR 0121, ADR 0175).
A userspace network service runs Ethernet, ARP, IPv4, ICMP echo and a DHCP
client over a second isolated driver, for one legacy virtio-net function;
the kernel's part is discovery, a dedicated interrupt route, a wait on that
line, and a DMA domain per device ([networking](networking.md), ADR 0190,
ADR 0191). The same service owns a loopback interface, and a separate
resolver service, `resolved`, answers host names from the hosts table, its
cache and DNS servers it reaches only through the network service
(ADR 0193, ADR 0194).
Native composition also has bounded unidirectional byte-stream objects with
separate read and write capabilities, backpressure, and close-driven EOF. These
objects are the non-POSIX foundation for standard-stream redirection and carry
concurrent shell pipelines of up to four processes, composed with bounded
sequential execution, bounded stdout truncate/append redirection, and bounded
file-backed stdin production.
The kernel exposes independent typed `MONOTONIC` and UTC `REALTIME` clocks;
the latter is initialized once from UEFI and then advanced only from the
existing monotonic TSC source (ADR 0113).

Native userspace allocates from a reclaiming per-process heap with a
conservative stop-the-world collector (ADR 0201). A process may run sibling
native threads in parallel (ADR 0203), and selected images use a dedicated
TinyGo target that schedules M:N goroutines over those threads, integrating G
and M stacks into the collector and parking supported waits through the
runtime (ADR 0204). The VFS uses the goroutine runtime for its bounded
concurrent read pool by default; the serial VFS remains a tested fallback.

## Further reading

- [boot sequence](boot.md);
- [filesystem stack](filesystem.md);
- [networking](networking.md);
- [timekeeping](timekeeping.md);
- [Linux compatibility](linux-compat.md);
- [threat model](../security/threat-model.md);
- [capabilities: security view](../security/capabilities.md);
- [memory safety](../security/memory-safety.md);
- [native-call attack surface](../security/attack-surface.md);
- [debugging](../development/debugging.md);
- [coding style](../development/coding-style.md);
- [adding a service](../development/adding-service.md).

## Documentation map

`docs/architecture/` describes the system as a whole: boot, processes,
scheduling, memory, IPC, capabilities, the filesystem stack and networking.
`docs/kernel/` holds kernel mechanisms that are not system-wide design:
[interrupts](../kernel/interrupts.md), the [native image format](../kernel/native-images.md)
and [byte streams](../kernel/streams.md). `docs/userspace/` holds the services
and programs above the kernel: [init](../userspace/init.md),
[terminal](../userspace/tty.md), [shell](../userspace/shell.md),
[VFS](../userspace/vfs.md), [RAMFS](../userspace/ramfs.md),
[storage](../userspace/storage.md), [configuration](../userspace/configuration.md)
and [logging](../userspace/logging.md). Security, development practice and
decisions live in `docs/security/`, `docs/development/` and `docs/adr/`.
