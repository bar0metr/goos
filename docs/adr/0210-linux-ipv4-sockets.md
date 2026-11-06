# ADR 0210: Linux IPv4 sockets

## Status

Accepted. Builds on ADR 0138, ADR 0168, ADR 0195, ADR 0196 and ADR 0206.

## Context

Static Linux programs run on GoOS, but every socket call returned `ENOSYS`.
`curl`, BusyBox `wget`, `nc` and `httpd`, and anything that calls
`getaddrinfo`, could not use the network. TCP and UDP already live in
`netd` (ADR 0195, ADR 0196) behind GTCP1 and GUDP1, which move payload
through userspace shared pools.

Two shortcuts were rejected. A TCP/IP stack in the kernel would duplicate
`netd` and its bounds. Teaching the kernel to speak GTCP1 directly would make
the kernel a client of a userspace pool protocol it does not own, and GTCP1
parks reads, writes, accepts and connects inside `netd` with no way to
cancel them, so an interrupted Linux call would leave an operation in
flight whose bytes nobody owns.

## Decision

### Components

```text
Linux program
   | socket syscalls
   v
kernel/compat/linux  -- descriptors, errno, signals, poll/select, sockaddr
   | GLNXSOCK3, one session per Linux family
   v
linuxsockd  -- Linux socket objects, readiness, options
   | GTCP version 3          | GUDP version 2
   v                         v
netd  -- TCBs, UDP endpoints, bind conflicts, routing, ARP
```

`linuxsockd` (`/system/services/linuxsockd`, logic in `userspace/linuxsock`) is a
supervised native service that PID 1 starts after `netd`. It holds one
GLNXCTL2 control channel from PID 1, one GTCP session and one GUDP session
with an explicit endpoint quota, and the service side of every family
session. It holds no VFS, configuration, network-admin, device, IRQ, DMA or
launch authority.

### Family sessions

A family session is one channel and one 76-page pool
(`userspace/linuxsockapi`). PID 1 creates both for every Linux launch while
`linuxsockd` is running and grants them to the launch as two more roles,
after `vfs` and the optional `linux-tty`:

| Role | Type | Rights |
| --- | --- | --- |
| `linux-net` | channel | Read, Write, Wait |
| `linux-net-pool` | memory | Read, Write |

It sends the other channel end and a second handle to the pool to
`linuxsockd` with GLNXCTL2 `AddFamily`, under a family token. The kernel
keeps both roles for the family, not for one process: `fork`, threads and
`execve` share the session, and an independent launch has its own. A launch
without the roles runs; `socket(AF_INET, ...)` fails with `ENETDOWN`.

The pool is five readiness pages, a ring page, six snapshot pages for
diagnostics, eight TX slots and eight RX slots of 16 KiB each. The channel carries the control plane and the
doorbells; payload never travels in a message, and no message carries an
address.

### Data plane

A Linux send copies the program's bytes into TX slots the kernel owns and
commits each slot on the submit ring of the ring page: slot, length, socket,
a generation, and a datagram's destination. The call returns once its
slots are committed. `linuxsockd` queues the slots per socket in ring order
and drains each queue into `netd`, then hands every slot back on the
complete ring with the generation the commit gave it. One socket holds at
most four TX slots, 64 KiB, so a stream leaves room for the family's other
sockets; a whole 64 KiB `write` enters the pool in one call.

For receive, `linuxsockd` keeps reads parked in `netd` for every socket that
can receive, each into an RX slot it reserved. A read that `netd` answers
fills its slot, and the slot goes on the complete ring for its socket, with
a datagram's source. The kernel queues published slots per socket in ring
order and copies from them straight into the Linux buffer; a slot goes back
on the submit ring once reads consumed all of it, or at once for a
datagram. One socket holds at most four RX slots, and the service always
leaves two free, so sockets with unread data cannot starve another: a
socket without a read ahead gets its data through a Read request, which
reads from `netd` into any free slot.

Ownership is explicit on both sides. A TX slot is free, being filled by one
call, committed, or held by the service; an RX slot is free, under a read,
published, or held by the kernel. A slot handed back with the wrong
generation, a slot published twice, a full ring or an entry outside the
layout fails the family session.

Each ring has a doorbell. The kernel sends a Doorbell notification only when
it claims the doorbell token, which the service clears before it drains the
submit ring; the service drains the ring before every message it takes, so a
Doorbell lost to a full queue loses nothing. The complete ring shares the
readiness token and its ReadyChanged notification.

### Progress under backpressure

