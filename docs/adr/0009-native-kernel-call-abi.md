# ADR 0009: Native kernel-call ABI and validated user memory

Status: Accepted. DebugWrite was retired by ADR 0094; the rest stands.

## Context

The first ring 3 program used `INT 0x80` only to prove a privilege round trip.
The handler did not dispatch the operation number, define typed errors, or
validate user pointers. Directly extending that assembly path would mix the
native API with its x86 transport and make untrusted memory handling difficult
to audit.

## Decision

Define an architecture-independent native call dispatcher. RAX carries the
operation number; RDI, RSI, RDX, R10, R8, and R9 carry up to six arguments.
RAX returns a typed native status and RDX returns an operation value. Statuses
are part of the native API and are not Linux errno values.

Keep `INT 0x80` as the bootstrap transport for this iteration. The x86 entry
stub saves the complete general-purpose register state, calls the dispatcher,
and restores userspace for returning operations. A later `SYSCALL/SYSRET`
transport must preserve the same architecture-independent contract.

The first operations are Exit and a bounded DebugWrite. DebugWrite accepts at
most one page and copies through `CopyIn`; it never dereferences a user virtual
address directly. `CopyIn` and `CopyOut` validate overflow, the bootstrap user
range, every page-table level, the user bit, and destination write permission.

## Alternatives

Implementing `SYSCALL/SYSRET` immediately would combine a new entry mechanism,
MSR setup, canonical return checks, user-memory validation, and dispatch in one
change. Keeping call semantics in assembly would make testing and future
architecture support harder. Passing raw user pointers to Go code would let a
malformed request fault the kernel.

## Consequences

Native calls now have explicit operations and typed results, and the kernel has
one audited boundary for user-memory copying. DebugWrite is a bootstrap debug
operation, not a TTY API, and will be removed or capability-restricted when the
userspace TTY exists. Exit still returns through a single bootstrap context;
process-aware scheduling and lifecycle remain the next iteration.
