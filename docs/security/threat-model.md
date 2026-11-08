# Threat model

Scope: GoOS 0.2.9 on x86_64 UEFI. The release supports SMP (up to 64 online
processors by the current architecture bound); the primary validation platform
is QEMU q35 with OVMF, with selected KVM/Proxmox gates. This document states
what is protected, from whom, and which mechanisms enforce it. Mechanism
details live in the linked documents.

## Assets

| Asset | Where it lives | Why it matters |
| --- | --- | --- |
| Kernel integrity | kernel image, page tables, GDT/IDT/TSS, kernel stacks | Every other guarantee depends on it |
| Physical memory | PMM bitmap, owned user pages, shared image frames, the two drivers' DMA windows | Aliasing a page breaks isolation |
| Handle tables and registries | per-process `handle.Table`, channel, stream, process registries | They *are* the authority model |
| Resource accounts | per-process quota counters (ADR 0072) | Stop one process from starving the rest |
| PID 1 authority | PID 1 handle table | Root of every capability granted after boot |
| Boot archive | initramfs USTAR on the ESP, held in firmware-reserved pages and exposed read-only at `/bootstrap` and `/etc` | Defines PID 1, bootstrap/recovery services and recovery commands; it is replaced with the ISO |
| Installed system | protected `system` directory on the FAT32 volume, published read-only through VFS as `/system` | Defines normal-mode services, Linux fixtures, native commands and their command index; compatibility is checked, authenticity is not |
| Persistent data | FAT32 image on the virtio-blk disk (`/persist`, with `/home` and `/state` on the same volume) | Survives reboot; reachable only through VFS |
| System log and configuration | kernel log ring, `/state/system.log`, `/state/config` | Diagnostics and the settings components act on |
| Console | COM1 through the console device capability, and the terminal output stream the terminal service relays | Operator I/O, warning echo, and read access to the log ring |
| Network interface | `netd`'s address, ARP table and DHCP lease, the frame pools | What the guest says and answers on the link |
| Name resolution | `resolved`'s cache and server list, the hosts table in `/state/hosts` | Decides which address a name, such as one given to `ping`, reaches |
| CPU time | scheduler slots, wait sets | Liveness of every service |

## Trust boundaries

```text
host (QEMU/firmware, ISO, disk image file)
 └─ kernel (ring 0)                        trusted computing base
     ├─ PID 1 (ring 3)                     holds initial policy authority
     │   ├─ VFS service                    maps the boot archive read-only and serves RAMFS plus the FAT32-backed namespace
     │   ├─ FAT32 service                  parses the volume and serves /persist, /system, /apps, /home and /state through a partition-scoped block endpoint
     │   ├─ block service                  owns block endpoints and the storage-driver client endpoint
     │   ├─ virtio-blk driver              holds storage port, IRQ and DMA authority
     │   ├─ virtio-net driver              holds NIC port, IRQ and DMA authority, the netd channel and frame pools
     │   ├─ netd                           owns Ethernet/ARP/IPv4/ICMP/DHCP/UDP/TCP state and bounded native sessions; no device authority
     │   ├─ linuxsockd                     owns Linux-family socket state and bounded GTCP 4/GUDP 3 sessions to netd; no VFS or device authority
     │   ├─ resolved                       owns bounded UDP/TCP sessions, network status, DNS configuration/hosts read access and resolver client sessions
     │   ├─ config service                 holds the configuration store and a VFS session
     │   ├─ log service                    holds log-read, console-write and a VFS session
     │   ├─ terminal service               holds console-read and the terminal output stream reader; a session with the shell
     │   ├─ remoted / workers / sftpd      split listener/signing, SSH-session and rooted-SFTP filesystem authority
     │   └─ shell                          holds terminal, log, VFS, exec and configuration sessions; no console-read authority
     └─ applications (ring 3)              always receive bounded standard streams; optional VFS/network/resolver authority is granted by launch policy
```

