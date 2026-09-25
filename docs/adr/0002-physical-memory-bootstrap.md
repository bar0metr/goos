# ADR 0002: Fixed bitmap physical allocator

Status: Accepted; the one-GiB limit is replaced by ADR 0103

## Context

The kernel needs an allocator immediately after leaving UEFI, before it can map
dynamic allocator metadata. The supported QEMU machine has 512 MiB of RAM.
Firmware memory types must not be collapsed into a single free range.

## Decision

Use a statically allocated one-bit-per-page bitmap covering the first 1 GiB.
Free only `EfiConventionalMemory`; retain boot-services memory as reclaimable
but reserved. Permanently reserve the first MiB. Normalize the raw UEFI map at
the boot boundary and expose no firmware descriptor values to the allocator.

## Alternatives

A free-list needs metadata stored in pages before page ownership is established.
A bitmap sized for the entire 64-bit physical address space is wasteful. Marking
boot-services pages free immediately risks reclaiming live TinyGo runtime data.

## Consequences

The allocator is deterministic, auditable, and sufficient for the current QEMU
memory size. RAM above 1 GiB is ignored rather than mismanaged. Sparse or staged
bitmap metadata is required before increasing the supported memory limit.
