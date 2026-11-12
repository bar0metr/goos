# ADR 0222: Filesystem volume instances

## Status

Accepted. Extends ADR 0096 (separate filesystem service), ADR 0098 (file
cache), ADR 0115 (block slices) and ADR 0221 (ext4 adapter). ADR 0226
replaces the read-only rule for additional slices and probe authority:
a read-write mount gets a writable slice on a new endpoint.

## Context

PID 1 starts one filesystem service, FAT32, for one block slice. The block
service keeps one filesystem endpoint and one slice. The VFS keeps one
remote backend, one file cache and one recovery fence. Every layer assumes
there is exactly one persistent volume.

ext4 needs a volume of its own, and the next filesystem will need one too.
Adding an ext4 service, an ext4 endpoint, an ext4 cache and an ext4 fence
next to the FAT32 ones would repeat every piece of that plumbing once per
filesystem type.

## Decision

**Types and instances.** A filesystem type is a driver program in a static
registry. A volume instance is one driver process serving one block slice
at one mount path. Two ext4 partitions run two processes of the same
`/bootstrap/ext4` program; a damaged volume crashes its own process and no
other.

**Slots.** Block service, VFS and PID 1 each keep the same table of volume
slots. Slot 0 is the persistent volume: FAT32 at `/persist` with its
aliases, the protected `/system`, and the recovery fence of ADR 0178,
unchanged. Slots 1 to 4 hold additional volumes. A slot number and a
generation travel in every control message that names an instance, and the
generation rises with every launch, so a message about an earlier instance
cannot act on its replacement.

**Block slices.** `SetFilesystem` names a slot and whether the slice is
writable. Each slot has its own endpoint and slice. A request on one slot's
endpoint can only reach that slot's slice. Additional volumes get read-only
slices in this build.

**Detection.** `userspace/fsdetect` reads at most 64 KiB from the start of
a slice and recognizes FAT32 by its boot sector and BPB geometry and ext4 by
its superblock magic and geometry. Recognition only chooses a driver. The
driver validates the volume before it serves anything, and its refusal
(unsupported feature, corrupt metadata, recovery needed) reaches the
caller as a refusal. A slice that matches two signatures is refused as
ambiguous. No other driver is tried when one refuses a volume, so damage
cannot hide behind a fallback.

**Probe authority.** To detect a volume, PID 1 asks the block service for a
read-only endpoint on the slot, reads the probe window through it, then
moves that same endpoint to the driver it launches. PID 1 never holds a
writable slice for an additional volume, and the driver gets no more than
PID 1 had.

**Drivers live in the boot archive.** A program on the installed volume
pages its code in through the VFS (ADR 0121). The VFS waits on a
filesystem driver for every exchange, so a driver loaded that way would
fault on a page the VFS cannot serve until the driver answers. Drivers are
archive members, `/bootstrap/fat32` and `/bootstrap/ext4`, whose pages the
kernel copies from the archive.

**Policy.** Detection never mounts. PID 1 mounts an additional volume only
when a configuration entry (`system.mount.1` through `system.mount.4`, each
containing `TYPE SOURCE PATH`) or an administrator's `mount` command names it.
A source names a partition by GPT
name, GPT unique GUID or number. Configuration is read once the
configuration service runs, after the persistent volume.

**File caches.** Each additional instance gets a file cache memory object
that PID 1 creates for that launch and grants to the driver and, with the
attach, to the VFS. A replacement instance gets a new object. The VFS stops
using the old cache when it detaches the old instance, so an old process
that is still alive cannot write into pages the VFS reads from its
successor. The persistent volume keeps the kernel-provided cache and its
fence.

**Identity.** PID 1 records the volume identity the probe reports (the ext4
UUID, the FAT32 serial) at the first mount. After a crash it probes again
and reattaches only a volume with the same type and identity. The VFS also
requires the same root node. UUIDs, serials and root inode numbers can be
reused, so they do not prove that the contents or open objects are unchanged.
The block service therefore returns the resolved partition identity with the
first grant. PID 1 presents it on every replacement: GPT identity includes the
disk GUID, unique partition GUID and geometry; MBR identity includes a nonzero
disk signature, entry type and geometry. A changed identity, or an MBR disk
without a stable signature, fails closed before the filesystem probe. Driver
replacement discards cached geometry and rescans the table before any slot is
served.
VFS revokes open handles for an additional volume when its service goes
offline; callers must reopen files after reattachment. Control replies are
accepted only for the exact request correlation, so a delayed reply from an
earlier use of a slot cannot authorize its replacement.

**Failure.** A transport failure on an additional volume detaches its
backend in the VFS, which keeps the mount and answers I/O errors, and sends
PID 1 a `VolumeLost` notice with slot and generation. PID 1 ends the old
process, if it still runs, and launches a replacement within the restart
budget. Other slots keep running. When the budget runs out the slot stays
failed and the mount answers errors until an administrator unmounts it.

**Unmount.** `umount PATH` flushes the backend, refuses while files are open
through it, removes the mount, stops the process, and releases its block
endpoint, cache object and channels.

## Consequences

- The VFS mount table grows from 8 to 16 entries. The persistent volume and
  the boot archive use 8.
- Additional volumes use the serial read lane of the VFS read pool, which
  serializes them with each other; the persistent volume keeps its own
  lane.
- Each mounted additional volume costs a process, three channels, and a
  516 KiB cache object.
- The persistent volume still has its own attach code. Moving it onto the
  same instance path needs the shared kernel cache replaced by per-launch
  objects, a change to the boot path that gets its own review.
- One disk only: the block service still drives one virtio-blk device, so
  every slot is a partition of it.

## Verification

Host tests cover each layer: `userspace/fsdetect` against Linux-built ext4
images and host-built FAT32 images, ambiguous and unknown slices;
`userspace/volume` for sources, slot conflicts, restart budget and identity
checks; `userspace/block` for the slot and writable fields; `kernel/vfs` for
the attach, detach and lost messages; `userspace/vfs` for two ext4 volumes
beside the persistent one, a failed instance that stays in its slot, a
replacement that ignores the old cache, busy and offline unmounts; and
`tools/diskcompose` for a composed GPT disk resolved through the partition
scanner and the probe.

`make test-volumes` boots a disk with the installed system, two Linux ext4
partitions and an empty one. It checks the configured mount at boot, `mount`
and `umount` from the shell, every refusal (no filesystem, a partition
already mounted, the persistent partition, a missing partition, a path
outside `/mnt`, a forced type), a write refused on the read-only volume, an
instance killed and replaced while the other volume keeps serving, and a
released slot mounted again.
