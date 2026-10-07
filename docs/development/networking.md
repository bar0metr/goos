# GoOS networking specification: native IPv4 and ICMP

Status: implemented by [ADR 0190](../adr/0190-network-device-authority.md) and
[ADR 0191](../adr/0191-network-service.md). This document records the
requirements the implementation was built from; the ADRs and
[networking.md](../architecture/networking.md) describe what was built.

Later work changed the system this document describes. ADR 0192 added
runtime tuning and RFC 5227 address-conflict detection, ADR 0193 a loopback
interface and a hosts table, and ADR 0194 a DNS resolver, so `ping` now
accepts host names. Where this document puts DNS out of scope or says `ping`
takes only a numeric address, it describes the first milestone, not the
current system.

Baseline: `main` at `18f659e` (2026-10-06). Implementation must reconcile this
specification with the source at the time of the change. The milestone is complete
only when an installed GoOS system sends ICMP echo requests from its own `ping`
command, receives replies over its own network driver and stack, and answers
echo requests from a peer.

## 1. Scope and outcome

The first supported device is one transitional, legacy-I/O `virtio-net` PCI
function (`1af4:1000`) under QEMU `q35`, with `disable-modern=on`. The network
stack is a supervised native userspace service. A separate userspace driver
owns the NIC. Native commands use private capability-scoped sessions; they do
not issue raw packets or touch device registers.

The required protocols are untagged Ethernet II, ARP, IPv4, ICMPv4 echo, and
the UDP subset needed by a DHCPv4 client. DHCP is the default configuration
mode. A validated static address, prefix and optional on-link gateway support
isolated networks. The user can inspect the interface and run `ping` against a
numeric IPv4 address from the native shell.

This milestone does not promise DNS, TCP, general UDP sockets, IPv6, Linux
`socket` syscalls, multiple NICs, VLANs, packet forwarding, NAT, or arbitrary
physical PCI hardware. A hostname given to `ping` fails with a clear
`numeric IPv4 address required` diagnostic. Linux network calls keep their
documented `ENOSYS` result. DHCP's UDP implementation remains private to the
network service; it is not an application transport API.

The network is optional for boot. An absent or failed NIC leaves storage,
the installed system, the terminal and the shell operational. `net status`
reports the network state; `ping` returns a network error within a bounded
deadline. A missing NIC must not put the system in recovery mode.

## 2. Existing contracts and required changes

| Current source | Contract or limitation | Required change |
| --- | --- | --- |
| [Architecture overview](../architecture/overview.md), [README](../../README.md) | Networking belongs in userspace and is not implemented. | Keep the kernel to PCI discovery, bounded device authority, IRQ delivery, DMA and waits. |
| [PCI discovery](../../kernel/pci/pci.go), [boot setup](../../kernel/main.go) | Discovery matches only legacy `virtio-blk`; PID 1 has a fixed initial handle layout. | Discover the net function independently; add only its device capabilities to PID 1 and update the checked layout and startup constants together. |
| [IRQ routing](../../kernel/arch/amd64/tables_uefi.go), [IRQ wait](../../kernel/waits.go), [assembly](../../kernel/arch/amd64/io.S) | GSI 11, its pending bit and its single waiter belong to block. `WaitAny` excludes IRQ handles. | Give the NIC a separate, validated route and pending/waiter state; let its driver wait for both IRQ and IPC without polling. Preserve block's IRQ behavior. |
| [Device calls](../../kernel/devicecalls.go), [teardown](../../kernel/lifecycle.go) | Port and DMA calls are capability checked, but DMA teardown resets the block device through a global port. | Bind each DMA domain to one discovered device; reset and quiesce that device before releasing its driver's DMA pages. A net-driver exit must not reset the disk. |
| [Typed IPC](../architecture/ipc.md), [bulk I/O ADR](../adr/0107-bulk-io-through-shared-memory.md) | IPC carries at most 256 inline bytes; shared memory exists for larger data. | Carry frame metadata over versioned IPC and frame bytes in bounded shared pages. Never carry a DMA address in IPC. |
| [Supervisor](../../userspace/cmd/init/services.go), [service guide](adding-service.md) | PID 1 creates endpoints and supervises services; a service has at most seven grants. | Register the network stack and driver, renew their channels and shared pages after restart, and keep their grants within seven. |
| [Command grants](../../userspace/commands/commands.go), [launch path](../../userspace/cmd/init/applications.go) | PID 1 grants only declared command capabilities from a trusted command index. | Add separate network-use and network-admin grants; make one private session per launched client. |
| [Linux ABI matrix](../architecture/linux-x86_64-abi.md) | Linux network calls are unsupported. | Leave the compatibility layer unchanged for this milestone. |