Every piece of work `linuxsockd` defers has an event that resumes it. A
reply from `netd` resumes what waited for a native TX slot. A send that
finds the queue to `netd` full sets the link's write-blocked mark, and the
event loop then waits for that channel to turn writable as well as
readable: a queue full of requests that park in `netd`, such as reads and
readiness waits, empties without a single reply, so a reply cannot be the
signal. When the channel turns writable the link sends the RX slot releases
it held back, then the core retries every submission that found the queue
full. A readiness refresh, which a stream needs after a write that took only
a prefix, never disappears: one asked while another is out goes again once
that one answers, and one that finds no free operation stays owed for the
pump. A validation build checks after every event that each stream waiting
for room in `netd`'s send buffer has a held wait, a refresh or a resend
registered.

`netd` keeps the same rule toward its clients: a reply that finds a
client's queue full waits in the session, at most 128 of them, and goes
when the channel turns writable.

### GLNXSOCK3

Requests are fixed 48-byte bodies with a nonzero correlation; replies echo
it. Unused fields and reserved bytes are zero; a malformed message fails the
family session. The operations are Open, Bind, Listen, Accept, Connect,
Read, Write, SendTo, Shutdown, GetSockName, GetPeerName, GetOption,
SetOption, GetPendingError, QueryReady, Close and Cancel, plus two
notifications: Doorbell from the kernel and ReadyChanged from the service.
Read, Write and SendTo carry no data. The kernel sends Read only when it
holds nothing for the socket; the reply says to look at the complete ring
again, to wait, or that the stream ended. It sends Write or SendTo only when
the readiness entry does not let it commit a send, and the reply is the
error the send reports, or OK once the socket takes sends: an unbound
datagram socket binds to an ephemeral port first.

No request waits in the service. An operation that cannot finish answers
WouldBlock, or InProgress for a connect, and the kernel sleeps on readiness.
An interrupted Linux call therefore never leaves a read, write, accept or
connect in flight. The one exchange in flight is a reply not yet read, and
the kernel reads every reply, so it can still apply one whose caller went
away: an accepted child nobody installs is closed. Bytes a send committed
belong to the socket: a signal after the commit cannot take them back, and a
call interrupted after some slots went returns the bytes it committed.
Received slots stay queued for the socket across an interrupted read.

Socket identities carry the session generation in the high 32 bits, a
20-bit per-slot generation, and the family table index in the low 12 bits.
A reused slot or a new session never matches an old identity.

### Readiness

The service publishes each socket's state in the readiness page: a Ready
mask (in, out, error, hangup, read hangup), the bytes `netd` holds for a
read, the pending error, and whether the kernel may commit sends now and,
for a datagram socket, whether it has a fixed peer. The kernel adds what it
holds itself: queued RX slots make a socket readable and count in FIONREAD,
and a socket that sends through slots is writable only while it has slot
credit. The kernel reads an entry before it drains the complete ring: the
service publishes a slot before the entry that stops counting its bytes, so
the end of a stream never overtakes its last bytes. Each entry is guarded by a sequence count, odd while
it is rewritten. After a change the service sends ReadyChanged only when
the token word says none is outstanding; the kernel clears the token before
it reads the entries. A change after that read draws a new notification, and
a failed send leaves the token clear, so a full queue loses no change.

The kernel evaluates a socket for `poll`, `select` and a blocking call from
the page, with no message. A waiter registers on its socket's wait key, then
reads the page again before it sleeps, so a change between the check and the
sleep wakes it. Readiness is level-triggered.

The service learns readiness from `netd` without consuming data. GTCP and
GUDP version 2 add a readiness query and a readiness wait that `netd` holds
until the endpoint's mask differs from the one the request names. The
service keeps one such wait per socket.

### Native protocol changes

GTCP version 2 (`userspace/tcpapi`) grows requests to 24 bytes and replies
to 32:

- byte 17 of a request is a flags byte: NoWait for Accept, Connect, Read and
  Write, Peek for Read, ReuseAddr for Bind. A flag on any other opcode makes
  the request malformed;
- without NoWait, Accept, Connect, Read and Write park in `netd` as in
  version 1. With it, Read and Accept answer WouldBlock, Write takes what
  fits and answers WouldBlock only when nothing fits, and Connect answers
  InProgress with the chosen four-tuple. A loopback connect can finish
  inside the request; its reply is then the final result;
- `OpReady` returns the endpoint's mask, the readable byte count and the
  send buffer room. `OpWaitReady` names a mask in bytes 18 and 19, and
  `netd` holds it until the endpoint's mask differs, answering at once when
  it already does. An endpoint holds one wait; `OpClose` answers it with
  Closed before its own reply. Neither consumes a byte;
