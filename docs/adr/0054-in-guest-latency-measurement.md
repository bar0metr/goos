# ADR 0054: In-guest latency measurement

## Status

Accepted.

## Context

Choosing a timer rate (ADR 0052) or judging a scheduler change needs latency
numbers for real commands and IPC round trips. The only clock userspace had
was the PIT tick count. At 100 Hz one tick is 10 ms, coarser than most of what
needs measuring, and a tick-based measurement changes resolution with the rate
it is meant to compare. Timing from the host through the serial console adds
QEMU's character device path and the harness's polling interval to every
sample.

## Decision

The kernel reads the TSC. At boot, after the PIT starts, it counts TSC cycles
across `TicksPerSecond/10` ticks (about 100 ms at every rate), taking both
readings just after a tick, and derives the counter rate from the fixed
1.193182 MHz PIT input and the programmed divisor. `clock.Counter` does the
conversion in host-tested Go; it cannot overflow for counters below 18 GHz.

Native operation 24, `ClockReadNanos(address, 8)`, copies out nanoseconds since
the timer started. Like `ClockRead` it needs no capability. It returns
`Unsupported` if calibration failed, and boot prints
`CLOCK_CALIBRATION_FAILED` in that case.

Three tools use the clock:

- the shell builtin `time <command line>` runs the rest of the line through
  the normal execution path and prints `time: <N> us`. It is a line prefix and,
  like the other builtins, cannot be a pipeline stage or be redirected;
- the coreutils `bench` utility times N VFS `Stat` requests (`bench vfs N`) or
  N open/read/close cycles of one file (`bench read PATH N`) inside a single
  process, which separates IPC and storage cost from launch cost;
- in `DIAG=1` builds `spawnExecutable` charges its five stages (image lookup,
  address space, image load, process and grant setup, task install) to a fixed
  table that `diag` prints as `DIAG launch` lines. Normal builds compile this
  out.

`make test-latency` boots one guest, runs a fixed set of commands under
`time`, and writes the median, p90, and minimum per case, plus the `bench`
results, to `build/reports/latency-hz<rate>.md`.

## Alternatives

PIT ticks alone cannot resolve anything shorter than a tick. The HPET or the
LAPIC timer would need ACPI table parsing and MMIO mappings the kernel does not
have yet; the TSC needs one instruction. Letting userspace execute `rdtsc`
directly would still need the kernel's calibration, and a native call costs
far less than the milliseconds being measured: the `help` case, which launches
nothing and makes two clock calls, takes about 0.2 ms under TCG.

## Consequences

The first reports show a VFS round trip costing about four ticks at both
100 Hz (40 ms) and 1000 Hz (4 ms), and command latency scaling with the tick
rather than with work done. The reports make that visible without source
edits; they do not change it.

The clock assumes one CPU and a constant-rate TSC. Under QEMU TCG the TSC
follows the host clock; on hardware without an invariant TSC the result drifts
with frequency scaling, and SMP will need per-CPU synchronization. Calibration
adds about 100 ms to boot.

Every process can now read time at nanosecond resolution, a finer timing
source than ticks. If that matters later, operation 24 can require a
capability without changing its encoding.