The source currently allows 32 supervised services, seven grants per launch,
16 interests per `WaitAny`, and 16 configuration sessions. Do not copy older
numeric limits from narrative documentation without checking current code.

## 3. Process and authority model

```text
ping / net (separate native commands)
       | private GNETAPI1 session: ping/status, or admin
       v
network service (netd): DHCP, ARP, IPv4, ICMP, routes, timers
       | GNETDRV1 control + two directional shared-memory pools
       v
virtio-net driver: virtqueues, device ISR, frame copying
       | ports + IRQ + DMA capabilities
       v
virtio-net PCI function
```

PID 1 is the only endpoint creator and grant broker. The network service has
no port, IRQ or DMA capability. The driver has no VFS, configuration or client
session capability. `ping` receives only standard streams and a network-use
session; `net` receives standard streams and a network-admin session. Neither
command can transfer its session to another process.

PID 1 creates two distinct shared-memory objects for each driver generation:

| Pool | Size and slots | Driver right | Network service right |
| --- | --- | --- | --- |
| Receive | 2 pages, four 2048-byte slots | Read + Write | Read |
| Transmit | 2 pages, four 2048-byte slots | Read | Read + Write |

Both objects are software frame buffers, not DMA memory. The driver alone
maps its private pinned DMA window and copies validated frames between it and
the pools. The driver copies a transmit slot into its private buffer before
submitting a descriptor, so a service write cannot change a submitted DMA
buffer. Protocol state, not shared-memory access, controls when a slot is
reused. A slot has one owner at a time and an explicit release/complete event.

The driver gets exactly seven named startup grants: `console` (Write), `ports`
(Read + Write), `irq` (Wait), `dma` (Write), `service` (Read + Write + Wait),
`rx` (Read + Write), and `tx` (Read). The network service starts with
`console` (Write), `control` (Read + Write + Wait), and `config` (Read +
Write + Wait channel rights; the configuration-service session is scoped
read-only to `system.net.`). PID 1 later transfers its
`device` channel (Read + Write + Wait), `rx` pool (Read), and `tx` pool (Read
+ Write) through `SetDevice`. An absent NIC requires no dummy device handle:
the service starts without a device binding and reports `down`. No child
receives Transfer.

The NIC driver is part of the effective trusted computing base until an IOMMU
confines bus-mastering DMA, as with the current block driver. Port bounds and
process isolation contain accidental faults; they do not make a malicious
DMA-capable driver safe. Document this in the security model when implemented.

## 4. Kernel and PCI prerequisites

1. Extend PCI discovery to identify the one legacy network function by vendor
   and device ID. Validate its I/O BAR type, nonzero base, probed size, range
   within 16-bit I/O space, and non-overlap with the block BAR. Restore the
   original BAR value after sizing it. Reject an
   unsupported or second matching NIC with a diagnostic; do not pick one by
   enumeration accident. Enable I/O and bus mastering only on that function,
   preserving unrelated command-register bits. Keep the block discovery path
   working with or without a NIC.
2. Route the NIC's INTx line to a dedicated vector on the bootstrap CPU.
   Validate the PCI interrupt pin/line and Q35 route before enabling it. This
   first version requires a line distinct from block GSI 11 and console lines
   1 and 4; a shared or unknown route is reported as unsupported. Pin the QEMU
   device placement in the network test so the required route is reproducible.
   Store one pending flag and one generation-safe waiter per supported device.
   The interrupt handler masks its level-triggered route, records the event,
   wakes the waiter, and acknowledges the LAPIC. It performs no packet parsing.
3. Extend `WaitAny` with a readiness-only IRQ interest and a nonblocking
   `IRQTake` operation. Both require a `TypeIRQ` handle with Wait. Register the
   waiter, arm the route and recheck pending state before sleeping; avoid a
   lost wake between check and registration. `WaitAny` reports readiness but
   does not consume the event. After waking, the driver reads the device ISR,
   calls `IRQTake`, drains completed receive and transmit descriptors, and
   arms the next wait. Existing `IRQWait` behavior for block remains intact.
   The driver waits on its IPC channel and IRQ in one bounded `WaitAny`, with
   no periodic device polling.
