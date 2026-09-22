# Native kernel attack surface

The only intentional ring 3 → ring 0 entry points are `int 0x80` native calls,
CPU exceptions, and the timer and virtio interrupts. This document covers the
native calls defined by `kcall.Operation` in `kernel/kcall/kcall.go`.

## Entry path

The operation number is in `EAX`; up to three arguments follow in
general-purpose registers (stubs in `userspace/runtime/amd64.S`). The assembly
bridge in `kernel/arch/amd64/io.S` routes the five blocking operations (5, 12,
17, 21, 22) to dedicated dispatchers in `kernel/main.go`. Everything else goes through
`kcall.Dispatch`, which checks fixed sizes before calling the handler in
`kernel/main.go`, `kernel/diagnostics.go`, or `kernel/timing.go`. Unknown
numbers return `Unsupported`.

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
| 3 | `IPCSend` | channel handle, Write | wire length 32..320; `CopyIn`; `ipc.Decode` checks header and sizes; each attached handle (≤ 4) needs Transfer | 16-message queue; full → `ResourceExhausted` | no |
| 4 | `IPCReceive` | channel handle, Read | capacity ≥ 320; `CopyOut` | static wire buffer | no; empty → `NotFound` |
| 5 | `IPCWaitReceive` | channel handle, Read + Wait | 24-byte descriptor `{buffer, capacity, timeout}`; `now + timeout` wrap check | 64 waiters; per-task status slot | yes, tick deadline; 0 → `Timeout` |
| 6 | `ConsoleWrite` | device handle, Write, console object | length 1..4096; `CopyIn` | static 4 KiB buffer | no |
| 7 | `ConsoleRead` | device handle, Read, console object | length exactly 1; `CopyOut` | one byte | no; no input → `NotFound` |
| 8 | `ClockRead` | — | length exactly 8; `CopyOut` | none | no |
| 9 | `StartupRead` | registered | length exactly 128; `CopyOut` | none | no |
| 10 | `ChannelCreate` | caller is PID 1 | length exactly 16; two free handle slots; `CopyOut` with rollback | 16 channels, global | no |
| 11 | `HandleClose` | own handle | generation and slot | none | no; wakes peers |
| 12 | `ProcessWait` | process handle, Wait | result address range (8 B); `now + timeout` wrap check | 64 waiters | yes, tick deadline; 0 → `Timeout` |
| 13 | `SystemInfo` | registered | length exactly 40; `CopyOut` | none | no |
| 14 | `PortRead` | device handle, Read, port object | spec: width 1/2/4, bits 24+ zero, `offset + width ≤ BAR size` (32-bit sum); `CopyOut` | one port access | no |
| 15 | `PortWrite` | device handle, Write, port object | as `PortRead`; value must fit the width | one port access | no |
| 16 | `DMAAllocate` | device handle, Write, DMA object | virtual address must equal the fixed DMA address; four target pages must be unmapped; output buffer probed before allocation | 4 contiguous pages; at most once per address space | no |
| 17 | `IRQWait` | IRQ handle, Wait, IRQ 11 | none beyond the handle | one waiter slot | yes, **no deadline** |
| 18 | `StreamCreate` | registered | length exactly 16; two free handle slots; `CopyOut` with rollback | 16 streams × 4 KiB, global | no |
| 19 | `StreamRead` | reader handle, Read | 24-byte descriptor; size 1..256; result field must be 0; `CanCopyOut` before consuming | static 256 B buffer | no; empty → `ResourceExhausted` |
| 20 | `StreamWrite` | writer handle, Write | as `StreamRead`; `CopyIn` of payload | static 256 B buffer | no; full → `ResourceExhausted` |
| 21 | `StreamWait` | stream handle, Wait | timeout must be non-zero; wrap check | 64 waiters | yes, tick deadline |
| 22 | `WaitAny` | per interest: type-specific rights + Wait | count 1..16; interest array and 16-byte result validated; per-interest handle, type, event mask, rights, and live object; wrap check | fixed table, one operation per task slot | yes; deadline or infinite (`^0`) |
| 23 | `Diagnostics` | device handle, Write, console object; `goos_diag` build only | optional address, only for the caller's own address space | output proportional to kernel state | no |
| 24 | `ClockReadNanos` | — | length exactly 8; `CopyOut` | none | no |
| 25 | `LogWrite` | device handle, Write, console object | length 1..181; `CopyIn`; `logfmt.Decode` checks version, level, printable ASCII, component and field syntax; time and PID are set by the kernel | 64-record ring, oldest overwritten | no |
| 26 | `LogRead` | device handle, Read, console object | index < 64; 240-byte `CopyOut` | none | no |
| 27 | `HandleInfo` | registered; own table only | slot < 128; 16-byte `CopyOut` of ID, type, rights | none | no |
| 28 | `ProcessInfo` | registered | index < 32; 32-byte `CopyOut` of PID, state, task count, name | none | no |
| 29 | `SystemReset` | device handle, Write, system-control object (PID 1 only) | handle lookup | none | resets the machine |

Operations 23 and 24 are diagnostic and measurement aids (ADR 0052, 0054).
In a normal build operation 23 returns `Unsupported` and all trace recording
compiles away.

## Checklist notes

### Why in the kernel

- Handles, channels, streams, waits, and process lifetime are kernel objects;
  their integrity is the authority model.
- Port I/O, DMA memory, and IRQ routing need ring 0.
- `ConsoleRead`/`ConsoleWrite` are in the kernel because no TTY service owns
  COM1 yet (docs/architecture/tty.md). They are candidates to move to
  userspace.
- `DebugWrite` is a bootstrap path and a candidate for removal once boot
  validation leaves the normal kernel (roadmap Priority 4).
- `SystemInfo`, `ClockRead`, and `ClockReadNanos` expose kernel counters and
  have no userspace source.
- Process launch is not a native call. It is requested over the lifecycle
  channel that only PID 1 holds and runs in kernel housekeeping.

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

See [capabilities.md](capabilities.md). Eight calls authorize by caller
identity or need nothing: `Exit`, `DebugWrite`, `ClockRead`, `StartupRead`,
`SystemInfo`, `ChannelCreate`, `StreamCreate`, and `ClockReadNanos`.

### Resource limits

Limits are global fixed arrays, not per-process quotas. A process with no
special authority can exhaust the 16-entry stream registry with
`StreamCreate`, and any process can fill its own 128-entry handle table.
`ChannelCreate` is limited to PID 1. A component holding the console
capability can fill the 64-record log ring and, with warning-level records,
the console; logging is bounded by record size, not rate. Process and task slots are consumed only
through PID 1 launches.

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
| `ProcessInfo` | PID, scheduling state, task count, and executable name of every process |
| `LogRead` | every retained log record, including other components' events |
| `DMAAllocate` | physical address of the caller's DMA window |
| `Diagnostics` (`goos_diag`) | every process's handle table, channel state, physical addresses |

## Adding an operation

A new operation needs: a number appended to `kcall.Operation`, size checks in
`kcall.Dispatch` or a dedicated dispatcher, a handler that looks up handles
before touching user memory, bounded buffers, a deadline if it blocks, a row
in this table, host tests, and an ADR.
