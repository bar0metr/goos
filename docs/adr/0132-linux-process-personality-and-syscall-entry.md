# ADR 0132: Linux process personality and syscall entry

## Status

Accepted for GoOS 0.2.0. Phase 1 is implemented; the Linux `SYSCALL` entry is
deferred to Phase 2.

## Context

Native GoOS programs enter the kernel through DPL 3 interrupt vector `0x80`.
Linux x86_64 programs use the `SYSCALL` instruction and a different register,
status, and startup contract. Selecting a dispatcher from a syscall number
would let a program cross ABI boundaries and attempt to use guessed native
handles.

## Decision

Every Process has a kernel-owned immutable ABI: `Native` or `LinuxX8664`. The
validated executable path supplies it when initializing the process slot; no
runtime setter exists. All Tasks in one Process share that identity.

The native `int 0x80` trampoline checks the current task's kernel-owned page
table root against the process registry before any normal or blocking native
operation is decoded. Only `Native` is accepted. Rejection returns the native
`PermissionDenied` status, increments a bounded counter, and logs the first
failure.

Phase 2 will add a distinct Linux x86_64 `SYSCALL` entry. It will accept only
`LinuxX8664`, use the Linux register and negative-errno convention, switch to a
guarded kernel entry stack, and return through the safe high-half IRETQ path.
Neither gate infers personality from a request number.

## Consequences

Native behavior and request numbering remain unchanged. A Linux process cannot
use the native gate as a capability bypass. Adding another ABI requires a new
explicit identity and entry policy rather than widening either existing gate.
