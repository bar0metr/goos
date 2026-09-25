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
FAT32 filesystem service translate file semantics into block operations
without acquiring device authority. That service is a process of its own
between the VFS and the block service (ADR 0096); it restarts on failure, and
PID 1 hands the block service and the VFS fresh endpoints to the new instance.

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

## Mounting an untrusted volume

Everything the FAT32 service computes comes from the boot sector: where the FATs
begin, where a cluster is, how far a chain may run. The image is on a device the
system does not control, so the geometry is checked against that device before an
offset is derived from it (ADR 0077, ADR 0102):

- the device reports its capacity, which must be a whole number of sectors;
- sectors are 512 bytes, sectors per cluster a power of two up to 128, and there
  are one or two FATs; other sector sizes are refused as unsupported;
- the boot sector must describe FAT32 (no fixed root directory, no 16-bit FAT
  size), and a FAT named active by the extended flags must exist;
- the declared volume must fit the device and its metadata must fit the volume;
- a FAT too small for the data region limits the volume to the clusters it can
  describe, as Linux does, and no volume may number more clusters than FAT32
  addresses;
- the root cluster must be inside the data area; an FSInfo sector is used only
  for its next-free hint, and only when both signatures and the value check out.

A refused volume is not fatal. The filesystem service reports it, prints
`[WARN] no persistent storage`, and serves the rest of the namespace: the
volatile root and the read-only system image are already mounted, so the machine
comes up to a prompt with `/persist`, `/home` and `/state` simply absent. That
prompt is where the volume gets looked at. The log service keeps draining the
kernel ring and retries its file on each pass; the configuration service serves
its declared defaults.

## The FAT32 volume

The volume is mounted at `/persist`, and its `home` and `state` directories are
mounted again at `/home` and `/state` so that user data and mutable system state
are named apart from the immutable system image (ADR 0071). The filesystem
service creates those directories on a volume that lacks them. The three mounts
share one backend instance and one sector cache, so `/persist/home` and `/home`
are one directory rather than two views; the namespace refuses to remove a
directory another mount points at, under either name.

Files are limited only by FAT32 itself: 4 GiB − 1 bytes, the free space, and the
caller's memory. Directories are cluster chains of any valid length, up to the
65536 entries FAT allows, and grow one zeroed cluster at a time; nesting depth
is bounded only by the VFS path length. Names are ASCII of at most 63 bytes
(ADR 0066). A lower-case name that fits the 8.3 form stays a single directory
entry, carrying the Windows NT lower-case flags; anything longer or mixed case
gets a VFAT long-name chain and a generated alias — `~1` to `~9`, then a hashed
form — so any number of similar names fit (ADR 0102).

Truncation shrinks and grows. Shrinking records the new size, ends the chain
after the last cluster still needed, and frees the rest; growing, like a write
past the end, writes zeros over the new range first, so no stale disk bytes are
ever read back. A write claims all the clusters it needs before writing, so a
full volume refuses it whole with "no space left on device". Rename moves
entries between directories, repointing a moved directory's `..`; a directory
cannot be moved into its own subtree. A directory can be removed only when it
holds nothing but `.` and `..`. Each FAT change is written to every FAT copy in
use.

A node ID packs the directory key (0 for the root, otherwise the directory's
first cluster) above bit 17 and the entry index plus two below it, so root
entries keep their historical IDs. A subdirectory key must lead to a directory
whose `.` entry names it, so an ID that outlived its directory is not found.
Chains that loop, reach a free or bad cluster, leave the data area, or end
before the file does are reported as a corrupt filesystem; every walk is bounded
by the volume's cluster count.

There is no journal. Changes are ordered so that an interruption leaves at worst
lost clusters, a chain longer than its file, or an entry under two names after
a move — never a chain shared by two files or an entry naming freed clusters.
Timestamps are not maintained. The boot gate copies `/system/motd` through VFS
and verifies the result in the host-visible disk image, and a second VM boot
reads it back, proving persistence across a complete guest restart.

A journal, timestamps, dynamic interrupt routing, multiple devices, and block
payloads larger than the inline IPC limit remain future work.

## Resident sectors and bounded failure

The FAT32 service keeps a write-through window of sixteen resident metadata
sectors, replaced least recently used. Directory entries and FAT links are
packed many to a sector, so a metadata scan costs one sector fetch per sector,
not one device round trip per entry. Writes reach the device immediately and
update any resident copy, and a failed device write drops that copy rather than
leaving superseded bytes behind. File data is not kept there: it moves between
the device and the request directly, one device request per run of consecutive
clusters, and the VFS caches it (ADR 0098). The last position reached in each of
eight cluster chains is remembered, so sequential access follows one FAT link
per cluster.

Every storage wait is bounded. One block exchange and one VFS call expire after
a few seconds of timer ticks and report an I/O error. Services answer requests
they cannot parse with an invalid-argument reply whenever the caller can be
identified, because a silent drop would strand a caller that is waiting for
exactly that correlation. Both rules exist so that a fault anywhere in the
storage path degrades into a reported error instead of an apparent freeze.

## A driver that fails while running

The driver is the component that talks to hardware and the one most likely to
fail. It restarts (ADR 0078): PID 1 creates a fresh channel to the block
service, tells that service over a control endpoint only PID 1 holds the client
of, and launches a replacement with the same ports, interrupt line and DMA
domain. The dead process's teardown has already reset the device and masked its
interrupt, so the replacement starts against quiesced hardware.

The block service installs the endpoint without probing it, because PID 1 hands
it over before the replacement exists; the next real request is what discovers
whether the driver works. While it knows the driver is gone it refuses requests
at once rather than waiting out a deadline against a process that is not there.

The block service restarts on failure too (ADR 0101). A restarted instance is
launched with a new control channel and public endpoint and without a driver or
filesystem endpoint, and learns the device's geometry from the first request
after a driver is handed over. The driver exits when its service channel dies,
and the FAT32 service when its block endpoint closes; both restart after the
block service, the FAT32 service after the driver, and each hands the new block
service its endpoint over the control channel.