| Boundary | Crossing | Enforcement |
| --- | --- | --- |
| user → kernel | native `int 0x80`, Linux `SYSCALL`, CPU faults | ABI-specific dispatch, bounded native/Linux call contracts, handle/type/right checks, `CopyIn`/`CopyOut` ([attack-surface.md](attack-surface.md), ADR 0132) |
| process ↔ process | typed IPC, streams, handle transfer, shared pages | per-process address spaces, generation-safe handles, non-escalating transfer ([capabilities.md](capabilities.md)); shared image pages are never writable (ADR 0081) |
| shell, commands → terminal | terminal output stream (write-only copies) | only the terminal service holds a reading end besides PID 1; stream ends are reference counted, so no holder can close the stream for the others (ADR 0089) |
| PID 1 → kernel launch | lifecycle channel | only PID 1 holds the lifecycle endpoint; the kernel validates every grant and resource profile; bootstrap images are archive extents and installed/user images are stable file-backed executable sources (ADR 0093, ADR 0121, ADR 0175) |
| shell → PID 1 | exec channel and exec area | PID 1 copies the request out of the shared page before validating it, accepts only `/system/bin/<name>` paths, and grants a VFS session only when the command's entry in the command index asks for one (ADR 0030, 0064, 0080) |
| VFS → storage | `GBLK001` IPC | block service validates ranges and overflow; VFS never holds device authority (ADR 0033, 0036) |
| VFS → boot archive | `ImageMap` | read-only, non-executable, unowned mapping of the boot archive; the capability is held only by VFS and serves `/bootstrap` and `/etc` (ADR 0070, ADR 0175) |
| client → config service | `GCF1` session | scope and rights are fixed by PID 1 when it creates the session (ADR 0074, 0076) |
| driver → hardware | port, IRQ, DMA calls | BAR-bounded ports, one line per driver, fixed DMA window, a DMA domain per device (ADR 0034, 0035, 0190) |
| link → `netd` | frames the driver copies into the receive pool | the driver bounds slot/length ownership; `netd` validates every protocol field and uses bounded fixed or capacity-controlled state (ADR 0191, ADR 0192, ADR 0213) |
| `netd` ↔ driver | `GNETDRV1` and two shared pools | slot numbers only, exact slot ownership, a generation per binding; the driver copies frames out of the pool before the device sees them (ADR 0191) |
| commands → `netd` | `GNETAPI1` session | rights class fixed by PID 1 at `AddSession`; one echo in flight per session (ADR 0191) |
| programs, `resolved` → `netd` | GUDP 2 session and two shared pools | PID 1 fixes session quota and pool rights; endpoint IDs are generation-safe; an ordinary session holds at most 8 endpoints, while the global endpoint table defaults to 1024 (ceiling 4096); receive payloads come from a 512-buffer pool with at most 256 held by one owner (ADR 0195, ADR 0215) |
| programs, `resolved` → `netd` | GTCP 2 session and two shared pools | PID 1 fixes session quota and pool rights; endpoint IDs are generation-safe; live endpoint storage and stream rings grow on demand inside endpoint, memory and TIME_WAIT budgets; indexed lookup replaces full-table receive scans (ADR 0196, ADR 0213) |
| Linux family → `linuxsockd` | GLNXSOCK3 channel and shared family pool | one session per Linux family; kernel validates the pool and generation-safe socket identities; family/service/reserve quotas bound sockets and in-flight operations (ADR 0210) |
| `linuxsockd` → `netd` | GTCP 4 and GUDP 3 sessions | control stays on typed IPC; stream/datagram payload moves through shared rings with generation and ownership checks; `netd` grants up to 512 endpoints per protocol to the service (ADR 0210, ADR 0214, ADR 0216) |
| `resolved` → `netd` | `GNETAPI1` status session | `RightsResolveStatus`: status and the lease's DNS servers only; handed over with the UDP session under a growing generation (ADR 0195) |
| commands → `resolved` | `GRESOLV1` session | rights class fixed by PID 1 at `AddSession`, from `GrantResolve` or `GrantResolveAdmin`; separate from the network session (ADR 0194) |
| shell, `resolved` → hosts table | `GHST1` on a configuration session | `HostsRead` and `HostsWrite` session rights fixed by PID 1; the shell holds both, the resolver only `HostsRead` (ADR 0193) |
| host → guest | firmware, ISO, disk image | firmware/ISO are trusted inputs to this model; disk bytes are untrusted parser input and are not authenticated, so a syntactically compatible installed `/system` can influence what normal mode executes (ADR 0175) |