- `OpError` returns the pending transport error in reply byte 3 and clears
  it. A Connect, Read or Write reply that carries the error clears it too;
- `OpOption` reads the effective send MSS, send buffer, receive buffer or
  TTL, selected by request byte 20.

GUDP version 2 (`userspace/udpapi`) grows requests to 24 bytes and replies
to 32. Request byte 2 holds the flags (NoWait and Peek on RecvFrom,
ReuseAddr on Bind) and byte 3 the option selector. It adds `OpReady`,
`OpWaitReady` with the same rules as GTCP, and `OpOption` for the TTL and
for the largest payload the path to a named destination carries. A Send or
SendTo larger than its path, max-payload locally or the MTU less 28 bytes
on the wire, answers MessageSize and sends nothing. A RecvFrom reply's
length is the whole datagram.

GNETCTL version 5 carries an endpoint quota in byte 2 of
`ControlAddTCPSession` and `ControlAddUDPSession`, 1 to 64, and zero in
every other request. Commands and `resolved` get 8, which
`max-endpoints-per-session` can lower; PID 1 asks for 64 for `linuxsockd`
(`openLinuxSocketSessions` in `userspace/cmd/init`), and the tuning does
not lower a grant above 8. `netd` holds 96 TCP and 96 UDP endpoints, 16 TCP
listeners and 128 UDP receive buffers, of which one session holds at most
64.

These numbers were the limits when this ADR was accepted. ADR 0213 replaced
the TCP side: the control quota is 16 bits, PID 1 grants `linuxsockd` 512
endpoints per protocol, TCP allocates endpoints and rings per connection up
to a tuned 1024 endpoints, and the native backlog is 128. Its revision
grows the UDP registry the same way, 1024 endpoints by default.

SO_REUSEADDR follows Linux. A TCP bind conflicts with an endpoint or
TIME_WAIT four-tuple on the same port and an overlapping address, equal or
either one the wildcard, unless both carry the flag and the existing one is
not a listener. TIME_WAIT keeps the flag of the connection it came from,
and an accepted child takes its listener's. A UDP bind that overlaps
succeeds only when both endpoints carry the flag; the endpoint bound last
then receives unicast.

### Kernel

A socket is an open-file description of kind `FDSocket` whose object is the
socket identity. Descriptor rules are the existing ones: duplicates and fork
share the description and its `O_NONBLOCK`; close-on-exec is per
descriptor; the last reference sends Close. `fstat` reports `S_IFSOCK`,
`lseek` is `ESPIPE`, `fsync` is `EINVAL`.

The kernel implements socket calls 41 to 55 and 288, sockets in `read`,
`write`, `readv`, `writev`, `ioctl(FIONREAD)`, `fcntl`, `poll` and `ppoll`,
and `select` and `pselect6` over the same readiness code as `poll`.
Blocking calls sleep through the syscall continuation and honor
`SO_RCVTIMEO` and `SO_SNDTIMEO`, handled signals and `SA_RESTART`. A reply
wait is bounded by a deadline and is not interrupted by a signal, because
the exchange it waits for always ends at once.

`linuxsockd` dying ends the session's channel: every socket of the family
fails with `ENETRESET` and waiters wake. When the service is back, PID 1
gives each running Linux launch a new session through the native call
`LinuxNetAttach(process, channel, pool)`, which requires PID 1's handle to
the launch's process. A `netd` restart keeps the family sessions:
`linuxsockd` fails every socket that the dead native sessions backed, and
takes fresh native sessions from PID 1. A failed socket reports every
readiness condition and holds `ServiceLost` as its pending error; each later
request on it but Close, the name queries, `QueryReady` and
`GetPendingError` answers `ServiceLost`, and new sockets use the new
sessions.

### DNS

`resolved` answers on `127.0.0.1:53` over UDP and TCP with its cache, hosts
table and upstream path. The image carries a read-only `/etc` with
`resolv.conf` (`nameserver 127.0.0.1`), `hosts` and `nsswitch.conf`, so a
static libc resolves names with ordinary socket traffic.

## Consequences

- The Linux ABI gains real IPv4 TCP and UDP; IPv6, `AF_UNIX`, raw and packet
  sockets, netlink, `epoll` and ancillary data fail explicitly.
- A payload byte is copied twice more than a native client copies it:
  user memory to the family pool, then to the GTCP pool.
