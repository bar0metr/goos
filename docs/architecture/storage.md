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

The driver resets and probes the legacy device, validates its feature register,
queue size, capacity, IRQ capability, and DMA mapping in QEMU. The block service
still uses its private volatile validation backend. IRQ delivery/wait semantics,
virtqueue ownership and teardown, request completion, connection of the block
service to the driver, FAT32, VFS mounting, and reboot persistence remain
required. The current driver bootstrap is not persistent storage.