The kernel and the host/firmware/ISO build are trusted. Nothing in the boot
path cryptographically verifies the kernel, boot archive, or installed system;
the installed-system checks establish compatibility and in-guest write
protection, not provenance. See [boot-chain.md](boot-chain.md) for the planned
verified boot chain. PID 1 is
trusted to apply policy but is still subject to kernel checks. Services, the
driver, and utilities are untrusted relative to each other.

## Attacker models

### Malicious utility

A program launched from the shell, for example a compromised command image in
`/system/bin` or a command fed hostile arguments.

- Always holds only bounded standard streams plus the optional authorities its
  command-index entry names: a private VFS session and/or bounded network and
  resolver sessions. `stdout` and `stderr` are write only, and where they
  reach the terminal they are copies of one writing end shared with the shell
  and other commands (ADR 0080, ADR 0089). It cannot read what other processes
  print, holds no console capability, and receives no Transfer right on these
  grants.
- Sharing the terminal's writing end lets it write into the same stream as
  everything else that prints, as the shared console did before: it can spoof
  another command's output on the screen, and a command that writes without
  pause fills the 8176-byte stream ring so that other writers wait. It cannot close the
  stream for anyone else, and the terminal service keeps serving the keyboard
  while it relays (ADR 0089).
- A command whose index entry asks for a network session can, by its class,
  have `netd` send echo requests to any address it names, one at a time, or
  read the interface and have `netd` reload the stored settings. It builds
  no packet, holds no device, and cannot pass the session on (ADR 0191).
- A command whose index entry asks for a resolver session can have
  `resolved` look up names, read its status and, with `GrantResolveAdmin`,
  flush the cache and reload its settings. It cannot send a datagram, choose
  a server or write the hosts table, and a resolver session grants no echo
  (ADR 0194).
- A command whose index entry asks for `GrantUDP` can bind any free port,
  well-known ports included, and send datagrams to any unicast IPv4 address
  and port, at most one link MTU less 28 bytes each off the host. An ordinary
  session holds at most 8 endpoints; each endpoint's receive queue has hard
  capacity 16, and one owner can hold at most 256 of the global 512 receive
  buffers. It cannot reach another session's endpoints, builds no IPv4 header
  and reads no status (ADR 0195, ADR 0215).
- `dnsq` asks for `GrantUDP | GrantTCP`, so a compromised `dnsq` can do what
  `udp` and `tcp` can together and no more: it holds no resolver session and
  cannot touch the cache, the hosts table or the server list.
- Cannot create channels (`ChannelCreate` is restricted to PID 1), cannot
  launch or stop processes, cannot reach block, driver, lifecycle, or
  configuration endpoints.
- Its VFS session is a dedicated channel; it cannot read replies meant for
  another process (ADR 0030, docs/userspace/vfs.md).
- A fault terminates only its task; exit closes its handles, so peers see
  `PeerDied`/EOF instead of hanging (ADR 0050).
- Residual risks: it can create up to eight streams under the `tiny` launch profile,
  which leaves the rest of the registry to everyone else (ADR 0072); it can
  read the high-resolution clock (ADR 0054) and the process list with each
  process's CPU and resource use (`ProcessInfo`); it can run without yielding,
  because CPU is accounted but not limited; and it can write anything the VFS
  namespace permits, including `/persist`, `/home`, and `/state`.

### Compromised service

VFS, block service, config service, log service, or shell executing
attacker-controlled code.

- Each service holds only the endpoints listed in its PID 1 manifest, with
  Transfer removed, so it cannot forward authority it received.
- VFS compromise exposes every file in its namespace, including the persistent
  volume through the FAT32 protocol, but not the raw disk: it holds no block
  endpoint (ADR 0096). It gains no device, port, IRQ, or DMA authority.
- FAT32 service compromise, for example through a malformed volume, exposes
  the persistent volume through its partition-scoped block endpoint and the
  file cache it shares with VFS (ADR 0098, ADR 0115). It gains no namespace
  control outside that backend and no port, IRQ, or DMA authority. Because the
  installed system has lived on that same volume since ADR 0175, a malicious
  FAT32 service can lie about the protected `/system` bytes it serves: it can
  affect normal-mode session-service images, commands and the installed command
  index on later reads/restarts. PID 1 still applies index format,
  architecture/release compatibility checks and kernel launch/capability
  validation, but those checks are not authenticity. Bootstrap/recovery
  services and commands under `/bootstrap` remain archive-backed and outside
  the FAT32 service's authority.
