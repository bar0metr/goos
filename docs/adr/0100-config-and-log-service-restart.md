# ADR 0100: The configuration and log services restart on their own

## Status

Accepted. Extends ADR 0099. The block service, the one left that did not
restart, does since ADR 0101.

## Context

The configuration and log services were registered `RestartOnFailure`, but
neither could actually be restarted. Their first launch consumes the sessions
PID 1 opens for them: a filesystem session for both, a control endpoint for
the configuration service, a configuration session and, since ADR 0099, a
control channel for the log service. A restart was launched with the same,
now dead, handles, the kernel refused it, and after three attempts the
supervisor gave up. ADR 0099 made both survive a restart of the VFS without
restarting them; a crash of either one still left it down.

The configuration service also serves sessions of its own: the shell's and
the log service's. They die with it.

The log service had one more problem to solve. A new instance starts at the
oldest record the kernel ring still holds, and most of those its predecessor
already wrote to the file.

## Decision

**PID 1 renews what a restart needs.** Before the configuration service is
launched again, PID 1 opens a new filesystem session and a new control
channel, keeping the client end, over which it opens every configuration
session from then on. Before the log service is launched again, it opens a new
filesystem session, a new configuration session confined to `system.log.`,
read-only, and a new control channel. A configuration session that cannot be
opened is dropped from the grant, as the shell's is (ADR 0089).

**The configuration service's clients are handed new sessions.** Once a
restarted configuration service runs, PID 1 opens a session for the shell,
unconfined and writable as at boot, and one for the log service, and sends
each as a session notice, a new message of the configuration control protocol
(version 2), over the channel that client already holds with PID 1: the
shell's exec channel and the log service's control channel. Neither client
restarts. The service restores its settings from `/state/config` as at boot;
a change it had not persisted when it died is lost.

**A restarted service is told it was restarted.** PID 1 launches a service
that has been restarted in this boot with `RESTART=1` in its environment. The
log service, when it sees it, reads the last record of `/state/system.log`, or
of the rotated file when the current one is empty, and starts after the last
ring record not newer than it. Without the variable the file's last record may
be an earlier boot's, whose time says nothing about this one, and the service
starts at the oldest record as before.

## Alternatives

**Keeping the log cursor somewhere that survives the process.** A cursor in a
file or in PID 1 would need writing on every pass; the file already ends with
the answer.

**Comparing sequence numbers instead of times.** The file holds the records
as text, without the ring's sequence numbers. Two records in the same
microsecond, one written and one not, would both be skipped; it has not been
seen and would lose one record, not write one twice.

**Restarting the shell and the log service with the configuration service.**
The shell would lose its jobs, as ADR 0099 describes.

## Consequences

The validation build kills the configuration service and then the log service
under a running system. The boot test requires each to start again, the shell
and the log service to take new configuration sessions, the shell to read a
setting back and write one through its new session, and the log service to
resume after its predecessor's last record. In a run by hand, the new log
service skipped the 41 records already in the file and wrote none twice.

The block service is now the only system service that does not restart.