- A send succeeds once its bytes are in the pool. A transport error that
  `netd` reports later becomes the socket's pending error, as a Linux
  socket's does for bytes already in its send buffer.
- Native protocol versions move to GTCP 2, GUDP 2 and GNETCTL 5, and GTCP 3
  for the session of `linuxsockd`. Every client builds against the new
  codecs, and each decoder rejects any other version.

## Amendments

- `linuxsockd`'s TCP session speaks GTCP 3, which PID 1 names in GNETCTL
  version 6: 16 KiB slots, eight each way, a stream offset on every write so
  pipelined writes of one endpoint keep their order, and up to four parked
  reads per endpoint. Other sessions keep GTCP 2. See the GTCP 3 section of
  [the networking architecture](../architecture/networking.md).

- `kill`, `tkill` and `tgkill` reach any process of the caller's family,
  where ADR 0166 allowed only the caller and its descendants. A Linux server
  or test helper commonly signals its parent; the family stays the boundary,
  and a target outside it is still `ESRCH`.
- The kernel holds 16 socket contexts, the families `linuxsockd` serves. A
  family beyond them runs without sockets. With a context for every process
  the kernel image grew past 8 MiB, where OVMF reserves memory, and the
  firmware moved it.
- `eventfd` and `eventfd2` are part of the ABI: curl 8.22's resolver thread
  wakes its transfer loop through one, and failed with "Out of memory"
  without it.
- `netd` answers a held readiness wait only when the mask changes. After a
  read that uses up the readable count it last reported, or a write that
  finds the send buffer full, `linuxsockd` asks for the readiness again
  instead of trusting the old mask; without that a reader and a writer on
  one connection could both sleep.
- A nonblocking stream connect is always `EINPROGRESS`, also when `netd`
  finished a loopback connect inside the request, as Linux reports it.
- A dead network driver makes `netd` drop the interface address and abort
  the connections bound to it. Linux sockets follow that state: such a
  connection fails with a network error and new ones work once the driver
  is back.
- A stream stalled now and then, longest in the diagnostic build. The
  stress target caught it with a snapshot: the writer's socket in
  `linuxsockd` waited for send room with four slots queued and its wait
  held in `netd`, while `netd`'s send buffer for it was empty. A write had
  taken only a prefix; the refresh that followed saw the full buffer, but
  the held wait still named the mask with room, and the buffer emptied
  again before `netd` looked at the wait, so the wait never answered. The
  stream went on only when `netd`'s 30-second idle wake came round. A
  refresh that sees a mask other than the one the held wait names now asks
  again until the two agree or the wait answers, and a validation build
  checks after every event that a socket waiting for room has something
  registered that will tell it. Two more holes of the same family went with
  it: `netd` delivered a session's parked requests in one pass, so an
  answer that made an earlier endpoint ready waited for the next event (it
  now repeats passes until one answers nothing), and `linuxsockd` retried
  work held back by a full native queue only on a reply (the writable rule
  above). A host model of the service against the real native stack, with
  queues of two to four messages and 300 orders of events, reproduces the
  last one when the writable retry is taken out.
- A fixed count of delivery passes left the same hole open for a chain of
  answers longer than the count. `Deliver` keeps 16 passes as one call's
  budget and reports whether its last pass still answered something; a
  loopback read or write reaches another session's endpoint at once, so
  `netd` also runs a second round over its TCP sessions when two or more
  ran and one made progress. While either says work is left, the event
  loop polls the driver and the sessions with a zero timeout and delivers
  again instead of sleeping. Before a validation build sleeps it checks
  that no parked request could finish now, and stops with
  `NETD_DELIVERY_STALLED` naming the session, endpoint and request. Host
  tests chain 40 endpoints so each answer wakes the one before it in the
  table, and settle them with no clock advance or other event.
- `netd` hands a TCP segment between two endpoints of its own stack to the
  receiving state machine as a logical segment, with no header, checksum or
  parse: the same lookup, sequence, window, congestion, FIN and TIME_WAIT
  steps run as for a parsed one, and a test runs one connection both ways
  and compares the endpoints field by field. Such a segment carries up to
  16 KiB, the stream ring's size, instead of the 1460 bytes the reorder
  slots bound a wire segment to, and stops at the end of the sender's ring
  instead of wrapping.
- `netd` takes at most eight requests of one session per pass of its event
  loop and delivers what they made ready before the next batch.
- A pure ACK or window update that finds `netd`'s eight-frame transmit
  queue full is marked and goes on the next pass with a free frame. It was
  dropped before: a host streaming to a guest program saw a zero window,
  waited out its 204 ms retransmit timer 282 times in 32 MiB, and moved
  0.52 MiB/s. It now moves 31 MiB/s with no retransmission.
