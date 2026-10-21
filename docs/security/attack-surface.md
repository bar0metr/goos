# Native kernel attack surface

The only intentional ring 3 → ring 0 entry points are `int 0x80` native calls,
CPU exceptions, and the timer, virtio-blk (GSI 11), virtio-net (its routed
line, vector `0x34`, ADR 0190), and console UART (GSI 4, ADR 0075)
interrupts. This document covers the native calls defined by
`kcall.Operation` in `kernel/kcall/kcall.go`.

## Entry path

The operation number is in `EAX`; up to three arguments follow in
general-purpose registers (stubs in `userspace/runtime/amd64.S`). The assembly
bridge in `kernel/arch/amd64/io.S` routes the five blocking operations (5, 12,
17, 21, 22) to dedicated dispatchers in `kernel/ipccalls.go`, `kernel/streamcalls.go`
and `kernel/waits.go`. Everything else goes through `kcall.Dispatch`, which checks
fixed sizes before calling the handler in the file for its area: `devicecalls.go`,
`ipccalls.go`, `streamcalls.go`, `launch.go`, `lifecycle.go`, `diagnostics.go`,
`timing.go`, `logging.go`, `sysinfo.go`, `memory.go`, `processkill.go`,
`imagemap.go`, `randomness.go`.
Unknown numbers return `Unsupported`.

Every call runs with interrupts disabled and takes the locks of the state it touches (ADR 0184, ADR 0187). The caller is identified by its CR3 root, and blocking calls also
receive the task slot. Results are a `kcall.Status` in `RAX`. Blocking calls
park the task and re-execute the call on wake (ADR 0015).

## Operations

Authority column: `—` means none; "registered" means the caller must be a
process in the registry. Handles are always looked up in the caller's own
table with type, generation, and rights checks.

