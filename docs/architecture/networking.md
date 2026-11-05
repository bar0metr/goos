# Networking

`/system/bin/fetch` is a native HTTP/1.1 client for HTTP and HTTPS. It asks
`resolved` for IPv4 addresses through GRESOLV1, then uses GTCP to connect to
`netd`. Its response parser lives in `userspace/httpwire`; URL and redirect policy live in
`userspace/fetch`. An optional VFS session receives file output. The network
services do not interpret HTTP, TLS or X.509.

GoOS has two IPv4 interfaces: `lo`, which always exists and owns
`127.0.0.0/8`, and `net0` on one legacy virtio-net function. The kernel
discovers the function, routes its interrupt and bounds the driver's
authority (ADR 0190). Everything that knows what a frame means runs in
userspace: a driver that moves frames, a network service that runs the
protocols (ADR 0191) and UDP endpoints for programs (ADR 0195), a resolver
service that turns names into addresses (ADR 0194), and commands that ask
them for work. The
[networking specification](../development/networking.md) records the
requirements the first version was built from.

Implemented: IPv4 with RFC 5227 address-conflict detection, ICMP echo, a
DHCP client, the loopback interface, a hosts table (ADR 0193), DNS name
resolution through a stub resolver with a cache, and IPv4 UDP for any
program PID 1 grants a UDP session (ADR 0195) or TCP session (ADR 0196). Not implemented: IPv4
fragmentation and reassembly, multicast and broadcast sends, ICMP errors to
UDP senders (no Port Unreachable for an unbound port), persistent DNS over TCP, multicast DNS, IPv6 transport, DNSSEC, and the
Linux socket calls. `dnsq` shows AAAA records; nothing sends IPv6.

## HTTPS

`userspace/tlsclient` wraps a connected GTCP byte stream in the pinned Go
1.25.5 software `crypto/tls` and `crypto/x509` implementations, compiled with
TinyGo 0.42.0 (ADR 0198). It performs no DNS lookup or transport dial itself.
`fetch` uses the same request, HTTP framing and VFS output code for both schemes;
HTTPS defaults to port 443. HTTP redirects may target HTTP or HTTPS, while a
followed HTTPS-to-HTTP redirect fails before opening the next connection.
Cross-origin redirects strip Authorization, Proxy-Authorization, Cookie and a
custom Host header.

The client selects verified signed state roots or the immutable bootstrap
`/system/security/ca-certificates.pem`,
kernel entropy through `cryptosys.Reader`, and REALTIME through `cryptosys.Now`.
Missing or invalid roots, unavailable entropy or realtime, and a clock rollback
during certificate validation fail closed. It uses TLS 1.3, X25519, the standard
TLS 1.3 suites, and HTTP/1.1 ALPN. Numeric IPv4 names require IP SAN and omit SNI;
DNS names supply SNI. Standard X.509 checks enforce signatures, chain and CA/path
constraints, usages, validity and SAN identity, including unknown-critical-extension
rejection and no Common Name fallback.

The native compatibility tools prepare temporary toolchain roots without
modifying installed Go, TinyGo or the kernel runtime. The overlay rejects QUIC
and cancelable contexts without a scheduler, bridges randomness to the kernel,
and disables the weak certificate cache. Native TLS consumers call
`arena.InitPackages` after heap setup to run the pinned runtime's `initAll`;
other native entry points remain unchanged. It limits peer lists to eight
certificates before slice allocation, each DER to 32 KiB, DNS SAN entries to 64,
and candidate chain depth to eight; the standard verifier retains its
100-signature-check budget. The existing TLS 1.3 certificate-message buffer can
still reach 256 KiB before the per-certificate checks. One monotonic 35-second
deadline covers the whole handshake; later TCP operations each have a 35-second
bound.

Native primitive and explicit-root X.509 probes pass. The offline native
`test-tls` gate passed 69 assertions, including authenticated TLS 1.3 with
AES-128-GCM/SHA-256 (cipher 4865), verification failures, downgrade rejection and
VFS output. A separate native run negotiated ChaCha20-Poly1305 (4867).
Production-root loading used 1360864 allocated bytes with a 1363216-byte arena
high-water mark for 121 roots; [release notes](../releases/0.2.6.md) record the
other measurements. The complete `release-check` passed; the initial `test-all` exposed an obsolete
boot directory-count assertion, corrected and verified by the release boot gate.
Stock TinyGo TLS is
a device-offload shim that reports handshake success without wire output and
cannot secure this stream. See [ADR 0198](../adr/0198-native-tls-and-system-trust.md)
and [the evidence report](../development/crypto-feasibility.md).

