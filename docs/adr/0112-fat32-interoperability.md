# ADR 0112: FAT32 interoperability

## Status

Accepted. Amended by ADR 0113 (the filesystem service now supplies realtime)
and ADR 0114 (full VFAT name capacity, complete simple case mapping, and
backup FSInfo handling).
Amends ADR 0102 (sector size, FSInfo free count, timestamps), ADR
0066 and ADR 0110 (the printable-ASCII rule for FAT32 names). Everything else
in those records stands.

## Context

After ADR 0102 the FAT32 service handled any cluster size and file size, but a
volume made elsewhere still met GoOS-specific assumptions (GoOS 0.1.4
specification, Phase 1):

- only 512-byte logical sectors were accepted; 1024, 2048 and 4096-byte
  sectors, which formatters use on large-sector media, were refused;
- long names had to be printable ASCII, so a volume written by Windows, macOS
  or Linux showed any other name as its 8.3 alias, and such names could not be
  created;
- 8.3 names were always shown lower case, whatever the Windows NT case flags
  said, and bytes from 0x80 up passed through undecoded;
- no timestamp was read or written, READ_ONLY was ignored and ARCHIVE never
  set;
- the FSInfo free count was replaced by "unknown" on the first change;
- the backup boot sector was written by `Format` but never read, and a FAT
  copy that disagreed with the first was invisible;
- the only interoperability test ran the host checker over volumes GoOS built;
  no volume built by another system was ever mounted.

Linux `fs/fat`, the Microsoft FAT specification, and the behaviour of the
macOS msdos driver were the behavioural references. None of their
infrastructure is used.

## Decision

**Geometry comes from the BPB, in the volume's own sector size.** Logical
sectors of 512, 1024, 2048 and 4096 bytes are accepted; a larger power of two
is `unsupported`, anything else `corrupt`. Sectors per cluster is any power of
two up to 128, so clusters run from 512 bytes to 512 KiB, as Linux accepts.
All offsets are derived from `BytesPerSector`. The metadata window stays in
512-byte units: every logical sector is a whole number of them and no FAT or
directory entry straddles one, so the window does not depend on the volume and
the device may have any granularity the block service translates.

**The BPB is validated field by field before anything is derived.** The boot
signature; an exFAT or NTFS OEM name (`unsupported`); the sector size; sectors
per cluster; a nonzero reserved count; one or two FATs (more is
`unsupported`); no root-entry count and no 16-bit FAT size (FAT12/16,
`unsupported`); the media byte (`0xf0` or `0xf8`–`0xff`, as Linux requires);
FAT32 version 0.0; a nonzero 32-bit FAT size and total; a volume that fits the
device and holds more than its metadata; a cluster count between 1 and the
FAT32 maximum, clamped to what the FAT describes; an existing active FAT; a
root cluster inside the data area. FSInfo and backup sector numbers must lie in
the reserved region, apart from the boot sector and each other; one that does
not is treated as absent and reported. All products are formed in 64 bits from
fields of at most 32 bits, so none overflows. The type follows the BPB rather
than the cluster count, as in Linux: a FAT32-shaped volume with fewer than
65525 clusters is served and reported (`FewClusters`).

**Mounting reports what it noticed and repairs nothing.** `Health` records
which boot sector was used, the state of the backup (absent, matching,
differing, invalid), the state of FSInfo, a short FAT, a small cluster count,
and whether the mount is read-only. The service logs a degraded volume.

**The backup boot sector is compared, and used only when the primary is
corrupt.** A backup whose BPB differs from the primary's is reported; the
primary governs, as everywhere else. When the primary fails validation, the
backup is looked for at logical sector 6 for every supported sector size and
accepted only if it validates, declares that sector size, and names itself as
the backup. Such a mount is read-only — every change answers `permission
denied` — and the damaged primary is not rewritten: choosing which copy is right
is a repair, left to `fsck.fat`. A primary that is `unsupported` is never
second-guessed from sector 6.