| # | Operation | Authority | Untrusted inputs and validation | Bounds | Blocks |
| ---: | --- | --- | --- | --- | --- |
| 1 | `Exit` | — | exit code, not interpreted | none | ends task |
| 2 | retired (`DebugWrite`) | none: refused with `Unsupported` in every build, never reaches a handler (ADR 0094) | — | — | no |
| 3 | `IPCSend` | channel handle, Write | wire length 32..320; `CopyIn`; `ipc.Decode` checks header and sizes; each attached handle (≤ 4) needs Transfer; an attached stream end must have room for four more references, checked before anything moves | 16-message queue; full → `ResourceExhausted` | no |
| 4 | `IPCReceive` | channel handle, Read | capacity ≥ 320; `CopyOut` | static wire buffer | no; empty → `NotFound` |
| 5 | `IPCWaitReceive` | channel handle, Read + Wait | 24-byte descriptor `{buffer, capacity, timeout}`; `now + timeout` wrap check | 64 waiters; per-task status slot | yes, tick deadline; 0 → `Timeout` |
| 6 | `ConsoleWrite` | device handle, Write, console object | length 1..4096; `CopyIn` | static 4 KiB buffer | no |
| 7 | `ConsoleRead` | device handle, Read, console object | length exactly 1; `CopyOut` | one byte from the 8 KiB ring the UART interrupt fills | no; no input → `NotFound`; wait with `WaitAny` |
| 8 | `ClockRead` | — | length exactly 8; `CopyOut` | none | no |
| 9 | `StartupRead` | registered | length 1..64 KiB; `CopyIn` of the caller's own read-only startup pages in 512-byte chunks, zeros past them, `CopyOut` | none | no |
| 10 | `ChannelCreate` | caller is PID 1 | length exactly 16; two free handle slots; `CopyOut` with rollback | 256 channels, global, three pages each while they live (ADR 0111); charged to PID 1's channel quota | no |
| 11 | `HandleClose` | own handle | generation and slot; a stream handle drops one reference, refused below zero | none | no; wakes peers when a stream end's last handle closes |
| 12 | `ProcessWait` | process handle, Wait | result address range (8 B); `now + timeout` wrap check | 64 waiters | yes, tick deadline; 0 → `Timeout` |
| 13 | `SystemInfo` | registered | length exactly 40; `CopyOut` | none | no |
| 14 | `PortRead` | device handle, Read, port object | spec: width 1/2/4, bits 24+ zero, `offset + width ≤ BAR size` (32-bit sum); `CopyOut` | one port access | no |
| 15 | `PortWrite` | device handle, Write, port object | as `PortRead`; value must fit the width | one port access | no |
| 16 | `DMAAllocate` | device handle, Write, DMA object of a discovered device's domain | virtual address must equal the fixed DMA address; 24-byte descriptor `CopyIn`, its size a whole number of pages up to 256 KiB (zero for 16 KiB); the window must be free in the region map | up to 64 contiguous low pages, charged with their page tables to the page quota; a pinned region; at most once per address space | no |
| 17 | `IRQWait` | IRQ handle, Wait, IRQ 11 | none beyond the handle | one waiter slot | yes, **no deadline** |
| 18 | `StreamCreate` | registered | length exactly 16; two free handle slots; `CopyOut` with rollback | 256 streams of one page each, global; 8/16/32 by launch profile, 64 for PID 1 (ADR 0111, 0122) | no |
| 19 | `StreamRead` | reader handle, Read | 24-byte descriptor; size 1..256; result field must be 0; `CanCopyOut` before consuming | static 256 B buffer | no; empty → `ResourceExhausted` |
| 20 | `StreamWrite` | writer handle, Write | as `StreamRead`; `CopyIn` of payload; a write that does not fit whole is refused, never split | static 256 B buffer | no; full → `ResourceExhausted` |
| 21 | `StreamWait` | stream handle, Wait | timeout must be non-zero; wrap check | 64 waiters | yes, tick deadline; a writer wakes when 256 bytes are free |
| 22 | `WaitAny` | per interest: type-specific rights + Wait; the console device needs Read only | count 0..64, where 0 is a timed sleep and needs a finite deadline; interest array and 16-byte result validated; per-interest handle, type, event mask, rights, and live object; an IRQ interest must name the routed network line or line 11, Readable only; a thread-end interest (`WaitAnyThreadEnd`) must name a running or unjoined thread of the caller's native process, Completed only; wrap check | fixed table, one operation per task slot | yes; deadline or infinite (`^0`) |
| 23 | `Diagnostics` | device handle, Write, console object; `goos_diag` build only | optional address, only for the caller's own address space | output proportional to kernel state | no |
| 24 | `ClockReadNanos` | — | length exactly 8; `CopyOut` | none | no |
| 25 | `LogWrite` | device handle, Write, console object | length 1..181; `CopyIn`; `logfmt.Decode` checks version, level, printable ASCII, component and field syntax (failure → `CorruptData`); time and PID are set by the kernel | 64-record ring, oldest overwritten | no |
| 26 | `LogRead` | device handle, Read, log object (ADR 0082) | index < 64; 240-byte `CopyOut` | none | no |
| 27 | `HandleInfo` | registered; own table only | slot < 128; 16-byte `CopyOut` of ID, type, rights | none | no |
| 28 | `ProcessInfo` | registered | index < 32; 80-byte `CopyOut` of PID, state, task count, name, CPU ticks, and the held and permitted resource counts | none | no |
| 29 | `SystemReset` | device handle, Write, system-control object (PID 1 only) | handle lookup | none | resets the machine |
| 30 | `MemoryMap` | memory handle, Read (+ Write when writable), shared-memory object | flags: writable, anywhere; a named address page-aligned, in the user range, and free in the region map; with anywhere, an 8-byte result `CopyOut` | whole object, NX, non-owned, recorded as a region and counted as a reference to the object; page tables charged to the page quota | no |
| 31 | `ProcessKill` | process handle, Write | handle lookup; refuses PID 1 and the caller's own process | none | retires the target's tasks |
| 32 | `ImageMap` | device handle, Read, system-image object (held only by VFS) | virtual address must equal `vm.UserImageAddress`; archive must be page-aligned and fit the image window; 16-byte descriptor probed before mapping; the window must be free in the region map | page tables charged to the page quota; archive mapped read-only, NX, non-owned, as a pinned region | no |
| 33 | `LogBounds` | device handle, Read, log object (ADR 0082) | 16-byte `CopyOut` of oldest and next sequence numbers | none | no |
| 34 | `ConsoleInterrupt` | device handle, Read, console object | handle lookup only | consumes one pending Ctrl-C flag | no; none pending → `NotFound` |
| 35 | `ValidationFault` | validation build only (`Unsupported` otherwise); device handle, Write, system-control object (PID 1 only) | handle lookup only | none: overflows the caller's kernel stack on purpose and ends the machine with a double-fault report (ADR 0091) | no |
| 36 | `SystemPowerOff` | device handle, Write, system-control object (PID 1 only) | handle lookup only; the port and value come from ACPI tables checked at boot (`kernel/acpi`, ADR 0092) | none: one port write that switches the machine off; `Unsupported` without usable tables | no |
| 37 | `ExecutableCreate` | device handle, Write, loader object (PID 1 only, not transferable) | size 1..256 MiB; 8-byte result probed with `CopyOut` before anything is allocated; a free handle slot | 64 objects, global; `ceil(size/4096)` zeroed pages and their page-list index pages charged to the page quota, moved to the first process launched from the object; freed with the last reference — the handle or a process started from it (ADR 0097, ADR 0106) | no |
| 38 | `ExecutableWrite` | memory handle, Write, executable object | length 1..1 MiB in `Dispatch`; append only, refused whole past the object's end or once sealed; `CopyIn` page by page, the length advanced only after the whole copy | none beyond the object's own pages | no |
| 39 | `VMMap` | registered | protection read/write only (execute refused); flags: fixed only; 16-byte descriptor `CopyIn`; length page-aligned, nonzero, bounded; a fixed address page-aligned, in the user range, free in the region map | address space only: pages are demand-zero, each charged on first touch with its page tables; 128 regions per process | no |
| 40 | `VMUnmap` | registered; own map only | range page-aligned, non-wrapping, in the user range; pinned regions refused; a split needs a free region slot, checked before anything changes | frees and uncharges owned pages | no |
| 41 | `VMProtect` | registered; own map only | range wholly covered by unpinned anonymous regions; protection valid and never executable | none | no |
| 42 | `VMDiscard` | registered; own map only | range wholly covered by unpinned anonymous regions | frees and uncharges the pages; the reservation stays | no |
| 43 | `VMQuery` | registered; own map only | 24-byte `CopyOut` of the region at an address | none | no |
| 44 | `MemoryCreate` | registered | page count 1..65536; 8-byte result probed with `CopyOut` before anything is allocated; a free handle slot | pages plus page-list index pages charged to the page quota before allocation; 64 objects system-wide, 16 per payer; freed with the last handle or mapping (ADR 0107) | no |
| 45 | `HandleDuplicate` | a memory handle; rights must be a subset of the original's | handle lookup; rights derivation; 8-byte result probed before insertion; a free handle slot | one handle-table slot; one reference to the object | no |
| 46 | `ClockNow` | registered | clock ID; 16-byte result; monotonic or realtime only | none | no |
| 47 | `ClockSet` | device handle, Write, system-control object | realtime value and validity bounds | none | changes the realtime offset |
| 48 | `ExecutableSource` | loader device with Write; prepared VFS channel with Read, Write, Wait; sealed executable header object with Execute and Write | 32-byte descriptor; file handle nonzero; file size 1..256 MiB; header object size `min(file size, 4096)`; live channel endpoint | consumes the channel; converts the existing one-page object without allocating payload pages; later faults allocate and charge one process page at a time | no |
| 49 | `ConsoleInfo` | device handle, Read, console object | length exactly 16; `CopyOut` | columns, rows, availability flags, and a zero reserved word; no hardware address | no |
| 50 | `CPUInfo` | registered | length exactly 320; version and size fixed; `CopyOut` | immutable normalized CPUID and topology summary collected at boot | no |
| 51 | `CPUEntry` | registered | index below the bounded present-CPU table; 32-byte `CopyOut` | immutable logical CPU, APIC, ACPI UID, state, and normalized topology IDs | no; past the table → `NotFound` |
| 52 | `CPUCacheInfo` | registered | index below the bounded cache table; 48-byte `CopyOut` | immutable deterministic-cache geometry collected at boot | no; past the table → `NotFound` |
| 53 | `CPURuntime` | registered | index below the present-CPU table; 80-byte `CopyOut` | one processor's online state, current task ID, tick, idle, switch, steal, migration, and interrupt counts; no addresses or scheduler state it can change | no; past the table → `NotFound` |
| 54 | `TaskAffinity` | handle zero for the calling task; otherwise a process handle with Wait to read, Write to set | mask must name an online processor; kernel tasks stay on the housekeeping processor; optional 8-byte `CopyOut` | the processors a task may run on; a scheduling constraint, not authority | no |
| 55 | `IRQTake` | IRQ handle, Wait, the routed network line or line 11 | none beyond the handle | clears one pending flag | no; nothing pending → `NotFound` |
| 56 | `RandomRead` | — | length 1..64, checked in `Dispatch`; `CopyOut` of generator output, after which the kernel clears its copy | at most 64 bytes from a per-CPU buffer; stirs the pool with the cycle counter | no; no generator output → `Unsupported` |