- Block service compromise exposes the disk through the driver channel. It
  gains no hardware authority.
- Config service compromise exposes and can rewrite every setting, including
  `/state/config`, through its own VFS session.
- Log service compromise can read every retained log record and write records
  and warnings to the console; logging is bounded by record size, not rate
  (ADR 0059, 0073).
- Shell compromise can ask PID 1 to run any `/system/bin` image with the grants
  its index entry names, stop its own jobs, write to the terminal, read the
  lines the terminal service gives it, read and write every configuration
  setting through its full-scope session, and ask PID 1 to sync and reset the
  machine (`reboot`, ADR 0061). It cannot reset the machine itself: only PID 1
  holds the system-control capability. It holds no console capability and
  cannot read the terminal's output stream (ADR 0089).
- Terminal service compromise can read everything typed and everything the
  shell and commands print, and write anything to the console. It holds nothing
  else: no filesystem, no process control, and a control channel that only
  accepts a session from PID 1.
- The storage driver, the FAT32 service, the config service, and the log
  service restart on failure; the terminal service and the shell always
  restart, independently, each handed a fresh session with the other
  (ADR 0089). The VFS restarts on failure, and PID 1 hands the shell, config
  and log services new sessions over channels they already hold; `/tmp` and
  the files open at the time are lost (ADR 0099). The config and log services
  get the sessions their first launch consumed, and the config service's
  clients new sessions with it (ADR 0100). The block service restarts on
  failure, with the driver and the FAT32 service restarting behind it
  (ADR 0101). While a service is down its clients observe `PeerDied`, and
  bounded storage deadlines report I/O errors instead of freezing (ADR 0048,
  0078).

- `netd` compromise, for example through a malformed frame, controls what
  the guest sends and answers on the link and what `ping` and `net status`
  report. It holds no device authority, no VFS session, and a configuration
  session that can only read `system.net.`. It cannot write the receive
  pool, and the frames it writes to the transmit pool are copied by the
  driver before the device reads them. It restarts on failure with a new
  control channel, and the driver restarts behind it (ADR 0191).
- `resolved` compromise, for example through a malformed DNS reply, controls
  the answers commands get for every name except `localhost`, and can send
  UDP and open TCP connections to any address and port with a command's
  limits, since its UDP and TCP sessions are ordinary ones. It holds no device, no VFS session, a status session
  that reads only status and DNS servers, and a configuration session that
  can only read `system.dns.` and the hosts table. It cannot write its RX
  pool. It restarts on failure, and PID 1 hands it new sessions, so queries
  on the old ones fail (ADR 0194, ADR 0195).

### Compromised driver

The `virtio-blk` process programs a bus-mastering device. There is no IOMMU:
the device can DMA to any physical address the driver writes into the
virtqueue or into the legacy queue-address register inside its port range. A
malicious driver can therefore corrupt kernel memory, the boot archive, and
the shared read-only image pages every process running an image maps
(ADR 0081); sharing does not widen what it can reach. Capability scoping
(ADR 0034) contains accidental driver faults and removes ambient port access;
it does not contain a malicious driver. The driver is part of the effective
TCB until an IOMMU is used. PID 1 keeps the driver's port, IRQ, and DMA
authority so that it can grant it to a restarted driver (ADR 0078).

The `virtio-net` driver is in the same position (ADR 0190): its device is
bus-mastering, nothing confines its DMA, and it is part of the effective TCB
for the same reason. Its own DMA domain means that its exit resets the NIC
and never the disk. That contains a crash and does nothing against a
malicious driver.

### Linux program using sockets

A static Linux program that calls the socket ABI with hostile arguments.

- It reaches the network only through its family session to `linuxsockd`.
  It holds no GTCP or GUDP session, no `netd` control, no pool of another
  family, and no handle it could pass on (ADR 0210).
- The kernel copies every sockaddr, iovec array, `msghdr`, option value and
  length through the checked copy helpers and bounds each count before it
  indexes anything; a bad pointer is `EFAULT` and a short length `EINVAL`.
- A socket identity names a slot of its own family under the current session
  generation. An identity from another family, a closed slot, or an older
  session answers `Stale` and becomes an errno.
- One family holds at most 512 sockets and eight requests in flight, and
  `linuxsockd` holds 16 families and 1024 sockets. Each live family keeps 32
  sockets that no other family can take, so one family that fills its quota
  leaves room for the rest. `netd` grants `linuxsockd` up to 512 endpoints per protocol; ordinary native command sessions remain at 8 by policy.
