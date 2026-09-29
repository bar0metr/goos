# ADR 0134: Kernel-resident Linux translation

## Status

Accepted design for GoOS 0.2.0.

## Context

Linux syscall translation needs validated access to the caller address space.
A separate service would require cross-process memory authority or pervasive
bounce-buffer protocols before the ABI boundary is proven.

## Decision

The first Linux compatibility adapter is a separate kernel subsystem. It may
decode Linux registers and structures and implement Linux syscall semantics,
but it has no ambient authority. Every operation must resolve the calling
Process and use only its address space, compatibility state, handles, quotas,
streams, time primitives, and VFS session.

The adapter must not call FAT32, RAMFS, block, virtio, ports, IRQ, DMA, console,
or framebuffer implementations as a compatibility shortcut. Unknown or
deliberately unsupported Linux syscalls return `-ENOSYS`; fake success is
forbidden. Linux wire structures remain independent of host Go structures.

## Consequences

The trusted kernel surface grows by a bounded ABI decoder, not by a second
privilege model. A later userspace split remains possible if it can preserve
the same authority and memory-safety boundary.
