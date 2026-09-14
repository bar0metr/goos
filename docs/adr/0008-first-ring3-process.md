# ADR 0008: First ring 3 native process

Status: Accepted

## Context

Distinct page tables do not prove CPU privilege isolation. Entering userspace
requires ring 3 descriptors, a kernel-owned stack for privilege transitions,
strict page permissions, and a controlled path back into the kernel.

## Decision

Add user code/data descriptors and a 64-bit TSS to the GDT. Give TSS.RSP0 a
dedicated four-page kernel stack with an unmapped guard page. Map a zeroed RX
user code page and a separate RW+NX stack page in one process root. Enter CPL 3
through `IRETQ`, issue a temporary `INT 0x80` Exit operation, validate the saved
user CS in the handler, then restore kernel CR3 and the boot context.

## Alternatives

Running user payloads at ring 0 would test neither privilege transitions nor
page-table user permissions. `SYSCALL/SYSRET` is faster but requires a defined
native ABI, MSR initialization, canonical-address checks, and an explicit
register-clobber contract. Reusing a kernel stack for RSP0 removes overflow
isolation between asynchronous kernel work and the boot task.

## Consequences

GoOS executes real instructions at CPL 3 with hardware-enforced RX and NX
mappings and returns through a kernel-owned RSP0 stack. The bootstrap bytecode
and vector `0x80` are deliberately temporary. A loader, stable kernel-call ABI,
copyin/copyout, process-aware scheduler state, and user-fault termination are
required before general native programs can run.
