# ADR 0059: Structured system log

## Status

Accepted. Implements the early backend of spec §27. Amended by ADR 0113:
record timestamps are now UTC realtime rather than time since boot.

## Context

The kernel and the services reported events as bare uppercase codes written
to the serial console: `LIFECYCLE_QUEUE_OVERFLOW`, `INIT_LAUNCH_ERROR: 2`,
`APP_VFS_SESSION_FAILED`. These codes carried no time, level, component,
process, or fields. Informational events were not recorded at all, and a quiet
normal boot (ADR 0058) needs somewhere to keep them.

## Decision

`internal/logfmt` defines one record, shared by the kernel and userspace. A
record has a level (debug, info, warn, error), a component name, a message,
and `key=value` fields. Its wire form is bounded: 16-byte component, 64-byte
message, 96 bytes of fields, printable ASCII only. Its text form is:

```text
[2026-09-26T15:44:31.205971Z] INFO  vfs[4]: mounted fs=fat32 path=/persist
```

The kernel keeps the most recent 64 records in `kernel/klog`. Two native
operations use it:

- `LogWrite` (25) validates an encoded record and stamps it with the UTC
  realtime clock and the caller's process ID, so neither can be forged.
- `LogRead` (26) returns the index-th retained record as text.

Both require the console device capability: write rights to log, read rights
to read. Utilities hold no console capability and report through their
standard streams instead. Records at warning level and above are also echoed
to the console. The kernel logs its own events as component `kernel`,
process 0.

`userspace/log` wraps the call as `log.Info`, `log.Warn`, and `log.Error` with
typed fields. PID 1, VFS, the block service, and the virtio-blk driver log
service starts, exits, readiness, and mounts. Their fatal paths log an error
and fall back to a raw console write if logging itself fails. Existing
diagnostic codes are kept as messages, so harnesses that match them still
work. The shell's `log` builtin prints the retained records.

## Alternatives

A userspace log service reached over IPC is the target design in spec §27.
It needs a channel to every component before any of them can report, and
those channels are exactly what fails during early boot. The kernel ring can
feed such a service later without changing the record format.

Free-form text lines would be simpler, but they give up level filtering and
field parsing.

## Consequences

A normal boot keeps its informational history out of the console, and `log`
shows it. Warnings and errors are visible immediately, with time and process.

The ring holds 64 records, so older events are lost on a busy system until a
persistent log service exists. A component holding the console capability can
flood the ring and the console. It is limited only by record size, not by rate.
