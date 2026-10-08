# ADR 0195: Generic IPv4 UDP

## Status

Accepted. Builds on ADR 0191, ADR 0192, ADR 0193 and ADR 0194. Replaces the
resolver transport of ADR 0194.

## Context

ADR 0194 gave the resolver a private transport, `GNETDGRAM1`: one query to
port 53 from a fresh port, one reply, then the exchange ends. Nothing else
could send or receive UDP. No program could bind a port, keep an endpoint,
talk to a peer over `lo`, or receive a datagram it had not asked for. The
stack already parsed and built UDP, but only DNS exchanges and DHCP used it.

A real transport has to serve unrelated programs side by side, keep the
resolver's anti-spoofing properties, stay out of the kernel, and keep the
stack's fixed-size, allocation-free tables. It also has to be a backend a
later Linux socket layer can use without a second UDP implementation.

## Decision

### Endpoints in the stack

`userspace/network` gains UDP endpoints (`udp.go`, `udp_send.go`). An
endpoint has a local address (the wildcard, the interface's address or a
`127.0.0.0/8` host), a local port, an optional connected peer, and a queue
of received datagrams. It moves from open to bound to connected. The kernel
learns nothing about UDP.

Every endpoint has an owner, the index of the client session `netd` serves
it on, and every call checks the owner. An endpoint ID is a generation above
the table slot: closing an endpoint and opening another in its slot gives
the new one a new ID, so an old ID matches nothing.

Bind takes port 0 for an ephemeral port. A port conflicts with an endpoint
on the same port whose address equals it or where either address is the
wildcard; a conflict is `AddressInUse`. Port 68 belongs to the DHCP client
while the interface runs DHCP. There is no privileged-port rule: the UDP
session is the authority.

Ephemeral ports come from the stack's generator within the tuned range,
49152..65535 by default (RFC 6335). Four random draws are followed by a walk
from the last one. At most 32 ports are bound, so the walk ends within 33
steps whenever the range is larger than that.

`Connect` stores the peer and sends nothing. An endpoint without a port gets
an ephemeral one and the local address the peer is reached from at that
moment. An explicit bind stays as it was: an endpoint bound to the wildcard
remains on the wildcard after `Connect`, picks its source address at each
send, and so keeps working when `net0` changes address. A connected
endpoint sends only to its peer and keeps only datagrams from that address
and port. `SendTo` on it is `InvalidArgument`; so is a second
`Connect`. Close and reopen to change peers.

### Sending

A send checks its arguments, then the binding, then the interface. A
destination in `127.0.0.0/8` or equal to the interface's address goes
straight to the endpoint table: no ARP, no frame, no driver, and `lo` needs
no device. Anything else takes the usual route and ARP, with a checksum over
the pseudo-header (0xffff when the sum is zero). A datagram whose next hop
is not yet resolved waits in an 8-entry table and is framed when ARP
answers, or dropped and counted when it gives up. Every check that can
refuse a datagram runs before an unbound endpoint gets its ephemeral port,
so a refused send changes nothing.

Broadcast, multicast, a subnet's network or broadcast address and port 0
are refused. There is no fragmentation: an external payload is at most the
MTU less 28 bytes, 1472 on a 1500-byte link, and one byte more is
`InvalidArgument`. Loopback and own-address payloads may reach
`max-payload`. Zero-length datagrams are valid both ways. Success means the
stack took the datagram, not that anyone received it.

### Receiving

A datagram from the link is checked first: a UDP length inside the IPv4
payload, padding ignored, and a valid checksum unless the checksum field is
zero. A bad checksum is dropped and counted before anything else sees it.
DHCP's server-to-client datagrams go to the DHCP client while the interface
runs DHCP. Only a datagram addressed to the interface's own address reaches
an endpoint; the broadcasts and offers DHCP admits never do.

