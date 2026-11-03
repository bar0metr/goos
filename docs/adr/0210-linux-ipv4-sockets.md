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
   | GLNXSOCK1, one session per Linux family
   v
linuxsockd  -- Linux socket objects, readiness, options
   | GTCP version 2          | GUDP version 2
   v                         v
netd  -- TCBs, UDP endpoints, bind conflicts, routing, ARP
```

`linuxsockd` (`/system/services/linuxsockd`, logic in `userspace/linuxsock`) is a
supervised native service that PID 1 starts after `netd`. It holds one
GLNXCTL1 control channel from PID 1, one GTCP session and one GUDP session
with an explicit endpoint quota, and the service side of every family
session. It holds no VFS, configuration, network-admin, device, IRQ, DMA or
launch authority.

### Family sessions

A family session is one channel and one 17-page pool
(`userspace/linuxsockapi`). PID 1 creates both for every Linux launch while
`linuxsockd` is running and grants them to the launch as two more roles,
after `vfs` and the optional `linux-tty`:

| Role | Type | Rights |
| --- | --- | --- |
| `linux-net` | channel | Read, Write, Wait |
| `linux-net-pool` | memory | Read, Write |

It sends the other channel end and a second handle to the pool to
`linuxsockd` with GLNXCTL1 `AddFamily`, under a family token. The kernel
keeps both roles for the family, not for one process: `fork`, threads and
`execve` share the session, and an independent launch has its own. A launch
without the roles runs; `socket(AF_INET, ...)` fails with `ENETDOWN`.

The pool is a readiness page, eight TX slots the kernel writes and eight RX
slots the service writes, 4096 bytes each. A slot changes owner only by
message, as in GUDP1: Write and SendTo lend a TX slot until their reply; a
Read reply with data lends an RX slot until the kernel's Release
notification. No message carries an address.

### GLNXSOCK1

Requests are fixed 48-byte bodies with a nonzero correlation; replies echo
it. Unused fields and reserved bytes are zero; a malformed message fails the
family session. The operations are Open, Bind, Listen, Accept, Connect,
Read, Write, SendTo, Shutdown, GetSockName, GetPeerName, GetOption,
SetOption, GetPendingError, QueryReady, Close and Cancel, plus two
notifications: Release from the kernel and ReadyChanged from the service.

No request waits in the service. An operation that cannot finish answers
WouldBlock, or InProgress for a connect, and the kernel sleeps on readiness.
An interrupted Linux call therefore never leaves a read, write, accept or
connect in flight. The one exchange in flight is a reply not yet read, and
the kernel reads every reply, so it can still apply one whose caller went
away: unread RX bytes go back with Release, which tells the service to
return them first on the next Read of that socket, and an accepted child
nobody installs is closed.

Socket identities carry the session generation, a per-slot generation and
the family table index. A reused slot or a new session never matches an old
identity.

### Readiness

The service publishes each socket's state in the readiness page: a Ready
mask (in, out, error, hangup, read hangup), the bytes a read returns now,
and the pending error. Each entry is guarded by a sequence count, odd while
it is rewritten. After a change the service sends ReadyChanged only when
the token word says none is outstanding; the kernel clears the token before
it reads the entries. A change after that read draws a new notification, and
a failed send leaves the token clear, so a full queue loses no change.

The kernel evaluates a socket for `poll`, `select` and a blocking call from
the page, with no message. A waiter registers on the family channel, then
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
- Native protocol versions move to GTCP 2, GUDP 2 and GNETCTL 5. Every
  client builds against the new codecs, and each decoder rejects any other
  version.

## Amendments

- `kill`, `tkill` and `tgkill` reach any process of the caller's family,
  where ADR 0166 allowed only the caller and its descendants. A Linux server
  or test helper commonly signals its parent; the family stays the boundary,
  and a target outside it is still `ESRCH`.
- The kernel holds 16 socket contexts, the families `linuxsockd` serves. A
  family beyond them runs without sockets. With a context for every process
  the kernel image grew past 8 MiB, where OVMF reserves memory, and the
  firmware moved it.
