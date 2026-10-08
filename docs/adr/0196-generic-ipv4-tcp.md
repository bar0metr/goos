# ADR 0196: Generic IPv4 TCP

## Status

Accepted. Builds on ADR 0191, ADR 0192, ADR 0193 and ADR 0195.

## Context

GoOS had IPv4 routing, ARP, loopback and generic UDP, but no reusable stream
transport. A native client could not connect to a TCP service, and a native
server could not bind, listen or accept. Putting TCP in a command or in the
kernel would duplicate routing and weaken the existing capability boundary.

## Decision

### Transport state

TCP lives once in `userspace/network`. The kernel has no socket or TCP state.
The stack owns a fixed table of 32 generation-tagged endpoints. At most eight
belong to one session. An endpoint moves through open, bound, listen, handshake,
established, close and TIME_WAIT states. Exact four-tuples select connections;
an exact listener wins over a wildcard listener.

Bind accepts port zero for an ephemeral port. Wildcard and specific binds on
the same port conflict when either address is the wildcard. A listener has a
bounded backlog of established and embryonic children. Accept returns each
child once.

Each connection has 16 KiB send and receive arrays and eight fixed reassembly
slots. No packet or connection allocates memory in response to remote traffic.
Runtime limits can reduce the usable table, backlog and buffers but cannot grow
the arrays.

### Wire protocol

The parser validates the TCP header length, reserved bits, ports, options and
pseudo-header checksum before looking up a TCB. It accepts EOL, NOP, MSS and
well-formed unknown options. Sequence comparisons are wrap-safe. SYN and
SYN+ACK advertise the path MSS; there is no fragmentation, SACK, window scale,
timestamps, ECN or urgent-data delivery.

Active and passive handshakes retransmit with bounded deadlines. Data is a byte
stream. Writes copy as much as the send buffer can hold, and reads return any
contiguous prefix. The sender limits flight by the smaller of the peer window
and congestion window. The sender MSS is the smaller of the path MSS and the
peer's MSS option. The initial congestion window follows RFC 6928:
`min(10*SMSS, max(2*SMSS, 14600))`, or one segment after a retransmitted SYN
or SYN+ACK. The peer window changes only under the RFC 9293 SND.WL1/SND.WL2
rule, so an older segment that arrives late cannot replace a newer window. A
segment that acknowledges unsent data draws an ACK and is dropped with its
payload and FIN. The receiver advertises retained capacity, holds a
bounded set of later segments, trims duplicates and overlaps, and ACKs the
highest contiguous sequence. The first accepted FIN fixes the end of the
stream at its sequence number, even when it arrives out of order. Bytes at or
after that number are discarded, and nothing enters the receive stream after
the FIN is consumed. FIN consumes sequence space. Its sequence number must lie
inside the current receive window before it can fix the stream end. A FIN at
the right-exclusive window edge is rejected even when payload from the same
segment fits inside the window.

The retransmission timer uses SRTT, RTTVAR, Karn sampling, exponential backoff
and configured minimum and maximum values. A synchronized connection that
retransmits ten times in a row without new data being acknowledged ends with
`Timeout`. With the default RTO limits that takes about six minutes. A SYN,
data segment or FIN that waits for ARP is resent when the next hop resolves. If
resolution fails, the connection ends with `NeighborUnreachable`; connections
that only route through the same next hop stay open. Congestion control uses
slow start, congestion avoidance, duplicate ACK counting and fast retransmit. A
bounded persist sequence probes a zero window and resumes after a window
update.

`ShutdownWrite` drains accepted data and sends FIN while leaving reads open.
FIN is sequenced behind earlier bytes. Buffered bytes precede EOF. The state
machine retains FIN-WAIT, CLOSING, CLOSE-WAIT, LAST-ACK and TIME_WAIT. TIME_WAIT
defaults to 60 seconds and answers a retransmitted FIN. A valid RST refuses an
active open or resets a synchronized connection. An in-window non-exact RST
causes a rate-limited challenge ACK.

`Close` on a synchronized connection removes it from the owner's view and
leaves the stack to finish the close. The stack frees the endpoint when
LAST-ACK receives its final ACK, when TIME_WAIT ends, when FIN-WAIT-2 has
waited for the TIME_WAIT interval, or when the FIN reaches the retransmission
limit. Until then it counts against the owner's endpoint limit.

A client that closes first, such as the resolver after each DNS fallback,
leaves a TIME_WAIT behind each time and would be refused for up to
`time-wait-ms` after eight connections. An `Open` that finds the owner at
its limit, or the table full, therefore recycles the slot of the owner's
closed TIME_WAIT entry that expires first. It never takes another owner's
entry or one the owner still holds, and the new endpoint gets a new
generation, so the old ID stays invalid. The old connection's four-tuple,
its sequence numbers and its deadline move into a tombstone first.
Until that deadline no connection may use the four-tuple: ephemeral port
selection skips it and `Connect` with an explicit port refuses it with
`AddressInUse`. A segment for it is handled as TIME_WAIT handles one: a
retransmitted FIN is acknowledged again and restarts the interval, and
anything else, a new SYN or a reset included, is dropped without a reply or
a TCB. The table holds 64 tombstones, eight per session, so one owner can
never take another's share. An owner whose share is full gets `Busy` from
`Open` until a tombstone or a TIME_WAIT expires; protection is never
dropped to make room. Ending a session keeps its TIME_WAIT connections
until they expire, and a later session in the same place may recycle them.
Reconfiguration drops the tombstones of non-loopback addresses along with
the connections. A passive
child that no application has accepted is freed as soon as it ends, by RST,
timeout, reconfiguration or the close of its listener, and leaves the
listener's backlog at once.

