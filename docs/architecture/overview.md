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
versioned capability-bounded IPC; the current block backend is deliberately
volatile while PCI, MMIO/IRQ/DMA capabilities, virtio-blk, and persistent
filesystem layers remain future work.