4. Associate a DMA-domain capability with the discovered BDF, I/O BAR and IRQ
   route. `DMAAllocate` must reject an unknown domain and retain the current
   per-process page quota and pinned-window checks. On driver exit the kernel
   masks that device's interrupt, resets that device, then reclaims its DMA
   pages. Closing or crashing the NIC driver must not touch the block route,
   block status register, or disk DMA pages. An absent NIC gets no usable
   hardware authority; the checked PID 1 layout may use a typed inert
   placeholder, never a capability for another live device.
5. Keep bus configuration, IRQ state and teardown synchronized under the
   current SMP locking rules. Test concurrent interrupt, wait cancellation,
   process exit and restart, including stale task IDs.

Do not add packet parsing, IP routing, DHCP policy, global sockets, or a
network-specific path lookup to the kernel.

## 5. Virtio network driver

The driver implements one receive queue (index 0) and one transmit queue
(index 1) using the legacy split-ring layout. It reads and bounds each queue
size before allocating descriptors; a zero, too-small or overflowing layout
fails initialization. It allocates at most the existing 64-page DMA limit,
including both queue structures and a fixed number of 2048-byte device-owned
packet buffers. It preposts receive buffers before setting `DRIVER_OK` and
replenishes them after validated completions. It never puts a shared-memory
physical address in a descriptor.

Require the MAC feature and negotiate link-status reporting when the device
offers it. Do not negotiate
checksum, segmentation, mergeable receive buffers, control queue, multiqueue
or packed-ring features. Use the 10-byte legacy virtio-net header, one buffer
per packet, and a 1500-byte Ethernet MTU. Read the six-byte MAC from the
device configuration and reject all-zero, broadcast or multicast MAC values.
If link-status reporting is available, propagate changes; otherwise treat a
successfully initialized device as link-up until an I/O fault or peer death.
Publish receive descriptors before `DRIVER_OK`, but notify the device only
after `DRIVER_OK`; follow the legacy initialization order in the virtio spec.

On receive, validate the used-ring index, descriptor ID, written length and
header before copying at most one 1514-byte untagged Ethernet frame to a free
receive slot. On transmit, validate the slot, ownership and frame length
(14..1514), copy the complete frame into private DMA memory, publish a
descriptor, and report completion only after the used ring returns it.
Malformed device completions fail the driver safely. Full queues and pools
return a typed `Busy` or count a receive drop; no allocation or queue grows
with traffic. Report separate RX, TX, malformed and drop counters.

`GNETDRV1` version 1 uses the existing GIPC envelope. Its message bodies are
little-endian, fixed-size and reject nonzero reserved bytes, unknown operations,
invalid slot indices, lengths and generation numbers:

All driver messages have a 32-byte payload. Byte 0 is the opcode; bytes 1..7
are zero; bytes 8..15 are the nonzero `uint64` generation; bytes 16..17 are
the `uint16` slot; bytes 18..19 the `uint16` frame length; bytes 20..21 the
`uint16` typed status; bytes 22..23 the `uint16` MTU; bytes 24..29 the MAC;
byte 30 the link state; byte 31 is zero. Fields unused by an opcode must be
zero. All multi-byte fields are little-endian. PID 1 increments the generation
for every new binding and refuses wraparound within a boot.
Link state values are `down=0` and `up=1`.

| Direction | Operation | Fields and meaning |
| --- | --- | --- |
| driver to stack | `Ready` | generation, MAC, MTU, link state |
| driver to stack | `RxReady` | generation, RX slot, frame length; slot passes to stack |
| stack to driver | `RxRelease` | generation, RX slot; driver may reuse slot |
| stack to driver | `Tx` | generation, TX slot, frame length; slot passes to driver |
| driver to stack | `TxDone` | generation, TX slot, typed result; slot returns to stack |
| either | `Fault` | generation, bounded reason code; peer ends the session |

Opcode values are `Ready=1`, `RxReady=2`, `RxRelease=3`, `Tx=4`,
`TxDone=5`, and `Fault=6`. Driver result values are `OK=0`, `Busy=1`,
`InvalidFrame=2`, and `DeviceFault=3`; no other result is accepted.

