# Configuration

Configuration is a typed service, not a directory of text files. A key is
declared with a type before it can be set, a write is checked against that
declaration, and a client reaches the store over a session whose scope and
rights decide what it may see and change (ADR 0074).

## Keys and types

A key is dotted, lower case, and has at least two components, so it always names
a namespace and a setting within it. Values are one of three types:

| Type | Values |
| --- | --- |
| `bool` | `true` or `false` |
| `uint` | an unsigned decimal |
| `text` | up to 64 printable ASCII bytes |

The running image declares the schema at startup. A key it does not declare is
not a setting: setting one answers "no such setting" rather than creating it, so
a typo fails instead of becoming a value nothing reads.

Some entries are published rather than configured — the name, release and
machine of the running image. They are declared read-only, nothing may write
them, and they are not part of the stored file, because a stored copy would be a
second source of truth about what is executing.

## Sessions

A session carries a **scope**, the namespace prefix it is confined to, and
**rights** to read and to write. The service applies them to every request; a
message carries no authority of its own. PID 1 creates the session and decides
what it may do.

A key outside the scope reads as absent rather than denied, so a session
confined to `user.` learns nothing about what exists elsewhere. Scopes match
whole components: `user.` covers `user.name` and never `username.x`.

Sessions are created through a control endpoint whose only client side PID 1
holds, so PID 1 is the only process that can create one and the scope and rights
travel in the request (ADR 0076):

| Client | Scope | Rights |
| --- | --- | --- |
| shell | the whole namespace | read, write, hosts read, hosts write |
| log service | `system.log.` | read |
| network service | `system.net.` | read |

