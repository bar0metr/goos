# ADR 0192: Runtime network tuning

## Status

Accepted. Extends ADR 0191: adds ten `system.net.*` settings, the reload
contract for them, and two status fields to `GNETAPI1`.

## Context

ADR 0191 fixed every timer and rate in `userspace/network` at compile time:
TTL 64, a 60-second ARP lifetime, three ARP requests a second apart, two
address probes 500 ms apart, 20 echo replies a second, and DHCP
retransmission from 4 s to 64 s with a 10-second wait after a conflict. An
operator on a slow or lossy link had to rebuild the image to change any of
them.

Linux exposes hundreds of `net.*` sysctls. Most govern behaviour GoOS does
not have: forwarding, fragment reassembly, ICMP redirects, proxy ARP. A
switch for missing behaviour would claim a feature the stack lacks.

## Decision

`netd` reads ten more settings next to the four address keys. Each one maps
to a value the stack already used:

| Key | Default | Range | Linux analogue |
| --- | ---: | ---: | --- |
| `system.net.ipv4.default-ttl` | 64 | 1..255 | `net.ipv4.ip_default_ttl` |
| `system.net.ipv4.probe-count` | 2 | 1..10 | none |
| `system.net.ipv4.probe-interval-ms` | 500 | 100..5000 | none |
| `system.net.arp.cache-timeout-ms` | 60000 | 1000..3600000 | `neigh.*.gc_stale_time` |
| `system.net.arp.resolve-interval-ms` | 1000 | 100..10000 | `neigh.*.retrans_time_ms` |
| `system.net.arp.resolve-attempts` | 3 | 1..10 | `neigh.*.mcast_solicit` |
| `system.net.icmp.echo-reply-rate` | 20 | 0..10000 | ICMP rate limiting |
| `system.net.dhcp.initial-retry-ms` | 4000 | 1000..60000 | none |
| `system.net.dhcp.max-retry-ms` | 64000 | 4000..300000 | none |
| `system.net.dhcp.conflict-retry-ms` | 10000 | 1000..300000 | none |

The Linux names are references. GoOS keeps its own typed keys and offers no
`/proc/sys` view.

`network.Tuning` holds the values in nanoseconds. `ParseTuning` checks each
stored value against its range before converting it, so a huge number
cannot wrap into an accepted one, and `Tuning.Validate` adds the one
cross-field rule: `max-retry-ms` at least `initial-retry-ms`. The ranges cap
every time at an hour, far from deadline overflow.

`netd` reads all fourteen keys as one candidate, under a shared five-second
deadline with at most two seconds per key. `Stack.Apply` validates the
address configuration and the tuning before it changes anything, then sets
both. A failure reports the exact key through `ConfigInvalid`; the `Field`
enumeration grew from four values to fourteen, numbered alike in `network`
and `netapi`.

A tuning change applies at each value's next use and resets nothing:

- TTL and the reply rate apply to the next packet built. Queued frames keep
  the TTL they were built with.
- A new ARP lifetime applies to entries learned or refreshed later. Existing
  entries keep their expiry until refreshed.
- Each ARP resolution and each address probe copies its interval and count
  when it starts and finishes under them. A live address is not probed again.
- DHCP retry values apply at the client's next scheduling point. The lease,
  T1, T2 and expiry stay as the server set them.

Retransmission jitter stays at up to one second either way, but never more
than half the base, so a one-second initial retry cannot schedule a deadline
at the current time. A conflict wait below 10 s goes under the RFC 2131
recommendation; the operator chooses that.

Table sizes, the transmit ring, session count, pool layout and the 30-second
lease floor stay compile-time constants. They size static storage or guard
the protocol, and no setting can change them.

`GNETAPI1` stays at version 1 because commands and `netd` ship in one image.
Reply bytes 76..77 carry the link MTU and byte 78 a fallback flag, 0 or 1;
byte 79 stays zero. Both belong to `Status`: `MTU` is zero without a link,
and `Fallback` is set while `netd` runs on built-in defaults because the
stored settings were unreadable or invalid at startup. A successful reload
clears it. `netd` never rewrites stored settings.

The DHCP client advertises the link MTU in option 57 instead of the
compile-time 1500. `LinkUp` already keeps that MTU between 576 and 1500.

The shell's `config set` parses `uint` values with `config.ParseUint`, which
refuses values above the `uint64` maximum. Before, twenty digits could wrap
into a different stored number.

## Consequences

An operator can change TTL, ARP, probe, echo-rate and DHCP timing with
`config set` and `net reload` without a reboot, and a bad value names
its key. `net status` shows the MTU, and shows `settings: built-in
defaults; ...` while the fallback holds.

`net status` does not list tuning. `config get` shows the stored values, and
after a successful reload they match the running ones. A `net tuning`
command can come later if the two need comparing at other times.

TCP, general UDP sockets and forwarding will bring their own settings
under `system.net.tcp.*`, `system.net.udp.*` and similar names when those
protocols exist. None is declared now.
