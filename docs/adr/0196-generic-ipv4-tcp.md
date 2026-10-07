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
and congestion window. The receiver advertises retained capacity, holds a
bounded set of later segments, trims duplicates and overlaps, and ACKs the
highest contiguous sequence.

The retransmission timer uses SRTT, RTTVAR, Karn sampling, exponential backoff
and configured minimum and maximum values. Congestion control uses slow start,
congestion avoidance, duplicate ACK counting and fast retransmit. A bounded
persist sequence probes a zero window and resumes after a window update.

`ShutdownWrite` drains accepted data and sends FIN while leaving reads open.
FIN is sequenced behind earlier bytes. Buffered bytes precede EOF. The state
machine retains FIN-WAIT, CLOSING, CLOSE-WAIT, LAST-ACK and TIME_WAIT. TIME_WAIT
defaults to 60 seconds and answers a retransmitted FIN. A valid RST refuses an
active open or resets a synchronized connection. An in-window non-exact RST
causes a rate-limited challenge ACK.

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
result. A successful Read lends one RX slot until Release.

`userspace/tcpserve` owns deferred requests and pool transitions so host tests
exercise the same service logic as `netd`. Malformed messages, stale endpoint
IDs, foreign slots and invalid ownership are rejected. Closing a session
cancels its work and releases every endpoint and lent slot.

PID 1 creates TCP sessions only for a command whose manifest requests
`GrantTCP`. It passes the server channel and pools to `netd` with
`ControlAddTCPSession` and gives the client the `tcp`, `tcp-tx` and `tcp-rx`
roles. TCP authority is separate from ping, network administration, name
resolution and UDP.

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
service operations. Both attacker-controlled decoders are fuzz targets.
`tools/netpeer` supplies deterministic closed-port, delayed-handshake,
retransmission, FIN and RST behavior. The offline network VM gate exercises
GTCP1 through PID 1 and `netd`, including remote, loopback, own-address and
no-NIC paths.

## Consequences

Authorized native programs can act as bounded IPv4 TCP clients and servers.
The `tcp` command is a protocol client and diagnostic, not a privileged stack
shortcut. Future DNS fallback and Linux socket adapters can use GTCP1 and the
same TCB model.

IPv6, Linux socket syscalls, DNS-over-TCP migration, TLS, HTTP, SACK, window
scaling, timestamps, ECN, Fast Open, keepalive and port reuse remain outside
this decision.
