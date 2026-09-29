# ADR 0137: Linux FS-base and TLS context

## Status

Accepted design for GoOS 0.2.0; implementation is deferred to the memory/TLS
phase.

## Context

Static musl uses `arch_prctl(ARCH_SET_FS)` for thread-local storage. FS base is
CPU context, so keeping it in a global or restoring it only at syscall return
would leak TLS state across preemption. A future `SWAPGS` syscall entry also
needs an explicit ownership rule for GS.

## Decision

FS base is task context, validated as a canonical userspace address and saved
and restored on every context switch. `ARCH_SET_FS` is the only supported Linux
`arch_prctl` mutation in 0.2.0. `ARCH_SET_GS` is rejected.

Kernel GS ownership is reserved for the architecture entry mechanism. User GS
is not part of the 0.2.0 Linux contract. The Phase 2 entry implementation must
document `SWAPGS`, nested interrupt, exception, and all return-path rules before
enabling `SYSCALL`.

Process teardown and task-slot reuse clear saved FS state. Tests must preempt
between at least two Linux tasks with different bases and prove isolation.

## Consequences

TLS state follows the schedulable Task while ABI identity remains on the
Process. Linux TLS support cannot weaken native context isolation.
