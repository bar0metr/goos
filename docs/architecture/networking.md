# Networking

GoOS has one IPv4 interface on one legacy virtio-net function. The kernel
discovers the function, routes its interrupt and bounds the driver's
authority (ADR 0190). Everything that knows what a frame means runs in
userspace: a driver that moves frames, a network service that runs the
protocols, and two commands that ask the service for work (ADR 0191). The
[networking specification](../development/networking.md) records the
requirements this was built from.

## Processes

```text
ping                         net
  | GNETAPI1, PingStatus       | GNETAPI1, AdminStatus
  +-------------+--------------+
                |  one private session per command
                v
netd  <---- GNETCTL1 control ----  PID 1
  |  Ethernet, ARP, IPv4, ICMP echo, DHCP client
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
of `netd`'s control channel. It keeps the device's three capabilities for
the life of the system and grants them to each driver it starts. `netd`
holds no device authority, and the driver holds no filesystem,
configuration or client session.

`netd` waits on its driver channel, its control channel and up to eight
command sessions in one `WaitAny`, with a timeout set by the stack's earliest
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
file and server-name fields is followed, and DNS and other options are
ignored. A `NAK` or an expired lease removes the address and starts
discovery again, and so does a renewal acknowledged for a different
address.

In static mode the address, prefix and optional gateway come from the
configuration service and must pass the checks in
[configuration](../userspace/configuration.md#network). Both modes probe the
address with ARP before it goes live and announce it once it does.

The interface is in one of four states:

| State | Meaning |
| --- | --- |
| `down` | no driver bound, or no carrier |
| `configuring` | a live link without a usable address |
| `ready` | an address that passed validation and the conflict probe |
| `failed` | a device fault or a static address conflict; a reload or a new link-up starts again |

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
  packets use TTL 64 and are never fragmented. The connected prefix is
  tried first, then the default gateway; with neither an echo fails with
  `NoRoute` before anything is sent.
- ICMP echo request, echo reply, destination unreachable and time exceeded,
  each with its checksum checked. Echo requests to our unicast address are
  answered to the sender's MAC. Replies and errors must match a pending echo
  as ADR 0191 describes; anything else changes no state.
- UDP only from port 67 to port 68 for the DHCP client. A zero UDP checksum
  is accepted as "no checksum"; a nonzero one is verified.

## Timers

| Timer | Value |
| --- | --- |
| ARP entry lifetime | 60 s |
| ARP resolution | 3 requests 1 s apart, then `NeighborUnreachable` 1 s after the last |
| address probe | 2 ARP probes 500 ms apart, then 500 ms of silence |
| DHCP `DISCOVER` and `REQUEST` retransmission | 4 s, doubling to 64 s, with up to 1 s of jitter either way |
| DHCP `REQUEST` while selecting | 4 retransmissions, then discovery again |
| DHCP renew and rebind retransmission | half the time left before T2 (renew) or expiry (rebind), at least 60 s, never past that point |
| DHCP retry after a conflict | 10 s |
| echo deadline | 100 to 5000 ms per request, 1000 ms by default in `ping` |
| echo replies answered | 20 per one-second window |
| `ping` request spacing | 1 s between the starts of two requests |
| command reply wait | the request's timeout plus 2 s |
| `netd` idle wait | 30 s |
| `netd` configuration exchange | 2 s per key |
| driver wait | 10 s |

The stack holds no clock. `netd` passes monotonic nanoseconds into it and
asks it for its earliest deadline before every wait, so an idle interface
blocks.

## Limits

| Limit | Value | Source |
| --- | --- | --- |
| network functions | 1 | `pci.FindLegacyVirtioNet` |
| interrupt lines accepted | 3, 5, 6, 9, 10, 12, 14 | `kernel/netdevice.go` |
| interrupt vector | `0x34`, bootstrap processor | `kernel/arch/amd64/tables_uefi.go` |
| frame size | 14 to 1514 bytes, MTU 1500 | `netdev.MinFrame`, `netdev.MaxFrame` |
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

## Failure

A driver that dies, sends `Fault`, or is replaced unbinds: `netd` closes the
old channel and pools, fails pending echoes with `DriverFault`, and reports
`down` until the new driver's `Ready`. PID 1 starts the replacement with a
new generation, channel and pools, so nothing the old driver sent can reach
the new binding. A `netd` restart takes the driver with it, and commands
connected to the old service see `PeerDied`.

A missing NIC, a refused one, or network services that fail leave storage,
the installed system and the shell running; `net status` reports `down` and
`ping` fails within its deadline. Recovery mode runs neither service. See
[init](../userspace/init.md#network-services) for the supervision rules.
