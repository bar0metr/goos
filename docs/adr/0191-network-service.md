# ADR 0191: A userspace network service

## Status

Accepted. Builds on ADR 0190. Extends the command index of ADR 0080 and
ADR 0175 to format 3 and declares the first network settings in the
configuration service of ADR 0074.

Supersession note (2026-10-07): the 256-entry virtqueue ceiling in "The
driver" is superseded by the current 1024-entry ceiling. Proxmox VE 7.4 with
QEMU 7.2 exposes 1024-entry virtio-net queues; their rings and the fixed frame
buffers still fit within the unchanged 64-page DMA limit. The rest of this
decision remains in force.

Supersession note (2026-10-07): ADR 0192 makes the stack's timers, TTL and
echo reply rate configurable, adds tuning keys to the reload `Field` values,
and gives `Status` replies the link MTU at bytes 76..77 and a fallback flag
at byte 78. The fixed values below are now defaults.

Supersession note (2026-10-07): ADR 0193 adds the loopback interface and the
hosts table, and ADR 0194 the resolver service, its transport and host names
in `ping`; "Out of scope" below no longer holds for DNS or for `ping` taking
names. The network protocol is at version 2 (ADR 0192), and every address
claim follows RFC 5227. General UDP and TCP sockets for applications remain
out of scope: the resolver transport carries DNS to port 53 for the resolver
alone.

## Context

GoOS had no network. ADR 0190 gives a driver bounded authority over one
legacy virtio-net function and a way to wait on its line and a channel
together. Everything above the device was still missing: frame handling,
address configuration, ICMP, and a way for commands to use them.

The kernel was not to learn any of it. A protocol parser in ring 0 would turn
every malformed frame on the wire into a kernel attack, and the existing
storage stack already shows the shape that works here: a driver holds the
device, a service holds the protocol, and PID 1 brokers every endpoint
between them. Boot must not depend on the network: a machine without a NIC,
or with one that fails, keeps its storage, installed system and shell.

## Decision

### Two processes

The network runs as two supervised services from `/system/services`:

- `virtio-net`, the driver, owns the device: its ports, its line and its DMA
  window. It copies frames between the device's buffers and two shared
  frame pools, and it holds no VFS, configuration or client session.
- `netd`, the network service, runs the stack in `userspace/network`. It
  holds no port, IRQ or DMA capability. It reads frames from one pool and
  writes frames into the other.

Commands reach `netd` over private sessions PID 1 opens for them. They never
build packets or touch the device.

The driver gets seven grants: `console` (Write), `ports` (Read + Write),
`irq` (Wait), `dma` (Write), `service` (Read + Write + Wait), `rx`
(Read + Write) and `tx` (Read). `netd` starts with `console` (Write),
`control` (Read + Write + Wait) and, when the configuration service gave
PID 1 a session, `config`: a channel with Read + Write + Wait whose
configuration session is scoped to `system.net.` and read-only. PID 1 later
moves `netd`'s end of the driver channel and narrowed copies of the two
pools to it in `SetDevice`. No child receives Transfer.

### Frame pools

For each driver launch PID 1 creates two memory objects of two pages each,
four slots of 2048 bytes:

| Pool | Driver | `netd` |
| --- | --- | --- |
| receive | Read + Write | Read |
| transmit | Read | Read + Write |

The pools are ordinary shared memory, not DMA memory. The driver keeps its
own DMA window with the virtqueues, 16 posted receive buffers and four
transmit buffers, and never puts a pool address in a descriptor. It copies a
received frame from its buffer into a free receive slot, and copies a frame
out of a transmit slot into its own buffer before the device sees it, so a
later write by `netd` cannot change a buffer the device is reading.

A slot has one owner at a time. `netdev.Pool` keeps each side's view: a slot
is free, in use, or held by the peer. Only the message that hands a slot over
moves it to the peer, and only the message that returns it moves it back, so
a duplicate or forged message is a failed transition. A failed send leaves
the slot with the sender.

### GNETDRV1

The driver and `netd` speak `GNETDRV1` version 1 over GIPC. Every body is 32
bytes, little-endian, with no attached handles:

| Bytes | Field |
| --- | --- |
| 0 | opcode |
| 1..7 | zero |
| 8..15 | generation, nonzero |
| 16..17 | slot |
| 18..19 | frame length |
| 20..21 | result (`TxDone`) or fault reason (`Fault`) |
| 22..23 | MTU |
| 24..29 | MAC |
| 30 | link: down 0, up 1 |
| 31 | zero |

| Opcode | Direction | Kind | Fields |
| --- | --- | --- | --- |
| `Ready` 1 | driver to `netd` | notification | MTU 1500, a unicast MAC, link |
| `RxReady` 2 | driver to `netd` | notification | receive slot, length 14..1514 |
| `RxRelease` 3 | `netd` to driver | notification | receive slot |
| `Tx` 4 | `netd` to driver | request | transmit slot, length 14..1514 |
| `TxDone` 5 | driver to `netd` | reply to `Tx` | transmit slot, result |
| `Fault` 6 | either | notification | reason |

Results are `OK` 0, `Busy` 1, `InvalidFrame` 2 and `DeviceFault` 3. Fault
reasons are device 1, protocol 2, generation 3 and resources 4. A field an
opcode does not use must be zero, and the codec refuses any other value, a
wrong kind, a missing or extra correlation, and an unknown opcode. `Tx` and
`TxDone` pair through the channel's request tracking, which refuses a second
completion for one request. Slot numbers and lengths are the only references
to memory any message carries.

### The driver

The driver follows the legacy initialization order: reset, acknowledge,
driver, features, queues, receive buffers posted, `DRIVER_OK`, and only then
the first notification. It requires the MAC feature, accepts link status when
the device offers it, and negotiates nothing else, so frames carry the
10-byte legacy header and no offloads. It refuses a MAC that is zero or
multicast and a queue size that is not a power of two from 2 to 256.
Receive is queue 0 and transmit queue 1. Its DMA window holds both queues and
all buffers and must fit the 64-page limit.

It waits on its channel and the line in one `WaitAny`. On an interrupt it
reads the ISR and then calls `IRQTake`. It hands each completed receive
buffer to `netd` in a free slot; with no slot free it leaves the remaining
completions in the used ring, where the device runs out of buffers and drops
for itself. A link change sends `Ready` again. A malformed completion, a
message it cannot decode, or a generation that is not its own makes it send
`Fault`, reset the device and exit. It exits with code 2 when `netd` is gone.
Its wait has a 10-second timeout, after which it waits again without touching
the device.

### The stack

`userspace/network` is pure Go with no device and no clock: `netd` passes in
frames, monotonic time and requests and takes out frames and echo
completions. It handles untagged Ethernet II, ARP, IPv4 without options or
fragments, ICMP echo, and UDP only for its DHCP client. Every table and the
transmit ring are fixed arrays. The exact rules and limits are in
[networking.md](../architecture/networking.md); the ones that shape the
interface are:

- an echo request reaches the wire only from `netd`, which builds the ICMP
  packet, picks the identifier from a per-boot random base plus the session
  number, and fills the 32-byte payload with an 8-byte random token, the
  session and sequence, and a fixed pattern;
- a reply completes an echo only when its remote address, local address,
  identifier, sequence and whole payload match; an ICMP destination
  unreachable or time exceeded completes one only when the quoted header,
  identifier and sequence match;
- `netd` answers echo requests sent to its unicast address, at most 20 a
  second, and never to broadcast;
- before any address goes live, from DHCP or static configuration, the stack
  sends two ARP probes 500 ms apart and waits 500 ms more. A DHCP conflict
  sends `DECLINE` and starts over after 10 seconds; a static conflict leaves
  the interface failed until the next reload or link-up.

### GNETCTL1

`netd` serves `GNETCTL1` version 1 on its `control` channel, whose other end
only PID 1 holds. Requests and replies have a 16-byte body: opcode at byte 0,
the rights class in a request or the result in a reply at byte 1, zero bytes
2..7, and a generation at 8..15.