Loopback and the current `net0` address use the same TCP state machine without
ARP or a driver. All of `127.0.0.0/8` works without a NIC. Replacing the
configured address aborts external connections with `Reconfigured`; wildcard
and loopback listeners remain. A temporary link-down leaves TCBs to their
normal timers. Restarting `netd` destroys its sessions and TCBs.

### GTCP1 and authority

Native programs use `GTCP1`, never `userspace/network` directly. A session has
one channel, four 4096-byte TX slots and eight 4096-byte RX slots. Its requests
are `Open`, `Bind`, `Listen`, `Accept`, `Connect`, `Write`, `Read`,
`ShutdownWrite`, `Close` and `Release`. Accept, Connect, Write and Read may be
held until their result is known. A Write reply returns its TX slot on every
result. Once the transport ends a connection, Write reports why: `Reset`,
`Timeout`, `Reconfigured` or `NeighborUnreachable`. `Closed` means the
application shut down writing or the close completed normally. Connect succeeds
once the handshake completes, even if a FIN or RST arrives before `netd`
delivers the reply. A successful Read lends one RX slot until Release. A failed
Open has no endpoint: its reply carries endpoint zero and the result, such as
`Busy` when the session or the stack table is full.

`userspace/tcpserve` owns deferred requests and pool transitions so host tests
exercise the same service logic as `netd`. Malformed messages, stale endpoint
IDs, foreign slots and invalid ownership are rejected. Closing a session
cancels its work and releases every endpoint and lent slot.

PID 1 creates TCP sessions only for a command whose manifest requests
`GrantTCP`. It passes the server channel and pools to `netd` with
`ControlAddTCPSession` and gives the client the `tcp`, `tcp-tx` and `tcp-rx`
roles. TCP authority is separate from ping, network administration, name
resolution and UDP.

`resolved` is the first service that uses `GTCP1`. PID 1 opens it a TCP
session through the same `ControlAddTCPSession` path and hands it over with
`ControlSetTCP`, under the generation of the UDP and status sessions it sent
just before (`GNETCTL` version 4). A message carries at most four handles,
and `ControlSetNetwork` already uses four. The resolver repeats a query
whose UDP answer came back with TC=1 over that session: one connection to
port 53 per fallback, one length-prefixed query and one reply (ADR 0194).
It holds no device and no `GrantTCP`; a fault on the TCP session drops that
session alone, and UDP lookups go on.

### Configuration and status

The `system.net.tcp.*` block controls the ephemeral range, global and
per-session endpoint limits, listen backlog, send and receive buffer limits,
connect timeout, RTO bounds and TIME_WAIT. `netd` reads it in the same atomic
network candidate as the address, timer and UDP settings. A bad cross-field
relationship rejects the whole candidate and names the second key.

`GNETAPI` version 4 has a separate TCP status operation. `net status` reports
endpoint, listener and connection counts, state counts, segment and reset
counters, retransmissions, parser and window drops, and active hard limits. It
never exposes payload bytes.

### Validation

Host tests cover the parser, checksum, sequence arithmetic, loopback stream,
half-close, endpoint generations, GTCP1 codecs, pool ownership and deferred
service operations. Regression tests run Open exhaustion and repeated passive
close through the real GTCP1 codec, check the initial window for MSS 536, 1200
and 1460, and check future-ACK drops and window-update order across sequence
wrap. Others cover a held Connect that sees a FIN before delivery, held Writes
ending with `Reset`, `Timeout` and `Reconfigured` against a scripted peer,
data and FIN waiting on ARP that succeeds or fails, unaccepted children across
renumbering, and data, duplicate and out-of-order FINs around the end of the
stream. Both attacker-controlled decoders are fuzz targets.
`tools/netpeer` supplies deterministic closed-port, delayed-handshake,
retransmission, FIN and RST behavior. The offline network VM gate exercises
GTCP1 through PID 1 and `netd`, including remote, loopback, own-address and
no-NIC paths.

## Consequences

Authorized native programs can act as bounded IPv4 TCP clients and servers.
The `tcp` command is a protocol client and diagnostic, not a privileged stack
shortcut. The resolver's DNS fallback runs over GTCP1, and future Linux
socket adapters can use it and the same TCB model.

IPv6, Linux socket syscalls, persistent DNS-over-TCP connections, TLS, HTTP, SACK, window
scaling, timestamps, ECN, Fast Open, keepalive and port reuse remain outside
this decision.
