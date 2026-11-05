# ADR 0214: GTCP 4 moves stream data through shared rings

## Status

Accepted. Builds on ADR 0210 (GTCP 3, pipelined writes and queued reads)
and reuses the ring rule of the linuxsockd family data plane.

## Context

GTCP 3 costs a channel request and a reply for every 16 KiB slot a client
writes or reads. On the lab machine a native stream stops near 180 MiB/s,
short of the 258 MiB/s target. GTCP 4 removes the per-slot messages.
linuxsockd and the kernel already move Linux socket data
through shared rings without a message per slot.

## Decision

**A session speaks GTCP 4 when PID 1 creates it with `tcpapi.Layout4`.**
GTCP 2 and 3 stay as they are. A command asks with `GrantTCPRing`; the
command index moves to format 9. `netbench4` is the first client.

**Each pool ends in a control page with one writer.** The client writes the
page after its TX slots: the TX ring, the Release ring and the heads of the
rings it consumes. netd writes the page after the RX slots: the TXDone ring,
the RX ring, its heads and a readiness entry per endpoint. The rights PID 1
already grants (TX read-only in netd, RX read-only in the client) enforce
that split, so the session needs no third memory object.

**Rings carry the data, the channel carries the lifecycle.** A client queues
a TX slot with `EntryWrite`, which names the slot's generation and its
stream offset, mod 2^32, as GTCP 3 writes do. netd keeps the slot until the
stack took every byte and hands it back with `EntryWriteDone`. A client
posts read credit with `EntryCredit`; netd fills an RX slot whenever data,
the end of the stream or an error arrives, and publishes `EntryData` or
`EntryEnd`. OpWrite, OpRead and OpRelease are not GTCP 4 opcodes. The
channel keeps open, bind, listen, connect, accept, options, shutdown, close,
readiness waits and errors, and adds `OpOpenConnect`, `OpAcceptAhead` and
`OpDoorbell`.

**A doorbell goes only to a sleeping consumer.** Before it sleeps, a
consumer bumps an armed counter and checks its rings once more. After it
publishes, a producer sends `OpDoorbell` when the armed counter moved since
its last ring. netd arms its sessions just before its wait; a client arms
after `tcpring.Spins` yields found nothing. While both sides keep busy, a
stream costs no channel message at all.

**Every slot has one owner and a generation.** The owner numbers each hand-off
1, 2, 3, and the side handing the slot back repeats the number. netd ends a
session whose entry names a slot it holds, a generation out of turn, a
release of a slot the client does not hold, credit past `MaxCredit4`, or
corrupt ring indices.

**Close transfers ownership.** netd answers `OpClose` at once, sends what the
client had queued, hands those slots back, closes the connection and then
publishes `EntryClosed`. The client keeps its RX slots until it releases
them and gives the ID up with `EntryForget`, in batches. netd reuses the
table entry only after that, so an entry naming the old endpoint comes back
`ResultInvalidArgument` instead of reaching a new one.

## Consequences

- The in-process comparison (`tcpring.TestStreamMessageCounts`) streams
  8 MiB over loopback: GTCP 3 costs 24 channel messages per 64 KiB, GTCP 4
  none, with every byte and every slot checked.
- A session's table holds 512 endpoints (`Endpoints4`), the largest quota
  the stack grants, and a compile-time check keeps it there. Ring entries
  and replies name a table index in 16 bits. The readiness area takes three
  of netd's four control pages, and each GTCP 4 session costs netd about
  40 KiB of table.
- A FIN that the peer dropped at a closed window lost its retransmission
  timer when the peer's window update arrived. The ring tests exposed it;
  the stack now keeps the timer armed.
- FlagPeek and per-read lengths have no GTCP 4 form. A client that needs
  them stays on GTCP 3.
- linuxsockd still speaks GTCP 3 to netd; moving it is a separate change.
