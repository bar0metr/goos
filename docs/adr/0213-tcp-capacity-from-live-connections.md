# ADR 0213: TCP capacity from live connections

## Status

Accepted. Revises the transport storage of ADR 0196 and the TCP limits of
ADR 0210; applies ADR 0111's registry rule to `netd`'s TCP state.

## Context

ADR 0196 gave the stack a static table of endpoints, each with 16 KiB send
and receive arrays and eight reassembly slots of 1460 bytes. The sizes were
the prototype's, not decisions. By the time Linux sockets ran on GTCP 3 the
table had grown to 96 endpoints of about 45 KiB, 4.3 MiB of `netd`'s memory
whether a connection existed or not, and the limits still capped a server
far below what a Linux program expects:

| Limit | Value | Where it came from |
| --- | --- | --- |
| endpoints, all sessions | 96 | `MaxTCPEndpoints`, static table |
| TCP sessions | 8 | `MaxTCPSessions` |
| largest session grant | 64 | `MaxTCPSessionQuota`, a one-byte control field |
| listen backlog | 8 | tuning ceiling and `linuxsock.MaxBacklog` |
| send and receive buffer | 16 KiB each | fixed arrays |
| out-of-order data | 8 segments of 1460 bytes | fixed slots |
| TIME_WAIT tombstones | 4 per endpoint of the quota | fixed table of 2048 |

Every received segment walked the registry for its four-tuple and again for
a listener, and every `Advance` walked it for timers. The cost grew with the
table, not with the traffic.

## Decision

### Capacity is paid by live connections

The registry is a slice of pointers to control blocks. `TCPOpen` allocates a
416-byte control block in the lowest free slot and `releaseTCP` drops it;
the registry grows to the highest slot in use, up to `MaxTCPEndpoints`
(4096). An endpoint ID keeps its slot in the low 12 bits and a generation
above, so a closed ID matches nothing after its slot is reused.

The streams hang off the control block. A connection gets its receive ring
when its handshake starts and its send ring on its first write, and loses
both when it is released; the send ring also goes when the connection
closes or enters TIME_WAIT. A ring starts at 4 KiB, 16 KiB on loopback so a
loopback segment of `TCPLoopbackMSS` (16 KiB) fits whole, and doubles while
its stream fills more than half of it, up to the tuned buffer size.
Growth copies the ring once and never moves the window's right edge back.

Two budgets bound the rings: 16 MiB for all connections and 8 MiB for one
owner, set by `SetTCPMemoryBudget`. Each ring is charged at its full length
to the stack and to its owner. A handshake with no room for its first ring
is refused (`Busy` for a connect, a dropped SYN for a listener, counted in
`DropMemory`); a write takes what the budget lets the ring grow to and
returns `Busy` when nothing fits, and the endpoint reports no write room.

### Indexes for every lookup

The control blocks carry the links of five indexes, so linking and
unlinking cost no allocation:

- connections by four-tuple, SYN-SENT on;
- endpoints by local port, BOUND on, for the bind and ephemeral-port rules;
- listeners by local port, an exact address before the wildcard;
- endpoints by owner, for quotas and session end;
- each listener's children not yet accepted, oldest first.

The hashed tables double once they hold as many entries as buckets and mix a
per-stack seed into the hash, so a peer cannot aim its ports at one bucket.
`TCPStats` counts every index entry a received segment's lookups examined
(`LookupProbes`) and the most one segment needed (`MaxSegmentProbes`). With
300 loopback connections open, a segment averages 1.5 probes and never
needs more than 5. Statistics, reconfiguration and neighbour resolution
still walk the registry; they run once per event, not per segment.

### Timers in a heap

Retransmission, persist, connect timeout, FIN-WAIT-2 and TIME_WAIT
deadlines live in two fields per endpoint, and only `setRTO` and
`setDeadline` change them. A binary heap keyed by endpoint slot holds the
earlier of the two, so `Deadline` reads the root and `Advance` pops due
endpoints in order without visiting the others. An ACK that a full transmit
queue held back puts its endpoint on a pending list, which the next
`Advance` with a free frame sends. Tombstones have their own heap.

### Reassembly in the receive ring

Out-of-order bytes go into the receive ring at the offset their sequence
number gives past the contiguous bytes. A sorted list of spans records which
ranges hold data; an in-order segment that reaches a span pulls it into the
stream. The window bounds the spans, so the receive buffer, not a slot
count, bounds what a connection holds out of order, and held bytes no
longer shrink the advertised window. A ring tracks at most one span per 512
bytes, at least 16; a segment that would add one more gap is dropped and
the peer retransmits it. A FIN still fixes the end of the stream and drops
held bytes past it.

