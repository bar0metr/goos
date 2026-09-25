# ADR 0099: A VFS that restarts

## Status

Accepted. Amends ADR 0018, ADR 0096 and ADR 0098. The block service, left
`RestartNever` here, restarts since ADR 0101.

## Context

The VFS was the one service whose failure ended the storage stack for the
rest of the boot. It was registered `RestartNever`, and a fatal error inside
it did not even exit: `fatal` spun forever, so its clients waited out a
deadline on every request to a service that would never answer. The block
service is the other `RestartNever` service; the driver below it and the FAT32
service above it already restart (ADR 0078, ADR 0096).

Restarting the process is the easy part. Every client of the VFS holds a
channel that dies with it: PID 1's control endpoint, the shell's session, the
sessions of the config and log services, and the FAT32 service's channel.
Restarting the clients instead would cost the shell its jobs (ADR 0089) and the
log service its place in the kernel ring, and the config service holds the
sessions of the shell and the log service.

## Decision

**The VFS restarts on failure, and fails by exiting.** It is registered
`RestartOnFailure`. `fatal`, and the check that its session table is
consistent, exit with code 1 after reporting, instead of spinning. The FAT32
service's `fatal` exits the same way.

**PID 1 renews what the launch needs.** Before a restarted VFS is launched,
PID 1 creates a new control channel, keeps its client end as the control
endpoint from then on, and a new first session, whose server end the VFS is
launched with. The system image and file cache grants were never consumed and
are granted again.

**Clients are handed sessions, not restarted.** A new message of the VFS
control protocol (version 5), the session notice, carries the client end of a
new session as a notification. PID 1 sends it over a channel each client
already holds with it: the shell's exec channel, the config service's control
channel, and a control channel the log service now gets for this purpose.
The shell takes its notice as soon as the VFS runs; it keeps its jobs, its
working directory, and its terminal. The config and log services take theirs
once the filesystem service is attached again, so that what they write reaches
the volume: the config service writes out a setting it could not persist while
the VFS was down, and the log service's cursor still names the first record it
has not written, so nothing is lost or written twice while the ring holds it.

**The filesystem service follows the VFS.** When its VFS channel dies, the
FAT32 service exits with code 2 instead of 0, so the supervisor starts it
again; it now depends on the VFS as well as the block service, so it starts
only once the new VFS runs. Its restart is the one ADR 0096 describes: fresh
channels to the VFS and the block service, and a mount of the volume by the
new VFS.

**What lived in the VFS is gone.** `/tmp` is a RAMFS inside the VFS process
and starts empty. Open files belonged to sessions of the old VFS; their
holders get an error on their next request. Commands running at the time fail
their next filesystem request and exit. Nothing on the persistent volume
depends on the VFS surviving: every write already went through to the
filesystem service before it returned (ADR 0098).

## Alternatives

**Restarting every client of the VFS with it.** Simple in PID 1, but the shell
would lose its jobs, the log service would write out again every record still
in the kernel ring, and restarting the config service would take the sessions
of the shell and the log service with it.

**Keeping sessions and open files outside the VFS.** A VFS whose state lived
in shared memory could resume open files after a restart. It would make that
memory part of the VFS's trust boundary with every client and is far more than
this release needs.

**Leaving the block service `RestartNever`.** It still is. Its restart would
need the driver and filesystem channels renewed the same way; it holds no
parser of untrusted data, which is what the VFS and FAT32 restarts guard
against first.

## Consequences

The validation build kills the VFS under a running system (`fault vfs`), with
a background job running. The boot test requires the VFS to start again, the
shell, config and log services to take new sessions, the FAT32 service to
restart and be attached, the shell to use the namespace and read the volume,
`/tmp` to be empty, the job to still be listed, and the shell not to have
restarted. The whole restart takes about 20 ms under QEMU TCG.

The supervisor's rate limit applies to the VFS as to every service: three
restarts in ten seconds and it stays down. A restart of the VFS is also a
restart of the FAT32 service and counts toward its limit.

The log service is still restarted without renewing its grants if it crashes
itself, as is the config service; each would be launched without the sessions
its first launch consumed. That gap predates this change and is left for the
services' own restart.
