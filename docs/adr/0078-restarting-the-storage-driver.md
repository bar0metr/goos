# ADR 0078: Restarting the storage driver

## Status

Accepted. Extends ADR 0019, ADR 0034 and ADR 0035.

## Context

The supervision machinery — crash detection, restart policies, rate limiting,
dependency state, diagnostic events — has existed since ADR 0019, and the
storage services did not use any of it. All three were `restart: never`, so a
driver that failed took storage with it until the machine was rebooted.

They were registered that way for a reason. A service's endpoints are channel
halves, and a channel dies with the process holding either half. A restarted
driver cannot be handed the endpoint its predecessor had, and its peer, which is
still running, holds a client side whose other end has gone. Restarting one
service therefore needs the other to be told about a replacement, and nothing
existed to tell it.

Two other things made a crash worse than it had to be. The block service treated
a receive that reported a dead peer as fatal, so a client's death killed the
service every one of its other clients depends on. And a scheduled restart was
only ever acted on when the next lifecycle event arrived: the supervisor waited
on events with no deadline, so a service scheduled to restart and nothing else
happening stayed down forever. That defect was latent because the only service
that had ever restarted was one that crashed repeatedly.

## Decision

**The driver restarts; the services around it stay up.** `virtio-blk` becomes
`restart: on-failure`. The block service and the filesystem service do not
restart, because their clients hold sessions that would have to be rebuilt; the
driver is the leaf, the component that talks to hardware, and the one most
likely to fail.

**A replacement gets a fresh channel and the block service is told.** PID 1
creates a new pair, sends the client side to the block service over a control
endpoint it holds the only client of, and launches the driver with the server
side. Both the launch about to happen and the stored definition are updated, so
the launch after this one does not hand out a handle that is already dead.

**The driver's device authority stays with PID 1.** Ports, interrupt line and
DMA domain are granted rather than consumed, so a replacement can be given the
same authority. The dead process's copies go with its teardown, which already
resets the device and masks its interrupt before reclaiming its DMA pages — a
replacement therefore starts against quiesced hardware.

**The block service installs the endpoint without probing it.** PID 1 hands the
endpoint over before the replacement exists, so a probe would time out against a
process that has not started. The next real request is what discovers whether
the driver works, and reports it the way any other I/O failure is reported.

**A dead peer is not fatal.** A receive or a send that reports the peer gone
drops that request and leaves the service serving its other clients. A service
outlives the processes that use it.

**A driver known to be gone is not waited for.** When the block service does
observe the loss it stops issuing requests until an endpoint is installed, so
each one fails immediately instead of waiting out a deadline against a process
that is not there.

**The supervisor wakes on a restart deadline.** While a restart is outstanding
the supervision wait is bounded, and the expiry starts whatever is ready. A
scheduled restart is a deadline, not an event, and nothing else was going to
arrive to make it happen.

**The fault is injected on every validation boot.** PID 1 keeps the driver's
process handle in that build and kills it once, after the filesystem has mounted
and the system is up. Recovery from a driver crash is a path that is only
exercised when hardware misbehaves, which is to say never, unless something
makes it happen.

## Alternatives

**Restarting the whole storage stack together.** Tearing down the filesystem
service, the block service and the driver and starting all three with fresh
channels avoids the handover entirely. It also takes every VFS session with it —
the shell's, the log service's, every running utility's — so a driver hiccup
would end every open file in the system.

**Letting the block service reconnect by itself.** It could ask PID 1 for a new
driver endpoint when it notices the old one gone, which puts the recovery where
the failure is observed. It also makes the block service a client of process
creation, and the decision about what may hold the device belongs with the
process that owns that authority.

**Keeping the driver's capabilities consumed and re-deriving them from the
kernel.** PID 1 could ask the kernel for fresh port, IRQ and DMA objects per
launch. The kernel would then have to decide who may have them, which is the
ambient-authority arrangement capabilities exist to avoid. PID 1 holding them
and granting a copy is the same shape every other service already uses.

**Polling for restarts on a timer always.** Waking the supervisor periodically
regardless would be simpler than asking whether a restart is pending. It also
wakes PID 1 forever on a system where nothing has failed, which is the cost the
event-driven waits of ADR 0051 were introduced to remove.

## Consequences

The boot test kills the driver on every validation boot and requires the driver
to start twice, the block service to be told about the replacement, and the
block service itself not to restart. The storage checks that follow exercise the
replacement, and the persistence phases read back what was written through it.

Whether the block service observes the loss depends on whether a request was in
flight when the driver died, so the replacement is asserted and the loss is not.
The replacement is what recovery means.

The supervisor now wakes on a pending restart. That fixes a defect wider than
storage: any `restart: on-failure` service whose crash was the last event would
have stayed down, including the log and configuration services added since.

Injecting the fault found a second latent defect. The log service created its
file only at startup, so storage that was unavailable at that moment left it
never writing again. It now creates the file on the pass that has something to
write, which also means a volume that arrives late, or a file removed from under
it, is recovered from without a restart.

The block service and the filesystem service still do not restart. Their clients
hold sessions rather than a single endpoint, and reconnecting those needs each
client to be told, which is a larger change than the leaf of the chain needed.