## Processes

```text
resolve             ping                         net    udp, tcp, dnsq
  | GRESOLV1          | GRESOLV1, GNETAPI version 5 | GNETAPI v5 | GUDP 2 / GTCP 2
  v                   +-------------+---------------+--------------------+
resolved  <-- GNETCTL --  PID 1     |  one private session per command
  |  hosts: GHST1 on its configuration session
  |  GUDP 2 session, GTCP 2 session v
  |  and GNETAPI status     netd  <---- GNETCTL version 6 ----  PID 1
  +-------------------------------->|  lo, Ethernet, ARP, IPv4, ICMP echo,
                                    |  DHCP client, UDP endpoints, TCP TCBs
                                    |
                                    |  GNETDRV1 channel
  |  receive pool  (driver writes, netd reads)
  |  transmit pool (netd writes, driver reads)
  v
virtio-net driver
  |  ports, line, DMA window
  v
legacy virtio-net PCI function (1af4:1000)
```

PID 1 creates every channel and pool in the picture and is the only holder
of `netd`'s and `resolved`'s control channels. It keeps the device's three capabilities for
the life of the system and grants them to each driver it starts. `netd`
holds no device authority, and the driver holds no filesystem,
configuration or client session.

`netd` waits on its driver channel, its control channel, up to eight
command sessions, up to eight UDP sessions and up to 46 TCP sessions in one
`WaitAny`, with a timeout set by the stack's earliest
timer, or 30 seconds when no timer is due. The driver waits on its channel
and its interrupt line in one `WaitAny` with a 10-second timeout and does not
touch the device when the timeout expires.

## A received frame

1. The device writes the frame, behind a 10-byte header, into one of the 16
   buffers the driver posted in its DMA window, and raises its line.
2. The kernel's handler masks the route, sets the line's pending flag and
   wakes the kernel loop, which reports the line ready to the driver's
   `WaitAny`.
3. The driver reads the device ISR, which deasserts the line, and calls
   `IRQTake` to consume the pending event.
4. For each used-ring entry it checks the ring index, the descriptor ID, the
   written length and the header. It copies the frame into a free slot of
   the receive pool, posts the buffer again, and sends `RxReady` with the
   slot and the length. The slot now belongs to `netd`.
5. `netd` takes the slot, gives the frame to the stack, and sends
   `RxRelease`, which returns the slot to the driver.
6. The driver's next wait unmasks the route.

With all four receive slots held by `netd`, the driver stops and leaves the
remaining entries in the used ring. The device then runs out of posted
buffers and drops frames itself, so a slow service loses frames and costs no
memory.

## A transmitted frame

1. The stack builds the frame in its 8-frame transmit ring.
2. `netd` takes a free transmit slot, moves the frame into it, and sends `Tx`
   with the slot and the length as a request. The slot now belongs to the
   driver.
3. The driver copies the frame into its own transmit buffer, behind a zeroed
   header, adds a descriptor and notifies the device. A full queue answers
   `Busy` at once.
4. When the device returns the descriptor in the used ring, the driver sends
   `TxDone` with `OK` as the reply to that request, and the slot returns to
   `netd`, which counts any other result as a drop.

No descriptor ever holds a pool address, so `netd` cannot change a buffer
the device is reading. While every transmit slot is with the driver, frames
wait in the stack's ring; a frame that finds the ring full is lost like a
dropped one.

## Slot ownership

Each side keeps a `netdev.Pool` per pool, in which a slot is free, in use,
or with the peer:

| Pool | Starts with | Hand-over | Return |
| --- | --- | --- | --- |
| receive | the driver | `RxReady` | `RxRelease` |
| transmit | `netd` | `Tx` | `TxDone` |

A side marks a slot as the peer's only after the message that hands it over
was sent, and takes it back only on the message that returns it. A message
for a slot the sender does not hold fails that transition: `netd` answers
it with `Fault` and drops the binding, and the driver sends `Fault` and
exits. A new binding resets both pools.

## Address configuration

In DHCP mode, the default, the client sends `DISCOVER`, takes the first
valid `OFFER`, sends `REQUEST`, and on `ACK` probes the address before it
applies it. An offer or acknowledgement needs a server identifier, a subnet
mask and a finite lease of at least 30 seconds; a router is optional and
must lie inside the offered prefix. Absent T1 and T2 default to half and
seven-eighths of the lease, and supplied values must satisfy
T1 < T2 < lease. The client uses the first router listed. A repeated
option among those it reads fails the message, option overload into the
file and server-name fields is followed. Option 6 gives up to four DNS
servers that belong to the lease and go with it; other options are ignored. A `NAK` or an expired lease removes the address and starts
discovery again, and so does a renewal acknowledged for a different
address.