### TIME_WAIT outside the quota

A connection its owner has closed turns into a 56-byte tombstone as it
enters TIME_WAIT, with its own tuple and port indexes and expiry heap.
Tombstones have their own budget: 8192 in all and 2048 per owner, set by
`SetTCPTimeWaitBudget`. With no room, the connection stays an endpoint in
TIME_WAIT on its owner's quota, `TimeWaitHeld` counts it, and the owner's
next `Open` fails with `Busy` until room appears; at that point `Open`
turns the oldest such endpoint into a tombstone and takes its slot. The
stack never drops a four-tuple's protection to make room. A session that
ends hands its tombstones, and its TIME_WAIT endpoints, to the retired
owner, which no session pays for.

### Limits and defaults

| Limit | Ceiling | Default | Error when reached |
| --- | --- | --- | --- |
| endpoints, all owners (`max-endpoints`) | 4096 | 1024 | `Busy` from `Open` |
| owners (`MaxTCPSessions`) | 64 | | |
| ordinary session quota | 8 | 8 | `Busy` from `Open` |
| explicit session grant (`MaxTCPSessionQuota`) | 512 | | session refused |
| listeners | 256 | | `Busy` from `Listen` |
| listen backlog (`listen-backlog`) | 4096 | 128 | SYN dropped, `DropBacklog` |
| send and receive buffer | 4 MiB | 256 KiB | write `Busy`, window closes |
| ring memory | | 16 MiB, 8 MiB per owner | `Busy`, `DropMemory` |
| TIME_WAIT tombstones | | 8192, 2048 per owner | endpoint kept, then `Busy` |

`Listen` clamps a larger backlog to the tuned value, as Linux clamps to
`somaxconn`, and `linuxsock.MaxBacklog` follows the default, 128. The send
buffer, segment size and GTCP slot size are independent: a GTCP 3 slot stays
16 KiB, the wire MSS follows the MTU, and the loopback MSS stays 16 KiB.
The window field carries at most 65535 bytes; the stack sends no window
scale option, so a buffer past 64 KiB holds data the reader has not taken
rather than widening the window on the wire. GTCP 3's 32-bit stream offset
wraps as before; no buffer comes near 2 GiB.

`netd` serves at most 46 TCP sessions: one `WaitAny` names 64 objects, and
the driver, the control channel and the eight command and eight UDP
sessions take 18. The stack keeps 64 owners. The control protocol's quota
is now 16 bits (`netapi` control version 7), `netapi.MaxSessionQuota` is
512, and PID 1 grants it to `linuxsockd`'s TCP and UDP sessions. A
`tcpserve` or `udpserve` session sizes its endpoint table to its quota when
it starts. GTCP 4 entries name a table index in eight bits, so a GTCP 4
session refuses a quota above `tcpapi.Endpoints4` (64). UDP shares the
512-endpoint grant ceiling but keeps its 96-endpoint table, which bounds a
UDP session first. The status reply carries the wider TCP limits in
`netapi` version 7.

### Validation builds

A build with `goos_validation` compares every tuple, listener and bind
lookup with a walk of the registry and checks, on each `Advance`, the
indexes, owner lists, child lists, timer heap, pending ACKs, spans and
budget charges against the endpoints. A disagreement panics. Host tests run
the same check after every harness step.

## Alternatives

- **Larger static arrays.** 1024 endpoints of 45 KiB would be 45 MiB, more
  than `netd`'s page quota.
- **A Go map per index.** A map hides its probe count, allocates on growth
  of every bucket and costs more per lookup in TinyGo than a chained table
  threaded through the control blocks.
- **A timer wheel.** TCP deadlines span 100 ms to ten minutes; a heap of a
  few thousand entries costs about a dozen comparisons per change without a
  resolution or overflow policy.
- **Window scaling.** It would make buffers past 64 KiB useful on the wire,
  but it changes the SYN options both ends negotiate; it is left for a
  later change.

## Consequences

- An idle `netd` holds no TCP buffer memory. The stack's static size fell
  from 4.9 MiB to 0.55 MiB, most of it UDP receive buffers.
- A wire connection costs about 4.5 KiB before its first write; a loopback
  pair about 41 KiB once both sides have written. 300 loopback connections
  with a short exchange each charged 11.7 MiB.
- The ring and tombstone budgets are the policy. A host with more memory
  raises them; nothing else has to change.
- Allocation happens per connection and per ring growth, never per segment;
  the pending-ACK list reuses its two buffers.