- A segment advertises a window whose right edge moves only by a segment or
  half the receive buffer (RFC 1122 4.2.3.3); an ACK that offered each few
  hundred bytes a reader freed let a sender cut the stream that small.
- A Linux `read` whose buffer reached a page of the program's image not yet
  loaded returned the bytes up to that page and left it out, so every later
  read through the buffer stopped there too. The first stream of each new
  program read 512 to 1023 bytes a call, 38,000 calls for 32 MiB, and ran at
  a third of the rate of the streams after it. The call now fails before
  any effect, the dispatcher loads the page, and the call runs again, as
  `rangeLimit` already did for sockets.
- A GTCP 3 or GUDP request names, in a byte that was reserved, the RX slots
  its sender returns, and `netd` takes them back before it handles the
  request. `linuxsockd` returns the slot of the read it just copied on the
  read it posts next, which saves `netd` a `Release` message per direction
  of a request and reply round. A GUDP `RecvFrom` reply carries the
  endpoint's readiness after the datagram, so `linuxsockd` no longer asks
  `Ready` for the next datagram's length after each one.
- `netd` reads the clock once per pass of its loop, after the wait, and the
  requests and frames of the pass share that time; it read the clock once
  per message before. The stack's TCP scans walk a mark per slot in use: a
  slot holds two 16 KiB rings, and the scans read the state of all 96.
- Socket capacity moved from fixed tables to quotas. `linuxsockd` held 64
  sockets in one static table, 32 per family. It now keeps sockets and
  operations in 64-entry chunks it allocates as use grows, with a stack of
  free entries, so opening and closing cost the same at any size and no
  lookup scans the table. Three quotas (`Limits` in
  `userspace/linuxsock/registry.go`) bound the sockets, the ones still
  opening or accepted ahead included: 1024 across families, 512 per family,
  and 32 that the service keeps back for each live family. A family at its
  own quota gets `FamilyLimit`, which `socket` and `accept` report as
  `EMFILE`, since closing its own sockets frees the room. A full service
  gets `ServiceLimit`, reported as `ENFILE`. The service checks the family
  quota first, then admits a socket only if the reserves the other families
  still lack stay free: with 16 families each one keeps 32 sockets, half of
  the service in all. `SetLimits` changes the quotas while no socket is
  open; it refuses a family quota above the protocol's 512 or reserves that
  do not fit the service quota together.

  The protocol bound per family is a wire bound, so the session protocol
  moved to GLNXSOCK3. The readiness entries of 512 sockets take five pages,
  the socket snapshots move from the ring page to six pages of their own,
  and the pool grows from 66 to 76 pages. An identity's index field grew
  from 8 to 12 bits and its generation shrank from 24 to 20. The kernel
  reads each entry from the page that holds it, keeps its socket slots on a
  free stack, and maps the service's index to its slot, so a reply that
  names a socket resolves without a scan. GLNXCTL2's Status reply carries
  the service's use and quota, the family quota and reserve, and each
  family's use; the family snapshot in the ring page carries the same for
  its family, and the kernel's `linux-net-plane` diagnostic prints them.

  `netd` grants `linuxsockd` 512 endpoints per protocol (ADR 0213), and
  TCP and UDP sockets both reach that grant.

- `linuxsockd`'s TCP session moved to GTCP 4 (ADR 0214); PID 1 opens it
  with `tcpapi.Layout4` and the 512-endpoint quota. `linuxsock.RingLink`
  maps the core's native operations onto the rings. A write copies the
  family TX slot into a GTCP 4 TX slot and publishes `EntryWrite`; the
  family slot goes back on `EntryWriteDone`. A read posts one fill of
  credit, and `EntryData` fills the family RX slot before the link releases
  the GTCP 4 slot. Waits and readiness queries answer from netd's readiness
  page. A wait goes to netd only while a connect runs, or when the core
  marks it `NativeWake`: a stream with no read posted whose bytes the
  kernel does not hold, which learns of new bytes only through the wait. A
  stream socket opens its endpoint when a request first needs one, so a
  connect is one `OpOpenConnect`, and the warm pool keeps UDP endpoints
  only. A listener has netd accept up to four children ahead
  (`OpAcceptAhead`); `accept` takes them off the RX ring without a request.
  Close hands the endpoint to netd, which still sends the queued slots; the
  link gives closed endpoints up in batched `EntryForget`s. Before it
  sleeps, `linuxsockd` takes the rings, arms its doorbell and checks them
  once more; it waits on the TCP channel in the same WaitAny as the family
  channels. UDP kept GUDP 2 through `NativeLink` until GUDP 3 replaced it
  (below).

  The world tests (`userspace/linuxsock/world*_test.go`) run the core and
  the link against `tcpserve.Session4` and the real stack, each side
  sleeping after every exchange. A steady 1 MiB stream costs no channel
  request or reply, and 57 doorbells, 3.6 per 64 KiB; GTCP 3 costs 24
  channel messages per 64 KiB in `tcpring`'s comparison. 150 loopback
  connections (300 sockets) cost one `OpOpenConnect`, about one probe and
  four waits each: one for the connect, the rest for sockets that found no
  family RX slot free.

