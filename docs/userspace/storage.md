# Storage architecture

Persistent storage is split into capability-isolated layers:

```text
VFS -> filesystem service -> block service -> device driver -> device
                             (partition table, block slice)
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

## Partitions and block slices

The filesystem service reaches one volume, not the disk (ADR 0115). When the
block service first describes the device it reads its partition structures with
`userspace/partition`:

- a GPT behind a protective MBR: both headers and entry arrays are checked —
  signature, revision, header and array CRC-32, the header's own LBA, and that
  the usable range, the array and the other header lie inside the device and
  apart; each used entry must lie in the usable range and overlap no other. The
  primary governs; a damaged primary falls back to the backup, and a backup
  that is invalid, differs or is not at the last block is reported. Nothing is
  rewritten;
- otherwise a FAT, exFAT or NTFS boot sector at LBA 0: the whole disk is the
  volume ("superfloppy");
- otherwise an MBR: up to four primary entries, each after LBA 0, inside the
  disk and apart from the others. One extended partition is walked through a
  bounded, validated EBR chain; logical partitions are listed and selected
  with indices starting at five, while the container itself is not mountable.

A selector then picks the volume: `auto` (the whole disk without a table, the
first MBR FAT32 partition, the first GPT basic-data partition — never the EFI
system partition), a partition number, a GPT unique GUID or name, or the whole
disk. The filesystem endpoint is served through a block slice of that volume:
every offset is checked against the slice and moved to it, `Info` reports the
slice's size, and a read-only slice refuses writes. Without a volume the
endpoint answers I/O errors and the system boots without persistent storage.
PID 1 passes the selector — `auto` for now — with every filesystem endpoint it
hands over (control protocol version 3), and the table is read again each time.
`tools/mkfat32 -table none|mbr|gpt` builds a disk with the FAT32 volume on the
whole disk or in one partition from 1 MiB.

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
offset is derived from it (ADR 0077, ADR 0102, ADR 0112):

- the boot sector must carry its signature and describe FAT32: no fixed root
  directory, no 16-bit FAT size, FAT32 version 0.0; FAT12, FAT16, exFAT and NTFS
  are refused as unsupported;
- logical sectors are 512, 1024, 2048 or 4096 bytes, sectors per cluster a
  power of two up to 128 (clusters of 512 bytes to 512 KiB), and there are one
  or two FATs; the media byte must be one FAT defines;
- the declared volume must fit the device and its metadata must fit the volume;
- a FAT too small for the data region limits the volume to the clusters it can
  describe, as Linux does, and no volume may number more clusters than FAT32
  addresses; a FAT32-shaped volume with fewer than 65525 clusters is served as
  FAT32, as Linux serves it;
- the root cluster must be inside the data area, a FAT named active by the
  extended flags must exist, and the FSInfo and backup sectors must lie in the
  reserved region apart from each other and the boot sector.

The backup boot sector is compared with the primary. When the primary fails
validation, a backup at logical sector 6 that validates, for any supported
sector size, is used instead and the volume is mounted read-only: every change
answers "permission denied" and the damaged primary is left for `fsck.fat` to
repair. A mount records what it noticed — backup used, backup differing or
invalid, primary or backup FSInfo invalid or differing, a short FAT — and the service logs a degraded volume;
mounting repairs nothing.

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
is bounded only by the VFS path length.

Names are UTF-8 of at most 1024 bytes natively (ADR 0110), stored on
disk as VFAT UTF-16LE with surrogate pairs outside the Basic Multilingual Plane
(ADR 0112). Control characters, `" * / : < > ? \ |`, a leading space and a
trailing space or dot are refused for creation, and FAT32 independently limits
names to 255 UTF-16 units. Valid foreign trailing-space and trailing-dot names
remain listable and exactly openable without Win32 trimming. Names are kept
exactly as given — no Unicode normalization — and matched case-insensitively
through generated, locale-independent Unicode simple uppercase mapping. A name whose base
and extension each fit 8.3 in one case stays a single entry carrying the
Windows NT case flags (`readme.txt`, `README.TXT`); anything else gets a VFAT
long-name chain and a generated alias — `~1` to `~9`, then a hashed form, with
one `_` per non-ASCII character — so any number of similar names fit. An entry
also answers to its alias. On-disk 8.3 bytes from 0x80 up are read as code page
437. A long-name chain with broken ordinals, checksum, padding or UTF-16, or
whose name this system could not open, is shown by its alias; an entry without
any usable name is not listed.

Timestamps are read and written as UTC (FAT has no zone), clamped to FAT's
1980–2107 range. Creation records creation, modification and access; a data
change records modification and the access date and sets ARCHIVE; a create,
remove or rename in a directory records the directory's modification time; a
read records nothing. If native `REALTIME` was not initialized, new entries
have unknown times and existing ones are kept. HIDDEN,
SYSTEM and undefined attribute bits are preserved; READ_ONLY on a file refuses
writes and truncation with "permission denied" but not rename or removal.
The clock is native `REALTIME`; until timezone configuration exists, the
system timezone and FAT local-calendar interpretation are UTC. Times and
attributes are available as native `fat32.Metadata`; the backend
protocol does not carry them yet.

The first (or active) FAT is the one read, and every change is written to each
mirrored copy a whole unit at a time. The FSInfo free count is counted from the
FAT on the first change after mount, kept exact, and written with the next-free
hint at every sync. Primary and backup FSInfo are validated and compared; a
valid backup is the fallback for an invalid primary, mismatch is exposed in
mount health, and an explicit clean sync makes both copies consistent when at
least one validated. Mount alone does not repair either copy.

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
The boot gate copies `/system/motd` through VFS
and verifies the result in the host-visible disk image, and a second VM boot
reads it back, proving persistence across a complete guest restart.

A journal, a wall clock for timestamps, dynamic interrupt routing, multiple
devices, and block payloads larger than the inline IPC limit remain future work.

## Resident sectors and bounded failure

The FAT32 service keeps a write-through window of sixteen resident 512-byte
metadata units, replaced least recently used, whatever the volume's sector size. Directory entries and FAT links are
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