- Ports below 1024 are `EACCES`, so a program cannot take the resolver's
  stub on `127.0.0.1:53`.

### Hostile network peer

Anything on the link can send the guest arbitrary frames.

- Frames reach `netd` only through the driver. It checks each used-ring
  entry's index, descriptor, length and header, and a bad one ends the
  driver; PID 1 starts another. It copies at most one 1514-byte frame into a
  receive slot.
- `netd` checks every length and offset before reading a field and refuses
  oversized frames, IPv4 options and fragments, bad checksums, and DHCP
  replies for another transaction or MAC. Fixed control state includes 16 ARP
  entries, 4 resolutions and an 8-frame transmit ring. UDP/TCP capacity is
  separately bounded: UDP endpoints default to 1024 with a 4096 ceiling and
  512 receive buffers; TCP endpoints default to 1024 with a 4096 ceiling and
  grow-on-demand stream storage inside explicit memory and TIME_WAIT budgets.
- UDP reaches only endpoints bound to the port on the interface's own
  address, after the length and checksum checks. An endpoint queue has hard
  capacity 16, and one owner can hold at most 256 of the global 512 receive
  buffers, so flooding one owner cannot consume the entire pool. Later
  datagrams are dropped at those bounds. No ICMP error answers a datagram to
  an unbound port (ADR 0195, ADR 0215).
- The ARP table learns a sender only when it is on the prefix and talking to
  us, or already known or being resolved. A peer on the link can still
  answer for another address, as on any ARP network, and so take traffic
  meant for it.
- Echo replies are matched by addresses, identifier, sequence and a random
  payload token, so a reply cannot be forged without seeing the request.
  ICMP errors quote only eight bytes of the request and are matched without
  the token.
- A DHCP server on the link decides the address, prefix and router. An
  address another host holds is declined after the ARP probe.
- `netd` answers at most 20 echo requests a second, so an echo flood cannot
  fill its transmit ring.
- Address conflicts follow RFC 5227. Before using an address `netd` probes
  for it, and once the address is live it keeps watching: the first claim on
  it by another host is answered with one announcement, a second claim within
  the defence interval makes `netd` give the address up, and after a run of
  conflicts the next claim waits the rate-limit interval. A host that keeps
  claiming the address can keep the guest off it, as on any ARP network, but
  cannot make it probe without pause (ADR 0192).
- Loopback traffic never leaves the guest: nothing sent to `127.0.0.0/8` is
  framed, resolved with ARP or given to the gateway. A frame from the link
  with a loopback source or destination, IPv4 or ARP, is dropped and
  counted, and a static address, gateway, DHCP lease or DNS server may not be
  a loopback address (ADR 0193).
- DNS answers are protected against off-path spoofing, not against a peer
  that sees the queries. Each query leaves from a fresh random port in
  49152..65535 with a random transaction ID, both from generators seeded by
  `RandomRead`. Each query has its own endpoint connected to the server's
  address and port 53, which drops every other source and closes when the
  transaction stops waiting, and `resolved` requires the transaction ID, the server and
  exactly one question equal to the one asked, then checks every count,
  length, label and compression pointer before using anything. A reply that
  fails any check is dropped whole, so nothing partial reaches the cache,
  and a truncated reply is not used. Only a reply that passed the ID,
  server and question checks can make `resolved` repeat the query over TCP,
  and only to the server it asked. The TCP reply's two-byte length goes
  into one of two fixed 65535-byte buffers, a length below 12 bytes ends the
  connection, and the reply then passes the same checks as a datagram; a
  wrong ID or question fails the server instead of waiting for another
  message. A server cannot hold a buffer past the lookup's deadline, and
  TC=1 over TCP does not start another fallback. `localhost` and the hosts table are
  answered before any query is sent (ADR 0193, ADR 0194).
- `dnsq` sends one query to the server you name and trusts nothing it gets
  back until `userspace/dnswire` has checked the same things it checks for
  `resolved`: ID, one matching question, counts, lengths, labels and
  pointers, and then the data of every record it will print. A reply that
  fails prints nothing but the failure. TC=1 over UDP repeats the query over
  TCP once, to the same server and within the same deadline; TC=1 over TCP
  is an error. The response lands in one fixed 65535-byte buffer, so no
  length from the server sizes an allocation. Names and TXT strings print
  with every byte outside printable ASCII escaped as `\DDD`, so a server
  cannot send terminal control sequences through `dnsq`.