| 71 | `IRQPoll` | IRQ handle, Wait, line 11, checked by the same validator as `IRQWait` | ignores unused arguments | atomically consumes one pending flag; registers no waiter | no; nothing pending → `NotFound` |

Operations 23 and 24 are diagnostic and measurement aids (ADR 0052, 0054).
In a normal build operation 23 returns `Unsupported` and all trace recording
compiles away.

## Checklist notes

### Why in the kernel

- Handles, channels, streams, waits, and process lifetime are kernel objects;
  their integrity is the authority model.
- Port I/O, DMA memory, and IRQ routing need ring 0. The network line's
  handler records an event and parses nothing; Ethernet, ARP, IPv4, ICMP,
  UDP and DHCP run in `netd` (ADR 0191, ADR 0195), and DNS runs in
  `resolved` (ADR 0194). The kernel knows nothing of UDP ports or endpoints.
- `RandomRead` gives native processes the generator that already serves
  `AT_RANDOM` and `getrandom` (ADR 0185). `netd` and `resolved` seed their
  own generators from it for ephemeral UDP ports, transaction IDs and
  RFC 5227 delays (ADR 0192, ADR 0194, ADR 0195); a userspace source would have nothing
  unpredictable to start from.
- `ConsoleRead`, `ConsoleWrite`, and `ConsoleInterrupt` stay in the kernel
  because COM1 is also the early console, the panic path and the log's warning
  echo. The terminal service holds the only read right and relays everything the
  shell and commands print (ADR 0082, 0089); console write remains with it and
  with the system services that start before it.
