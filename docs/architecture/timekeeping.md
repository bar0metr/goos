# Timekeeping

GoOS has two independent native clocks (ADR 0113):

- `MONOTONIC` is the existing PIT-calibrated TSC timeline. Scheduler deadlines,
  waits, sleep, uptime, and performance measurements continue to use it. It is
  never derived from the calendar clock.
- `REALTIME` is UTC Unix time with signed seconds and nanoseconds. UEFI
  `GetTime` is read once before `ExitBootServices`; the kernel stores that
  calendar reading and its adjacent TSC reading. After the normal TSC
  calibration, realtime is the stored UTC base plus monotonic elapsed time.
  Firmware and RTC are not polled again.

Native operation 46, `ClockNow(clock, record, 16)`, returns signed seconds,
uint32 nanoseconds, and four reserved zero bytes. Reading either clock requires
no capability. The older tick and monotonic-nanosecond operations remain for
ABI compatibility and keep their existing semantics.

Native operation 47 reserves `ClockSet(authority, record, 16)`. It requires a
device capability carrying the distinct `CLOCK_SET` right and naming the clock
object. No such handle is issued and setting returns `unsupported`; this keeps
future manual setting, NTP, and RTC write-back behind explicit authority.

FAT32 receives `REALTIME` through the native API and performs all FAT date/time
conversion inside the FAT32 implementation. FAT stores local calendar fields
without timezone metadata. The GoOS system timezone is UTC until explicit
timezone configuration exists; there is no DST or timezone database.

Structured logs use `REALTIME` and render UTC calendar timestamps. Monotonic
time remains the source for ordering inside the kernel and for latency.