- A DHCP server on the link decides which DNS servers the guest asks, and a
  server it asks can answer with any address. A cached answer lasts at most
  `system.dns.cache.max-ttl-sec`.
- HTTPS adds server authentication and encrypted application data within the
  native client process. A forged DNS or ARP answer can redirect or deny the
  connection, but the selected verifier still requires the requested DNS/IP SAN
  and a chain to explicit system roots. Plain HTTP and link protocols retain
  their existing unauthenticated behavior.
- A hostile TLS peer controls records and certificates. Native bounds limit
  peer lists to eight, DER to 32 KiB, DNS SAN entries to 64 and candidate depth
  to eight, with the standard 100-signature-attempt budget. One 35-second
  monotonic deadline covers authentication; per-operation transport deadlines
  bound later I/O. The standard TLS 1.3 certificate buffer can reach 256 KiB.
- Trust depends on the immutable bootstrap bundle, update public key, signed
  state, build inputs and initial
  firmware REALTIME. Missing or malformed roots, invalid time and rollback
  during validation fail closed. Public-CA compromise, a false initial firmware
  clock and out-of-band Mozilla/Firefox trust constraints remain external trust
  risks. The client does not fetch AIA intermediates or perform CRL, OCSP or CT
  checks. Its CLI offers neither `-k` nor a mutable CA path. A followed HTTPS
  downgrade fails before connecting; cross-origin redirects strip credentials,
  cookies and a custom Host header. The native HTTPS gate passed 69 assertions,
  including bad CertificateVerify, Finished and record-MAC failures. Certificate
  and handshake rejection produced zero HTTP requests. `ADR 0198` records the TLS implementation and its validation evidence; the current release gate is tracked separately.

### Malformed storage image

The raw disk image is host-provided and persists across boots.

- The FAT32 service validates every BPB field and checks the declared geometry
  against the device capacity before deriving any offset, checks every FAT link,
  and walks chains with loops bounded by the cluster count. Long names are
  decoded from UTF-16 only when valid, bounded to 255 UTF-16 units and a
  1024-byte native result. Primary and backup FSInfo signatures and values are
  checked; hints and free counts are never trusted as allocation authority. A corrupt primary boot sector is served read-only from a
  valid backup and never rewritten at mount (ADR 0077, 0102, 0112). A seeded
  corruption corpus covers each case.
- The partition table is parsed in the block service, which already holds the
  whole disk, so the parser gains no authority. MBR entries and GPT headers,
  CRCs and entries are checked against each other and the device; a GPT falls
  back to its backup but is never rewritten. The FAT32 service is given only a
  block slice of its partition: every offset is checked against the slice
  before the device sees it, so a compromised filesystem service cannot reach
  the table or another partition (ADR 0115).
- A volume that fails the checks is refused, not fatal: the system boots with
  `/persist`, `/home`, and `/state` absent, and the log and config services
  continue without their files (ADR 0077).
- Parsing runs inside the FAT32 service, never in the kernel or the VFS
  (ADR 0096). A parser defect is contained to that service: a channel to the
  VFS and a block endpoint for the one volume.
- The block service rejects out-of-range and overflowing requests before they
  reach the driver.
- Crash consistency is not addressed: an interrupted write can leave a volume
  that mounts and reads inconsistently.

### Malformed IPC

- The kernel copies at most 320 bytes of wire data, validates the header,
  payload size, and attached-handle count before queueing, and checks Transfer
  authority on every attached handle (docs/architecture/ipc.md).
- Protocol decoders (`GVF1`, `GRF1`, `GBLK001`, lifecycle, exec and its
  control protocol, `GCF1` and its control protocol, `GHST1`, `GNETDRV1`,
  `GNETAPI1`, `GNETCTL1`, GUDP/GTCP, `GLNXSOCK3`, `GRESOLV1` and the remote/SFTP protocols) check protocol ID,
  version, sizes, and require reserved fields to be zero.
- Services answer unparsable but attributable requests with an
  invalid-argument reply instead of dropping them (ADR 0048). A request whose
  peer has died is dropped without stopping the service (ADR 0078).
