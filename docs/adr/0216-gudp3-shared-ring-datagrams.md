# ADR 0216: GUDP 3 moves datagrams through shared rings

## Status

Accepted. Builds on ADR 0195 (GUDP), ADR 0210 (Linux sockets) and ADR 0214
(GTCP 4), whose ring and doorbell rules it reuses.

## Context

linuxsockd carried every Linux datagram over GUDP 2: a Send and its reply
for each datagram out, and a RecvFrom, its reply and a Release for each
datagram in. A Linux UDP request and reply round cost netd four requests
and four replies, and missed the 144 us target at 228 us. GTCP 4 had
already taken the same per-slot messages off TCP.

## Decision

**A session speaks GUDP 3 when PID 1 creates it with `UDPVersion` 3.**
GNETCTL version 8 carries the GUDP version of `ControlAddUDPSession` in
byte 2: zero for GUDP 2, 3 for GUDP 3. PID 1 opens linuxsockd's UDP
session in GUDP 3; commands and the resolver keep GUDP 2. A command asks
for GUDP 3 with `GrantUDPRing`, and the command index moves to format 10.

**Each pool ends in one control page with one writer.** The TX pool holds
16 slots of 4 KiB and the client's page: its TX and Release rings and the
heads of the rings it consumes. The RX pool holds 16 slots and netd's
page: its Done and RX rings and their heads. The pool rights PID 1
already grants enforce the split. `udpapi/ring3.go` lays the pages out.

**Rings carry datagrams, the channel carries the lifecycle.** A client
sends with `EntrySend`, which names the endpoint, the slot, the length and,
with `FlagTo`, the destination. netd sends the datagram at once and hands
the slot back with `EntrySent` and the send's result. A client posts
receives with `EntryCredit`; while credit remains and a datagram waits,
netd fills a free RX slot and publishes `EntryDatagram` with the source,
the local address and the endpoint's readiness after it. `EntryRelease`
hands the slot back. Send, SendTo, RecvFrom and Release are not GUDP 3
opcodes; open, bind, connect, close, Ready, WaitReady and Option stay on
the channel, which adds `OpDoorbell` both ways.

**The doorbell rule is GTCP 4's.** A consumer arms before it sleeps and
checks its rings once more; a producer that published rings only when the
consumer armed since its last ring. netd arms its GUDP 3 sessions before
its wait together with its GTCP 4 ones, and linuxsockd arms both links in
`settleRing`.

**Every slot has one owner and a generation.** The side handing a slot
over numbers its hand-offs 1, 2, 3, and the side handing it back repeats
the number. netd ends a session whose entry names a generation out of
turn, releases a slot the client does not hold, or posts credit past
`MaxCredit3`.

**Close drains first.** netd takes the client's rings before it handles
any channel request, so the sends published before an `OpClose` go out
first. Close drops the endpoint's credit. Datagrams already on the RX ring
for a closed endpoint come back to the client, which releases their slots.

## Consequences

- `linuxsock.UDPRingLink` maps linuxsockd's native UDP operations onto the
  rings. A NoWait read with no read posted asks netd with `OpReady` first
  and posts credit only when a datagram waits, since credit with nothing
  queued would block. Peek has no GUDP 3 form; linuxsockd never asks for
  it.
- `udpserve.Session3` keeps the liveness contract of netd's sessions:
  Deliver reports more work past its budget, Progressed a call that did
  anything, Runnable work a call would do now, and Idle arms the doorbell.
- The in-process test (`userspace/linuxsock/udpworld_test.go`) runs the
  core and the link against `Session3` and the real stack, each side
  sleeping after every exchange: 400 datagrams cost no channel request,
  802 doorbells to netd and 400 back.
- `netbench4` measures a native GUDP 3 request and reply as
  `native-u3-udp-request-reply`, and the lab pairs Linux UDP with it.

## Measurements

Release build, HZ 250, two warm-up and nine measured rounds on the lab's
four-core KVM guest (run 20261106T160316Z at a317ed30), medians:

| Measurement | GUDP 3 | GUDP 2 |
| --- | --- | --- |
| Native 64-byte request and reply | 18 us | 67 us |
| Channel messages per round | 0.92 to 1.03, all doorbells | 8 |
| Linux 64-byte request and reply | 228 us | 228 us (14a523a3) |

The native round costs a quarter of what it did. The Linux round did not
move: the datagram hop between linuxsockd and netd was not where its time
went. ADR 0210 accounts for the rest.
