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
PCI, virtio queues, FAT, paths, or VFS handles. A dedicated endpoint lets the
FAT32 backend in the VFS service translate file semantics into block operations
without acquiring device authority.

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
The driver treats the virtqueue used index as the authoritative completion
signal and IRQ delivery only as a wakeup. It rechecks the used ring after every
acknowledged IRQ and also accepts completion that raced ahead of the wait.
Freestanding protocol encoding and port reads reuse fixed storage so repeated
interactive filesystem operations do not accumulate heap allocations.

The block service now uses a private IPC channel to the driver instead of a
volatile backend. Its public `GBLK001` protocol remains hardware-independent.
Its main loop uses one readiness-only `WaitAny` operation for the public service
endpoint and the dedicated filesystem endpoint, then receives and serves every
bit reported ready. The older one-object wait APIs remain unchanged for the
driver exchange and other single-endpoint paths.
The QEMU gate verifies an unaligned write spanning two sectors, a readback,
flush, repeated relative-path FAT32 reads and writes, and the resulting bytes in
the persistent raw disk image. Driver teardown
resets the device and masks the IRQ route before owned DMA pages are reclaimed.

A bounded FAT32 backend is mounted at `/persist`. It is intentionally limited
to 8.3 ASCII names, one-sector clusters, and 64 KiB files. It supports the
regular file lifecycle needed by native utilities and preserves the raw image
across QEMU boots. The boot gate copies `/system/motd` through VFS and verifies
the result in the host-visible disk image. A second VM boot mounts the same
image and reads the file without issuing another copy, proving persistence
across a complete guest restart.

Subdirectories are supported (ADR 0060). `mkdir` allocates one zeroed cluster
with `.` and `..` entries, and `..` records cluster 0 when the parent is the
root, as the FAT specification requires. Every directory, the root included,
holds at most four clusters (64 entries, 62 usable in a subdirectory) and grows
one cluster at a time. Nesting depth is bounded only by the VFS path length. A
node ID packs the directory key (0 for the root, otherwise the directory's
first cluster) above bit 16 and the entry index plus two below it, so root
entries keep their historical IDs. Rename stays within one directory, and a
directory can be removed only when it holds nothing but `.` and `..`; its
cluster is freed. Directory chains that are too long, loop, reach a free
cluster, or leave the data area are reported as errors, as are file chains
that leave the data area.

Cross-directory rename, long-file-name creation, general FAT32 geometry, crash
consistency, dynamic interrupt routing, multiple devices, scatter-gather
payloads larger than the inline IPC limit, and recovery from a failed storage
driver remain required.

## Resident sectors and bounded failure

The FAT32 backend keeps a small write-through window of resident sectors.
Directory entries and FAT links are packed many to a sector, so a metadata scan
that once cost one device round trip per 32-byte entry now costs one sector
fetch. Writes reach the device immediately and update the resident copy, and a
failed device write drops that copy rather than leaving superseded bytes behind.
A full-sector write becomes resident without reading the sector back.

Every storage wait is bounded. One block exchange and one VFS call expire after
a few seconds of timer ticks and report an I/O error. Services answer requests
they cannot parse with an invalid-argument reply whenever the caller can be
identified, because a silent drop would strand a caller that is waiting for
exactly that correlation. Both rules exist so that a fault anywhere in the
storage path degrades into a reported error instead of an apparent freeze.