`Ready`, `RxReady`, `RxRelease` and `Fault` are notifications with no
correlation ID. `Tx` is a request and `TxDone` is its correlated reply; the
channel's request tracking prevents a duplicate or orphan completion. The
control protocol `GNETCTL1` version 1 has `AddSession` (one attached client
endpoint) and `SetDevice` (driver endpoint, RX pool and TX pool, three
attachments). Only PID 1 holds its client endpoint. The service checks the
exact attachment count, handle types, rights, generation and reserved fields,
and answers each attributable request. PID 1 duplicates any retained
shared-memory master handle before transferring a copy.

At most four RX and four TX slots may be outstanding. The driver does not
publish more notifications than the peer's fixed 16-message IPC queue can
hold; it pauses handing off RX slots when that queue is full. Every accepted
`Tx` receives one `TxDone` while the driver remains alive, including an I/O
failure. Peer death terminates the outstanding exchange. A failed send keeps
ownership with the sender. No message carries an address, arbitrary offset or raw
physical page number.

## 6. Network service

The network service owns one interface, one IPv4 address and prefix, one
optional default gateway, a bounded ARP table, DHCP state, and up to eight
private command sessions. Its `WaitAny` set is at most 11 objects: driver,
control, configuration, and eight clients. It uses monotonic deadlines for
DHCP, ARP and ping; idle operation blocks. It resets its request arena after
each operation. It answers attributable malformed requests with a typed error
and closes unexpected transferred handles.

### 6.1 Frame and IPv4 processing

- Accept only Ethernet II types ARP (`0x0806`) and IPv4 (`0x0800`); drop VLAN
  and other types with a counter. Validate destination MAC, minimum frame
  size, declared lengths and every offset before reading fields.
- ARP accepts Ethernet/IPv4 request and reply with hardware length 6 and
  protocol length 4. Learn only plausible unicast sender mappings, answer
  requests for the configured address, and never learn from an invalid or
  truncated packet. Keep at most 16 entries with a 60-second expiry. Resolve
  one next-hop address at a time per destination; send at most three requests
  at one-second spacing, then return `NeighborUnreachable`. Bound pending
  resolutions to four; do not flood or wait forever.
- Outgoing IPv4 uses a 20-byte header, TTL 64, a valid header checksum, and
  the configured unicast source. The connected prefix chooses the destination
  directly; other destinations use the configured default gateway. If neither
  route exists, return `NoRoute` before transmitting. Do not fragment outgoing
  packets. Incoming IPv4 validates version, IHL, total length, header checksum,
  destination, TTL and contained transport length. Reject fragments and
  unsupported IP options with counters. Accept limited broadcast only for
  DHCP before an address is configured.
- ICMP validates its checksum and length. Answer echo requests addressed to
  the interface's unicast IPv4 address with the same identifier, sequence and
  payload, within a limit of 20 replies per second. Never answer broadcast
  echo. Match echo replies to a pending request by remote address, local
  address, identifier, sequence and payload token. Ignore stale, duplicate or
  mismatched replies. Parse destination-unreachable and time-exceeded errors
  for a pending echo request only when the quoted IPv4 header and echo
  identifier/sequence match that request; return a typed diagnostic. Do not
  interpret arbitrary ICMP as success.
- UDP is internal: validate IPv4 and UDP lengths and checksum rules, accept
  only DHCP client port 68 from server port 67, and transmit only DHCP client
  packets to port 67. Accept an IPv4 UDP checksum of zero as the protocol's
  no-checksum form; verify any nonzero checksum. No client session can send a
  UDP datagram.

The service must work with an Ethernet frame of 1514 bytes and reject any
larger frame without truncating it into a valid-looking packet. Every parser
must be host-testable without a running kernel or VM.

### 6.2 Address configuration

The configuration service declares these typed keys in its existing store:

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `system.net.mode` | text | `dhcp` | `dhcp` or `static` |
| `system.net.ipv4.address` | text | `0.0.0.0` | Static unicast IPv4 |
| `system.net.ipv4.prefix` | uint | `24` | Static prefix 1..30 |
| `system.net.ipv4.gateway` | text | `0.0.0.0` | Static on-link gateway; zero means none |

