# System log

Structured events share one record format (`internal/logfmt`): level,
component, message, and `key=value` fields, bounded to 181 encoded bytes and
printable ASCII. The kernel assigns the UTC `REALTIME` timestamp and the
process ID when it accepts a record. Ordering and deadlines continue to use
the independent `MONOTONIC` clock.

`kernel/klog` retains the most recent 2048 records, 416 KiB of kernel BSS
(ADR 0211). Each record gets an absolute sequence number; the ring's oldest
retained sequence counts the records it overwrote. Native operation 25,
`LogWrite(console, record, length)`, appends a record and needs the console
device handle with write rights. Operation 26,
`LogRead(console, index, buffer)`, returns the index-th retained record as a
240-byte, zero-padded text line and needs read rights. Records at warning level
and above are echoed to the console as they are accepted.

`userspace/log` provides `Init(console, component)` and
`Info`/`Warn`/`Error(message, fields...)` with `Str`, `Bytes`, and `Uint`
fields. Services call `Init` once with their startup console handle. The
shell's `log` builtin prints every record the kernel ring retains, up to
2048, oldest first:

```text
[2026-09-26T15:44:31.152328Z] INFO  kernel[0]: kernel initialized free-pages=93568
[2026-09-26T15:44:31.188156Z] INFO  init[1]: service started service=vfs pid=4
[2026-09-26T15:44:31.205971Z] INFO  vfs[4]: mounted fs=fat32 path=/persist
```

Operation 33, `LogBounds(console, buffer)`, writes the sequence number of the
oldest retained record and one past the newest. Operation 58,
`LogReadSequence(log, cursor, buffer)`, copies up to 64 structured records
starting at a cursor, with the ring's oldest and end sequences at that moment
and a gap flag when the cursor fell below the oldest. A reader continues at
`First + count` until it reaches the end. Both take the same read rights
`LogRead` does, because they describe the same records. A `WaitAnyCursor`
interest on the log handle with a cursor is ready once the ring holds a record
at or past that cursor.

The kernel's diagnostic dump prints one `DIAG log` line: `capacity`,
`appended` (every record since boot), `retained`, and `overwritten`.

## Persistence

`log` (`/system/services/log`) is a supervised userspace service that writes
the ring to the persistent volume (ADR 0073, ADR 0211). The ring stays the
ordering point: every component still writes to it with `LogWrite`, and this
service is a consumer rather than a path records travel through, so a
component's diagnostics do not depend on another process being alive.

The service holds the log capability that authorises reading the ring, console
write for its own records, a VFS session for its files, and a read-only
configuration session for `system.log.`. It keeps an absolute cursor, the
sequence of the next record to write, and blocks in `WaitAnyCursor` on the log
handle with that cursor, on PID 1's control channel, and on the VFS session's
peer-closed event. A new record wakes it. It reads `LogReadSequence` batches,
appends them in 2 KiB writes, and stops when the cursor reaches the ring's end.
The wait's five-second timeout runs maintenance: rereading settings, retrying
storage after a failure, and reporting counters. The cursor advances only after
a write succeeds.

| Path | Contents |
| --- | --- |
| `/state/system.log` | the live segment |
| `/state/system.log.1` to `.3` | older segments, newest first |

Every line ends with ` #N`, the record's kernel sequence. A cursor below the
oldest retained sequence means the ring overwrote records before the service
read them; the service writes `[lost N records] #M`, M the sequence before the
next record, and continues, so a hole in the persisted log reads as a hole.

At startup the service cuts a final line that has no newline, the remains of a
write a crash interrupted. A service PID 1 restarts within the boot resumes
after the sequence on the file's last line, once the ring's record at that
sequence renders to the same line, so it writes nothing twice. After a reboot
it starts at sequence zero: whatever the ring dropped before the service ran
becomes a gap marker.

Rotation keeps 4 segments of 16 MiB, the live one included: 64 MiB of history.
Before a line would carry the live segment past its bound, the service moves
`.2` onto `.3`, `.1` onto `.2`, and the live file onto `.1`, each with
`Replace`, then starts a new live file. A refused step ends the rotation
before the live file moves, so the newest previous segment survives, and the
live segment grows past its bound until a later pass rotates.

| Setting | Default | Bounds |
| --- | --- | --- |
| `system.log.persist` | `true` | |
| `system.log.segment-bytes` | 16777216 (1536 in a validation build) | 1024 to 1073741824 |
| `system.log.segments` | 4 | 2 to 9 |

The service refuses an out-of-bounds rotation setting with an error record and
keeps the bounds it had. Turning persistence off moves the cursor with the
ring, so turning it on again starts from the present.

The service writes a `counters` and a `rotation counters` record: the ring's `appended` and
`overwritten` counts at its last read, and its own `persisted`, `lost`,
`failed`, `rotations`, `refused`, `repaired` and `cut-bytes`. It writes them
when a loss or failure count changes, and once an hour otherwise.

PID 1 asks VFS for the service's session through the control endpoint after
the filesystem is healthy and before the shell starts, which is the only
service whose endpoint is created that way rather than handed over at boot. A
failure to start it warns and continues: the ring is the system log and keeps
working without it.
