# ADR 0028: Generation-safe task identities and bounded process wait

## Status

Accepted

## Context

Architecture task slots are reused after exit. A slot-only identifier lets a
late IPC timeout or wakeup target an unrelated replacement task. Process exit
delivery also removes the live registry entry, so a waiter that observes the
completion after delivery needs a stable bounded result rather than a pointer
to reclaimed process state.

## Decision

Encode Task IDs as a 32-bit generation and a one-based 32-bit slot. Increment
the generation on every architecture and policy-scheduler slot installation.
All external wake, exit-code, reclaim, process ownership, and IPC waiter paths
validate both fields. Raw slots remain internal to the interrupt scheduler.

Retain the latest 32 process completion records independently from the live
process registry and lifecycle delivery queue. `Registry.Wait` reports running,
exited, or not found without consuming the lifecycle event. Process IDs remain
monotonic, so an old completion cannot name a replacement process.

## Consequences

Late wakeups cannot make a replacement task runnable. Recent process results
remain observable after teardown without retaining address spaces, stacks, or
handles. Completion retention is deliberately bounded; process handles and a
native wait kernel call remain future API work.
