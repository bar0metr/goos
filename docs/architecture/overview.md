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

The current boot vertical slice is intentionally small. It enters through the
TinyGo UEFI target, initializes COM1, writes machine-readable boot markers, and
halts. It does not claim process isolation, scheduling, or userspace services.
