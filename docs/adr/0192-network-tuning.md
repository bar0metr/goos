# ADR 0192: Runtime network tuning

## Status

Accepted. Extends ADR 0191: adds `system.net.*` timer and rate settings, the
reload contract for them, and status fields to the network protocol.

Amended 2026-10-07: address-conflict detection follows RFC 5227, so the
single `probe-interval-ms` setting gave way to the probe, announcement,
defence and rate-limit settings below; the configuration read has a strict
deadline that drops stale replies; and the network protocol moved to
version 2 for its new status layout.

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

`netd` reads seventeen settings next to the four address keys. Each one
maps to a value the stack uses. The last column names the nearest Linux
setting as a rough reference only: GoOS defines its own semantics, and the
two may differ in meaning as well as in name.

| Key | Default | Range | Rough Linux reference |
| --- | ---: | ---: | --- |
| `system.net.ipv4.default-ttl` | 64 | 1..255 | `net.ipv4.ip_default_ttl` |
| `system.net.ipv4.probe-count` | 3 | 1..10 | RFC 5227 PROBE_NUM |
| `system.net.ipv4.probe-wait-ms` | 1000 | 0..10000 | RFC 5227 PROBE_WAIT |
| `system.net.ipv4.probe-min-ms` | 1000 | 100..10000 | RFC 5227 PROBE_MIN |
| `system.net.ipv4.probe-max-ms` | 2000 | 100..10000 | RFC 5227 PROBE_MAX |
| `system.net.ipv4.announce-count` | 2 | 1..10 | RFC 5227 ANNOUNCE_NUM |
| `system.net.ipv4.announce-interval-ms` | 2000 | 100..10000 | RFC 5227 ANNOUNCE_INTERVAL |
| `system.net.ipv4.defend-interval-ms` | 10000 | 1000..60000 | RFC 5227 DEFEND_INTERVAL |
| `system.net.ipv4.max-conflicts` | 10 | 1..100 | RFC 5227 MAX_CONFLICTS |
| `system.net.ipv4.conflict-rate-limit-ms` | 60000 | 1000..600000 | RFC 5227 RATE_LIMIT_INTERVAL |
| `system.net.arp.cache-timeout-ms` | 60000 | 1000..3600000 | none; see below |
| `system.net.arp.resolve-interval-ms` | 1000 | 100..10000 | `neigh.*.retrans_time_ms` |
| `system.net.arp.resolve-attempts` | 3 | 1..10 | `neigh.*.mcast_solicit` |
| `system.net.icmp.echo-reply-rate` | 20 | 0..10000 | ICMP rate limiting |
| `system.net.dhcp.initial-retry-ms` | 4000 | 1000..60000 | none |
| `system.net.dhcp.max-retry-ms` | 64000 | 4000..300000 | none |
| `system.net.dhcp.conflict-retry-ms` | 10000 | 1000..300000 | none |

`arp.cache-timeout-ms` is GoOS's own policy: a hard lifetime counted from an
entry's last accepted refresh, after which the entry is gone. Linux keeps
neighbours in a reachability state machine, and its `gc_stale_time` only
decides when a stale entry may be collected; neither setting behaves like
this one. GoOS offers no `/proc/sys` view.

Address claims follow RFC 5227. A claim waits a random time in
`0..probe-wait-ms`, sends `probe-count` probes a random
`probe-min-ms..probe-max-ms` apart, waits a fixed 2 s ANNOUNCE_WAIT, then
goes live and announces itself `announce-count` times `announce-interval-ms`
apart. The stack keeps watching after that: the first claim on its live
address by another host is answered with one announcement, and a second
claim within `defend-interval-ms` makes it give the address up, declining a
DHCP lease or failing a static address until the next reload. After
`max-conflicts` conflicts in a row the next claim waits
`conflict-rate-limit-ms`; an address that holds through its announcements
ends the run. Randomness comes from the kernel generator through the native
`RandomRead` call. `net status` reports the claim and conflict counters and
says when the next claim is rate-limited.