- `DebugWrite`, the console write authorized by the caller's identity, is
  retired; PID 1 prints through `ConsoleWrite` with its console capability
  (ADR 0094).
- `ImageMap` maps the firmware-reserved boot archive, which only the kernel can
  name; serving it through VFS keeps the archive parser in userspace
  (ADR 0070).
- `ExecutableCreate` and `ExecutableWrite` build the sealed header object.
  `ExecutableSource` binds it to a stable VFS open object and consumes the
  private endpoint. The kernel requests bytes by offset and never resolves a
  path; VFS refuses mutation until the source closes (ADR 0121).
- `LogWrite`, `LogRead`, and `LogBounds` serve the kernel log ring, which must
  work before any service exists (ADR 0059, 0073).
- `SystemInfo`, `CPUInfo`, `CPUEntry`, `CPUCacheInfo`, `ClockRead`, and `ClockReadNanos` expose kernel counters or normalized hardware facts and
  have no userspace source.
- Process launch is not a native call. It is requested over the lifecycle
  channel that only PID 1 holds and runs in kernel housekeeping. The request
  names an extent of the system image and a handle; the kernel requires Execute
  on the image object, checks the extent against the image length without
  overflow (`loader.ImageExtent`), and validates the bytes as a native image.
  A request may instead name a sealed executable source with Execute, whole:
  offset zero and the source length. It resolves no path (ADR 0093, ADR 0121).

