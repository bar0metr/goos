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

A userspace log service with persistent storage is future work; it can drain
this ring without a format change (ADR 0059).