| Opcode | Attachments | Rights class | Generation |
| --- | --- | --- | --- |
| `AddSession` 1 | one channel | `PingStatus` 1 or `AdminStatus` 2 | zero |
| `SetDevice` 2 | driver channel, receive pool, transmit pool | zero | nonzero |

Results are `OK` 0, `InvalidArgument` 1, `Busy` 2 and `DriverFault` 3. A
reply echoes the opcode and generation and carries the request's
correlation. `netd` asks the kernel with `HandleInfo` for each attached
handle's type and rights and requires the exact rights above: a receive pool
it could write would let it change frames the driver is reading. It closes
every attachment of a request it refuses. `SetDevice` needs a generation
greater than any it has accepted. `AddSession` past eight sessions is
`Busy`.

The same channel carries one more message: after the configuration service
restarts, PID 1 sends `netd` a renewed configuration session as a `GCF1`
session notice, and `netd` replaces the one it held.

### GNETAPI1

A command and `netd` speak `GNETAPI1` version 1 on the session channel. A
request has a 16-byte body: opcode at byte 0, zero bytes 1..3, the IPv4
destination at 4..7, a sequence at 8..9, a timeout in milliseconds at
10..11, and zero bytes 12..15. `Status` and `Reload` require every field
after the opcode to be zero; `Echo` requires a timeout of 100..5000 ms.

A reply has an 80-byte body: opcode, result, interface state and mode at
bytes 0..3; local address at 4..7; gateway at 8..11; responder at 12..15;
round-trip time in nanoseconds at 16..23; MAC at 24..29; prefix and link at
30 and 31; lease seconds remaining at 32..39; sequence at 40..41; the
invalid setting at 42..43; receive, transmit, drop and malformed counts at
44..75; and zero bytes 76..79. Which fields an opcode may set is fixed, and
every other field is zero:

| Request | Who may ask | Reply |
| --- | --- | --- |
| `Status` 1 | any session | state, mode, address, prefix, gateway, MAC, link, lease, counters |
| `Echo` 2 | `PingStatus` | sequence; the responder and RTT on `OK`; the responder on `RemoteUnreachable` and `TimeExceeded` |
| `Reload` 3 | `AdminStatus` | `OK`, or `ConfigInvalid` with the setting that failed |

Results are `OK` 0, `Down` 1, `NoAddress` 2, `NoRoute` 3,
`NeighborUnreachable` 4, `Timeout` 5, `Busy` 6, `Reconfigured` 7,
`PermissionDenied` 8, `InvalidArgument` 9, `DriverFault` 10,
`ConfigInvalid` 11, `RemoteUnreachable` 12 and `TimeExceeded` 13. Interface
states are down 0, configuring 1, ready 2 and failed 3; modes are DHCP 0
and static 1. A session has at most one echo in flight; a second is `Busy`.
`netd` answers every request it accepted exactly once while it lives,
including a timeout, a dead driver and a reconfiguration. It answers an
unparsable request it can attribute with `InvalidArgument`.

### Sessions and grants

A command declares `GrantNetworkPing` (`0x10`) or `GrantNetworkAdmin`
(`0x20`) in its manifest. Because the bits change what an index entry
means, the command index format is now 3; an installed system built with
format 2 is refused and boots into recovery until `make disk-overwrite`
installs it again. PID 1 grants a session only from the index of the
command's own origin, as for every other grant. For each launch it creates a
channel, sends one end to `netd` with `AddSession` and the matching rights
class, and consumes the other into the command's `net` role. A command that
declares both gets the ping class, the narrower one. `netd` decides what a
session may do from its own record, never from the request. When a command
exits, `netd` sees its peer die, forgets the session's echo and frees the
slot. A command that declared a network grant and received no session, for
lack of a service, fails with `network service unavailable`.

The recovery index lists no network command.

### Configuration

The configuration service declares four keys:

| Key | Type | Default |
| --- | --- | --- |
| `system.net.mode` | text | `dhcp` |
| `system.net.ipv4.address` | text | `0.0.0.0` |
| `system.net.ipv4.prefix` | uint | `24` |
| `system.net.ipv4.gateway` | text | `0.0.0.0` |

