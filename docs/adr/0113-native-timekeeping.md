# ADR 0113: Native monotonic and realtime clocks

## Status

Accepted. Amends ADR 0059 and ADR 0073 (log timestamps), and ADR 0112 (FAT32
now receives a wall clock).

## Context

GoOS already had a PIT-calibrated TSC clock for deadlines and elapsed time, but
no native calendar clock. FAT32 therefore preserved foreign timestamps but
recorded unknown time for new files, and structured logs printed time since
boot. Linux compatibility, filesystem metadata, and persistent logs require a
wall clock without making scheduler correctness depend on firmware or RTC.

## Decision

`MONOTONIC` remains the existing TSC timeline and keeps every current scheduler,
wait, sleep, uptime, and benchmark user unchanged. `REALTIME` is a separate UTC
Unix timestamp represented as signed seconds plus nanoseconds.

The boot path calls UEFI `GetTime` exactly once before `ExitBootServices`,
validates the Gregorian date, nanoseconds, timezone, and daylight bits, converts
the result to UTC Unix time, and records an adjacent TSC reading. After the
existing PIT calibration, realtime advances from that pair using the calibrated
TSC. The firmware and RTC are not polled after initialization.

Native `ClockNow` takes a typed clock identifier and returns a 16-byte record.
Reading needs no capability. Existing tick and monotonic-nanosecond calls stay
unchanged. `ClockSet` is reserved but unimplemented; it first requires a clock
device capability carrying the distinct `CLOCK_SET` right. No process receives
that authority.

FAT32 obtains realtime through `ClockNow(REALTIME)` and alone translates to and
from FAT fields. The system timezone is UTC until timezone configuration
exists. Structured logs store realtime and render UTC calendar timestamps.

## Consequences

- Scheduler and timeout behaviour has no new dependency on calendar time.
- Files created or changed through FAT32 receive UTC timestamps.
- Persistent logs carry calendar timestamps across reboots.
- Future Linux `CLOCK_MONOTONIC`, `CLOCK_REALTIME`, `gettimeofday`, clock
  setting, synchronization, NTP, and RTC write-back have a native boundary.
- NTP, timezone databases, DST policy, Linux time syscalls, and RTC write-back
  remain out of scope.