`netd` starts after the normal-mode configuration service. It reads the four
keys through a `system.net.` read-only session. `net reload` reads all four,
validates the candidate, then changes the active configuration in one step.
An invalid candidate leaves the previous working configuration active and
reports the invalid key. The existing configuration service has no multi-key
transaction: concurrent `config set` calls can yield a mixed but validated
candidate. The documented sequence completes all `config set` calls before
`net reload`. Static mode requires a unicast address, a prefix in range and
a gateway inside that prefix if set.
Changing mode or address clears ARP entries and fails pending pings with
`Reconfigured`. Configuration changes persist through the existing
`/state/config` store; the service does not create a second network config file.

DHCP mode implements DISCOVER, OFFER, REQUEST, ACK/NAK, BOUND, RENEW and REBIND
for one interface. Use a per-attempt transaction ID and MAC check; accept a
server identifier, subnet mask, first router, lease, T1 and T2. Validate the
BOOTP header and bounded options, including pad, end and option-overload
handling. Require an OFFER server identifier, subnet mask and finite lease;
allow no router. Default absent T1/T2 to one-half and seven-eighths of the
lease and validate supplied T1 < T2 < lease. Reject leases shorter than
30 seconds and routers outside the offered prefix. Ignore DNS options. Apply an
address only after ACK and an ARP
conflict check. Send DECLINE on a detected conflict; do not assign the address.
Static mode performs the same conflict check before becoming `ready`.
Renew and rebind before the lease expires; at expiry remove the address and
route, clear ARP, fail pending pings, and restart discovery. Retransmission
uses monotonic deadlines with bounded exponential backoff and jitter; DHCP
failure is `configuring`/`no address`, never a boot blocker or busy loop.

`net status` prints link, MAC, mode, address/prefix, gateway, lease remaining
when applicable, and counters. `down` means no usable driver or no carrier;
`configuring` means a live link without a usable address; `ready` means an
address passed validation and conflict detection; `failed` means a device or
configuration fault requiring a reload or driver restart. It prints no DNS server because DNS is out of
scope. The command has no hardware or filesystem authority.

## 7. Client protocol and commands

`GNETAPI1` version 1 uses one PID 1-created channel per client. The network
service's control endpoint accepts `AddSession` only from PID 1, with a
channel handle and rights fixed to `Ping + Status` or `Status + Reload`.
The service verifies rights from its session record, not from flags supplied
by a client. It drops a dead client session without affecting others.

Request opcode values are `Status=1`, `Echo=2`, `Reload=3`. A reply echoes the
request opcode. Result values are `OK=0`, `Down=1`, `NoAddress=2`, `NoRoute=3`,
`NeighborUnreachable=4`, `Timeout=5`, `Busy=6`, `Reconfigured=7`,
`PermissionDenied=8`, `InvalidArgument=9`, `DriverFault=10`,
`ConfigInvalid=11`, `RemoteUnreachable=12`, and `TimeExceeded=13`.
Interface state values are `down=0`, `configuring=1`, `ready=2`, `failed=3`;
mode values are `dhcp=0`, `static=1`. Validation field codes are `none=0`,
`mode=1`, `address=2`, `prefix=3`, `gateway=4`.

Each request has a 16-byte payload: opcode at byte 0, zero bytes 1..3,
destination IPv4 at bytes 4..7, `uint16` sequence at 8..9, `uint16` timeout
in milliseconds at 10..11, and zero bytes 12..15. `Status` and `Reload`
require every field after the opcode to be zero. Replies have a fixed 80-byte
payload: opcode, typed result, interface state and mode in bytes 0..3;
local address at 4..7; gateway at 8..11; echo responder at 12..15;
`uint64` RTT nanoseconds at 16..23; MAC at 24..29; prefix and link state at
30..31; `uint64` lease seconds at 32..39; `uint16` sequence and validation
field code at 40..43; four `uint64` RX, TX, drop and malformed counters at
44..75; and zero bytes 76..79. Multi-byte values are little-endian. Fields
not used by the reply opcode are zero. The codec rejects a wrong payload size,
nonzero reserved field, unknown enum or correlation mismatch. The service
saturates a counter before encoding rather than wrapping it.

`GNETCTL1` uses a 16-byte request and reply body: opcode at byte 0, rights
class in requests or result in replies at byte 1, zero bytes 2..7, and a `uint64` generation at
8..15. Opcode values are `AddSession=1` and `SetDevice=2`; rights classes
are `PingStatus=1` and `AdminStatus=2`; result values are `OK=0`,
`InvalidArgument=1`, `Busy=2`, and `DriverFault=3`. `AddSession` attaches exactly one
channel and uses generation zero;
`SetDevice` attaches exactly one channel and two memory objects, in that
order, uses a nonzero generation and has a zero rights-class byte. Each reply
echoes the request opcode and generation, has a correlated ID and no attachments.
Unsupported rights classes and partial attachments
are refused and closed.