The store checks types; `netd` checks meaning. It reads all four at start
and at each `Reload` and parses them as one candidate. DHCP mode ignores the
other three. Static mode requires a unicast host address, a prefix from 1 to
30, and a gateway of zero or a host address inside that prefix other than
the address itself. An invalid candidate changes nothing and names the first
key that failed. A valid one applies in one step: a new mode or address
clears the ARP table and fails pending echoes with `Reconfigured`, and a new
prefix or gateway on the same address applies in place. Settings that are
invalid at start leave `netd` on DHCP. There is no transaction across
several `config set` calls, so a person sets every key and then runs
`net reload`.

### Restarts and generations

PID 1 starts `netd` in normal mode after the configuration and log
services, and registers the driver only when its port slot holds a live
device (ADR 0190). Nothing in the boot waits for either. Both restart on
failure; the driver depends on `netd` and follows it.

Every driver launch, the first included, gets a binding of its own. PID 1
increments the generation and refuses to wrap it, creates a channel and two
pools unless the last ones were never used, duplicates the pools with the
rights `netd` needs, sends `SetDevice`, and waits for `OK` before it launches
the driver with the other ends and `NET_GENERATION` in its environment. The
driver stamps every message with it. `netd` refuses a message of any other
generation with `Fault`.

When the driver dies or reports a fault, `netd` closes the binding's channel
and pool handles and mappings, discards queued frames, and completes every
pending echo with `DriverFault` at once. The interface reads down until the
next driver's `Ready`. When `netd` restarts, PID 1 gives it a new control
channel and configuration session. The driver, following its dependency,
restarts behind it with a new binding, and command sessions with the dead
service see `PeerDied`.

Recovery mode runs no network: entering it stops both services. When a
storage service fails, PID 1 holds both, since both run from the volume, and
releases them with the session services once storage is back. A network
service that uses up its restarts stays failed and never sends the system
into recovery. The installed-system check requires both images in
`/system/services`.

The validation build's `fault net-driver` kills the driver once, and PID 1
replaces it with a new binding. `netd` prints `NETD_READY` and its address
when the interface first becomes ready.

### Out of scope

DNS, TCP, UDP sockets for applications, IPv6, multicast, VLANs, forwarding
and NAT, several interfaces, and any NIC but legacy virtio-net. `ping` takes
a numeric address only. The Linux network calls, `socket` among them, keep
returning `ENOSYS`; the compatibility layer is unchanged.

## Consequences

`make test-network` runs three scenarios, each from its own disposable disk.
On QEMU user networking DHCP reaches ready on `10.0.2.x/24` with gateway
`10.0.2.2` within 15 seconds of the prompt, `net status` reports it, and
four rounds of `ping -c 4 -W 1000 10.0.2.2` print four replies each with no
loss. The driver is killed twice between rounds; the disk and the shell keep
working, and the resource counts `quota` reports for PID 1, `netd` and the
driver match exactly across a round of pings and across the second restart.
Two guests on a QEMU `stream` socket with static `192.0.2.10/24` and
`192.0.2.11/24` and no gateway each get four replies from the other, which
proves inbound echo handling without host ICMP privileges. A guest without a
NIC reaches normal mode, `net status` reports `down`, and `ping` exits 1 in
its deadline.

Host tests cover the three codecs, slot ownership, virtqueue bookkeeping and
malformed completions, Ethernet, ARP, IPv4, ICMP, UDP and DHCP parsing,
lease transitions, static validation, deadlines and client authorization.
Fuzz targets exist for the stack's frame receive path, the DHCP and IPv4
parsers, the driver codec and used ring, and the client and control codecs.

`ping` to an address beyond `10.0.2.2` under QEMU user networking depends on
the host's ICMP support and permissions, and its failure there says nothing
about GoOS.

The driver joins the block driver in the effective trusted computing base
until an IOMMU confines DMA (ADR 0190). `netd` parses untrusted frames, so a
defect in it is reachable from the network; it holds the frame pools, its
control channel, a read-only configuration session and the command
sessions, and nothing else. `test-network` is part of `test-all` and the
release gate.
