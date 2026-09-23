# System log

Structured events share one record format (`internal/logfmt`): level,
component, message, and `key=value` fields, bounded to 181 encoded bytes and
printable ASCII. The kernel assigns the time (nanoseconds since the timer
started) and the process ID when it accepts a record.

`kernel/klog` retains the most recent 64 records. Native operation 25,
`LogWrite(console, record, length)`, appends a record and needs the console
device handle with write rights. Operation 26,
`LogRead(console, index, buffer)`, returns the index-th retained record as a
240-byte, zero-padded text line and needs read rights. Records at warning level
and above are echoed to the console as they are accepted.

`userspace/log` provides `Init(console, component)` and
`Info`/`Warn`/`Error(message, fields...)` with `Str`, `Bytes`, and `Uint`
fields. Services call `Init` once with their startup console handle. The
shell's `log` builtin prints the ring, oldest record first:

```text
[    0.152328] INFO  kernel[0]: kernel initialized free-pages=93568
[    0.188156] INFO  init[1]: service started service=vfs pid=4
[    0.205971] INFO  vfs[4]: mounted fs=fat32 path=/persist
```

Operation 33, `LogBounds(console, buffer)`, writes the sequence number of the
oldest retained record and one past the newest. It takes the same read rights
`LogRead` does, because it describes the same records.

## Persistence

`/system/log` is a supervised userspace service that drains the ring onto the
persistent volume (ADR 0073). The ring stays the ordering point: every component
still writes to it with `LogWrite`, and this service is a consumer rather than a
path records travel through, so a component's diagnostics do not depend on
another process being alive.

The service holds the console capability that authorises reading the ring and a
VFS session for its own file, and nothing else. It keeps a cursor inside the
window `LogBounds` reports and appends whatever is new:

| Path | Contents |
| --- | --- |
| `/state/system.log` | the live file |
| `/state/system.log.1` | the previous one, kept by a single rotation |

A cursor below the oldest retained sequence means records were overwritten
before the service reached them. It writes `[lost N records]` and continues, so
a hole in the persisted log reads as a hole. The live file is bounded; crossing
the bound removes the old rotation, renames the live file over it, and starts a
fresh one. The validation build uses a smaller bound so a boot test session
exercises the same rotation a release build performs.

The service drains on a timer, once a second, because there is no event source
for "a record was written". PID 1 asks VFS for its session through the control
endpoint after the filesystem is healthy and before the shell starts, which is
the only service whose endpoint is created that way rather than handed over at
boot. A failure to start it warns and continues: the ring is the system log and
keeps working without it.