- Requests that travel through shared memory (launch staging, exec area) are
  copied into private memory once, before validation, so the sender cannot
  change them afterwards (ADR 0062, 0064).
- The same holds for data moved through shared memory: the VFS copies paths out
  of a client's attached object, the VFS and the FAT32 service copy a long name
  out of the file cache's name page, and the kernel copies a startup template
  once, validates the whole `GST4` object, and binds handles in its own copy
  (ADR 0109, ADR 0110). An attached object is mapped for one request, bounded
  in size, and never with more rights than its handle carries.

### Malicious native image

Images come from the initramfs built into the ISO or, for a command outside
`/system`, from an immutable file-backed source. VFS pins its stable open object
against writes and truncation; FAT32 refuses unlink while it remains open.
A tampered
image is still loaded through the bounded native loader: fixed header, at most
eight page-aligned segments inside the image area, no W+X, no overlaps, entry
in an executable segment, and a stack the kernel reserves with a 64 KiB guard
below it (docs/kernel/native-images.md). Its pages are read by bounded offset
through the source only as it touches them, each charged to its page quota. A valid but hostile image runs with
exactly the manifest PID 1 grants it. Its read-only segments are shared with
every other process running the same image, but never writable by any of them
(ADR 0081). Images are not signed or measured, so whoever controls the ISO
controls the system ([boot-chain.md](boot-chain.md)).

### Authenticated or compromised SFTP party

| Compromised party | Reach |
|---|---|
| `remoted-worker` | The SSH connection and its plaintext, and the byte streams of its own `sftpd`. No VFS session. |
| `sftpd` | Files its rooted VFS session permits, below the configured root. No host key, signer, listener, launch, terminal or configuration authority. |
| SFTP client | The authenticated session's root, through `sftpd`'s requests. |

Inside the root, a client can read, replace and delete ordinary files, including
readable copies of public keys. It cannot write the configured `authorized_keys` or
move a directory holding it. A compromised `remoted` could name another directory
below `/home` in the policy page; PID 1 refuses any root outside `/home`.
The root confines paths, not backends, so a mount below it is in reach, and an open
handle keeps working if another session moves the file out (ADR 0209).
A cross-linked FAT32 directory inside the root can alias another directory's files,
as it can for any session (ADR 0117); scopes on that target still apply by node.

## Containment summary

| Failure | Contained by |
| --- | --- |
| user CPU fault | per-task fault path; process teardown releases handles (ADR 0008, 0050) |
| wild user pointer | `CopyIn`/`CopyOut` page-walk validation; kernel never dereferences user VAs |
| forged or stale handle | generation + type + rights check in `handle.Table.Lookup` |
| authority escalation | `Derive` refuses added rights; IPC transfer strips Transfer |
| peer death | channel and stream close on teardown; waiters woken with `PeerDied` |
| resource exhaustion | fixed-size tables and bounded launch profiles for channels, streams, tasks and pages (ADR 0072, 0122); failure is a status, not a panic |
| one process starving the rest | each process holds a share of each table, not the table; a refused charge reaches no global structure |
| runaway command | Ctrl-C or `kill` makes PID 1 stop the job through `ProcessKill`, which runs the ordinary teardown (ADR 0067) |
| crash loop | bounded restart count and backoff in the PID 1 supervisor (ADR 0019) |
| stalled storage peer | bounded storage deadlines (ADR 0048) |
| storage driver crash | PID 1 restarts the driver on a fresh channel and hands the block service the new endpoint (ADR 0078) |
| network driver crash | `netd` fails pending echoes and unbinds; PID 1 starts a driver with a new generation, channel and pools; the disk is untouched (ADR 0190, 0191) |
| missing or failed NIC | the network reports down; storage, the installed system and the shell keep running, and recovery is never entered for it (ADR 0191) |
| resolver crash or `netd` restart | PID 1 restarts the service and hands the resolver a new UDP and status session with a new generation; lookups on the old ones fail as `Reconfigured`, and `ping` with a numeric address keeps working (ADR 0194, ADR 0195) |
| `linuxsockd` crash | every Linux socket of the old sessions fails with `ENETRESET` and its waiters wake; PID 1 restarts the service and gives each running Linux launch a new family session with `LinuxNetAttach`, and old socket identities match nothing (ADR 0210) |
| `netd` restart under Linux sockets | `linuxsockd` fails the sockets the dead native sessions backed and takes fresh sessions from PID 1; family sessions and later `socket` calls keep working (ADR 0210) |
| no DNS server known | `localhost` and the hosts table still answer; other names give `NoServers` (ADR 0194) |
| terminal or shell crash | PID 1 restarts it on a fresh session and hands the survivor the other end; a dead shell's jobs are stopped with it (ADR 0089) |
| a pipeline stage exiting | closes only its own copies of shared stream ends; the terminal's writing end stays open for the other stages (ADR 0089) |
| corrupt persistent volume | mount refused, system runs without persistent storage (ADR 0077) |
| driver exit with DMA armed | teardown masks that device's line and resets that device before freeing pages (ADR 0035, 0190) |

