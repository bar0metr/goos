# ADR 0193: Loopback interface and the hosts table

## Status

Accepted. Extends ADR 0191 and the configuration service of ADR 0074.

Supersession note (2026-10-07): the resolver transport mentioned here was
replaced by generic UDP in [ADR 0195](0195-generic-ipv4-udp.md). UDP to a
loopback address now reaches the endpoint bound there, with no network
device.

## Context

The network stack had one interface, net0, and nothing else. `ping
127.0.0.1` was refused as a non-unicast address, and a machine without a
NIC had no network at all. Every later local service, a resolver among them,
needs an address that always exists and never leaves the machine.

Names had nowhere to live either. A local name such as a NAS on the LAN
needs a fixed mapping the operator can change, and `localhost` must mean the
loopback address whatever any file or server says (RFC 6761). The
configuration service already stores typed, bounded settings, but its text
values hold 64 bytes: a hosts table in one setting would either be tiny or
force a global limit up for one consumer.

## Decision

### lo

`netd` owns a loopback interface, `lo`, from the moment its stack starts. It
holds `127.0.0.1/8` and a logical MTU of 65536 and depends on no device,
carrier, DHCP lease, static address, gateway, state volume or resolver. The
whole of `127.0.0.0/8` routes to it (RFC 1122), not just one host route:

- nothing sent to `127.0.0.0/8` is framed, resolved with ARP, or given to
  the gateway; the stack answers an echo to any loopback host at once with
  zero round-trip time;
- a frame from the link with a loopback source or destination, IPv4 or ARP,
  is dropped and counted;
- `127.0.0.0` and `127.255.255.255` name no host and are refused;
- a static address, a gateway, a DHCP lease and a DNS server may not be a
  loopback address, so conflict detection never sees one.

Echo is lo's only user. The route lives in the stack's routing step, so a
later UDP or TCP socket finds the same local path. A datagram for the
resolver transport to a loopback address fails as unreachable: no UDP
listener exists there.

`netd` runs in every normal boot, NIC or not, so lo exists wherever the
network commands do. Recovery mode carries no network command and runs no
network service; nothing there could use lo.

`net status` prints `lo` first, with its address, MTU and loopback echo
count, then `net0`.

### The hosts table

The configuration service owns a separate table of up to 64 records, each a
normalized host name and an IPv4 address, persisted in `/state/hosts` apart
from `/state/config`. Names follow the shared syntax of
`userspace/hostname`: ASCII letters, digits and hyphens, compared without
case, stored lowercase without a trailing dot, at most 248 bytes so a record
fits one IPC payload with its header. The file has one `ADDRESS NAME` line
per record and `#` comments; a malformed or duplicate line is skipped and
counted, as at the restore of `/state/config`.

`localhost` is system policy, not a record. The table always answers
`127.0.0.1` for it, even when the file is missing or corrupt or names it
itself; no operation can remove it or point it elsewhere, and it uses no
slot. Names under `.localhost` are not stored: the resolver answers them.

A separate protocol, `GHST1`, runs on the configuration session: `HostsGet`,
`HostsList`, `HostsSet` and `HostsRemove`. Two new session rights govern it
apart from the setting scopes: `HostsRead` for lookups and listing,
`HostsWrite` for changes. The shell's session holds both, the resolver only
`HostsRead`; no command reaches the table except through the shell's
`hosts` builtin.

## Consequences

`ping 127.0.0.1` and `ping localhost` work with no NIC, no DNS server and no
state volume. The network gate checks both on a guest without a NIC, and
checks a hosts record resolving and answering pings on the private segment.

The resolver asks the table for every name that misses `localhost`, so a
change shows on the next lookup without a notification or a restart. When
the configuration service is down the table is out of reach, lookups go on
to DNS, and `localhost` still resolves.

A hosts table of 64 records is enough for a home or lab network; anything
larger belongs in DNS.