In static mode the address, prefix and optional gateway come from the
configuration service and must pass the checks in
[configuration](../userspace/configuration.md#network). Both modes claim the
address as RFC 5227 describes: a random wait of up to 1 s, three ARP probes
1 to 2 s apart, a 2 s quiet period, then two announcements 2 s apart. Any ARP
from another host that uses or probes for the candidate rejects it. Once
the address is live the stack answers other hosts' probes for it and keeps
watching: the first claim by another host is answered with one announcement,
a second claim within 10 s gives the address up, declining a DHCP lease or
failing a static address. Ten conflicts in a row, with no address held
through its announcements in between, delay the next claim by 60 s. The
values are settings (ADR 0192); `net status` shows the counters.

## Loopback

`lo` holds `127.0.0.1/8` from the start of `netd`, with or without a NIC,
lease or address, and the whole of `127.0.0.0/8` routes to it. An echo to a
loopback host is answered inside the stack with zero round-trip time; nothing
for `127.0.0.0/8` is framed, resolved with ARP or sent to the gateway. A
frame from the link carrying a loopback source or destination, in IPv4 or
ARP, is dropped. `net status` lists `lo` with its MTU of 65536, a logical
limit, and the count of echoes answered on it (ADR 0193).

## UDP

A program reaches UDP through a GUDP version 2 session (`userspace/udpapi`):
one channel, a TX pool of four 4096-byte slots the program writes and `netd`
only reads, and an RX pool of eight slots `netd` writes and the program only
reads. PID 1 creates the three objects for a command whose manifest asks for
`GrantUDP` and hands `netd` its ends with `ControlAddUDPSession`. The
requests are `Open`, `Bind`, `Connect`, `Send`, `SendTo`, `RecvFrom`,
`Close`, `Ready`, `WaitReady` and `Option`, and `netd` answers each once;
`Release` returns an RX slot and gets no reply. `netd` closes a session that
sends a malformed request or hands back a slot it does not hold, and every
endpoint of that session closes with it. `netd` treats a message of another
version as malformed.

Requests are 24 bytes and replies 32. A flags byte carries `NoWait` and
`Peek` for `RecvFrom` and `ReuseAddr` for `Bind`. A `NoWait` receive with
nothing queued answers `WouldBlock`; a `Peek` receive copies the datagram
and leaves it queued. A receive reply's length is the whole datagram, up to
4096 bytes, the slot size, and it carries the endpoint's readiness after the
datagram left the queue, as `Ready` reports it. `Ready` reports the
readiness mask (readable, writable), the next datagram's length and the
number queued. Byte 18 of a request other than `Release` is a mask of RX
slots the client returns; `netd` takes them back before it handles the
request, as one `Release` each would. `WaitReady`
names the mask the client saw; `netd` answers it once the endpoint's mask
differs, at once if it already does. An endpoint holds one, and `Close`
answers it with `Closed` first. `Option` reads the TTL or the largest
payload the path to a destination carries: max-payload on the local path,
the MTU less 28 bytes on the wire. A larger send answers `MessageSize`, and
`netd` sends nothing.

The endpoints live in the stack. Each has an owner, the index of the UDP
session it was opened on, and every call checks it. An endpoint ID carries a
generation above its table slot, so an ID kept after `Close` matches
nothing. Bind takes port 0 for a random port from the ephemeral range,
49152 to 65535 by default; a port is taken when another endpoint holds it
on the same address or either address is the wildcard, unless both
endpoints bound with `ReuseAddr`, and port 68 belongs to the DHCP client
while the interface runs DHCP. `Connect` fixes the peer,
binds an ephemeral port if needed and sends nothing; a connected endpoint
sends only to that peer and keeps only datagrams from its address and port.

A send to `127.0.0.0/8` or to the interface's own address goes straight to
the endpoint table with no frame, so loopback UDP works without a NIC.
Anything else is routed and resolved with ARP like an echo, and waits in an
8-entry table while ARP runs. An external payload is at most the MTU less 28
bytes, 1472 on a 1500-byte link; there is no fragmentation. Broadcast,
multicast, a subnet's network or broadcast address and port 0 are refused as
destinations. A received datagram goes to the endpoint bound to its
destination address and port, or else to the one bound to the wildcard on
that port. Among endpoints that share a binding by `ReuseAddr`, the one bound
last receives unicast, as on Linux. Queued payloads share 128 receive
buffers of 4096 bytes, 512 KiB in all, and one session holds at most 64 of
them, one for each endpoint of a session at the largest quota; a full queue
or share drops the newcomer and keeps what is queued. No ICMP Port
Unreachable is sent for an unbound port.

## TCP

TCP uses the same route, ARP and local-delivery paths as UDP, but keeps its
TCBs in `userspace/network`. Native programs reach it through GTCP version
2: a channel, four TX slots and eight RX slots. PID 1 creates these objects
only for `GrantTCP` and registers the service side with `netd` through
`ControlAddTCPSession`. Endpoint IDs carry generations and every operation
checks the owning session.

Requests are 24 bytes and replies 32. Without flags, `Accept`, `Connect`,
`Read` and `Write` park in `netd` until they finish, as in version 1. The
`NoWait` flag answers them at once: `Read` and `Accept` with `WouldBlock`,
`Write` with the prefix that fit or `WouldBlock` when nothing fit, and
`Connect` with `InProgress` and the chosen four-tuple once the handshake has
started. `Peek` reads bytes without consuming them; `ReuseAddr` binds with
SO_REUSEADDR. `Ready` reports a readiness mask, the readable byte count and
the send buffer room without consuming anything. The mask says whether a
listener has a child, the connect finished or failed, bytes are buffered,
the peer's FIN follows them, a write would take a byte, the write side is
closed, an error is pending, the connection hung up, a connect is in
progress, or the endpoint listens. `WaitReady` names the mask the client
saw; `netd` answers it once the endpoint's mask differs, at once if it already
does, so a client that calls `Ready` and then `WaitReady` with the mask it
got cannot miss a change between the two. An endpoint holds one wait;
`Close` answers it with `Closed` first. `Error` returns and
clears the pending transport error; a `Connect`, `Read` or `Write` reply
that reports the error clears it too. `Option` reads the send MSS, the send
and receive buffer sizes and the TTL.

### GTCP 3

`linuxsockd`'s TCP session speaks GTCP version 3; every other session
speaks version 2. PID 1 names the version in byte 3 of
`ControlAddTCPSession` (GNETCTL version 6), and the version fixes the pools
(`tcpapi.LayoutFor`): version 2 has four TX and eight RX slots of 4 KiB,
version 3 eight TX and eight RX slots of 16 KiB, 128 KiB a pool. `netd`
checks that each mapped pool holds its layout, decodes every message of the
session with that layout, and closes the session on a message of another
version. The replies and every opcode keep version 2's layout and rules,
except for three changes.

- Requests grow to 28 bytes. Bytes 24..27 hold a `Write`'s stream offset:
  the bytes the endpoint had accepted from the client since it opened, mod
  2^32, when the client sent the write. A client may send several `NoWait`
  writes of one endpoint without waiting for replies, and should keep at
  most four (64 KiB) in flight. `netd` takes bytes only from a write whose
  offset equals the endpoint's count. When an earlier write took a prefix,
  a later one names an offset past the count and answers `WouldBlock`
  with nothing taken; the client resends from its first unaccepted byte.
  An offset behind the count answers `InvalidArgument` and takes nothing.
  Every other opcode carries a zero offset.
- An endpoint holds up to four parked `Read`s. `netd` answers them in the
  order they arrived, each with up to its `Length` bytes in an RX slot of
  its own, and a read that finds every RX slot lent waits for a `Release`.
  A `NoWait` read on an endpoint with parked reads answers `WouldBlock`,
  since arriving bytes belong to the parked ones. `Close` answers a held
  `WaitReady` first, then the parked reads in order, all with `Closed`.
- Byte 2 of a request other than `Release` is a mask of RX slots the client
  returns. `netd` takes them back before it handles the request, as one
  `Release` each would. `linuxsockd` returns the slot of the read it just
  copied on the read it posts next, so a request and reply round costs
  `netd` one message less per direction.

`netd` serves a session's channel until its queue is empty before it waits
again, so it takes a batch of writes and reads of one endpoint in one
activation. A version 3 session maps 256 KiB of pools in both `netd` and
`linuxsockd`, against 48 KiB for version 2.

A bind follows the Linux SO_REUSEADDR rule: it conflicts with any endpoint
or TIME_WAIT four-tuple on the same port whose address overlaps, equal or
either one the wildcard, unless both carry `ReuseAddr` and the holder is not
listening. A TIME_WAIT entry keeps the flag of its connection, and an
accepted child takes its listener's. A connect may share an ephemeral local
port with other connections, which the four-tuple check keeps apart. `remoted`
and `tcp listen` bind with `ReuseAddr`, so a restarted server rebinds while
its old connections wait out TIME_WAIT.

The stack supports bind, ephemeral ports, listen, deferred accept and connect,
bounded byte-stream reads and writes, retransmission, receive and congestion
windows, bounded out-of-order reassembly, zero-window probes, FIN half-close,
RST and TIME_WAIT. The stack allocates a control block per endpoint, a
receive ring when a handshake starts and a send ring on the first write;
rings start at 4 KiB (16 KiB on loopback) and double up to the tuned buffer,
256 KiB by default. Out-of-order bytes wait in the receive ring. The
endpoint limit is 1024 by default and 4096 at most, with at most 256
listeners; ring memory is bounded to 16 MiB, 8 MiB per owner. Indexes find
a segment's connection, a listener or a port's holders without walking the
endpoints, and a heap holds the timers (ADR 0213).

### Session quotas

Each TCP and UDP session has an endpoint quota that PID 1 names in
GNETCTL version 7 (`ControlAddTCPSession` and `ControlAddUDPSession` carry
it in bytes 4 and 5, 1 to 512). Commands and `resolved` get the ordinary
quota of 8; `max-endpoints-per-session` can lower it. PID 1 asks for 512 for
`linuxsockd`, and the tuning does not lower a grant above the ordinary
quota. `netd` holds 46 TCP and 8 UDP sessions, and each session's table
fits its own quota. A UDP session still holds at most the 96 endpoints of
the UDP table. A connection its owner closed leaves TIME_WAIT as a
tombstone outside the quota, 2048 per owner and 8192 in all; with the
budget full the connection stays an endpoint on the quota and the owner's
next `Open` gets `Busy`. A passive child counts against its listener's
session.

Traffic to `127.0.0.0/8` or the current interface address is fed back through
the TCP parser and state machine. It does not use ARP or the virtio driver.
Replacing the configured external address aborts connections bound to it;
wildcard and loopback listeners remain. A plain link-down leaves connection
state to its retransmission deadlines.

`net status` obtains TCP counters and limits through the separate TCP status
operation in GNETAPI version 5. Version 5 adds four `uint64` counters at
reply bytes 152..183: GTCP 2 requests decoded and replies sent, then the
same for GTCP 3, over every session. `net status` prints them on its `gtcp`
line; their deltas give GTCP messages per byte moved. Stream contents are
never diagnostic data.
See ADR 0196 for the state, ownership and protocol rules.

## Names

`resolved` answers `localhost` and names under it with `127.0.0.1` itself,
refuses `.local` names, asks the configuration service's hosts table, then
its cache, then the upstream DNS servers: those of the DHCP lease first,
then `system.dns.server1..4`. It reaches the servers over a UDP session
like any program: each query opens an endpoint, connects it to the server's
port 53, which binds a fresh ephemeral port, sends, and waits for the
answer. The connected endpoint drops anything but that server's port 53,
the resolver checks the source, transaction ID and question before anything
reaches its cache, and the endpoint closes when its transaction stops
waiting. The lease's servers arrive over a `GNETAPI` status session through
a `Servers` request that `netd` holds until the list changes. PID 1 opens
both sessions and hands them over with `ControlSetNetwork` whenever either
service starts (ADR 0194, ADR 0195).

An answer with TC=1 that matches the query makes `resolved` ask the same
server the same question over TCP. PID 1 gives it a `GTCP` session with
`ControlSetTCP` right after the UDP pair, under the same generation
(ADR 0196). Each fallback opens one connection to port 53, writes the
two-byte length and the query, reads exactly one length-prefixed reply into
one of two 65535-byte buffers, and closes. The reply goes through the same
checks, CNAME handling and cache as a UDP answer. `resolved` holds a UDP
session, a TCP session and a status session, and no device.

The DNS code splits into five packages:

```text
                       +--> userspace/dns --> resolved
userspace/dnswire -----+--> userspace/dnsstub --> resolved
                       +--> userspace/dnsq --> dnsq

userspace/dnstcp ------+--> userspace/tcpupstream --> resolved
                       +--> userspace/dnsstub --> resolved
                       +--> userspace/dnsq --> dnsq
```

`dnswire` holds the message format: names, query encoding for any QTYPE,
response validation, query validation and reply encoding for the local
server, record iteration and presentation text. It is the
only DNS parser in the system, with the bounds above: 63-byte labels,
255-byte names, backward compression pointers, at most 64 of them in a
name, and record lengths checked against the message. `dnstcp` holds the
two-byte length framing and the 65535-byte message limit, and nothing about
sockets. `dns` keeps resolver policy: the cache, transactions, retries,
server rotation and CNAME chains. `netd` knows nothing of DNS.

`dnsq` is a command, not a client of `resolved`. With its own UDP and TCP
sessions it sends one query to the server on its command line and prints the
answer. It shares `dnswire` and `dnstcp` with the resolver, holds one
65535-byte buffer for a response from either transport, and runs UDP and a
TCP fallback against one deadline. Each TCP query uses one connection that
`dnsq` closes like any `GTCP` client, TIME_WAIT included.

### Local DNS server

Programs that speak DNS themselves, such as a static C library, find
`resolved` on `127.0.0.1:53`. The boot archive's `/etc/resolv.conf` says
`nameserver 127.0.0.1`, `/etc/hosts` holds `127.0.0.1 localhost`, and
`/etc/nsswitch.conf` says `hosts: files dns` (see
[filesystem.md](filesystem.md)). `userspace/dnsstub` serves the address
over UDP and TCP. It binds `127.0.0.1` alone, so only loopback traffic
reaches it, and it answers through the same `dns` resolver as a GRESOLV1
lookup: localhost, the hosts table, the cache, then the upstream servers
with the TCP fallback and the resolver's deadlines. A client gets its ID
and question back with the answer, and nothing else it wrote leaves the
host: `resolved` asks upstream with its own ID and the folded name.

| Query | Reply |
| --- | --- |
| A, class IN | the addresses, or the resolver's failure |
| AAAA, class IN | the rcode of the A lookup for the name, no records: GoOS has no IPv6, and the C library gets its IPv4 answer without waiting out a timeout |
| other types, other opcodes | NOTIMP |
| other classes, names outside host-name syntax, `.local` | REFUSED |
| EDNS version above 0 | BADVERS |
| fewer than 12 bytes, or QR set | no reply |
| any other malformed query | FORMERR |

A lookup that found the name is NOERROR, NXDOMAIN stays NXDOMAIN, and a
timeout, SERVFAIL upstream, a busy resolver or a network change is
SERVFAIL. A reply carries at most 8 addresses and an OPT record when the
query had one, 410 bytes at most, so the stub never sets TC.

The stub runs on `resolved`'s own UDP and TCP sessions. Its correlations
have the top bit set, and `resolved` hands each reply to the stub or to the
upstream clients by that bit. It shares their TX slot trackers: on UDP it
takes a slot only while one stays free for each upstream query still
opening, and on TCP it holds at most two, leaving the other two for the two
fallbacks.

| Bound | Value |
| --- | --- |
| UDP queries in progress | 6; the stub keeps one RecvFrom out only while a slot is free, so a flood waits in `netd`'s queue and drops there |
| TCP connections | 2 accepted, listener backlog 2, one query at a time per connection |
| TCP query size | 1232 bytes; a longer length prefix closes the connection |
| idle connection | 5 s to deliver a query or take a reply |
| resolver client slots | 16 for GRESOLV1 sessions, 8 for the stub |

A connection reads exactly one framed query at a time, so a client that
pipelines keeps its next query in the stream. Against `netd`'s 8 endpoints
per session, `resolved` uses at most 5 UDP endpoints (4 upstream queries and
the stub's) and 7 TCP endpoints (2 fallbacks, the listener, 2 in the backlog
and 2 accepted). New sessions from PID 1 end the stub's queries in progress
on the old ones; the stub opens its endpoint and listener again on the new
ones.

The interface is in one of four states:

| State | Meaning |
| --- | --- |
| `down` | no driver bound, or no carrier |
| `configuring` | a live link without a usable address |
| `ready` | an address that passed validation and the conflict probe |
| `failed` | a device fault or a static address conflict or relinquished static address; a reload or a new link-up starts again |

## What the stack accepts

- Ethernet II frames of 14 to 1514 bytes, sent to our MAC or broadcast,
  carrying ARP or IPv4; a longer or shorter frame counts as malformed and is
  never truncated into a parseable one. VLAN tags and other types count as
  drops.
- ARP request and reply for Ethernet and IPv4. The table learns a sender
  only when it is on the prefix and the packet is addressed to us, or the
  sender is already known or being resolved, so broadcast traffic cannot
  churn it.
- IPv4 with a 20-byte header, a valid checksum, and the interface's address
  as destination. Fragments and options count as drops. Before an address
  exists only DHCP traffic is accepted, broadcast or unicast. Outgoing
  packets carry the configured TTL, 64 by default, and are never fragmented. The connected prefix is
  tried first, then the default gateway; with neither an echo fails with
  `NoRoute` before anything is sent.
- ICMP echo request, echo reply, destination unreachable and time exceeded,
  each with its checksum checked. Echo requests to our unicast address are
  answered to the sender's MAC. Replies and errors must match a pending echo
  as ADR 0191 describes; anything else changes no state.
- UDP whose length fits the IPv4 payload, with a valid checksum or a zero
  one, which means "no checksum"; a bad checksum is dropped and counted
  first. Server-to-client DHCP goes to the DHCP client while the interface
  runs DHCP. A datagram addressed to the interface's own address goes to
  the endpoint bound to its port; one with no endpoint is a counted drop.

## Timers

The operator sets these through `system.net.*` (ADR 0192); the table shows
the defaults. See [configuration](../userspace/configuration.md#network) for
ranges and for when a reloaded value takes effect.

| Timer | Default | Key |
| --- | --- | --- |
| ARP entry lifetime | 60 s | `arp.cache-timeout-ms` |
| ARP resolution | 3 requests 1 s apart, then `NeighborUnreachable` 1 s after the last | `arp.resolve-attempts`, `arp.resolve-interval-ms` |
| address probe | a random wait of 0 to 1 s, then 3 ARP probes a random 1 to 2 s apart | `ipv4.probe-wait-ms`, `ipv4.probe-count`, `ipv4.probe-min-ms`, `ipv4.probe-max-ms` |
| announcements | 2, 2 s apart | `ipv4.announce-count`, `ipv4.announce-interval-ms` |
| defence | a second claim within 10 s gives the address up | `ipv4.defend-interval-ms` |
| conflict rate limit | 60 s before the next claim after 10 conflicts in a row | `ipv4.max-conflicts`, `ipv4.conflict-rate-limit-ms` |
| DHCP `DISCOVER` and `REQUEST` retransmission | 4 s, doubling to 64 s, with up to 1 s of jitter either way, at most half the base | `dhcp.initial-retry-ms`, `dhcp.max-retry-ms` |
| DHCP retry after a conflict | 10 s, or the rate limit when longer | `dhcp.conflict-retry-ms` |
| DNS lookup | one deadline of 6 s, `query-timeout-ms` times `attempts`, shared among the servers | `system.dns.query-timeout-ms`, `system.dns.attempts` |
| echo replies answered | 20 per one-second window | `icmp.echo-reply-rate` |

DNS `attempts` controls the logical retry rounds for each queried name. An
EDNS-to-classic fallback does not consume a logical attempt. Every datagram,
including that fallback and queries for CNAME targets, consumes a separate
per-server budget. No upstream server receives more than three datagrams in
one resolution, and the resolver keeps the original absolute deadline. A
TCP fallback after TC=1 belongs to the attempt that drew it. It waits at
most `query-timeout-ms` inside the same deadline, and no server gets more
than two in one resolution.

These are fixed:

| Timer | Value |
| --- | --- |
| DHCP `REQUEST` while selecting | 4 retransmissions, then discovery again |
| DHCP renew and rebind retransmission | half the time left before T2 (renew) or expiry (rebind), at least 60 s, never past that point |
| echo deadline | 100 to 5000 ms per request, 1000 ms by default in `ping` |
| `ping` request spacing | 1 s between the starts of two requests |
| command reply wait | the request's timeout plus 2 s |
| `netd` idle wait | 30 s |
| quiet period after the last probe | 2 s (RFC 5227 ANNOUNCE_WAIT) |
| `netd` and `resolved` configuration read | 2 s per key, 5 s for the whole candidate, in scheduler ticks; a late reply to an earlier key is dropped |
| `resolved` hosts lookup | 500 ms |
| driver wait | 10 s |

The stack holds no clock. `netd` passes monotonic nanoseconds into it and
asks it for its earliest deadline before every wait, so an idle interface
blocks.

## Limits

No setting changes these. They size static storage, shared memory or
protocol bounds.

| Limit | Value | Source |
| --- | --- | --- |
| network functions | 1 | `pci.FindLegacyVirtioNet` |
| interrupt lines accepted | 3, 5, 6, 9, 10, 12, 14 | `kernel/netdevice.go` |
| interrupt vector | `0x34`, bootstrap processor | `kernel/arch/amd64/tables_uefi.go` |
| frame size | 14 to 1514 bytes, MTU 1500 | `netdev.MinFrame`, `netdev.MaxFrame` |
| link MTU | 576 to 1500 as the driver reports it; shown by `net status` and sent as DHCP option 57 | `network.Stack.LinkUp` |
| pool size | 2 pages, 4 slots of 2048 bytes | `netdev.PoolPages`, `netdev.Slots` |
| receive buffers posted | 16 of 2048 bytes | `userspace/cmd/virtio-net` |
| transmit buffers | 4 of 2048 bytes, one per slot | `userspace/cmd/virtio-net` |
| queue size | power of two, 2 to 1024 | `netdev.MaxQueueSize` |
| DMA window | 64 pages | `userspace/cmd/virtio-net` |
| ARP entries | 16 | `network.MaxNeighbors` |
| ARP resolutions in progress | 4 | `network.MaxResolutions` |
| transmit ring | 8 frames | `network.TxQueueFrames` |
| command sessions | 8 | `network.MaxSessions` |
| echoes in flight | 1 per session | `userspace/cmd/netd` |
| echo payload | 32 bytes | `network.EchoPayload` |
| static prefix | 1 to 30 | `network.MinPrefix`, `network.MaxPrefix` |
| shortest DHCP lease | 30 s | `userspace/network/dhcp.go` |
| `ping` count | 1 to 16, 4 by default | `userspace/bin/ping` |
| DNS servers from a lease | 4 | `network.MaxDNSServers` |
| UDP sessions | 8 | `network.MaxUDPSessions` |
| UDP endpoints | 32, 8 per session | `network.MaxUDPEndpoints`, `network.MaxUDPSessionEndpoints` |
| UDP receive queue | 16 datagrams per endpoint | `network.UDPQueueCapacity` |
| UDP receive buffers | 32 of 4096 bytes, 16 per session | `network.UDPReceiveBuffers`, `network.UDPSessionBuffers` |
| UDP sends waiting for ARP | 8 | `network.UDPPendingSends` |
| UDP payload | 4096 bytes; MTU less 28 off the host | `udpapi.MaxPayload` |
| UDP session pools | TX 4 slots, RX 8 slots, 4096 bytes each | `udpapi.TXSlots`, `udpapi.RXSlots` |
| DNS message | 1232 bytes either way | `dns.DatagramSize` |
| resolver cache | 256 entries, 8 addresses each | `dns.MaxCacheEntries`, `dns.MaxAddresses` |
| resolver lookups | 4 transactions, 8 waiters each, 16 sessions | `dns.MaxTransactions`, `dns.MaxWaiters`, `dns.MaxClients` |
| CNAME chain | 8 links | `dns.MaxCNAMEChain` |
| hosts table | 64 records, names up to 248 bytes, `localhost` built in | `config.MaxHosts`, `config.MaxHostName` |

## Failure

A driver that dies, sends `Fault`, or is replaced unbinds: `netd` closes the
old channel and pools, fails pending echoes with `DriverFault`, and reports
`down` until the new driver's `Ready`. PID 1 starts the replacement with a
new generation, channel and pools, so nothing the old driver sent can reach
the new binding. A `netd` restart takes the driver with it, and commands
connected to the old service see `PeerDied`.

A `netd` restart also ends every UDP session and the resolver's status
session. PID 1 hands the resolver new ones, lookups waiting on the old ones
fail with `Reconfigured`, and the cache stays; a command holding a UDP
session sees `PeerDied`. A `resolved` restart loses the cache and nothing else: `ping` with a
numeric address keeps working.

A missing NIC, a refused one, or network services that fail leave storage,
the installed system and the shell running; `net status` reports `net0`
`down`, `lo` keeps answering, and `ping` to another address fails within its
deadline. Recovery mode runs none of the network services and carries no
network command. See
[init](../userspace/init.md#network-services) for the supervision rules.

## Signed trust updates

Native `trust` and `fetch` reuse `userspace/fetchtransport`; `trust` does not
implement a second resolver, TCP, HTTP or TLS client. The immutable system carries
bootstrap roots, an Ed25519 update public key and the build-configured HTTPS
package base URL. HTTPS protects transport, but the signed trust package is
authorized by the immutable trust-update public key.

`userspace/truststore` verifies canonical metadata, the exact PEM digest and the
Ed25519 signature before root parsing. `userspace/truststate` selects the highest
surviving published signed generation under `/state/security/generations`.
It rejects corrupt state and reports the immutable fallback. Each TLS adapter
keeps its selected roots through its lifetime, including all three update
component downloads. Installing roots changes future client processes, not a
live TLS connection. Neither network service gains trust policy or write access.
