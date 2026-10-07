# ADR 0194: A userspace DNS resolver

## Status

Accepted. Builds on ADR 0191, ADR 0192 and ADR 0193.

## Context

Commands could reach only numeric addresses. Resolving names needs DNS over
UDP, a cache, and somewhere to keep the servers a DHCP lease names, but GoOS
has no UDP sockets for applications, and an IPC message holds 256 bytes,
less than a DNS answer. Name resolution must not widen anyone's network
authority, put DNS parsing in the kernel, or make a DNS failure break the
numeric path that already worked.

## Decision

### The service

`resolved` is a stub resolver with a cache. It forwards queries to recursive
servers and does not walk the DNS tree itself. PID 1 runs it from the
installed system in normal mode, next to `netd`. For a name it tries, in
order:

1. `localhost` and names under it, answered `127.0.0.1` at once and never
   sent anywhere (RFC 6761);
2. names under `.local`, refused as `UnsupportedNameScope`: they belong to
   multicast DNS, which GoOS does not implement, and must not go to unicast
   DNS (RFC 6762);
3. the hosts table of ADR 0193;
4. its cache;
5. the upstream servers.

Commands parse numeric addresses themselves and never send one to the
resolver.

The resolver core is `userspace/dns`, pure Go with fixed tables, driven by
the service with time and datagrams. It sends A/IN queries with RD set and
EDNS(0) at 1232 bytes (RFC 6891, RFC 9715), without the DO bit. A server
that answers FORMERR without OPT is asked again once without EDNS. Each
resolution has one absolute deadline, `query-timeout-ms` times `attempts`;
servers are asked one at a time in list order, round by round, each wait
`query-timeout-ms` divided among the servers with a 500 ms floor and
doubling per round. A server that answers SERVFAIL, REFUSED, a malformed or
truncated message leaves the rotation for that resolution. CNAMEs are
followed up to 8 links with loop detection, inside the same response when it
carries the target's A records and by a new query otherwise. Identical
lookups share one transaction, up to 8 waiters each.

Every reply is checked before any of it is used: QR set, opcode QUERY, the
transaction ID and server of the query it answers, exactly one question
equal to the one asked, counts and lengths within the message, compression
pointers that point backwards inside it with a bounded number of hops, labels
of at most 63 bytes and names of at most 255. A reply that fails does not
touch the cache. A reply with TC set is not used; with no TCP in GoOS there
is no fallback, and when every server truncates the result is `Truncated`.

The cache holds up to 256 entries, of which `system.dns.cache.entries` may be
used; lowering it evicts at once, expired entries first, then the least
recently used. Positive answers keep up to 8 addresses for the smallest TTL
in the answer, capped by `cache.max-ttl-sec`; TTL 0 is returned and not
kept. NXDOMAIN and NODATA are kept apart, for the SOA's negative TTL (RFC
2308) capped by `cache.max-negative-sec`, and not at all without an SOA.
Timeouts, SERVFAIL, REFUSED, truncation and send failures are kept as
failures for `failure-cache-sec`, at most 300 s (RFC 9520). A change of
servers keeps the cache; `resolve flush` empties it, never the hosts table
or `localhost`.

### Servers

DHCP option 6 gives the lease's servers. The stack keeps the first four
usable ones in the server's order, rejecting zero, loopback, broadcast and
multicast entries; they live and die with the lease, survive a tuning reload,
change with a new ACK and go with an expiry, NAK, relinquished address or
lost link. They are never written to `/state/config`. `netd` sends the list
to the resolver under a growing generation, and the resolver ignores an
older one. `system.dns.server1..4` add up to four configured servers after
the DHCP ones, skipping duplicates, so they serve as fallback in DHCP mode
and as the whole list in static mode. A reload reads all ten `system.dns.*`
settings as one candidate and changes nothing if one fails.

### The transport

The resolver reaches DNS only through a transport `netd` serves:
`GNETDGRAM1` on a channel PID 1 creates for the pair, plus two shared pools
of eight 1280-byte slots. The query pool is writable by the resolver and
read-only for `netd`; the reply pool is the reverse. A slot changes owner
only by a message: `Send` hands a query slot to `netd` and `Accepted` hands
it back at once, after `netd` copies the datagram into its stack; `Result`
hands a reply slot to the resolver and `Release` returns it. Each side
refuses a message about a slot it does not expect.

`netd` sends each query from a fresh random port in 49152..65535 (RFC 6335)
and accepts one reply from the server's address and port 53 to that port
and its own address, until the exchange's deadline (RFC 5452). The
resolver's transaction IDs come from its generator, seeded like `netd`'s from
the kernel's `RandomRead`. The transport allows destination port 53 only, at
most 1232 bytes either way, and eight exchanges at once. No command can open
it.

PID 1 makes a new transport, with a new generation, whenever `netd` or
`resolved` starts. Exchanges on the old one fail as `Reconfigured`, and a
late reply cannot reach the new one; the cache survives a restart of `netd`.

### Clients and rights

Commands speak `GRESOLV1` on a private session PID 1 opens. A request is the
opcode and a name of up to 253 bytes, which fits the payload. Results are
`OK`, `NotFound`, `NoData`, `NoServers`, `Timeout`, `TemporaryFailure`,
`Truncated`, `InvalidName`, `UnsupportedNameScope`, `PermissionDenied`,
`Busy`, `Reconfigured` and, for reload, `ConfigInvalid`. A command asks for
`GrantResolve`, which may resolve and read status, or `GrantResolveAdmin`,
which may also flush and reload. `ping` asks for the first and `resolve` for
the second; the two grants are separate from the network ones, so resolving
grants no echo and pinging grants no name lookups.

## Consequences

`resolve NAME`, `resolve status`, `resolve flush`, `resolve reload` and
`ping NAME` work. When `resolved` is down, `ping` with a numeric address
still works; when no server is known, `localhost` and the hosts table still
answer and other names give `NoServers`.

What remains unsupported: DNS over TCP, so a truncated answer fails;
multicast DNS and `.local`; IPv6 and AAAA; DNSSEC; encrypted DNS; search
domains; `/etc/resolv.conf` and `/etc/hosts`; UDP and TCP sockets for
applications; the Linux network calls.

The network gate runs a host-side peer, `tools/netpeer`, as hub, DHCP server
and DNS server on a private segment, so name resolution is tested without
the public Internet: option 6, a cache hit without a second query, a flush
forcing one, a CNAME, NXDOMAIN, a configured server, a hosts record and a
ping by name.

`resolved` parses untrusted DNS replies; the transport's port and address
checks and the core's validation stand between them and the cache. It holds
no device, no general network session and no configuration write right.
