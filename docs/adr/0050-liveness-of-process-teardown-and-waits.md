# ADR 0050: Liveness of process teardown, waits, and lifecycle delivery

## Status

Accepted.

## Context

Raising the timer rate for measurement (ADR 0049) turned rare, hard-to-reproduce
stalls into frequent ones and exposed several places where the system could stop
making progress. Each had the same shape: a step that another party was waiting
on was skipped, deferred, or dropped, and nothing reported it.

- `deliverProcessExits` performed the observable half of teardown, waking
  waiters and closing the handles that make peers observe EOF, only after
  bookkeeping that can fail. A failed kernel-stack reclaim therefore left an
  exited process's waiters blocked and its peers reading a stream that would
  never close.
- `ReportTaskExit` recorded a process completion only if a bounded lifecycle
  queue had room, so a burst of exits could lose the record that a waiter needs.
- The kernel's launch reply was sent once. When the lifecycle channel was
  momentarily full, typically because exit notifications were still queued, the
  reply was dropped and PID 1 waited for it until its deadline.
- Every wait checked its condition and then registered, so an event arriving in
  between woke nobody.
- Deadlines, the last line of defence behind a missed wakeup, were expired by a
  leftover demo task rather than by the kernel's own loop.

## Decision

Separate liveness from cleanup in process teardown. Once an exit is known, the
kernel records the completion, wakes waiters, and closes the process's handles
unconditionally; reclaiming the kernel stack and the address space is attempted
independently, and failures are counted and reported rather than aborting
delivery.

Make completion recording unconditional. `ReportTaskExit` now returns the exit
event together with an explicit report of what it did, and the bounded queue it
used to depend on is gone.

Retain lifecycle messages the kernel cannot deliver yet in a bounded queue,
retry them from the kernel loop before accepting new work, and report overflow.
Answer a lifecycle request that cannot be decoded instead of dropping it.

Register before re-checking in every wait. Each wait now adds itself to its wait
set, re-evaluates its condition, and cancels the registration if the condition
is already satisfied, which closes the window in which an event is delivered to
nobody. Wait sets gained `Cancel` for this, and any resume status left by a
waker that lost the race is discarded.

Drive deadline expiry from the kernel's own loop, so it cannot depend on any
particular task being scheduled.

Report a child that outlives several wait deadlines instead of stalling silently.

## Consequences

The boot gate passes repeatedly at 100 Hz, and the failure modes that remain are
reported rather than silent. Teardown does more work unconditionally, which is
the intended trade: correctness of observable state before reclamation of
resources.

At 1 kHz an intermittent stall remains in roughly half of stress runs: PID 1
stops servicing exec requests while the kernel keeps delivering exits, waking
waiters, and expiring deadlines, and the shell reports `exec: no reply` rather
than hanging. That is the remaining blocker for adopting the higher rate and is
recorded as the next investigation.
