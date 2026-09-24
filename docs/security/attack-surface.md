# Native kernel attack surface

The only intentional ring 3 → ring 0 entry points are `int 0x80` native calls,
CPU exceptions, and the timer, virtio (GSI 11), and console UART (GSI 4,
ADR 0075) interrupts. This document covers the native calls defined by
`kcall.Operation` in `kernel/kcall/kcall.go`.

## Entry path

The operation number is in `EAX`; up to three arguments follow in
general-purpose registers (stubs in `userspace/runtime/amd64.S`). The assembly
bridge in `kernel/arch/amd64/io.S` routes the five blocking operations (5, 12,
17, 21, 22) to dedicated dispatchers in `kernel/main.go`. Everything else goes through
`kcall.Dispatch`, which checks fixed sizes before calling the handler in
`kernel/main.go` or one of its sibling files (`diagnostics.go`, `timing.go`,
`logging.go`, `sysinfo.go`, `memory.go`, `processkill.go`, `imagemap.go`).
Unknown numbers return `Unsupported`.

Every call runs with interrupts disabled, inside the kernel exclusion domain
(ADR 0053). The caller is identified by its CR3 root, and blocking calls also
receive the task slot. Results are a `kcall.Status` in `RAX`. Blocking calls
park the task and re-execute the call on wake (ADR 0015).

## Operations

Authority column: `—` means none; "registered" means the caller must be a
process in the registry. Handles are always looked up in the caller's own
table with type, generation, and rights checks.

