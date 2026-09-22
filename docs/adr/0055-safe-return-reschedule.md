# ADR 0055: Safe-return rescheduling and sleeping kernel tasks

## Status

Accepted. Completes the scheduling half of ADR 0051.

## Context

ADR 0051 made a wake mark the waiter runnable, but a publisher that kept
running held the CPU until the next PIT tick, so request latency still scaled
with the tick length.

Four kernel tasks made this worse. The housekeeping loop (slot 0), both demo
tasks (slots 1 and 2), and the idle task (slot 3) were always runnable and each
ended its turn with `hlt`. Round-robin selection landed on them in turn, and each
one then idled until the next interrupt. A task woken just after the current
slot therefore waited up to four ticks behind halted kernel tasks. The kernel
loop itself ran only when its turn came up, so a launch request from PID 1 and
the exit of a child were serviced on a tick boundary. The block driver's IRQ
made its waiter runnable and then returned into whatever task it had
interrupted, often the idle task, which slept until the next tick.

## Decision

A wake marks the task runnable and sets `need_reschedule`. It never switches
from inside the code that published the event. The switch happens at the next
safe point:

- a native call whose work is complete returns with `need_reschedule` set. It
  is parked with its result in RAX and its instruction pointer after the
  `INT`, exactly as if the timer had preempted it on return;
- IRQ11 returns after waking the driver.

Blocking, exiting, and faulting native calls already switch.

Kernel tasks give up the CPU through vector 0x81 instead of halting. The vector
saves the same frame as IRQ0, does not advance time or acknowledge the PIC,
and has DPL 0. `KernelSleep` clears the caller's runnable bit and switches
through it. Because the housekeeping loop sleeps inside the exclusion domain
of ADR 0053, no wake can fall between its last check and the sleep. The
loop is woken by each tick for deadlines and retries, by a user task exit or
fault, and by a lifecycle request from PID 1. The demo tasks sleep for good
once the demonstration completes.

Selection skips the idle slot unless nothing else is runnable. Every switch
clears `need_reschedule`, and a switch to the current address space no longer
reloads CR3.

The boot IPC demonstration no longer depends on relative CPU speed: the sender
first waits out a ten-tick deadline, so the receiver's five-tick timeout is
always observed before the message.

## Alternatives

Switching directly from the waker's kernel code would need a second kernel
stack frame format and reentrancy analysis for every publisher. Waking the
woken task first (direct handoff) would favour request/reply pairs but starve
the rest of the round; round-robin from the current slot is kept until
priorities exist. Waking the kernel loop only when a deadline is due would save
wakeups at high rates but needs a deadline index; a tick-driven wake is kept
because the loop's work per tick is bounded and small.

## Consequences

Event delivery, exit delivery, launch requests, and block I/O completions no
longer wait for a tick. A publisher that wakes a peer is preempted when its
call returns, which adds a context switch to request/reply exchanges in
exchange for not holding the peer behind a full quantum. ADR 0057 records the
resulting latencies at each supported rate.

The timer still preempts every tick. A configurable quantum, priorities, and
per-CPU queues remain future work, and the `need_reschedule` flag becomes
per-CPU with SMP.
