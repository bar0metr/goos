# ADR 0035: Userspace virtio-blk data path

## Status

Accepted.

## Context

PCI discovery and capability transfer established the driver boundary but did
not move block requests through hardware. Q35 routes PCI INTx through the
IOAPIC, so the legacy PIC cannot provide a correct blocking completion path.
DMA reclamation is unsafe unless the device is quiesced first.

## Decision

The isolated `virtio-blk` process owns one legacy virtqueue in a fixed,
four-page contiguous DMA window. It negotiates only flush support, uses three
descriptors per request, and implements sector-aligned reads plus bounded
read-modify-write for unaligned public requests. Its public-facing peer is the
hardware-independent block service over a private channel.

The kernel maps the LAPIC and IOAPIC into two supervisor-only virtual pages and
routes Q35 GSI 11 to vector 43. `IRQWait` requires a wait-only IRQ capability.
The interrupt handler masks the level-triggered route and records at most one
pending event. Reading the device ISR acknowledges the source; the next wait
rearms the route on demand, preventing stale completions from satisfying later
requests or flooding the CPU while idle.

If a process holding the DMA authority exits, handle teardown resets the device
and masks and clears the IRQ route before its owned address space is destroyed.

## Consequences

The block service now performs persistent read, write, and flush operations on
the QEMU raw disk without direct hardware authority. The implementation is
intentionally single-device and single-request. Dynamic ACPI interrupt routing,
parallel queues, indirect descriptors, large transfers, driver restart, and a
persistent filesystem remain future work.
