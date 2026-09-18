# Architecture overview

GoOS follows a pragmatic microkernel/hybrid design. The privileged kernel will
own CPU state, address spaces, scheduling, interrupts, timers, IPC primitives,
handles, and capability enforcement. Filesystems, TTY, storage, networking,
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
DMA, and IRQ authority. A bounded FAT32 backend mounted at `/persist` reaches
that block service through a dedicated capability-scoped endpoint.
Native composition also has bounded unidirectional byte-stream objects with
separate read and write capabilities, backpressure, and close-driven EOF. These
objects are the non-POSIX foundation for later standard-stream redirection and
carry concurrent shell pipelines of up to four processes, composed with bounded
sequential execution. File redirection remains later composition work.
