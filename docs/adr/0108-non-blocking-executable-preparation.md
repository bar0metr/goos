# ADR 0108: Non-blocking executable preparation

## Status

Accepted. Extends ADR 0097 (programs from files), ADR 0106 (lazy executables),
and ADR 0107 (bulk I/O).

## Context

PID 1 read a program from a file synchronously: while it waited for the VFS,
it served no lifecycle event, no other job, no restart, and no shutdown. A
large program on the FAT32 volume stalled the whole supervisor for as long as
the read took (GoOS 0.1.3 specification §7). Bulk I/O made reads fast, but a
slow or stuck filesystem service could still hold PID 1 hostage.

## Decision

**Preparation is a job state.** A job whose stages name programs outside
`/system` is first prepared: for each such stage, PID 1 opens a VFS session of
the job's own, checks the file, creates the executable object, sends one
`FillExecutable` request with a Write-only duplicate of the object handle, and
returns to its loop. The session is one more channel in PID 1's `WaitAny`
set; its answer seals the stage's object or records why it failed, and the
next stage starts. When no stage is left, the pipeline starts as before and
takes the prepared objects in stage order.

**One fill per job, jobs in parallel.** A job has at most one fill in flight,
so its state is one pending record; separate jobs prepare independently. Each
fill has a deadline, the session deadline plus a second per MiB, which bounds
PID 1's wait alongside restart backoff. An overdue fill is abandoned by closing
its session and object, and its stage fails to launch.

**Stopping is answered.** A job stopped (`kill`, end of the shell session)
while it is being prepared finishes the fill in flight, closes every object it
prepared, and answers the shell with a new launch status, `LaunchStopped`
("stopped before it started"). No process was created, so there is no exit
status to report.

**Failures keep their meaning.** A missing file, a file that is not a regular
file or is too large, a refused object, and a short fill map to the launch
statuses the synchronous path used, and the pipeline reports them the same
way.

## Alternatives

- **A helper process that reads programs for PID 1.** It would isolate the
  wait, but adds a process, a protocol, and a capability to create objects on
  PID 1's behalf, for what one asynchronous request already achieves.
- **Cancelling the fill in the VFS.** The request is bounded and short; letting
  it finish and dropping the object keeps the VFS protocol unchanged.

## Consequences

- PID 1 stays responsive while a large program loads: the boot test starts a
  12 MiB program in the background and runs a command while it loads.
- Stopping a job during preparation leaks nothing: its objects are closed, and
  their pages return to PID 1's quota.
- A job's state now includes its preparation; `execJob` holds the prepared
  objects until the pipeline takes them or the job ends.