**The first (or active) FAT is authoritative.** It is the only copy read. Every
change is written to all mirrored copies a whole 512-byte unit at a time, so a
unit that had drifted agrees again after the next change within it. Mounting
does not compare copies, which would cost a read of every FAT on every mount;
`CompareFATs` compares them entry by entry for a checker and reports the first
differing cluster and the count. With mirroring disabled only the active copy
is read or written and there is nothing to compare.

**Cluster values are checked on every link.** Free, cluster 1, values past the
last cluster, the reserved range `0x0ffffff0`–`0x0ffffff6` and the bad-cluster
mark are corruption inside a chain; `0x0ffffff8` and above end it. Every walk is
bounded by the cluster count.

**FSInfo is validated, its hints are untrusted, and its count is kept.** The
three signatures of both primary and backup FSInfo are checked. The primary
governs when both validate; the backup is the fallback when the primary does
not. A missing, invalid, or differing backup is exposed in `Health`. The next-free hint is used when it names a data cluster. The recorded
free count is never believed: the count is taken from the FAT the first time a
change (or `FreeClusters`) needs it — one sequential read of the FAT, once per
mount — kept exact through every allocation and release, and written with the
hint at every sync. A clean sync writes consistent primary and backup copies
when at least one copy validated; mount alone performs no repair.

**Names are UTF-8 natively and UTF-16LE on disk.** A request must be valid UTF-8
(no surrogates, overlong forms or code points past U+10FFFF) of at most 1024
bytes, without control characters or `" * / : < > ? \ |`, not starting with a
space or ending with a space or dot. It is written as UTF-16LE, with surrogate
pairs outside the BMP, and is rejected when it exceeds VFAT's independent
255 UTF-16-unit limit. A chain on disk is accepted only
when its ordinals count down from one flagged last, each entry has type 0 and
cluster 0 and the checksum of the short entry after it, the terminator and
`0xffff` padding appear only in the last entry, the UTF-16 is valid, and the
resulting UTF-8 fits the native 1024-byte representation. Valid foreign names
ending in a space or dot are listed and opened by exact spelling without
Win32 trimming, but GoOS refuses to create or rename to such names. Anything
else falls back to the 8.3 alias, so every listed name can be opened.

**No normalization, simple case folding.** Names are stored exactly as given;
a precomposed and a decomposed spelling are two names. Matching is
case-insensitive, as VFAT requires, through the generated Unicode simple
one-to-one upper-case table. The mapping is deterministic, locale-independent,
and checked over every Unicode scalar value. Full multi-code-point folding is
not performed.

**8.3 names are read as other systems write them.** The NT flags in byte 12
decide whether the base and extension are shown lower case; without them the
name is shown as stored. A leading `0x05` stands for `0xe5`. Bytes from 0x80 up
are code page 437, the Linux default, checked against `iconv`. A name whose
parts are each in one case and fit 8.3 is stored in one entry with the flags it
needs (`readme.txt`, `README.TXT`, `readme.TXT`); a mixed-case part gets a
chain. An entry answers to its alias as well as its long name, as in Windows and
Linux, which also keeps a new 8.3 name from duplicating an existing alias. An
alias replaces each non-ASCII character with one `_`, and its `~1`–`~9` numbers
are found in one pass over the directory. An entry whose 8.3 name holds a NUL,
a control character or a reserved character, and that has no usable long name,
is not listed or matched; its slots stay in use for a checker.