### Untrusted data and validation

All handle IDs, addresses, lengths, descriptors, timeouts, event masks, port
specifications, and IPC wire bytes are untrusted. Fixed-size arguments are
checked in `kcall.Dispatch` before the handler runs. Handlers look up handles
before touching user memory, so a wrong type or missing right fails without
side effects.

### Integer overflow

- User ranges: `vm.ValidateUserRange` and the copy helpers reject
  `address + length` wraparound and anything outside
  `[0x1000, 0x00007ffffffff000)`.
- Deadlines: `now + timeout < now` fails (`ResourceExhausted` for the
  one-object waits, `InvalidArgument` for `WaitAny`).
- Ports: `offset + width` is computed in 32 bits against a 16-bit BAR size.
- Wire sizes: the kernel copies at most 320 bytes, so derived offsets stay
  inside static buffers.

### Unbounded waiting

Every wait except `IRQWait` has a deadline or is explicitly infinite
(`WaitAny` with `^0`). Peer closure and process exit wake waiters with a
status, and task exit cancels its `WaitAny` registration (ADR 0050, 0051). Each
wait registers, re-checks readiness, and only then blocks, so a wake between
check and block is not lost. `IRQWait` has no deadline; the only holder is the
virtio-blk driver, which re-checks the used ring after every wake (ADR 0035).
A `WaitAny` on the network line registers, then unmasks the route, then
re-checks the pending flag, so an interrupt that arrives at any point either
completes the registration or is seen by the re-check (ADR 0190).

### Capabilities

See [capabilities.md](capabilities.md). Thirteen calls authorize by caller
identity or need nothing: `Exit`, `ClockRead`, `StartupRead`,
`SystemInfo`, `ChannelCreate`, `StreamCreate`, `ClockReadNanos`,
`HandleInfo` (own table only), `ProcessInfo`, `CPUInfo`, `CPUEntry`,
`CPUCacheInfo`, and `RandomRead`.

### Stream reference counts (ADR 0089)

- *Why in the kernel*: the count describes handle tables, which only the
  kernel sees; a userspace count could be forged or forgotten by a process that
  exits.
- *Untrusted inputs*: the handle a process closes, the handles it attaches to a
  message (including the same handle twice), and, from PID 1, launch grants.
  A handle is resolved in the caller's own table with type and rights before
  its count is touched, so a forged or stale ID changes nothing.
- *Overflow*: counts are 16-bit; no end can have more handles than the
  32 × 128 slots that exist, which a test holds below the bound, and every path
  that adds a reference checks the headroom it needs before installing anything.
  A drop with nothing counted is refused rather than wrapping.
- *Waits*: none added. Writers are woken only when a whole 256-byte write fits,
  so a woken writer is not refused again.
- *Resources*: two 16-bit counters per stream; the registry is 256 streams, each a page taken when it is created.

### Network parsers outside the kernel

The kernel parses no packet. These surfaces sit in userspace services that
hold no device authority, listed here because they take input from the link
or from other processes:

- `netd` parses Ethernet, ARP, IPv4, ICMP, UDP and DHCP, including the DNS
  servers of DHCP option 6, of which it keeps the first four that are not
  zero, loopback, broadcast or multicast. It drops any frame from the link
  with a loopback source or destination and runs RFC 5227 conflict detection
  on ARP (ADR 0191, ADR 0192, ADR 0193).
