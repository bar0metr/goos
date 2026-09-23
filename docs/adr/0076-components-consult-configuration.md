# ADR 0076: Components consult configuration

## Status

Accepted. Extends ADR 0074.

## Context

ADR 0074 built the configuration service and said plainly what it did not do:
nothing read it. The schema declared settings that described the system, and
every component that owned one still used a compiled-in constant. A store with
no consumers is a store, not configuration.

It also deferred the control endpoint. One session existed, granted to the shell
with the whole namespace and both rights, so the scope and rights a session
carries were enforced by the code and exercised by nothing. A capability check
with one caller that always passes is a claim, not a property.

## Decision

**Sessions are created through a control endpoint.** PID 1 holds the only client
side of it, so it is the only process that can create a configuration session,
and the scope and rights travel in the request. A client receives an endpoint
that is already limited and has no mechanism to ask for more. This is the same
shape the VFS control endpoint has, for the same reason.

**Different clients get different sessions.** The shell administers
configuration, so its session is the whole namespace with read and write. The
log service gets a read-only session confined to `system.log.`: it consults the
one setting that governs it, cannot change it, and cannot see anything else.
The difference is now real rather than available.

**The shell reads its prompt.** It asks at startup and again after a successful
write, so a change applies at once rather than at the next boot. The shell owns
that setting, and the component that owns a setting is the one that should
notice it changing.

**The log service reads its setting on every pass.** Persistence can be turned
off and on while the system runs. The cursor still advances while it is off, so
re-enabling starts from the present rather than writing out a backlog the ring
has mostly lost anyway.

**A component that cannot reach the service keeps its default.** A missing
session, an unreachable service, or an answer of the wrong type leaves the
built-in value in place. Configuration decides what a component does, not
whether it starts: the shell must come up to fix a configuration that stops
things coming up.

**A setting nothing reads is not declared.** `system.storage.volume` described a
mount the filesystem service performs before configuration exists, so nothing
could consult it. It is removed rather than left as a value that looks like it
controls something.

## Alternatives

**Reading configuration once at startup everywhere.** Simpler, and it needs no
re-read path. It also means every change waits for a reboot, which for the
prompt is absurd and for a service setting makes the store a boot-time file with
extra steps.

**Reading on every use.** The shell could ask for the prompt before printing
each one, which is always current and costs a round trip per prompt. At about
0.15 ms that is affordable, but it puts an IPC dependency on the interactive
path for a value that changes when the shell itself changes it.

**Pushing changes to interested clients.** The service could notify sessions
that a key they care about moved, which removes both the re-read and the poll.
It needs a subscription model, a delivery guarantee for clients that are not
reading, and a story for a slow client. Two consumers do not justify it.

**One session for everything.** Keeping the single full-authority session and
sharing it would have avoided the control endpoint entirely. It also hands the
log service the ability to rewrite the shell's prompt, which is exactly what
scoped sessions exist to prevent.

## Consequences

`config set system.shell.prompt "$ "` changes the prompt on the next line.
`config set system.log.persist false` stops the log service writing at its next
pass, and it says so. Both survive a reboot, because the service persists them.

The boot test asserts the prompt applies live and is restored, and that two
sessions were created with different rights. The confinement itself is covered
by the store's unit tests and the control protocol's round trip: proving from
inside the system that the log service cannot read `user.name` would need a
client built to try, which is a test fixture rather than a system component.

The configuration service now serves up to six sessions and drops one whose peer
has gone, the same way the VFS service does.

PID 1 creates one channel per configuration session. With the shell and the log
service that is two more long-lived channels, taking its steady-state count to
ten of the sixteen its quota allows.

Two consumers is not many. The remaining settings — the published build
identity, the user name — are either read-only facts or values nothing needs to
act on, and the next component to acquire a tunable now has somewhere to put it.
