# ADR 0068: Timed sleep without spinning

## Status

Accepted.

## Context

The `sleep` utility took timer ticks, not seconds, so `sleep 5` at 100 Hz
returned after 50 milliseconds. It then spun on `ClockRead` until the deadline,
burning a full time slice per scheduling round for the whole duration, and
every one of those calls entered the kernel.

The kernel had no operation for waiting on time alone. `WaitAny` waits for
readiness on one to sixteen objects, with a timeout, and rejected an empty
interest set.

## Decision

`WaitAny` with no interests is a timed sleep. It requires a deadline, since a
wait on nothing with an infinite timeout would never return, and it completes
with `Timeout` the way a wait on objects does. The wait table already keyed
expiry on the deadline rather than on the interests, so this is the removal of
a restriction rather than a new mechanism; it is `poll(NULL, 0, ms)`.

`userspace/wait` exposes it as `Sleep(ticks)`. The `sleep` utility takes
seconds, with up to three decimals, and converts them with the build's timer
rate: `sleep 5` sleeps five seconds at any supported rate, `sleep 0.25` sleeps a
quarter of a second, and a duration that would round down to no ticks at all
sleeps for one. It blocks in the kernel instead of spinning.

## Alternatives

A dedicated `Sleep` operation would be one more entry in the native ABI for
behaviour `WaitAny` already implements, and processes that want to wait for a
deadline *or* an event would still use `WaitAny`.

Keeping ticks in the utility and documenting the rate would have left every
command line dependent on how the kernel was built, which is exactly what the
timer-rate work of ADR 0049 set out to hide.

## Consequences

`sleep` means what it says, and a sleeping process no longer competes for the
CPU. The boot and latency scripts that used `sleep 1` as a short pause now wait
a full second there.

A process still cannot be woken early from a sleep except by being stopped
(ADR 0067); the wait ends on its deadline.