- UDP from the link reaches an endpoint only after `netd` checks the UDP
  length against the IPv4 payload and verifies a nonzero checksum; a bad
  one is dropped and counted before any other use. Only datagrams to the
  interface's own address reach endpoints, and a connected endpoint drops
  any source but its peer. Received payloads share 32 buffers of 4096
  bytes, at most 16 per session, so a flood toward one session's ports
  leaves the other half to the rest (ADR 0195).
- `GUDP1` carries programs' UDP requests to `netd` over sessions PID 1 opens
  for commands whose index entry asks for `GrantUDP`, and for `resolved`.
  The decoder, fuzzed, refuses an unknown opcode or version, a wrong size,
  nonzero reserved bytes, a field its opcode does not use, a slot outside
  the pool and any attached handle. Each session has a TX pool of four
  4096-byte slots that only the program writes and an RX pool of eight that
  only `netd` writes; `netd` checks the exact rights through `HandleInfo`
  and closes a session that sends a malformed request or hands back a slot
  it does not hold. An endpoint answers only the session that opened it,
  and a stale endpoint ID matches nothing. A session holds at most eight of
  the 32 endpoints (ADR 0195).
- `resolved` parses DNS replies. Before any of a reply is used it checks QR,
  opcode, transaction ID and server, exactly one question equal to the one
  asked, counts and lengths against the message, compression pointers that
  point backwards with at most 64 hops, labels of 63 bytes and names of 255.
  A reply that fails is dropped whole, and a truncated reply is not used.
  The parser in `userspace/dns` has fixed tables and fuzz tests (ADR 0194).
- `GRESOLV1` carries commands' requests to `resolved`: an opcode and a name
  of at most 253 bytes. Sessions are opened by PID 1 for commands whose index
  entry asks for `GrantResolve` or `GrantResolveAdmin`, and the resolver
  holds at most 16 lookups at once.
- `GHST1`, the hosts protocol, runs on a configuration session and is checked
  against the session's `HostsRead` and `HostsWrite` rights, not its scope.
  The configuration service parses `/state/hosts` at restore, skipping and
  counting malformed and duplicate lines, and keeps at most 64 records
  (ADR 0193).
- `GTCP1` exposes TCP only to sessions PID 1 creates for `GrantTCP`. `netd`
  checks endpoint ownership, TX and RX slot transitions, request sizes and
  correlations. Remote TCP input is checksum- and bounds-checked before TCB
  lookup. Endpoint, backlog, stream and reorder storage are fixed, and stale
  endpoint IDs carry the wrong generation (ADR 0196).

- Native HTTPS adds TLS records, handshake messages and X.509 DER parsing to
  the `fetch` process, not to the kernel, driver, `netd` or `resolved`. The pinned
  Go software implementations run over the command's private GTCP1 stream.
  Explicit roots come through its VFS grant from signed state or the read-only
  bootstrap system namespace.
  The root loader rejects malformed or empty data and bounds the bundle to
  512 KiB, 256 roots and 32 KiB DER per root; it never loads host OS roots or
  unsigned trust data from mutable application/user paths.
- TLS peer certificate lists stop at eight entries before allocating their
  slices, each DER is at most 32 KiB, and candidate chain depth stops at eight.
  The cache adapter rejects more than 64 DNS SAN entries after bounded DER
  parsing. The standard verifier allows at most 100 signature
  attempts. TLS 1.3 still permits a 256 KiB certificate-message buffer before
  these checks. Authentication uses kernel entropy, explicit REALTIME and a
  whole-handshake monotonic deadline. Missing platform inputs and clock rollback
  fail closed. See [ADR 0198](../adr/0198-native-tls-and-system-trust.md) and
  [native evidence](../development/crypto-feasibility.md). The native HTTPS
  gate passed 69 assertions, including certificate, CertificateVerify, Finished
  and record-MAC rejection. Certificate and handshake failures reached no HTTP
  request. Production-root loading allocated 1360864 bytes; the arena high-water
  mark was 1363216 bytes. The complete release-check matrix passed.

### Resource limits

