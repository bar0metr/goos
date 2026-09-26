# ADR 0115: Partition layer and block slices

## Status

Accepted. Extends ADR 0033 (block service), ADR 0096 (FAT32 service) and ADR
0101 (restartable block service).

## Context

The FAT32 service was handed a channel to the whole disk and expected its boot
sector at byte 0. A disk made by any current tool puts FAT32 inside a
partition, behind an MBR or a GPT, so such a disk could not be used at all
(GoOS 0.1.4 specification, Phase 2). Teaching the FAT32 parser to read
partition tables would have mixed two formats in one parser and still given it
authority over the whole disk: a defect in it could reach the other
partitions. The specification asks instead for a separate partition layer and
a capability-protected block slice: a filesystem service must not gain
authority outside its partition.

## Decision

**A separate, pure partition package reads the tables.** `userspace/partition`
recognises, in order: a GPT behind a protective MBR; a filesystem boot sector
at LBA 0 — a strictly valid FAT BPB whose volume fits the device, or an exFAT
or NTFS name — meaning the whole device is one volume ("superfloppy"); and an
MBR. A sector with boot indicators other than `0x00`/`0x80`, or with no used
entry, is not a table, and the whole device is left to the filesystem. It
performs no I/O of its own beyond a `Reader`, allocates nothing, and bounds
everything it reads: at most 128 used partitions, an entry array of at most
4096 entries and 1 MiB, blocks of 512 to 4096 bytes.

**MBR.** The four primary entries are read. A used entry must start after LBA
0, have a length, lie inside the device (checked without overflow), and not
overlap another. An extended partition is listed but cannot be chosen: logical
partitions are not read in this release.

**GPT.** A header must carry the signature, revision 1, a size from 92 bytes to
the block size, a zero reserved field, a matching CRC-32, and its own LBA;
usable range, entry array and the other header must all lie inside the device,
the array clear of both headers, LBA 0 and the usable range; the entry size is
a power of two from 128 bytes. The entry array must match its CRC before any
entry is judged. A used entry must lie in the usable range with its end not
before its start, and apart from every other. The primary governs; when it or
its entries fail, the backup — found where the primary says or, without one,
at the last block — is used and the table reported degraded, as Linux does. A
backup that is invalid, describes another disk or array, or is not at the last
block (an image that was grown) is reported. Nothing is ever rewritten. A
protective MBR whose GPT fails in both copies is corrupt, not an MBR — unless
LBA 0 is a filesystem boot sector whose boot code merely held such an entry.
GPT names are decoded from UTF-16 and kept when valid.

**A volume is chosen by a selector.** `auto` takes the whole device when there
is no table, the first MBR partition of a FAT32 type (`0x0b`, `0x0c` and their
hidden forms), or the first GPT basic-data partition; the EFI system partition
is never chosen automatically. A partition can also be chosen by number, by
GPT unique GUID, or by GPT name, or the whole device taken explicitly. A
number, GUID or name takes any type; whether the volume is usable is for the
filesystem service to say.

**A block slice bounds what a client reaches.** `block.Slice` is a start, a
length and a write right. `Service.DispatchIn` checks every range against the
slice without overflow and moves it to the slice before the device sees it,
reports the slice's size as the capacity, and refuses writes to a read-only
slice; a slice that does not lie inside the device serves nothing. The whole
device is the slice from zero to its capacity.

**The block service holds the table and the slice; the filesystem service
holds neither.** The block service already has the whole device, so reading the
table there gives the parser no authority it lacks, and the process that reads
the table is the one that enforces the result. When it first describes the
device it reads the table (through its bulk buffer) and resolves the filesystem
endpoint's selector to a slice; every request on that endpoint is served
through the slice, and without one it is answered with I/O errors, so the
FAT32 service reports its volume refused and the system boots without
persistent storage (ADR 0077). The FAT32 service addresses its volume from zero
and cannot name a byte outside it. What was found is logged: the table, the
partition chosen, and any degradation.

**PID 1 says which volume.** The control request that hands the block service
a filesystem endpoint (`GBLKC`, version 3) carries a selector in a fixed layout
whose unused fields must be zero. PID 1 sends `auto`; choosing another volume
belongs to mounting (Phase 3). The table is read again whenever an endpoint is
handed over, so a restarted filesystem service gets the volume the table
describes now.

**Nothing in the running system writes a table.** `WriteMBR` and `WriteGPT`
exist for `tools/mkfat32 -table none|mbr|gpt` and for tests. The images they
write carry fixed GUIDs, so builds stay reproducible, and the FAT32 volume
inside records its partition's start as hidden sectors, as Windows expects.

**PID 1's storage probe puts back what it overwrote.** It wrote six bytes at
offset 4093 to prove the block path, which on a GPT disk lies in the entry
array. It now reads them first and restores them after checking its pattern.

**Bulk objects a request carries are closed when the request is refused.** The
block service used to leave the object of an out-of-range bulk request open;
with read-only slices a refusal became more common, so every refused bulk
request now closes its object.

## Alternatives

- **Parsing tables in the FAT32 service.** One parser for two formats, and the
  filesystem would keep authority over the whole disk.
- **A separate partition service.** It would need whole-device authority too,
  and a second hop for every request, to enforce what the block service can
  enforce where it already stands.
- **A kernel slice object.** The kernel would learn about block devices for a
  check a userspace service performs as well.
- **Trusting the GPT backup only when the primary header is intact.** Linux
  and firmware fall back to the backup; refusing a disk whose primary sector is
  damaged loses data the backup still describes.
- **Reading MBR logical partitions now.** They need a bounded walk of a linked
  list of extended boot records; nothing in this release needs them.

## Consequences

- Disks partitioned by macOS, Windows or Linux with MBR or GPT mount; the
  host's own tools read the tables `mkfat32` writes, and mount and check the
  FAT32 volume inside.
- The filesystem service's authority shrinks from the disk to its partition.
- `userspace/partition` is linked into the block service and the host tools.
- `mkfat32` keeps a development disk it recognises, with or without a table.
- Choosing a partition other than the automatic one is not yet possible at
  run time; the protocol and the selector for it are in place.
