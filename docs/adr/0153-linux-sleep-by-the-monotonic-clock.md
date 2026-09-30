# ADR 0153: A Linux sleep ends by the monotonic clock

## Status

Accepted. Amends the `nanosleep` decision of ADR 0145.

## Context

ADR 0145 turned a `nanosleep` into a deadline in timer ticks: the request
rounded up to whole ticks, plus one. The extra tick was meant to guarantee
that the sleep never ends early.

Ticks are interrupts, and an emulator that falls behind delivers the ones it
owes in a burst. Under host load three ticks arrived within a millisecond,
a 20 ms sleep returned at once, and the conformance probe, which reads the
monotonic clock around the sleep, reported it. The release gate failed in
one boot of seven, and three boot loops running side by side failed twice
in fewer than 40 boots. The monotonic clock reads the TSC and is not fooled
by the burst.

## Decision

`nanosleep` records the monotonic time at which the sleep ends, in the note
of its continuation. The tick deadline still does the waking. A wake that
comes before the recorded time starts another wait for the ticks that cover
what is left, as a new stage of the same call. The call returns when the
monotonic clock has passed the recorded time.

One call waits at most 200 times, which bounds the stages a continuation
counts. Before the clock is calibrated the tick deadline alone decides, as
before.

PID 1 also prints the probe's output when it differs from what it expects,
between `LINUX_PROBE_OUTPUT_BEGIN` and `LINUX_PROBE_OUTPUT_END`. The probe
names the group that failed, and the boot log used to say only that one had.

## Consequences

A Linux program's sleep lasts at least the time it asked for by the clock it
can read, on hardware and under a loaded emulator. The same three loops ran
120 boots with the change and none failed. A sleep can still run
long. Native waits keep their tick deadlines; a native program that needs
the same guarantee reads the clock itself.