Global fixed arrays bound the system; launch profiles bound each process's
share of them (ADR 0072, ADR 0122). A `tiny` command receives limits of two
channels, eight streams, one task and 2048 pages. A `normal` service receives
4, 16, 4 and 8192; `large` receives 8, 32, 8 and 16384. PID 1 chooses among
those names, while the kernel owns the numeric limits. PID 1 keeps a separate
bounded system quota of 256 channels, 64 streams, eight tasks and 16384 pages.

The shared image cache (ADR 0081) is not charged to any process. It grows only
when PID 1 launches an image from the immutable system image, it is keyed by
the launch extent in that archive, and it is bounded at 128 segments and 1024
frames, past which launches load private copies. PID 1 chooses the extent, but
an extent always names the same immutable bytes, which must validate as a native
image before anything is cached, and no other process can reach the launch
path (ADR 0093).

`StreamCreate` still needs no capability, because a stream is a primitive any
process may need, but a process calling it in a loop is refused at its share and
leaves the rest of the registry for other processes. A launch whose address space
would exceed the page quota fails rather than running over. A process can still
fill its own 256-entry handle table, which costs only itself.

A component holding the console capability can fill the 64-record log ring and,
with warning-level records, the console; logging is bounded by record size, not
rate. Process and task slots are consumed only through PID 1 launches.

CPU is accounted, not limited: the timer charges a tick to the task it
interrupted, and `ps` reports the total per process. Nothing refuses a process
that never yields. Under round-robin on one CPU that costs throughput, not
progress, on each processor alike.

### Work with interrupts disabled

Each call runs to completion with interrupts off on its processor, holding
the locks of the state it touches, so a long call delays the calls on other
processors that need the same locks. The longest native calls are
`ConsoleWrite` (up to 4096 bytes to the UART) and, in diagnostic
builds, `Diagnostics`. Process launch runs in housekeeping, also with
interrupts off, and takes about 2 ms (ADR 0057).

## Information exposure

| Operation | Exposes |
| --- | --- |
| `SystemInfo` | caller PID, process count, managed and free pages, tick count |
| `ClockReadNanos` | nanosecond TSC-derived time |
| `ProcessInfo` | PID, scheduling state, task count, executable name, CPU ticks, and resource use and limits of every process |
| `CPUInfo`, `CPUEntry`, `CPUCacheInfo` | CPUID identity and usable features, firmware-visible enabled processors, present/online state, normalized topology, and deterministic-cache geometry; no MSRs, kernel pointers, or CPU control authority |
| `HandleInfo` | ID, type, and rights of the caller's own handles; never the kernel object |
| `LogRead`, `LogBounds` | every retained log record, including other components' events, and the ring's sequence window |
| `DMAAllocate` | physical address of the caller's DMA window |
| `RandomRead` | output of the generator that also serves `AT_RANDOM` and `getrandom`; never its state |
| `Diagnostics` (`goos_diag`) | every process's handle table, channel state, physical addresses |

## Adding an operation

A new operation needs: a number appended to `kcall.Operation`, size checks in
`kcall.Dispatch` or a dedicated dispatcher, a handler that looks up handles
before touching user memory, bounded buffers, a deadline if it blocks, a row
in this table, host tests, and an ADR.

## Trust update input

The native client treats writable trust state as untrusted. It accepts raw
32-byte Ed25519 public keys only from the immutable system, raw 64-byte signatures,
canonical JSON up to 16 KiB and PEM up to 512 KiB. Metadata binds sequence,
provenance, digest, byte count and root count. Certificate limits remain 256 roots
and 32 KiB DER per certificate. No signature or digest failure reaches CA parsing.

The update downloader requires HTTPS and HTTP 200, follows no redirects, and
bounds all three bodies. The endpoint comes from immutable build configuration;
metadata `source_url` cannot redirect it. State enumeration stops at 256 directory
entries. Serial installations refuse 64 existing published generations or a full
enumeration before staging; incomplete staging stays outside the generation list.
The package verifier, installer and fallback remain userspace code.
