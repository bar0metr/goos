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

The current iteration supplies the protocol and a supervised ring-3 block
service. Its private volatile backend exists only to exercise real IPC reads,
writes across a logical-block boundary, device information, and flush in QEMU.
It does not claim persistent storage: PCI discovery, capability-scoped
MMIO/IRQ/DMA, the userspace virtio-blk driver process, FAT32, VFS mounting, and
reboot persistence remain required.