## Out of scope for 0.2.9

- IOMMU. DMA is not confined for either driver (see compromised driver).
- Full ASLR. Most kernel/native-user, stack and anonymous-map layouts remain
  fixed. Linux static PIE is the implemented exception: each launch and
  `execve` picks a bounded random base in `[0x40000000, 0x80000000)`
  (ADR 0219). What would have to change for the
  rest is described in [memory.md](../architecture/memory.md#aslr-readiness).
- Signed or measured images, Secure Boot, and initramfs integrity. The planned
  chain is described in [boot-chain.md](boot-chain.md).
- CPU limits. CPU time is accounted per process but never refused (ADR 0072).
- Side channels, including timing through `ClockReadNanos` and cache timing
  between processes that share an image's read-only pages (ADR 0081).
- Physical attacks, malicious firmware, and a malicious hypervisor.
- Confidentiality of `/persist` against anyone with host access to the image.
- Users, logins, and multi-user separation. There is no UID model.
- DNSSEC and encrypted DNS (DNS over TLS or HTTPS). A recursive server, or a
  peer that sees the queries, can answer with any address (ADR 0194).

## Known weaknesses

- PID 1 is a single point of authority. Compromising it grants every
  capability it still holds, including the console with Transfer, both ends of
  the terminal output stream, the lifecycle channel, the VFS, block,
  configuration, network, resolver and terminal control endpoints, the system-control
  capability, both drivers' port, IRQ, and DMA authority, and a stop right over
  every running job.
- The kernel's warning echo, and the reports of the services that start before
  the terminal, still write the console directly and can land in a line being
  typed (ADR 0089).
- The `goos_diag` build prints other processes' handle tables and physical
  addresses. It is for diagnosis only (ADR 0052).
- Some lock domains remain deliberately wider than one object (ADR 0184,
  ADR 0187). Linux compatibility state is partitioned by family/process where
  the current design permits, and each IPC channel has its own queue/state
  lock, so unrelated channels do not serialize on one global channel lock.
  Contention can still delay callers within a shared domain; the validation
  lock watchdog reports a spin longer than four seconds.
- Every kernel stack has an unmapped guard below it, including the boot
  task's since ADR 0091. Only the early boot, before paging is up, runs on the
  unguarded goroutine stack TinyGo allocated for `main`. A kernel stack
  overflow ends the machine with a double-fault report rather than being
  recovered from.

## Signed CA state

HTTPS protects transport, but the signed trust package is authorized by the
immutable trust-update public key. Possession of a currently trusted server
certificate cannot authorize new roots. Missing files, invalid metadata,
signatures, digests or certificates select the immutable system fallback and
report the reason; they cannot disable certificate verification.

Install compares the incoming sequence with the highest surviving published
state generation. Equal sequences allow only byte-identical reinstallation.
The selector cannot authorize an older surviving generation. Corrupt highest
state uses bootstrap roots rather than silently selecting an older update.
The installer keeps old generations and corrupted state for diagnosis.

This protects normal installations and surviving state, not deletion of all
history or host-side restoration of an old disk snapshot. There is no protected
monotonic counter. The immutable bootstrap bundle remains trusted even when it
predates state updates. A compromised update signing key or immutable system
image can replace trust; administrators must protect and back up the external
private signing seed. The normal build never reads that seed.

FAT32 completes successful writes and replacements with device flushes, but it
has no general journal and cannot promise recovery from arbitrary torn sectors
or a device that lies about flush. The installer publishes a complete verified
generation before replacing the selector; recovery verifies signed contents,
and otherwise uses bootstrap roots without blocking the shell.
