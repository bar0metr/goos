# ADR 0197: Shared DNS wire format and TCP framing

## Status

Accepted. Builds on ADR 0194, ADR 0195 and ADR 0196.

## Context

The resolver core in `userspace/dns` held the only DNS parser, private to
the package, and `userspace/tcpupstream` held the only DNS-over-TCP framing,
inside its GTCP1 stream state. The `dnsq` diagnostic command needs both: it
sends any of eight query types straight to a server and prints every
section. A second parser or a second framing state machine in the command
would give the system two sets of bounds to keep right, and a bug fixed in
one would stay in the other.

## Decision

Two packages leave the resolver:

- `userspace/dnswire` owns the message format: wire names and their
  comparisons, the bounded name decoder, query encoding with a QTYPE
  argument, response validation against an ID, question name and QTYPE,
  record iteration, record data checks and presentation text. Its bounds are
  the resolver's: 63-byte labels, 255-byte names, compression pointers that
  point backwards, at most 64 per name, and record lengths checked against
  the message.
- `userspace/dnstcp` owns the two-byte length framing: a sender that tracks
  partial writes of one framed query and a receiver that assembles one
  message from pieces of any size into a caller's 65535-byte buffer. It knows
  no socket, transaction, correlation or output.

`userspace/dns` keeps resolver policy and still asks only for A records.
`ParseResponse` checks record data for the types the resolver reads, A,
CNAME and SOA, as before; `dnsq` checks the other types it prints with
`ValidRData` before it prints anything, so a malformed MX in an answer
changes nothing for `resolved`. `MaxStreamMessage` in `userspace/dns` is now
`dnstcp.MaxMessage`, the one definition of the 65535-byte limit.

`dnsq` is a command over its own `GUDP1` and `GTCP1` sessions with
`GrantUDP | GrantTCP`. It uses no resolver session, so it adds no protocol,
grant bit or command index format. Its logic lives in `userspace/dnsq` and
runs in host tests against a scripted transport; the binary in
`userspace/bin/dnsq` supplies the sessions, the clock and the terminal.

## Consequences

The resolver and `dnsq` parse and frame DNS with the same code, and the
parser fuzz targets cover both. A change to either package changes
`resolved`, so its tests run against every change there. `dnswire` can
format records the resolver never asks for; that code links into `resolved`
only where it uses it. Automatic discovery of the system's DNS servers for
`dnsq` stays out: it would need a network status session, a wider grant
than a diagnostic query needs.
