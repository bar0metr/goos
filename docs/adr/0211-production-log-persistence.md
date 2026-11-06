# ADR 0211: Production log persistence

## Status

Accepted. Amends ADR 0059 and ADR 0073.

## Context

The kernel ring held 64 records. The log service woke once a second, asked
for the ring's bounds, and read new records one `LogRead` call at a time.
A burst of more than 64 records inside one second lost its oldest, and a
service that wanted every bind record of the socket service had to drop
them (commit 8368b78). The release build rotated at 24 KiB and kept one
previous file, on the grounds that a FAT32 file here held at most 64 KiB.
The FAT32 service has since grown to the format's 4 GiB file limit.

A restart within the boot compared record times to skip what the previous
instance had written, and a crash in the middle of a write left a torn last
line that the next instance appended after.

## Decision

**The ring holds 2048 records.** An entry is 208 bytes, so the ring costs
416 KiB of kernel BSS. `kcall.LogRingCapacity` publishes the bound to readers.
`LogReadSequence` still copies at most 64 records per call; its `Total` is now
the ring's end at the snapshot, and a reader continues at `First + count`
until it reaches `Total`. Each processor's copy area holds one batch, 64
entries, not the whole ring.

**The service waits on its cursor.** `logd` blocks in `WaitAnyCursor` on the
log capability with its cursor, the PID 1 control channel, and the VFS
session's peer-closed event. A new record wakes it; it drains batches until
the cursor reaches `Total` and waits again. The five-second timeout rereads
settings, retries storage that failed, and reports counters. No record waits
for it.

**Every line names its sequence.** A persisted line ends with ` #N`, where N is
the record's kernel sequence. A gap marker, `[lost N records] #M`, carries the
sequence before the first record after the gap. The file's last line therefore
names the last record it accounts for.

**Startup repairs, then resumes.** The service cuts a final line without a
newline before it appends. A service PID 1 restarts within the boot resumes
after the sequence of the file's last line, provided the ring's record at that
sequence renders to the same line; a tail left by an earlier boot never
matches. After a reboot the cursor starts at zero, so records the ring dropped
before the service ran become a gap marker. A write that fails part way sets
the same repair and resume for the next pass.

**Rotation keeps four segments of 16 MiB.** `system.log` is live and
`system.log.1` to `system.log.3` hold older records, 64 MiB in all. The
settings `system.log.segment-bytes` and `system.log.segments` change the
bounds; the service refuses values outside 1 KiB to 1 GiB and 2 to 9 segments
with an error record and keeps the ones it had. The validation build's
default segment is 1536 bytes. Rotation moves each segment up one name with
`Replace`, oldest first, and moves the live file last. The first refused step
ends the rotation, so the newest previous segment is never removed before the
file replacing it exists; the live file grows past its bound until a later
pass rotates.

**Counters.** The service counts records persisted, records lost before
persistence, failed passes, rotations, refused rotations and repaired lines,
and keeps the ring's appended and overwritten counts from its last read. It
writes them to the log when a loss or failure count changes, and once an hour
otherwise. The kernel's diagnostic dump prints a `DIAG log` line with the
ring's capacity, appended, retained and overwritten counts.

## Alternatives

**A variable-length ring.** Packing records by their encoded size would hold
about three times as many in the same memory. It costs a second index and
copies that wrap, in the one kernel structure every component writes to.

**Persisting on the timer with a larger ring.** A larger ring alone shrinks the
loss window but keeps a one-second wake on an idle system and a one-second
delay before a crash's last records reach the disk.

## Consequences

The bind records of `init` and `linuxsockd` are back. A boot fits in the ring
with room to spare, and the shell's `log` shows the newest 64 records, one
batch.

An idle system costs the service one maintenance wake every five seconds,
three configuration requests and no filesystem requests. A busy one writes
2 KiB chunks rather than a request per line.

A validation session rotates several times, so the boot test's
`system.log.1` after a reboot can hold records of the second session as well
as the first.
