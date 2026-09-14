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
preempts independent kernel tasks. It also constructs distinct process address
spaces and verifies through hardware CR3 switches that the same user virtual
address resolves to different physical pages. Ring 3 execution and userspace
services are not implemented yet.