- A socket wait wakes only the calls whose socket changed. Before, a
  sleeping call registered on the family's session channel, so each
  ReadyChanged woke every task of the family that slept on a socket, and
  each took the family lock to drain the channel. Now a call that sleeps
  on a socket, and a `poll` or `select` that names one, waits on the
  socket's own key (`KindLinuxSocket`, the description's object) and on
  the session channel's close. linuxsockd's send drains the family's
  channel in the kernel right after its message lands (`dispatchLinuxNet`),
  as an executable page reply does: a reply resumes the call that waits
  for it, and a ReadyChanged or a ring entry wakes the sockets whose
  readiness entry has a new sequence or that a ring entry named. One drain
  of a session runs at a time; a drain asked for meanwhile makes the
  running one go round again. A call that finds its reply filed only after
  it blocked takes it back itself. A `poll` keeps each socket's wake count
  from its scan and publishes the socket again after it registers when the
  count moved. Closing a socket and failing a session wake every call on
  its sockets.
- linuxsockd's UDP session moved to GUDP 3 (ADR 0216), through
  `linuxsock.UDPRingLink`: a datagram goes out as a TX ring entry and comes
  in against posted credit, with the endpoint's readiness on its RX entry.
- A socket call takes a narrower lock set (`linuxSockets` in
  `kernel/locksets.go`): the family's lock, the registry for reading, the
  executable table and its address space. It leaves out the stream
  registry, its handle table and the shared memory table, which every
  family's calls and every service's sends share. A `read` or `write`
  starts under the narrow set and runs again under the file set when its
  descriptor is not a socket. The kernel reaches a session's pool through
  `linuxPools`, filled and retained when the session attaches and let go
  when the family's holder exits, so a socket call needs no shared memory
  lock.
- A socket change wakes only the waits it can satisfy. A TX slot coming
  back wakes writers, a datagram or a stream's bytes wake readers, and a
  changed readiness entry wakes for what the entry now shows; an error or
  a hangup wakes every wait.
- linuxsockd spins for up to 2048 checks of the rings' indices before it
  sleeps while netd holds one of its sends or a Linux program it just
  woke is about to answer, so that answer costs no doorbell and no wake.

## Validation

`make test-linux-network` passes 80 checks under QEMU: all 16 `netprobe`
loopback groups, which match a Linux 6.12 kernel, libc DNS through the
local server with the TCP fallback, TCP client and server against the host,
UDP, descriptor inheritance, BusyBox 1.38.0 `nc`, `wget` and `httpd`, curl
8.22.0, and the `net-driver`, `netd` and `linuxsockd` faults.

`make test-pve-linux-network` passes on the Proxmox lab with KVM, one socket
of four host cores and 1 GiB: the same groups, BusyBox `httpd` serving
`wget` and curl, and a `linuxsockd` fault. `netprobe bench` measured over
loopback (run 20261104T163631Z):

| Measurement | Result |
| --- | --- |
| TCP stream between two processes, 32 MiB | 7 MiB/s |
| TCP one-byte request and reply | 815 us |
| TCP connect, accept and close | 1362 us |
| UDP 64-byte request and reply | 879 us |
| write to a `poll` waiter's return | 236 us |

The same probe on Linux 6.12 under TCG measured 272 MiB/s, 169 us, 233 us,
137 us and 423 us. That first data path moved at most 4 KiB per GLNXSOCK
exchange and ran each exchange through `linuxsockd` and the native session
before it started the next chunk. Channel and readiness wakes go through
`WakeTaskID` and a reschedule IPI; the timer tick provides deadlines and
preemption and is not the ordinary socket wake path.