| # | Operation | Authority | Untrusted inputs and validation | Bounds | Blocks |
| ---: | --- | --- | --- | --- | --- |
| 1 | `Exit` | — | exit code, not interpreted | none | ends task |
| 2 | `DebugWrite` | caller is PID 1 or bootstrap process A (root compare) | length ≤ 4096; `CopyIn` | static 4 KiB buffer | no |
| 3 | `IPCSend` | channel handle, Write | wire length 32..320; `CopyIn`; `ipc.Decode` checks header and sizes; each attached handle (≤ 4) needs Transfer; an attached stream end must have room for four more references, checked before anything moves | 16-message queue; full → `ResourceExhausted` | no |
| 4 | `IPCReceive` | channel handle, Read | capacity ≥ 320; `CopyOut` | static wire buffer | no; empty → `NotFound` |
| 5 | `IPCWaitReceive` | channel handle, Read + Wait | 24-byte descriptor `{buffer, capacity, timeout}`; `now + timeout` wrap check | 64 waiters; per-task status slot | yes, tick deadline; 0 → `Timeout` |
| 6 | `ConsoleWrite` | device handle, Write, console object | length 1..4096; `CopyIn` | static 4 KiB buffer | no |
| 7 | `ConsoleRead` | device handle, Read, console object | length exactly 1; `CopyOut` | one byte from the 256-byte ring the UART interrupt fills | no; no input → `NotFound`; wait with `WaitAny` |
| 8 | `ClockRead` | — | length exactly 8; `CopyOut` | none | no |
| 9 | `StartupRead` | registered | length exactly 1024; `CopyIn` of own startup page, `CopyOut` | none | no |
| 10 | `ChannelCreate` | caller is PID 1 | length exactly 16; two free handle slots; `CopyOut` with rollback | 24 channels, global; charged to PID 1's channel quota | no |
| 11 | `HandleClose` | own handle | generation and slot; a stream handle drops one reference, refused below zero | none | no; wakes peers when a stream end's last handle closes |
| 12 | `ProcessWait` | process handle, Wait | result address range (8 B); `now + timeout` wrap check | 64 waiters | yes, tick deadline; 0 → `Timeout` |
| 13 | `SystemInfo` | registered | length exactly 40; `CopyOut` | none | no |
| 14 | `PortRead` | device handle, Read, port object | spec: width 1/2/4, bits 24+ zero, `offset + width ≤ BAR size` (32-bit sum); `CopyOut` | one port access | no |
| 15 | `PortWrite` | device handle, Write, port object | as `PortRead`; value must fit the width | one port access | no |
| 16 | `DMAAllocate` | device handle, Write, DMA object | virtual address must equal the fixed DMA address; four target pages must be unmapped; output buffer probed before allocation | 4 contiguous pages, charged to the page quota; at most once per address space | no |
| 17 | `IRQWait` | IRQ handle, Wait, IRQ 11 | none beyond the handle | one waiter slot | yes, **no deadline** |
| 18 | `StreamCreate` | registered | length exactly 16; two free handle slots; `CopyOut` with rollback | 64 streams × 4 KiB, global; four per ordinary process, 24 for PID 1 (quota) | no |
| 19 | `StreamRead` | reader handle, Read | 24-byte descriptor; size 1..256; result field must be 0; `CanCopyOut` before consuming | static 256 B buffer | no; empty → `ResourceExhausted` |
| 20 | `StreamWrite` | writer handle, Write | as `StreamRead`; `CopyIn` of payload; a write that does not fit whole is refused, never split | static 256 B buffer | no; full → `ResourceExhausted` |
| 21 | `StreamWait` | stream handle, Wait | timeout must be non-zero; wrap check | 64 waiters | yes, tick deadline; a writer wakes when 256 bytes are free |
| 22 | `WaitAny` | per interest: type-specific rights + Wait; the console device needs Read only | count 0..16, where 0 is a timed sleep and needs a finite deadline; interest array and 16-byte result validated; per-interest handle, type, event mask, rights, and live object; wrap check | fixed table, one operation per task slot | yes; deadline or infinite (`^0`) |
| 23 | `Diagnostics` | device handle, Write, console object; `goos_diag` build only | optional address, only for the caller's own address space | output proportional to kernel state | no |
| 24 | `ClockReadNanos` | — | length exactly 8; `CopyOut` | none | no |
| 25 | `LogWrite` | device handle, Write, console object | length 1..181; `CopyIn`; `logfmt.Decode` checks version, level, printable ASCII, component and field syntax (failure → `CorruptData`); time and PID are set by the kernel | 64-record ring, oldest overwritten | no |
| 26 | `LogRead` | device handle, Read, log object (ADR 0082) | index < 64; 240-byte `CopyOut` | none | no |
| 27 | `HandleInfo` | registered; own table only | slot < 128; 16-byte `CopyOut` of ID, type, rights | none | no |
| 28 | `ProcessInfo` | registered | index < 32; 80-byte `CopyOut` of PID, state, task count, name, CPU ticks, and the held and permitted resource counts | none | no |
| 29 | `SystemReset` | device handle, Write, system-control object (PID 1 only) | handle lookup | none | resets the machine |
| 30 | `MemoryMap` | memory handle, Read (+ Write when writable), kernel-registered shared object | flags: only bit 0 (writable); address page-aligned and whole object inside `[0x40180000, 0x401c0000)`; every leaf must be free before any is installed | whole object, NX, non-owned; at most 4 registered objects | no |
| 31 | `ProcessKill` | process handle, Write | handle lookup; refuses PID 1 and the caller's own process | none | retires the target's tasks |
| 32 | `ImageMap` | device handle, Read, system-image object (held only by VFS) | virtual address must equal `0x40200000`; archive must be page-aligned and fit `[0x40200000, 0x40600000)`; 16-byte descriptor probed before mapping; every leaf must be free | one leaf table per 2 MiB of archive (at most two), each charged to the page quota; archive mapped read-only, NX, non-owned | no |
| 33 | `LogBounds` | device handle, Read, log object (ADR 0082) | 16-byte `CopyOut` of oldest and next sequence numbers | none | no |
| 34 | `ConsoleInterrupt` | device handle, Read, console object | handle lookup only | consumes one pending Ctrl-C flag | no; none pending → `NotFound` |
| 35 | `ValidationFault` | validation build only (`Unsupported` otherwise); device handle, Write, system-control object (PID 1 only) | handle lookup only | none: overflows the caller's kernel stack on purpose and ends the machine with a double-fault report (ADR 0091) | no |
| 36 | `SystemPowerOff` | device handle, Write, system-control object (PID 1 only) | handle lookup only; the port and value come from ACPI tables checked at boot (`kernel/acpi`, ADR 0092) | none: one port write that switches the machine off; `Unsupported` without usable tables | no |

Operations 23 and 24 are diagnostic and measurement aids (ADR 0052, 0054).
In a normal build operation 23 returns `Unsupported` and all trace recording
compiles away.

## Checklist notes

### Why in the kernel

- Handles, channels, streams, waits, and process lifetime are kernel objects;
  their integrity is the authority model.
- Port I/O, DMA memory, and IRQ routing need ring 0.
- `ConsoleRead`, `ConsoleWrite`, and `ConsoleInterrupt` stay in the kernel
  because COM1 is also the early console, the panic path and the log's warning
  echo. The terminal service holds the only read right and relays everything the
  shell and commands print (ADR 0082, 0089); console write remains with it and
  with the system services that start before it.
- `DebugWrite` is a bootstrap path. Boot validation now has its own build
  (ADR 0058); in a normal build only PID 1 passes its root comparison, and the
  call remains a candidate for removal.
