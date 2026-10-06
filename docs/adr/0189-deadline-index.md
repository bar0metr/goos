# ADR 0189: A deadline index for housekeeping

## Status

Accepted. Changes the deadline expiry of ADR 0053 and ADR 0111.

## Context

The housekeeping task expired deadlines on every tick by scanning every
registration of every wait table: channel waits, stream waits, process
waits, multi-object waits, and the retries of executable page requests.
Each table holds one registration per task slot, so a tick looked at up to
320 entries, nearly always to find nothing due.

## Decision

`kernel/deadline.Index` is a fixed-capacity min-heap of deadlines. An entry
names its time, its source table, and its task. A task waits in each table
at most once, so the index holds at most one entry per source and task slot:
5 × 64 = 320 entries, in a static array, with each entry's heap position
kept per source and slot. Inserting, replacing, and cancelling cost
O(log N); the earliest deadline is an atomic word read without the lock;
taking the expired entries costs O(k log N) for k of them.

Each wait table keeps its deadlines in the index itself, under its own lock:
a registration arms its deadline, and a wake, a cancel, or a completion
removes it. A wait without a deadline is not indexed. The channel and
process wait sets now keep each registration at its task's slot, so a
cancel or an expiry finds it without a scan, as the multi-object table
already did. The index's lock ranks above the wait tables' (`deadline`
level).

On each tick the housekeeping task asks the index for the deadlines at or
before now; with none due that is one load. For each one it asks the source
table to expire that task's wait. The table decides under its own lock: if
the registration is still there, with the same task identity and a deadline
that has come, it times out; if a wake or a cancel ended it first, nothing
happens. Readiness, timeout, signal, peer death, and cancellation therefore
still have one winner, decided where they were decided before. An entry
carries the task's generation, so an entry a task left behind can neither
cancel nor expire the wait of the slot's next task.

The tick itself stays at the timer rate on the housekeeping processor; the
index makes the deadline work proportional to what expires, not to what
waits. Waking housekeeping only when a deadline or lifecycle work is due is
left for later.

## Consequences

`BenchmarkNothingDue` measures a tick with 320 deadlines none of which is
due at about 2 ns on the host, against about 175 ns for a scan of 320
registrations (`BenchmarkScanAll`). Host tests cover ordered expiry, equal
deadlines, cancel, rearm, full capacity, invalid sources, slot reuse with a
new generation, a randomized comparison against a map, and, in
`kernel/ipc`, registrations that wakes and cancels end, readiness before a
timeout, a timeout before readiness, and a reused slot.

`make test-smp` runs the probe's deadline checks: a short `poll` timeout
fires; readiness beats a long timeout; readiness during a timeout ends the
wait, and a sleep that spans the old timeout lasts its full length; a
timeout beats readiness that comes after it; and six threads with
different timeouts all expire.
