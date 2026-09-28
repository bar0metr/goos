# Debugging

Start with the serial log, move to a diagnostic build when the log does not
explain the failure, and attach a debugger only when you need instruction-level
state. Build flags are described in [building.md](building.md); test harnesses
in [testing.md](testing.md).

## Console output

COM1 remains the authoritative diagnostic output. After the permanent GOP
mapping is installed, the kernel mirrors the logical console byte stream to
the framebuffer terminal. `make run` stays headless; `make run-gui` opens the
graphical window and still attaches COM1 to the terminal.
The display backend is configurable, for example `make run-gui
QEMU_DISPLAY=gtk`; changing it does not alter the emulated standard VGA device
or the GOP mode exposed by OVMF.

| Source | Log |
| --- | --- |
| `make run` | terminal only; redirect if needed: `make run 2>&1 \| tee build/logs/run.log` |
| `make run-gui` | graphical window plus COM1 in the terminal |
| `make test-boot` | `build/logs/boot.log`, `build/logs/reboot.log` |
| `make test-stress` | one log per guest under `build/stress/hz<rate>/` |
| `make test-latency` | `build/logs/latency-hz<rate>.log` |

Kernel panics print a `PANIC:` line and halt. Userspace services print a fatal
marker and, in a diagnostic build, request a kernel dump before stopping.
Boot-log markers are changing as validation output moves to a separate build
(roadmap Priority 4), so search for `PANIC`, `FAILED`, and `DIAG` rather than
relying on specific success markers.

## Diagnostic build

```sh
make DIAG=1 run
make HZ=1000 DIAG=1 test-stress STRESS_RUNS=8
```

`DIAG=1` adds the `goos_diag` tag (ADR 0052). `diag.Enabled` is a constant, so
normal builds contain none of this code. A diagnostic build:

- records the last 128 kernel events in a ring: wait-any begin, ready,
  re-check, completion, timeout, take, cancel on exit, and denial with reason;
  channel create and close; handle transfer; process spawn and exit;
- times the five `spawnExecutable` stages (image authority and extent check, address space, image
  load, process setup, task install);
- answers the `Diagnostics` native operation with a full dump.

The dump is requested by:

- the shell builtin `diag`;
- the shell after an exec request gets no reply;
- PID 1, VFS, and the block service before they stop on a fatal error;
- the stress harness after a failed round.

In a normal build `diag` reports that diagnostics are unavailable.

## Reading a dump

A dump is a block of lines starting with `DIAG`, from `DIAG begin` to
`DIAG end`. Field names are `name=value`; hex values are addresses, masks, or
IDs. The exact field set may change with the code in `kernel/diagnostics.go`.

| Line | Content | Look for |
| --- | --- | --- |
| `DIAG begin` | tick, `hz`, requesting PID | which process asked, and when |
| `DIAG sched` | runnable and dead task masks by slot, slot count, current slot | a service task neither runnable nor dead is blocked |
| `DIAG process` | PID, CR3 root, each task and its state, free handle slots | a blocked service; exhausted handle tables |
| `DIAG   handle` | handle ID, type, rights, channel/stream ID and side, peer liveness, queue depth | `peer-alive=0` on a channel a service still waits on |
| `DIAG channel` | channel ID, liveness and queue depth per side, holder PIDs | messages queued with no reader; a side with no holder |
| `DIAG waitany` / `interest` | task, state, deadline, each interest's object, events, readiness | registrations that never complete |
| `DIAG waits` | counts per wait set, retained lifecycle messages, overflows | non-zero overflows |
| `DIAG clock`, `DIAG launch` | TSC rate, uptime, per-stage launch counts and times | slow launch stages |
| `DIAG event` | time (µs), kind, PID, task, object, value or denial reason | the last events before the stall |

Decoding values:

- **Handle ID**: `generation << 32 | (slot + 1)`. `0x100000003` is slot 2,
  generation 1.
- **Rights**: Read `0x1`, Write `0x2`, Execute `0x4`, Transfer `0x8`,
  Wait `0x10`.
- **Channel object**: channel IDs carry a namespace bit; bit 63 marks side 1.
  A value like `0x4000000000000005` where a handle was expected is a kernel
  object ID leaking into user data, which is how ADR 0053 and ADR 0056 were
  diagnosed.
- **Task ID**: a generation-safe slot identity; compare it with the slot
  masks in `DIAG sched`.

When `Diagnostics` is given an address it also prints the physical page
behind that address, whether the allocator owns it, other processes or kernel
stacks mapping the same page, an audit of the caller's user pages, and 64
bytes at the address. Use it to tell memory aliasing from a stale value.

## Debugger

```sh
make run-debug
```

QEMU starts halted with a GDB server on TCP port 1234 (`-s -S`).

GDB:

```text
gdb
(gdb) target remote :1234
(gdb) continue
```

LLDB:

```text
lldb
(lldb) gdb-remote 1234
```

Symbols:

- The kernel is built with `-no-debug`, so it has no DWARF. `make kernel`
  writes `build/kernel/goos.sym` with `nm -n`; use it to find function
  addresses and set breakpoints with `break *0x...`. The file may be empty if
  the host `nm` cannot read the PE image. The kernel is linked at
  `0x400000` and runs identity-mapped, so addresses need no relocation.
- Userspace ELFs are kept as `build/userspace/<name>.elf`, linked at
  `0x40002000`. Every process uses the same user layout, so a breakpoint at a
  user address fires in whichever process runs code there. Check CR3 to tell
  processes apart; `DIAG process` lists each root.

Useful stops: the native-call gate (look up `goos_dispatch_kernel_call` and the
other `goos_dispatch_*` symbols), `serviceKernel`, and the exception handlers
in `kernel/arch/amd64/io.S`.

A debugger stops the virtual CPU, so tick-based deadlines expire while you
inspect state. Expect timeouts after a long stop.

## Timing problems

- Reproduce at every rate: `make HZ=100|250|1000 test-stress`.
- Scheduling and preemption defects appear more often at 1000 Hz, where
  preemption is ten times as frequent (ADR 0053, 0056).
- `make HZ=<rate> DIAG=1 test-latency` adds launch-stage timings to the
  latency report.
- Tick-based deadlines stretch slightly while housekeeping holds interrupts
  off during a launch (about 2 ms per launch, ADR 0057). `ClockReadNanos` is
  not affected.