**Timestamps are UTC, clamped, and never invented.** FAT records local time
without a zone; GoOS has no zone, so FAT times are read and written as UTC.
Times are clamped to 1980-01-01 00:00:00 – 2107-12-31 23:59:59 at FAT's
resolution (two seconds for modification, ten milliseconds for creation, a day
for access). A zero or invalid field is unknown and preserved. Creating an
entry records creation, modification and access; changing a file's data
records modification and the access date; creating, removing or renaming an
entry records its directory's modification time, best effort, since the
change itself is already made; `.` and `..` take the new directory's times; a
read writes nothing. The time comes from a `Clock` given
to the filesystem. ADR 0113 supplies the native UTC realtime clock; without a
valid clock, new entries still get zero, unknown, times and changes leave
existing times as they were. Callers see `Metadata` —
nanoseconds since the Unix epoch and native attribute flags — never DOS
encodings.

**Attributes are preserved and honoured.** No change rewrites the attribute
byte except to set ARCHIVE, so HIDDEN, SYSTEM and the two undefined bits
survive writes, truncation and renames. A file's data changing sets ARCHIVE.
READ_ONLY on a file refuses writes and truncation with `permission denied`;
rename and removal remain allowed, as in Linux. READ_ONLY on a directory means
nothing, as in Windows and Linux. Volume-label entries are never listed, opened
or reused.

**Interoperability is tested against the host's own FAT implementation.**
Volumes are formatted by `newfs_msdos` (macOS) or `mkfs.fat` (Linux) in every
sector size, mounted, filled and checked here, and accepted by `fsck_msdos` or
`fsck.fat`; volumes this code formats in every sector size are accepted by the
host checker; on macOS the host's msdos driver writes Unicode names, nested
directories and a large file that this code reads, timestamps included, and
then reads back what this code wrote. Two volumes made that way are kept in
`userspace/fat32/testdata`, stored sparse, so the foreign-volume cases run on
every host. Tests that need a host tool skip without it.

## Alternatives

- **Caching logical sectors in the window.** A 4 KiB sector would have made the
  window eight times larger for no gain: metadata structures are 4 and 32
  bytes, and 512-byte units hold them just as well.
- **Refusing a corrupt primary outright.** Safe, but it would leave a volume
  whose backup is intact unreadable until a checker runs. Serving it read-only
  loses nothing and changes nothing.
- **Restoring the primary from the backup at mount.** Silent repair on mount
  could overwrite the only good copy of a BPB edited on purpose; it belongs to
  `fsck.fat --repair`.
- **Comparing FAT copies at every mount.** Correct, but its cost grows with the
  volume on every boot; a checker is where a full comparison belongs.
- **Trusting FSInfo's free count, as Linux does.** A malicious or stale count
  would then propagate forever. Counting once is a single sequential read.
- **Unicode normalization (NFC).** It needs tables far larger than this
  service's budget, and it would make GoOS disagree with Linux, which stores
  names as given; the macOS driver writes composed names, which then match
  exactly.
- **Full Unicode case folding.** The same table cost; the scripts covered are
  the ones whose case pairs are regular, and Windows' own table differs in
  details either way.
- **Writing the FAT epoch when there is no clock.** It would claim a date that
  is certainly wrong; zero is FAT's own "not recorded".
- **Local time with a configured zone.** GoOS has no zone database or
  configuration for one; UTC is the only choice that is not a guess.

## Consequences

- Volumes from Windows, macOS and Linux, in any sector size, mount with their
  names, cases and times intact, and what GoOS writes reads back the same way
  on macOS.
- `fat32.Layout` gains `BytesPerSector` and `VolumeID`; `tools/mkfat32` gains
  `-sector-size`. `Format` zeroes the reserved region and writes a backup
  FSInfo, as other formatters do.
- `FileSystem` gains `Health`, `Metadata`, `FreeClusters`, `CompareFATs` and
  `SetClock`, the interfaces `fsck.fat` (Phases 4 and 5) will use.
- The node-ID backend protocol and the VFS do not yet carry timestamps or
  attributes; `Metadata` is available to in-service callers only.
- GoOS-created files receive UTC realtime timestamps when the native clock is available.
- Directory scans remain linear in the directory's entries; the window is
  still sixteen units.
