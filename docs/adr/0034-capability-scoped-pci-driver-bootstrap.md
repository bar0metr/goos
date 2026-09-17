# ADR 0034: Capability-scoped PCI driver bootstrap

## Status

Accepted. The deferred data-path work is completed by ADR 0035.

## Context

The block protocol and service were intentionally independent from hardware.
The next boundary requires a userspace driver without granting ambient x86 port
access or exposing arbitrary physical memory.

## Decision

The kernel discovers the legacy virtio-blk PCI function with configuration
mechanism 1. It encodes the discovered I/O BAR, IRQ line, and DMA domain as
separate capabilities held initially by PID 1. PID 1 derives bounded rights into
an isolated `virtio-blk` process through the existing launch manifest.

Port native calls validate handle type, read/write rights, access width, and the
offset against the discovered BAR range. The DMA native call requires a DMA
domain capability, allocates one zeroed contiguous page, maps it as an owned
RW+NX user page at an explicitly supplied free virtual address, and returns its
physical address only to that process. The IRQ capability is transferred with
wait-only authority but interrupt delivery is not part of this decision.

The first driver stage resets and probes the device and validates that all three
capabilities are present. It does not configure a virtqueue or serve block I/O.

## Consequences

Device drivers can no longer issue arbitrary port I/O merely because they run
in userspace. DMA memory is bounded and reclaimed with the process address
space. A future virtqueue implementation must add device quiescing before DMA
pages are reclaimed, IRQ wait delivery, queue-memory layout validation, and an
IPC connection to the block service. Until those conditions are met, the
volatile block backend remains the only block-service backend.