The old `system.net.ipv4.probe-interval-ms` is gone. Its two meanings, the
gap between probes and the quiet period, are now separate settings, and no
single old value maps onto both without guessing. A stored line for it is an
undeclared key, which the configuration service skips at restore and counts
like any other unknown line.

`network.Tuning` holds the values in nanoseconds. `ParseTuning` checks each
stored value against its range before converting it, so a huge number
cannot wrap into an accepted one, and `Tuning.Validate` adds two cross-field
rules: `probe-max-ms` at least `probe-min-ms` and `max-retry-ms` at least
`initial-retry-ms`; each failure names the second key. The ranges cap
every time at an hour, far from deadline overflow.

`netd` reads all twenty-one keys as one candidate under one deadline in
scheduler ticks: five seconds from the start of the read, with each key
waiting at most two seconds of it. Every wait asks for exactly the whole
ticks left before its deadline, a key whose turn comes at or after the
shared deadline is not asked for, and nothing extends the deadline. A
request that times out may still be answered later; the configuration client
in `userspace/config` numbers requests with a growing correlation and drops
any reply older than the one it waits for, so a late answer cannot be taken
for the next key's or poison the next reload. `Stack.Apply` validates the
address configuration and the tuning before it changes anything, then sets
both. A failure reports the exact key through `ConfigInvalid`; the `Field`
enumeration numbers every key alike in `network` and `netapi`.

A tuning change applies at each value's next use and resets nothing:

- TTL and the reply rate apply to the next packet built. Queued frames keep
  the TTL they were built with.
- A new ARP lifetime applies to entries learned or refreshed later. Existing
  entries keep their expiry until refreshed.
- Each ARP resolution, address probe and announcement sequence copies its
  values when it starts and finishes under them. A live address is not
  probed again.
- Defence and rate-limit values apply to the next conflict.
- DHCP retry values apply at the client's next scheduling point. The lease,
  T1, T2 and expiry stay as the server set them.

Retransmission jitter stays at up to one second either way, but never more
than half the base, so a one-second initial retry cannot schedule a deadline
at the current time. A conflict wait below 10 s goes under the RFC 2131
recommendation; the operator chooses that.

Table sizes, the transmit ring, session count, pool layout and the 30-second
lease floor stay compile-time constants. They size static storage or guard
the protocol, and no setting can change them.

The network protocol is at version 2: the `Status` reply grew to 152 bytes,
and a version-1 peer would read it wrongly, so a version-1 message fails to
decode. Bytes 76..77 carry the link MTU, byte 78 a fallback flag, byte 79
the rate-limited flag, bytes 80..143 the claim and conflict counters and the
loopback echo count, and byte 144 the current conflict run. `Fallback` is set
while `netd` runs on built-in defaults because the stored settings were
unreadable or invalid at startup; a successful reload clears it. `netd`
never rewrites stored settings. Any later change to a reply's layout bumps
the version again.

The DHCP client advertises the link MTU in option 57 instead of the
compile-time 1500. `LinkUp` already keeps that MTU between 576 and 1500.

The shell's `config set` parses `uint` values with `config.ParseUint`, which
refuses values above the `uint64` maximum. Before, twenty digits could wrap
into a different stored number.

## Consequences

An operator can change TTL, ARP, address-conflict, echo-rate and DHCP
timing with `config set` and `net reload` without a reboot, and a bad value
names its key. A default claim now takes up to 7 s instead of 1 s, which the
boot gate's 15-second DHCP budget absorbs. `net status` shows the MTU, and shows `settings: built-in
defaults; ...` while the fallback holds.

`net status` does not list tuning. `config get` shows the stored values, and
after a successful reload they match the running ones. A `net tuning`
command can come later if the two need comparing at other times.

TCP, general UDP sockets and forwarding will bring their own settings
under `system.net.tcp.*`, `system.net.udp.*` and similar names when those
protocols exist. None is declared now.