- `ImageMap` maps the firmware-reserved boot archive, which only the kernel can
  name; serving it through VFS keeps the archive parser in userspace
  (ADR 0070).
- `LogWrite`, `LogRead`, and `LogBounds` serve the kernel log ring, which must
  work before any service exists (ADR 0059, 0073).
- `SystemInfo`, `ClockRead`, and `ClockReadNanos` expose kernel counters and
  have no userspace source.
- Process launch is not a native call. It is requested over the lifecycle
  channel that only PID 1 holds and runs in kernel housekeeping. The request
  names an extent of the system image and a handle; the kernel requires Execute
  on the image object, checks the extent against the image length without
  overflow (`loader.ImageExtent`), and validates the bytes as a native image.
  It resolves no path (ADR 0093).

### Untrusted data and validation

All handle IDs, addresses, lengths, descriptors, timeouts, event masks, port
specifications, and IPC wire bytes are untrusted. Fixed-size arguments are
checked in `kcall.Dispatch` before the handler runs. Handlers look up handles
before touching user memory, so a wrong type or missing right fails without
side effects.

### Integer overflow

- User ranges: `vm.ValidateUserRange` and the copy helpers reject
  `address + length` wraparound and anything outside `[1 GiB, 2 GiB)`.
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

### Capabilities

See [capabilities.md](capabilities.md). Ten calls authorize by caller
identity or need nothing: `Exit`, `DebugWrite`, `ClockRead`, `StartupRead`,
`SystemInfo`, `ChannelCreate`, `StreamCreate`, `ClockReadNanos`,
`HandleInfo` (own table only), and `ProcessInfo`.

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
- *Resources*: two 16-bit counters per stream; the registry is 64 streams.

### Resource limits

Global fixed arrays bound the system; per-process quotas bound each process's
share of them (ADR 0072). Channels, streams, tasks and pages are charged to the
process that causes them to exist and released when it does. An ordinary process
holds at most four channels, four streams, four tasks and 192 pages; PID 1, which
creates the endpoints every other process communicates through, holds at most
twenty-four, twenty-four, eight and 512, and is refused past that like anything
else (ADR 0089).

The shared image cache (ADR 0081) is not charged to any process. It grows only
when PID 1 launches an image from the immutable system image, it is keyed by
the launch extent in that archive, and it is bounded at 128 segments and 1024
frames, past which launches load private copies. PID 1 chooses the extent, but
an extent always names the same immutable bytes, which must validate as a native
image before anything is cached, and no other process can reach the launch
path (ADR 0093).

`StreamCreate` still needs no capability, because a stream is a primitive any
process may need, but a process calling it in a loop is refused at its share and
leaves the rest of the registry for everyone else. A launch whose address space
would exceed the page quota fails rather than running over. A process can still
fill its own 128-entry handle table, which costs only itself.

A component holding the console capability can fill the 64-record log ring and,
with warning-level records, the console; logging is bounded by record size, not
rate. Process and task slots are consumed only through PID 1 launches.

CPU is accounted, not limited: the timer charges a tick to the task it
interrupted, and `ps` reports the total per process. Nothing refuses a process
that never yields. Under round-robin on one CPU that costs throughput, not
progress; it would need a policy before SMP.

### Work with interrupts disabled

Each call runs to completion with interrupts off. The longest native calls are
`DebugWrite`/`ConsoleWrite` (up to 4096 bytes to the UART) and, in diagnostic
builds, `Diagnostics`. Process launch runs in housekeeping, also with
interrupts off, and takes about 2 ms (ADR 0057).

## Information exposure

| Operation | Exposes |
| --- | --- |
| `SystemInfo` | caller PID, process count, managed and free pages, tick count |
| `ClockReadNanos` | nanosecond TSC-derived time |
| `ProcessInfo` | PID, scheduling state, task count, executable name, CPU ticks, and resource use and limits of every process |
| `HandleInfo` | ID, type, and rights of the caller's own handles; never the kernel object |
| `LogRead`, `LogBounds` | every retained log record, including other components' events, and the ring's sequence window |
| `DMAAllocate` | physical address of the caller's DMA window |
| `Diagnostics` (`goos_diag`) | every process's handle table, channel state, physical addresses |

## Adding an operation

A new operation needs: a number appended to `kcall.Operation`, size checks in
`kcall.Dispatch` or a dedicated dispatcher, a handler that looks up handles
before touching user memory, bounded buffers, a deadline if it blocks, a row
in this table, host tests, and an ADR.