| Request | Reply | Limit |
| --- | --- | --- |
| `Status` | interface snapshot and counters | One fixed-size reply |
| `Echo` | responder, sequence, RTT in monotonic nanoseconds, or typed error | One outstanding request per session, eight total |
| `Reload` | applied or named validation error | Admin session only |

An `Echo` request carries a four-byte numeric destination, sequence and
deadline; the service builds the ICMP packet and payload token. The deadline
is bounded to 100..5000 ms. While alive, the service replies once for every
accepted request, including timeout, driver peer death and reconfiguration.
A client of a crashed service observes `PeerDied` on its channel.
It never leaves a caller blocked on an unresolved ARP entry or dead driver.
Malformed or unauthorized operations return a typed error without changing
network state.

Extend the command index with `GrantNetworkPing` (`0x10`) and
`GrantNetworkAdmin` (`0x20`). The build-time manifest parser, index validator,
trusted-origin check, PID 1 launch path and user utility startup handling must
agree on the bits. Bump `IndexFormat` from 2 to 3 and update the recorded
release contract because the stored grant semantics change. PID 1 grants a
network session only to an installed
or recovery command whose trusted index entry asks for it; this milestone
installs no network commands in recovery.

`userspace/bin/ping` requests `GrantNetworkPing`. Syntax:

```text
ping [-c COUNT] [-W TIMEOUT_MS] IPv4
```

`COUNT` defaults to 4 and ranges from 1 to 16. `TIMEOUT_MS` defaults to 1000
and ranges from 100 to 5000. Requests are sequential, one per second, with a
32-byte ICMP payload. The command prints one result or explicit failure for
each sequence and a sent/received/lost summary. Exit 0 means at least one
reply, exit 1 means no replies, exit 2 means syntax or service error. A
hostname, multicast, broadcast or malformed IPv4 address is refused before
the first network request.

`userspace/bin/net` declares `GrantNetworkAdmin` in its manifest. Its exact
syntax is `net status` or `net reload`; any other arguments exit with a syntax
error. PID 1 gives it a `Status + Reload` session for either subcommand. Static configuration is
entered through the existing `config set system.net.*` shell command followed
by `net reload`;
there is no parallel setting syntax. Both commands follow the normal native
manifest, help, separate-image and standard-stream rules.

## 8. Restart and failure behavior

PID 1 keeps the NIC device capabilities so a replacement driver can receive
the same bounded authority. On initial start and each restart it creates a
fresh driver channel and fresh RX/TX shared objects, gives the driver its
ends, and sends the stack a new device binding over the control endpoint
before launching the driver. The stack closes the old mappings and handles,
rejects messages from an older generation,
clears ARP and DHCP state as needed, and resumes service only after the new
driver reports `Ready`. Driver death fails in-flight pings within their
deadline and does not kill the network service. The driver follows its stack
dependency; if the stack restarts, PID 1 also restarts the driver with fresh
channels and buffers. Existing command sessions with a dead stack fail with
`PeerDied`; new commands receive sessions to the replacement.

A device fault, DHCP timeout, invalid packet or lack of carrier must not panic
the kernel or stop the shell. Log state transitions and bounded error counters,
not every dropped packet. Normal boot does not inject faults; validation may
kill the NIC driver once and verify recovery. The storage driver must remain
running and the mounted filesystem usable throughout that test.

## 9. Build and implementation map

Implement in dependency order. Each step has a host-testable result before
the VM proof:

1. Kernel: PCI identity/BAR tests, independent IRQ route and wait semantics,
   per-device DMA teardown; retain block tests. Inspect the relevant
   `kernel/arch/amd64/io.S` assembly directly because its graph parse coverage
   is partial.
2. Pure packages: `userspace/netdev` for the driver IPC codec and queue
   bookkeeping; `userspace/network` for Ethernet/ARP/IPv4/ICMP/DHCP parsers,
   routing and timer state. Both reject hostile lengths and counters.
3. Freestanding programs: `userspace/cmd/virtio-net` and
   `userspace/cmd/netd`, using `userspace/runtime/sys` for every native call.
   Keep `unsafe` in that runtime boundary.