The hosts rights, `HostsRead` (4) and `HostsWrite` (8), govern the hosts
table and nothing else; `Read` (1) and `Write` (2) do not reach it. See
[Hosts](#hosts).

The service serves up to 16 sessions and drops one whose peer has gone.

## Protocol

`GCF1` version 1 carries an operation, a key and a typed value:

| Operation | Reply |
| --- | --- |
| `Get` | the value and whether it is read-only |
| `Set` | a status |
| `List` | one entry and a cursor, within the session's scope |

Refusals are typed: `NotFound`, `ReadOnly`, `TypeMismatch`, `PermissionDenied`,
`InvalidArgument`. There are no string commands and no untyped blobs, so a
malformed message is rejected at the boundary rather than interpreted.

## Storage

Writable entries live at `/state/config` in the exported form, one per line:

```text
system.log.persist bool true
system.shell.prompt text goos>
system.storage.volume text /persist
user.name text goos
```

This is the import format as well, and it is what a person reads or edits from
the host. Naming the type on each line is what lets a value read back be checked
against its declaration rather than guessed from its shape.

The whole store is rewritten after an accepted write, which is simpler than
tracking which entry changed and leaves the file consistent with the store. A
line that does not parse, names an undeclared key, or carries the wrong type is
skipped and counted at startup, and the system comes up with its declared
defaults.

Transactional update is future work: a single-writer store with a bounded
rewrite has nowhere to put a half-applied change today.

## Shell

`config` is a builtin, in the family of `services`, `handles`, `caps` and `log`,
because it administers the shell's own session:

```text
goos> config
system.build.release 0.1.0 (read-only)
system.log.persist true
system.shell.prompt goos>
goos> config get system.shell.prompt
goos>
goos> config set system.log.persist false
```

`config set` resolves the key's declared type first and parses the argument
against it, so a value is converted by what the key is rather than by how the
text looks.

## Consumers

A setting is not configuration until something reads it:

| Setting | Read by | When |
| --- | --- | --- |
| `system.shell.prompt` | the shell | at startup and after a successful write |
| `system.log.persist` | the log service | on every drain pass |
| `system.net.*` | the network service | at startup and on `net reload` |

A component that cannot reach the service, or gets an answer of the wrong type,
keeps its built-in default. Configuration decides what a component does, not
whether it starts — the shell has to come up to fix a configuration that stops
things coming up.

Turning `system.log.persist` off advances the log service's cursor without
writing, so turning it back on starts from the present rather than writing out a
backlog.

## Network

The network service's address settings (ADR 0191):

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `system.net.mode` | `text` | `dhcp` | `dhcp` or `static` |
| `system.net.ipv4.address` | `text` | `0.0.0.0` | the static address |
| `system.net.ipv4.prefix` | `uint` | `24` | the static prefix length, 1 to 30 |
| `system.net.ipv4.gateway` | `text` | `0.0.0.0` | the static default gateway; `0.0.0.0` means none |

Its timer and rate settings (ADR 0192), all `uint`, times in milliseconds:

| Key | Default | Range | Meaning |
| --- | ---: | ---: | --- |
| `system.net.ipv4.default-ttl` | 64 | 1..255 | TTL of every packet the stack builds |
| `system.net.ipv4.probe-count` | 2 | 1..10 | ARP probes before an address goes live |
| `system.net.ipv4.probe-interval-ms` | 500 | 100..5000 | gap between probes, and the quiet period after the last |
| `system.net.arp.cache-timeout-ms` | 60000 | 1000..3600000 | lifetime of an ARP entry after its last refresh |
| `system.net.arp.resolve-interval-ms` | 1000 | 100..10000 | gap between ARP requests for one next hop |
| `system.net.arp.resolve-attempts` | 3 | 1..10 | ARP requests before resolution fails |
| `system.net.icmp.echo-reply-rate` | 20 | 0..10000 | echo requests answered per second; 0 answers none |
| `system.net.dhcp.initial-retry-ms` | 4000 | 1000..60000 | first `DISCOVER` and `REQUEST` retransmission base |
| `system.net.dhcp.max-retry-ms` | 64000 | 4000..300000 | backoff ceiling; at least `initial-retry-ms` |
| `system.net.dhcp.conflict-retry-ms` | 10000 | 1000..300000 | wait after declining a conflicting lease |

The echo reply rate limits answers to other hosts' pings, not replies to
`ping` run here. Linux has similar sysctls (`ip_default_ttl`,
`neigh.*.retrans_time_ms` and others), but GoOS does not mirror
`/proc/sys/net`.

The store checks only their types. The network service reads all fourteen as
one candidate when it starts and when `net reload` asks it to. DHCP mode ignores
the other three keys. Static mode needs a unicast host address written as
plain dotted decimal, a prefix from 1 to 30, and a gateway that is zero or
another host address inside the prefix. A candidate that fails names the
first key at fault and leaves the running configuration as it was; one that
passes replaces it in one step. If the settings are unreadable or invalid when
the service starts, it runs DHCP with the default tuning and leaves the stored
values alone; `net status` then prints `settings: built-in defaults; stored
system.net.* settings not applied` until a reload succeeds.

A reload that changes only tuning keeps the address, the lease and the ARP
table. Each value takes effect at its next use: TTL and the reply rate on the
next packet, the ARP lifetime on the next learned or refreshed entry, resolve
and probe values on the next resolution or probe, DHCP retry values at the
client's next retransmission or transaction. A resolution or probe already
running finishes with the values it started with.

Each `config set` writes one key, and nothing applies them together, so set
every key first and reload once:

```text
goos> config set system.net.mode static
goos> config set system.net.ipv4.address 192.0.2.10
goos> config set system.net.ipv4.prefix 24
goos> config set system.net.ipv4.gateway 0.0.0.0
goos> net reload
network configuration applied
```

A refused candidate prints, for example,
`net: invalid setting system.net.ipv4.gateway; previous configuration kept`.
A cross-field failure names the second key:

```text
goos> config set system.net.dhcp.initial-retry-ms 30000
goos> config set system.net.dhcp.max-retry-ms 10000
goos> net reload
net: invalid setting system.net.dhcp.max-retry-ms; previous configuration kept
```

The settings persist in `/state/config` like any other; there is no separate
network file.

## Hosts

The service also keeps the hosts table: names that resolve to an IPv4 address
on this machine without asking a DNS server. It is records, not settings, so
it has its own file, its own protocol and its own rights.

### Records

The table holds up to 64 records of a host name and an IPv4 address. A name
follows the shared host-name syntax (`userspace/hostname`): labels of letters,
digits and hyphens, compared without case, stored in lower case without a
trailing dot. A name in the table is at most 248 bytes, not the 253 of DNS, so
that the 8-byte protocol header and the name fit one 256-byte IPC payload.
Names are unique; setting a name that exists replaces its address and keeps
its place.

An address must be a host address: not in `0.0.0.0/8`, not
`255.255.255.255`, not multicast (`224.0.0.0/4`) and not class E. A name other
than `localhost` may map into `127.0.0.0/8`.

`localhost` is policy, not a record. It always answers `127.0.0.1`, whether
or not the file exists or parses, it is listed first, and it does not use one
of the 64 records. Setting or removing it answers `ReadOnly`, as does any name
under `.localhost`; the resolver answers those names itself, so the table never
holds them, and a lookup of one here is `NotFound`.

The table has a 64-bit generation that goes up with every change that
succeeds. Setting the address a name already has is not a change.

### File

The table lives at `/state/hosts`, apart from `/state/config`, one record per
line, in the order the records were added:

```text
192.0.2.10 build.example
10.0.0.2 nas
```

The service rewrites the whole file after every change, as it does the
settings, and writes no `localhost` line. If the write fails, it tries again
when PID 1 hands it a new filesystem session.

When the service starts it reads the file after the settings. A missing file
is an empty table. Reading follows these rules:

- The address comes first, in strict dotted decimal; fields are separated by
  one or more spaces or tabs, which may also begin or end a line.
- `#` starts a comment that runs to the end of the line. Blank lines and
  comment lines are ignored.
- A trailing dot and upper case in a name are accepted and normalized.
- A line with a bad address, a bad name, a third field, a `localhost` name, a
  name already read, or a record past the 64th is skipped. The service logs one
  warning with the number of lines skipped.
- Reading stops at 21248 bytes, room for 64 records of the longest name and
  4 KiB of comments. A line the bound cuts is skipped.

Comments do not survive the next change, which rewrites the file from the
table.

### Protocol

`GHST` version 1 travels on the configuration session endpoint, told apart from
`GCF1` by its protocol ID. The session's scope does not apply. `Get` and `List`
need `HostsRead`; `Set` and `Remove` need `HostsWrite`; without them the answer
is `PermissionDenied`.

A request is an 8-byte header followed by the name:

| Bytes | Field |
| --- | --- |
| 0 | operation: 1 `List`, 2 `Get`, 3 `Set`, 4 `Remove` |
| 1 | name size, 0 for `List`, 1 to 248 otherwise |
| 2 | cursor, `List` only |
| 3 | zero |
| 4..7 | address, `Set` only |
| 8.. | name |

A reply is an 8-byte header followed by either a name or the generation:

| Bytes | Field |
| --- | --- |
| 0 | operation |
| 1 | status |
| 2 | name size |
| 3 | next cursor, `List` only |
| 4..7 | address, on success |
| 8.. | the name if its size is not zero, else the generation as a little-endian `uint64` |

Only a `List` reply that returns a record carries a name; every other reply,
including the one that ends a listing, carries the generation. The longest
name fills the payload, so the two cannot travel together. Fields an operation
does not use must be zero, and the sizes must match the payload exactly.

`List` starts at cursor 0, which is `localhost`, and returns one record and the
next cursor per request until `NotFound`. A change during a listing can move a
record past the cursor; a client that needs a consistent listing compares the
generation from a `Get` before it with the one that ends it.

Statuses: `NotFound` for a name not in the table, `ReadOnly` for `localhost`
and names under it, `ResourceExhausted` for a full table, `InvalidArgument` for
a bad name or address.

### Shell

```text
goos> hosts set 10.0.0.2 NAS.
goos> hosts
127.0.0.1 localhost
10.0.0.2 nas
goos> hosts get nas
10.0.0.2 nas
goos> hosts remove nas
goos> hosts set 10.0.0.2 localhost
hosts: localhost is reserved
```

The other errors are `hosts: invalid IPv4 address`, `hosts: invalid host
name`, `hosts: table full`, `hosts: no such host` and `hosts: service
unavailable`.
