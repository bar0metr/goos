# ADR 0049: Named timer rate and deadlines

## Status

Accepted. Refines the fixed rate chosen in ADR 0005 rather than replacing its
PIC/PIT bootstrap design.

## Context

Measured command latency on the persistent mount was dominated by waiting, not
by work. Services poll their endpoints with one-tick waits, and the kernel's
idle loop advances exit delivery and lifecycle work once per tick, so a request
crossing several services pays a tick at each hop. At 100 Hz a tick is 10 ms.

The legacy PIT divides a fixed 1.193182 MHz oscillator and is unrelated to CPU
frequency, so the rate is a free choice bounded by interrupt overhead rather
than by the hardware. Raising it is therefore a plausible lever, and an
experiment at 1 kHz measured the effect:

| command | 100 Hz | 1 kHz |
| --- | --- | --- |
| `cat` on the persistent mount | 1470 ms | 288 ms |
| `ls` on the persistent mount | 1120 ms | 482 ms |
| `echo x > file` | 2770 ms | 595 ms |
| `uname`, `echo` (no storage) | 530 ms | 587 ms |

Storage work is therefore tick-bound, and the roughly half second that a
single-process command costs is not: it is process launch, which the higher rate
left unchanged.

The same experiment surfaced two defects that the slower rate had been hiding.
Utility output was assembled from several console writes, so a line could
interleave with another process's output once preemption became frequent. More
seriously, when a pipeline stage fails to launch, a surviving stage is not
always woken by its peer's exit, and PID 1 then waits on a child that never
finishes.

## Decision

Keep the rate at 100 Hz for now, but stop encoding it implicitly. The new
`kernel/clock` package names the rate, the oscillator frequency, and the PIT
reload value; deadlines are expressed as multiples of `clock.TicksPerSecond`,
and a unit test keeps the reload value in step with the immediate programmed in
the assembly. Raising the rate is now a one-line edit whose effect on every
deadline is predictable.

Assemble each utility output line in one buffer and write it once, so that a
line reaches the console atomically with respect to other processes.

Adopting 1 kHz waits on the teardown wakeup defect, because a three to five
times faster system that can strand a shell is not an improvement.

## Alternatives

A much higher rate was rejected on its own terms: each tick costs an interrupt,
a context save, and a scheduler decision, so overhead grows linearly while the
benefit stops once tick latency is no longer the limiting factor. The
measurements above show non-storage commands are already past that point at
1 kHz. Deriving the tick from the CPU clock is not available from the PIT at
all; that belongs to the local APIC timer, which is future work.

## Consequences

Behaviour is unchanged today, and the timing constants are now readable and
consistent. Output lines no longer interleave.

The underlying weakness remains: services wait on their peers with
tick-granularity polls, so latency is a function of the tick rate. The
architectural fix is an event-driven wait across several endpoints, so a service
is woken by the message it waits for. That work, the teardown wakeup defect, and
the process-launch cost this experiment isolated are the next performance items,
and the local APIC timer belongs with them.