The endpoint bound to the destination address and port takes the datagram,
or else the one bound to the wildcard on that port; the bind rules leave at
most one of each. A connected endpoint drops a datagram from anyone but its
peer. Payloads wait in 32 receive buffers of 4096 bytes shared by all
endpoints, of which one session may hold 16. A full queue, a session at its
share or an empty pool drops the newcomer and keeps what is queued. There is no ICMP Port Unreachable.

### Limits and tuning

The compile-time constants are the capacities: 8 sessions, 32 endpoints, 8
per session, queues of 16, payloads of 4096 bytes, 32 receive buffers (16
per session) and 8 pending sends. `system.net.udp.*` sets how much of each one is in use:

| Key | Default | Range |
|---|---|---|
| `ephemeral-port-min` | 49152 | 1024..65535 |
| `ephemeral-port-max` | 65535 | 1024..65535, at least `ephemeral-port-min` |
| `recv-queue-depth` | 8 | 1..16 |
| `max-endpoints` | 32 | 1..32 |
| `max-endpoints-per-session` | 8 | 1..8, at most `max-endpoints` |
| `max-payload` | 4096 | 1232..4096 |

The values are a block of `network.Tuning`, read with the other
`system.net.*` keys in `netd`'s one bounded configuration read and applied
by the same `Apply`: all of the configuration and tuning, or none of it. The
1024 floor keeps the well-known ports, DHCP's 67 and 68 among them, out of
automatic allocation. The 1232 floor is the EDNS(0) payload size the
resolver advertises, so no setting lets the stack drop an answer the
resolver asked for; a test in `userspace/dns` holds the two together.

A new limit applies to the next acquisition and takes nothing back. Open
endpoints stay open with their ports, queued datagrams stay queued and can
still be received, and a lowered limit refuses the next `Open`, the next
queued datagram or the next payload over it. Explicit binds may use any
port; the ephemeral range only steers allocation.

Correctness has no switch: checksums both ways, length checks, the bind and
delivery rules, loopback isolation, zero-length datagrams, ownership of
endpoints, slots and addresses, and stale-ID protection hold whatever the
settings say. A later transport adds its own block, `system.net.tcp.*`, the
same way.

### GUDP1

Programs reach UDP through `GUDP1` (`userspace/udpapi`) over a session PID 1
opens for them. A session is one channel and two pools: a TX pool of four
4096-byte slots the client writes and `netd` maps read-only, and an RX pool
of eight the reverse. Requests are `Open`, `Bind`, `Connect`, `Send`,
`SendTo`, `RecvFrom` and `Close`, each answered once under its correlation;
`Release` is a notification. `Send` and `SendTo` hand a TX slot over and the
reply hands it back; a `RecvFrom` reply with OK hands an RX slot over and
`Release` hands it back. `RecvFrom` waits in `netd` until a datagram arrives
or the endpoint closes, one per endpoint; `Close` answers a waiting one with
`Closed` first. Results are `OK`, `InvalidArgument`, `Busy`, `Down`,
`NoAddress`, `NoRoute`, `NeighborUnreachable`, `Reconfigured`,
`AddressInUse`, `DriverFault` and `Closed`.

The codec refuses an unknown opcode or version, a wrong size, nonzero
reserved bytes, a field its opcode does not use, a slot outside the pool and
any attached handle; the decoder is fuzzed. `netd` closes a session that
sends a malformed request or releases a slot it does not hold. An unknown or
stale endpoint ID is answered `InvalidArgument`.

### Authority

`ControlAddUDPSession` (rights class `RightsUDP`) hands `netd` the session's
channel and pools. A command asks for `GrantUDP`; the grant is separate from
ping, admin and resolve, so none of those carries the right to send
datagrams. Only PID 1 opens sessions.

### The resolver

