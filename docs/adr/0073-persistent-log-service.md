# ADR 0073: Persistent log service

## Status

Accepted. Extends ADR 0059 and ADR 0071. Amended by ADR 0113: persisted records
now carry UTC realtime timestamps.

## Context

ADR 0059 gave the system a structured log: a bounded ring in the kernel, fed by
the kernel itself and by services through a native call, with warnings echoed to
the console and `log` in the shell to read it back.

The ring is sixty-four records and it is memory. It holds the newest records and
loses all of them at reset, which is exactly the moment a log is most wanted.
Nothing that happened before a reboot could be examined after one, and a burst
of activity overwrote its own beginning. A log that cannot outlive the failure
it describes is a debugging aid, not a log.

The specification puts the logging service in userspace and has it receive
structured events rather than keeping storage in the kernel. Persistent storage
and a named place for mutable system state now exist, so the missing piece is
the service.

## Decision

**A userspace service drains the ring onto the volume.** `/system/log` holds the
console capability that authorises reading the ring and a VFS session for its
own file, and nothing else. It is supervised by PID 1 with `restart: on-failure`
like any other service.

**The ring stays the ordering point.** Services keep writing to it through
`LogWrite` and do not log *through* this service. Making it the path every
record travels would put a restartable userspace process between every component
and its own diagnostics, and a service that crashes while logging its own crash
is not a good place for the record of it. The ring is in the kernel, it is
always there, and this service is a consumer of it.

**The kernel reports the window rather than the records.** `LogBounds` writes
the sequence number of the oldest retained record and one past the newest. A
reader keeps a cursor between them and reads what is new with the existing
`LogRead`. This is the smallest kernel addition that lets a consumer persist a
ring without re-reading it, and it needs the same console authority `LogRead`
does because it describes the same records.

**A gap is written down, not skipped.** A cursor below the oldest retained
sequence means records were overwritten before the service reached them. It
appends `[lost N records]` and continues, so a hole in the persisted log is
visible as a hole rather than as a quietly shorter file.

**Records already in the ring at startup are persisted.** They describe this
boot, and starting at the newest would mean the log never covers its own start.

**The file lives on `/state`.** A log is mutable system state: not user data,
and not part of the immutable system image.

**It is bounded and rotates once.** The live file is capped and, when it crosses
the bound, the previous rotation is removed, the live file is renamed over it
and a fresh one is created. History is therefore between one and two bounds,
which is what one FAT32 file can hold here anyway. The validation build uses a
smaller bound so a single boot test session crosses it; only the threshold
differs, and the rotation the test exercises is the one a release build
performs.

**It drains on a timer, not on an event.** There is no event source for "a
record was written", so the service sleeps between passes rather than being
woken. One wake per second is not the polling ADR 0051 moved away from: it is a
bounded periodic task with nothing to wait on.

**A failure is reported once.** Logging from the log service produces records
the service will itself try to persist, so repeating a failure every pass would
turn a broken volume into a growing ring of complaints about it.

**Its session is asked for, not handed over.** Every other service gets its
endpoints at boot, before VFS exists. This one needs a filesystem, so PID 1
creates its session through the VFS control endpoint the same way it does for an
application, after VFS is healthy and before the shell starts. That split the
service startup into two phases, which also puts `[ OK ] log` before the prompt
rather than after it.

**A failure to start it is not fatal.** The kernel ring is the system log and
keeps working; only its persistence is lost. That is worth a warning, not a dead
boot.

## Alternatives

**Writing the log from the kernel.** The kernel already has the records and
would need no service, no session and no protocol. It would also need a
filesystem, a block driver and a write path in kernel mode, which is the
opposite of where this system puts filesystems.

**Routing every record through the service.** It would make the service the
system log rather than a consumer of it, matching how larger systems do it. It
also puts a restartable userspace process on the path of every diagnostic,
including the ones emitted while it is not running, and it loses the ordering
the single ring gives for free.

**A sequence-addressed `LogRead`.** Replacing the index with a global sequence
number would remove the need for `LogBounds`. It also changes the call the shell
already uses, and a reader still has to learn the window to know what to ask
for, so it trades one call for a migration.

**Reading the ring on a channel the kernel writes to.** The kernel could push
records to a service endpoint, removing the timer. It makes the kernel a client
of a userspace queue on a path that must work when that service is dead or slow,
and the queue's backpressure would then apply to logging itself.

**Unbounded growth with no rotation.** Simpler, and a FAT32 file here tops out
at 64 KiB anyway, so the bound would just arrive as a write error instead of a
rotation. One rotation is a few lines and keeps the log useful after it fills.

## Consequences

`cat /state/system.log` reads this session's records, and after a reboot
`cat /state/system.log.1` reads the previous one's. The boot test asserts both,
including that a formatted record survives the reset, and that rotation produced
the second file.

The kernel gained one operation, `LogBounds`, which writes sixteen bytes and
takes the authority `LogRead` already took.

The service costs one wake per second. An idle pass is one `LogBounds` native
call and nothing else: it returns before touching the filesystem, so a quiet
system pays almost nothing. A pass with records to write opens the file once,
seeks once, writes each record in thirty-two byte chunks — the inline data one
`GVF1` request carries — and closes.

A pass that writes contends with everything else using storage, because the
filesystem service is single threaded and FAT32 writes go through to the disk.
Measured at 100 Hz: command latency medians are unchanged, but `bench vfs`, two
hundred VFS round trips inside one process, goes from 146 to about 320
microseconds per operation when a drain lands in its window. Raising the drain
interval to sixty seconds returns it to 150, which identifies the cost as the
writing rather than the service's presence, its session, or its sleeping wait
registration. One second is kept because it bounds how much is lost to a reset,
and the contention only appears while something else is using storage at the
same time.

Its first implementation exhausted its own bump allocator on the first drain,
because a pass that persists a whole ring builds far more request messages than
that arena holds. It now resets the arena per record, which bounds a pass to one
record's worth of messages. The failure was loud and the supervisor restarted
the service, which is the behaviour ADR 0019 is for, but the file was left with
a half-written line: the service appends at the end of what is there, and a
truncated final record stays truncated. Records are line-oriented, so a reader
loses one line rather than the file.

PID 1 now creates one more channel that lives as long as the system does, taking
its steady-state channel count from seven to eight of the sixteen its quota
allows.
