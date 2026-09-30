# ADR 0156: Linux families share file descriptions and serialize VFS access

## Status

Accepted.

## Context

Copying an fd table cannot copy open-file state. Parent and child must observe
the same file offset, status flags, stream endpoint, directory cursor, and
device state. Close-on-exec belongs to an fd entry and must remain local to
that entry.

The VFS protocol keeps position in service-owned open objects and uses one
ordered correlation stream. Concurrent requests from related processes would
race that state and could deliver a reply to the wrong task.

## Decision

Each process owns a 64-entry fd table. Entries point into a bounded global pool
of reference-counted open-file descriptions. A description stores its family
ID, kind, status flags, object, offset, and reference count. Inheritance copies
fd entries and increments description references. Duplication uses the same
operation. Close-on-exec remains in each copied fd entry.

Standard-stream descriptions own endpoint references directly. The launch
path removes the redundant native stream handles after installing the initial
descriptions. The last descriptor reference drops the endpoint reference and
wakes the peer when that stream end closes. Directory descriptions use
family-owned directory records, so a shared cursor remains valid after the
process that opened it exits.

Each family also owns one VFS context. It contains the correlation sequence,
failed-session state, current task, and a 15-entry FIFO waiter queue. A process
keeps its own VFS channel handle and I/O buffer. Only the task that owns the
context can issue a request. Completion transfers ownership to one queued task
and wakes that exact generation-safe task ID. Kill, fault, and exit remove the
task from the owner or waiter position before its scheduler slot is reclaimed.

## Consequences

Inherited and duplicated fds observe shared offsets and status flags while
retaining independent close-on-exec bits. A resource closes exactly once, on
the final description reference. VFS operations within a family are ordered
without spinning. Unrelated families retain independent correlation streams
and queues.