`resolved` uses `GUDP1` like any program. PID 1 opens a UDP session and a
network status session (`RightsResolveStatus`) from `netd` and hands both to
the resolver with `ControlSetNetwork`, under a new generation, whenever
either service starts. Each query gets its own endpoint: `Open`, `Connect` to
the server's port 53, which binds a random ephemeral port, `Send` and
`RecvFrom`. The connected endpoint drops anything but the server's address
and port 53, the resolver checks the source again along with the
transaction ID and question, and the endpoint closes once its transaction
stops waiting, so a late answer finds no endpoint. Each retransmission opens
a new endpoint on a new port. The per-server transmission budget, the
absolute deadline, EDNS fallback and the cache work as ADR 0194 describes;
DNS keeps its own 1232-byte limit. An answer with TC=1 closes the UDP
endpoint and sends the same query over a TCP session that PID 1 hands over
with `ControlSetTCP` right after this pair (ADR 0196).

The DHCP lease's servers reach the resolver as a `GNETAPI` `Servers`
request: it carries the generation the resolver has, and `netd` holds it
until the list changes.

`GNETDGRAM1`, its pools, `netd`'s resolver transport, the datagram exchange
table and `ControlSetTransport` are gone. `GNETCTL` is version 2.

### Status

`GNETAPI` is version 3: the status reply adds the UDP counters, the
endpoints open and the limits in force, and the request carries the
generation for `Servers`. `net status` prints them under `udp:`. Counters
saturate.

### Testing

The services' GUDP1 logic lives in two packages so host tests run it:
`userspace/udpserve` is `netd`'s side of a session, with the waiting
`RecvFrom`s and the RX slots the client holds, and `userspace/upstream` is
the resolver's per-query state. `netd` and `resolved` keep only handles,
mappings and the channel. The `udpserve` tests hold a `RecvFrom` with
nothing queued and complete exactly that request when a datagram arrives,
answer a waiting `RecvFrom` with `Closed` before the `Close` reply, end a
session with no receive, slot or endpoint left behind, wait for a free RX
slot, and refuse a request for a closed endpoint whose table slot a new
endpoint took. The `upstream` tests join a real stack, the resolver core,
the client and two server sessions through the GUDP1 codec: a query waits
in a deferred `RecvFrom` until the answer comes, a retransmission's new
port leaves the old query's answer with no endpoint, a mismatched answer
re-arms the receive, and a lost session cancels the query.

`udp send`, `udp listen` and `udp self` use nothing but a UDP session. The
network gate drives them against `tools/netpeer`, which now echoes UDP on
port 7 (with a decoy from port 8 on request) and sends plain, numbered,
sized and corrupt datagrams on command. It checks peer metadata, two ports
at once, a connected endpoint ignoring the decoy, queue order, `lo` with and
without a network device, own-address delivery, zero-length and 1472-byte
datagrams, the 1473-byte refusal, a corrupt checksum, an unbound port, DNS,
the EDNS fallback, a fresh source port per query, a DHCP renewal on a
40-second lease and a UDP tuning reload. Two checks take the deferred path
through the real services on purpose: the peer sends to a `udp listen` a
second after its receive went out, and it holds the answer for one name a
second, inside the resolver's first wait, so `resolved` takes it from a
waiting `RecvFrom` after exactly one query. The VM gate stays the
authority for `netd` and `resolved` themselves; the host tests cover the
logic they call.

## Consequences

Any program PID 1 grants `GrantUDP` can bind, connect, send and receive UDP
over IPv4, and the resolver is one such program. Loopback UDP works with no
network device.

Still missing: TCP, IPv6, IPv4 fragmentation and reassembly, multicast and
broadcast sends, `SO_REUSEADDR` and `SO_REUSEPORT`, ICMP errors to senders
(an unbound port gets no ICMP Port Unreachable), path MTU discovery, and the
Linux socket calls. The endpoint model is meant
to back a Linux UDP socket layer later without a second stack.

`netd` now serves datagrams for programs that are not the resolver. A
client, or a remote host flooding its ports, can fill that client's queues
and its half of the receive buffers, which leaves the other half to every
other session, the resolver's among them.
