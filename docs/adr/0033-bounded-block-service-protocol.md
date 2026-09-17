# ADR 0033: Bounded block service protocol

## Status

Accepted

## Context

Persistent storage requires a stable boundary between filesystem policy and a
hardware-specific driver. No PCI, MMIO, IRQ, or DMA authority exists yet, so
placing virtio logic in VFS or granting ambient port access would establish the
wrong architecture before the driver boundary is available.

## Decision

Define `GBLK001` version 1 as a bounded IPC protocol for device information,
byte-range read, byte-range write, and flush operations. Inline data is limited
to 232 bytes by the existing IPC payload. All requests use correlations and
strict reserved-field validation.

Implement a supervised ring-3 block-service process and core behind a backend interface with capacity,
logical block size, read, write, and flush methods. The core validates overflow
and device bounds before invoking the backend. Backend status preserves
read-only and I/O failures.

## Consequences

Filesystem code can target one device-independent contract, and the future
virtio-blk process can remain isolated behind a narrow backend adapter. The
initial private memory backend is explicitly volatile and exists only for live
IPC validation; it is not a storage implementation. Large
and zero-copy transfers will require shared-memory descriptors in a later
protocol version. This decision does not mark virtio-blk, a persistent
filesystem, or reboot persistence complete.
