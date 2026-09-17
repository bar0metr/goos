# Storage architecture

Persistent storage is split into capability-isolated layers:

```text
VFS -> filesystem service -> block service -> device driver -> device
```

The block service contract is `GBLK001` version 1. It exposes device
information, bounded byte-range reads and writes, and flush. Payload transfers
are limited to 232 bytes by the current inline IPC envelope. Every request is
correlated, reserved fields must be zero, integer ranges are checked without
overflow, and replies distinguish invalid arguments, out-of-range access,
read-only media, and backend I/O errors.

The service core depends on a narrow backend interface containing capacity,
logical block size, read, write, and flush operations. It does not know about
PCI, virtio queues, FAT, paths, or VFS handles. The filesystem service will be
the only layer translating file semantics into block operations.

The kernel discovers the QEMU legacy virtio-blk PCI function through PCI
configuration mechanism 1. PID 1 receives three independently typed authorities:
a bounded port-I/O range, the device IRQ line, and a DMA allocation domain. It
derives only the required rights into a separate ring-3 `virtio-blk` process.
Native calls reject out-of-range port offsets and unsupported widths. DMA
allocation is limited to one zeroed contiguous page, maps it only into the
caller's address space, and returns an explicit physical/virtual/size descriptor.

The driver resets and probes the legacy device, negotiates bounded features,
builds one legacy virtqueue in a four-page contiguous DMA region, and completes
read, read-modify-write, write, and flush requests. Q35 PCI INTx is routed
through a supervisor-only IOAPIC/LAPIC mapping. A wait-only IRQ capability is
required by the blocking native IRQ call; the route is masked in the interrupt
handler and acknowledged by reading the device ISR. It is rearmed on demand by
the next wait, preventing an idle level-triggered route from flooding the CPU.

The block service now uses a private IPC channel to the driver instead of a
volatile backend. Its public `GBLK001` protocol remains hardware-independent.
The QEMU gate verifies an unaligned write spanning two sectors, a readback,
flush, and the resulting bytes in the persistent raw disk image. Driver teardown
resets the device and masks the IRQ route before owned DMA pages are reclaimed.

FAT32, mounting a persistent filesystem into VFS, dynamic interrupt routing,
multiple devices, scatter-gather payloads larger than the inline IPC limit, and
recovery from a failed storage driver remain required.
