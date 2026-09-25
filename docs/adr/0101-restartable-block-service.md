# ADR 0101: A block service that restarts

## Status

Accepted. Amends ADR 0033, ADR 0078, ADR 0096 and ADR 0099.

## Context

After ADR 0099 and ADR 0100 the block service was the one system service
registered `RestartNever`. A crash in it left the persistent volume without a
device until the next boot, and its `fatal` spun rather than exiting.

It sits between two services that already restart. The driver below it
restarts on failure and hands its new endpoint over the block service's control
channel (ADR 0078); the FAT32 service above it does the same with its own
(ADR 0096). What neither did was react to the block service going away. The
driver treated a dead service channel as a fatal error and spun, holding the
device. The FAT32 service waited only on its VFS channel, so while idle it
would not notice, and its next request would fail.

The block service also could not start without a driver: it asked the driver
for the device's geometry before serving anything, and a restarted instance
starts before the new driver does.

## Decision

**The block service restarts on failure, and fails by exiting.** It is
registered `RestartOnFailure`, and `fatal` exits with code 1.

**It starts without a driver or a filesystem endpoint.** Both grants are
optional. At boot they are there and it describes the device at once, as
before. A restarted instance is launched with a new control channel and a new
public endpoint only; PID 1 drops the two consumed grants from its definition.
It asks for the device's geometry when the first request arrives after a
driver has been handed over, and answers I/O errors until then.

**The driver and the filesystem service follow it.** The driver exits with
code 2 when its service channel reports the block service gone, and its
teardown resets the device as after any driver exit. The FAT32 service now
waits on its block endpoint as well as its VFS endpoint and exits with code 3
when the block endpoint closes. Both are restarted by the supervisor, and each
hands the new block service its endpoint over the control channel exactly as
after a restart of its own.

**Dependencies order the restart.** The driver depends on the block service,
and the FAT32 service depends on the driver as well as the block service and
the VFS. The block service starts first, the driver next, and the FAT32
service last, so its mount finds a driver that can describe the device.

`fatal` in the driver exits with code 1 instead of spinning, for the same
reason as everywhere else: a process that spins cannot be replaced.

## Alternatives

**Handing the running driver a new channel.** The driver would need a control
channel of its own, and would keep a device it had been driving for a block
service that no longer exists, with requests in flight to nobody. Restarting it
resets the device and is the path its own failures already take.

**Waiting in the block service for a driver before serving.** It would hold
every client until PID 1 hands a driver over, and PID 1 may be waiting on one
of those clients.

## Consequences

Every system service now restarts on failure. The validation build kills the
block service under a running system; the boot test requires it to start again,
a new driver and a new FAT32 service to be handed to it, the volume to be
mounted again, a file on it to be read, and a new file to be written and read
back. The restart took 15 ms under QEMU TCG.

A write in flight when the block service dies fails, and may be partly applied
on the device, as a write in flight when the FAT32 service dies may be
(ADR 0096). The restart counts toward the driver's and the FAT32 service's
restart limits as well as the block service's own.