`make bench-pve-linux-network` measures the same host and VM shape with a
release build (OPT=2, DIAG=0, VALIDATION=0, HZ=100), two warm-up rounds and
nine measured ones, and reports medians. Host: Xeon Gold 6240, Proxmox 7.4,
QEMU 7.2. Native is the same operation over GTCP 2 and GUDP 2 from one
native process, without the Linux bridge.

| Measurement | 4 KiB exchanges | Shared data plane | Native |
| --- | --- | --- | --- |
| TCP stream, 32 MiB | 15.2 MiB/s | 71.7 MiB/s | 64.2 MiB/s |
| TCP one-byte request and reply | 418 us | 234 us | 119 us |
| TCP connect, accept and close | 842 us | 799 us | 658 us |
| UDP 64-byte request and reply | 399 us | 255 us | 88 us |
| write to a `poll` waiter's return | 165 us | 199 us | 51 us |

The diagnostic build counted, per 64 KiB of stream, 321 context switches
and about 170 reschedule IPIs before the data plane, and 25 switches and
12 IPIs after it; one stream write now takes four TX slots and one
doorbell. The median time from a wake to the woken task running stayed
between 7 and 13 us, so the scheduler is not what remains: the native
numbers show `netd` itself spending 120 us on a request and reply and
274 us on a connect.

Under KVM three defects showed that TCG had hidden: `WaitAny` refused a
writable wait on a channel, which `linuxsockd` uses while a family's queue
is full; the kernel failed a session when the service was preempted in the
middle of rewriting a readiness entry; and a server that clients close
first ran out of TIME_WAIT tombstones after about 128 connections a minute.


The same bench on the tree at 6c4afbf, a release build at HZ 100, 250 and
1000, two warm-up rounds and nine measured ones each (runs
20261105T133427Z and the two after it), against the targets this work set
and the medians at 1ccd4b0 before the fixes above:

| Measurement | Target | 1ccd4b0, HZ 100 | HZ 100 | HZ 250 | HZ 1000 |
| --- | --- | --- | --- | --- | --- |
| TCP stream, 32 MiB | 258 MiB/s | 45.3 | 148 | 150 | 142 |
| TCP one-byte request and reply | 178 us | 218 | 203 | 200 | 204 |
| TCP connect, accept and close | 245 us | 774 | 796 | 761 | 779 |
| UDP 64-byte request and reply | 144 us | 257 | 201 | 197 | 205 |
| write to a `poll` waiter's return | 248 us | 188 | 191 | 139 | 136 |
| host to guest stream | no regression | 0.52 | 29.6 | 29.8 | 29.7 |
| guest to host stream | no regression | 27.2 | 29.1 | 29.8 | 29.9 |
| native GTCP 3 stream | | 165 | 179 | 180 | 178 |
| native GTCP 3 request and reply | | 104 us | 99 | 100 | 101 |
| native GTCP 3 connect and close | | 642 us | 632 | 630 | 654 |
| native GUDP request and reply | | 90 us | 85 | 84 | 84 |

Only `poll` meets its target. The Linux numbers sit on top of the native
ones, which already miss two targets: a native connect and close takes
630 us against 245, and a native stream moves 179 MiB/s against 258. The
diagnostic build's loop stages show `netd` busy for most of a request and
reply round, about 30 us a request, most of it entering the kernel: a
clock read alone costs about 3 us. A Linux round costs `netd` six requests
and four replies where a native one costs two of each, and two more
processes take part. Meeting the targets needs fewer crossings per byte
and per round, such as a data path from the kernel to `netd` without
`linuxsockd` or a cheaper kernel entry, which this work does not change.

Five metrics differ by more than 10% across HZ. `poll` measures from a
writer that sleeps 2 ms first, so its clock starts at a tick, together with
the tick's own work: 191 us at HZ 100, 136 at 1000. Native open, accept and
close-server time 30 operations a round, and their medians move by 6 to
20 us between runs at any one HZ as well; no other metric moves with HZ.
At HZ 1000 a connect costs 2 to 4% more, which ten times the timer
interrupts explain.

A native accept once went unanswered, in round 3 at HZ 250 of an earlier
run of the same tree, after the stream phase of `netbench`; the connect
it waited for had finished. It did not recur in 66 later rounds or in 25
runs under QEMU. The bench now takes `net endpoints` when a step fails.

The same bench after TCP moved to GTCP 4 (ADR 0214), a release build at
HZ 250 with one warm-up and five measured rounds (run 20261106T124643Z on
14a523a3), against the medians at 4127a6c4 that another run of nine
rounds took:

| Measurement | Target | 4127a6c4 | 14a523a3 | Native |
| --- | --- | --- | --- | --- |
| TCP stream, 32 MiB | 258 MiB/s | 300 | 280 | 382 GTCP 4, 316 GTCP 3 |
| TCP one-byte request and reply | 178 us | 213 | 220 | 77 us GTCP 3 |
| TCP connect, accept and close | 245 us | 395 | 312 | 178 us GTCP 3 |
| UDP 64-byte request and reply | 144 us | 285 | 228 | 74 us |
| write to a `poll` waiter's return | 248 us | 222 | 173 | |

Stream and `poll` meet their targets; request and reply, connect and UDP
do not. The lab's runs spread by 10 to 15% from round to round: a run of
4127a6c4 with the accept fix measured 225 and 240 us for TCP request and
reply, 315 and 370 us for connect and 238 and 252 us for UDP.

Four changes went in. `udpserve` walked its whole endpoint table, 512
entries for linuxsockd's session, on every Deliver pass and every lookup;
it now walks the entries in use. A loopback send answers the receive it filled before its own
reply, so linuxsockd takes both on one wake. Every system call took the
global scheduler lock to drop a recorded wake; it now reads the wake bit
first and locks only when one is recorded, which the diagnostic build
counted at 313 scheduler lock acquisitions and 35 contended ones per TCP
request and reply round before. netd reads the clock once per lap instead
of twice. An idle loop that polled for woken tasks for 50 us before it
halted made every Linux metric slower on this host and was left out.

`netprobe` gained `bench-connect` and `bench-udp`, and the diagnostic pass
counts both and subtracts a baseline window, one launch with almost no
socket work, so the per-round figures leave out the dumps and the launch.
That pass puts the remaining cost in wakes and system calls. A TCP request
and reply round costs about 16 context switches, 8 wakes, 8.6 Linux calls
and 313 scheduler lock acquisitions, most of them native system call
entries in linuxsockd and netd; a wake took 14 us from `WakeTask` to the
task running on average. Both ends of the benchmark belong to one Linux family, whose lock
every Linux call holds, and a ReadyChanged wakes every task of the family
sleeping on the session channel: 4 to 5 contended acquisitions of the
family lock per round. In a UDP round netd never waited: it spent 174 us
of each 181 us round in its loop, 39 us of it in Deliver passes, on four
requests and four replies. A socket call that waits for linuxsockd
costs 80 to 100 us against 30 to 40 us for a native request to netd.
Meeting the remaining targets needs fewer hops per round, such as waking
only the family task whose socket changed and carrying UDP through shared
rings as GTCP 4 does for TCP, or a cheaper kernel entry and wake.

The bench after the targeted wakes and GUDP 3 (run 20261106T160316Z at
a317ed30, two warm-up and nine measured rounds) and after the narrower
lock set (run 20261106T170650Z at 13a1f4af), medians:

| Measurement | Target | 14a523a3 | a317ed30 | 13a1f4af | Native |
| --- | --- | --- | --- | --- | --- |
| TCP stream, 32 MiB | 258 MiB/s | 280 | 293 | 298 | 497 GTCP 4 |
| TCP one-byte request and reply | 178 us | 220 | 233 | 232 | 15 us GTCP 4 |
| TCP connect, accept and close | 245 us | 312 | 298 | 301 | 56 us GTCP 4 |
| UDP 64-byte request and reply | 144 us | 228 | 228 | 223 | 18 us GUDP 3 |
| write to a `poll` waiter's return | 248 us | 173 | 197 | 177 | |

None of the three changes moved a request and reply round. The diagnostic
pass at a317ed30 shows what they did remove: contended acquisitions of
the family lock fell from 4.2 to 0.07 per TCP round, and readiness wakes
from 4.6 to 4.0. It also shows where a round goes.
linuxsockd runs 158 to 168 us of CPU in each 180 to 194 us UDP or TCP
round, netd 75 to 128 us. Each half round crosses four processes: the
writer's `write` rings linuxsockd, linuxsockd hands the datagram to netd,
netd answers on the ring and rings linuxsockd, and linuxsockd publishes
to the reader's ring and wakes it. Native GUDP 3 crosses two, from one
process that never sleeps. The global stream and shared memory locks
spun for about 45 and 37 us a round at a317ed30; the narrower set took
the Linux calls off them, and the medians stayed where they were, so the
time sits in the crossings and in linuxsockd's own loop. Meeting the
remaining targets needs a data plane from the kernel to netd's rings
without linuxsockd between them, under a mapping PID 1 scopes to one
family.