4. PID 1: normal-mode service registration, installed image checks, fresh
   channel/pool generations, bounded client sessions and trusted grant policy.
   Place the driver and stack in `system/services/`; neither is required to
   mount the disk or enter recovery.
5. Commands and build: `userspace/bin/ping`, `userspace/bin/net`, command
   index, `tools/sysimage`, `Makefile` image rules and stack-frame checks.
   `make run` and `make run-gui` attach an explicit legacy virtio-net NIC to
   QEMU user networking, with a distinct MAC and a documented way to disable
   the NIC. They continue to use the existing disk without rewriting it.
6. Documentation: an accepted ADR for device/IRQ authority and an accepted
   ADR for the network service/protocol, a network architecture document,
   security capability/threat tables, command help, testing instructions,
   and truthful README/roadmap/Linux ABI status after implementation.

The implementation must not hide increased service, handle, memory or startup
requirements by silently raising global quotas. Measure the new steady-state
cost and change a quota only with a test showing the old bound is insufficient.

## 10. Verification and acceptance

Host tests cover device identity and BAR rejection; IRQ register/recheck and
teardown races; both virtqueues and malformed used entries; every IPC opcode
and slot ownership transition; Ethernet, ARP, IPv4, ICMP, UDP and DHCP
truncation/checksum/overflow cases; DHCP lease transitions; static validation;
ARP and ping deadlines; generation rejection; and client authorization.
Fuzz the pure packet decoders with bounded input. Run the existing storage and
capability tests after kernel changes.

`make test-network` is the canonical VM gate, with one run of each required
scenario and separate disposable disks:

1. Boot a normal installed GoOS validation build under QEMU `q35` with one
   legacy virtio-net device and user networking. DHCP must reach `ready` with a `10.0.2.x/24`
   address and gateway `10.0.2.2` within 15 seconds. From the native shell,
   `ping -c 4 -W 1000 10.0.2.2` must print four matching replies, 0% loss and
   exit 0. The packet path must go through the GoOS driver and network service.
   In this same boot, kill the NIC driver once while a disk file is open:
   the block driver, file and shell remain usable; the NIC replacement obtains
   a new generation and the gateway ping works again.
2. Boot two GoOS guests on one private QEMU stream backend with distinct MACs
   and static `192.0.2.10/24` and `192.0.2.11/24` addresses, no gateway and
   no host TAP privileges. Each guest must answer four echo requests sent by
   the other. This proves both transmit and inbound echo handling without
   relying on host ICMP permissions or an Internet endpoint.
3. Boot without a NIC: reach both normal shell and storage; `net status`
   reports down and `ping` fails within its configured deadline.

The gate checks command exit codes and exact packet/reply identity, not only
startup log markers. It checks no unbounded growth in process, channel, shared
object and DMA-page counts across repeated pings and one driver restart.
Expose exact object counters where current diagnostics are insufficient; a
stable coarse page count alone is not proof of leak freedom. Keep the network
gate separate from unrelated VM probes during development, then include it in
the release acceptance path once stable.

External public-IP ping is a manual diagnostic, not the automated acceptance
target. QEMU user networking documents that ping to its `10.0.2.2` router
works while general ICMP reachability depends on host support and permissions.
Do not report an external ping failure as a GoOS failure without a second
network backend check.

## 11. Standards and test-backend references

- [Virtio 1.2, PCI transport and network device](https://docs.oasis-open.org/virtio/virtio/v1.2/virtio-v1.2.html)
- [RFC 894, IP over Ethernet](https://www.rfc-editor.org/info/rfc894/), [RFC 826, ARP](https://www.rfc-editor.org/info/rfc826/)
- [RFC 791, IPv4](https://www.rfc-editor.org/info/rfc791/), [RFC 792, ICMP](https://www.rfc-editor.org/info/rfc792/), [RFC 1122, host requirements](https://www.rfc-editor.org/info/rfc1122/)
- [RFC 2131, DHCP](https://www.rfc-editor.org/info/rfc2131/), [RFC 2132, DHCP options](https://www.rfc-editor.org/info/rfc2132/)
- [QEMU network emulation](https://www.qemu.org/docs/master/system/devices/net.html) and [QEMU stream backend syntax](https://www.qemu.org/docs/master/system/qemu-manpage.html)
