# ADR 0134: Kernel-resident Linux translation

## Status

Accepted.

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

Linux syscall numbers use their own typed namespace. The entry decoder copies
all scalar registers before operation-specific interpretation. Unsupported-call
diagnostics are a bounded ring plus a saturating counter and do not emit one log
record per call.

An asynchronous translation has one bounded continuation per generation-safe
Task ID. It records only validated scalars and copied bounded data, marks each
external stage issued before the effect, and accepts only the matching
correlation on wake. The architecture removes a waiting task from the runnable
set and retries `SYSCALL` after wake. Kill, fault, and teardown cancel the
continuation. Peer death remains an exact correlated wake reason until the
handler converts it to an error and finishes the call.

## Consequences

The trusted kernel surface grows by a bounded ABI decoder and a fixed 64-slot
continuation table, not by a second privilege model. A later userspace split
remains possible if it can preserve the same authority and memory-safety
boundary.
