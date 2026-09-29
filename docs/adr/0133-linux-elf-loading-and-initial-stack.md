# ADR 0133: Linux ELF loading and initial stack

## Status

Accepted design for GoOS 0.2.0; implementation begins after the syscall-entry
phase.

## Context

GoOS native images use the `GOOSNAT` envelope and the `GST4` startup object.
Unmodified Linux programs require ELF64 program headers, lower-half Linux
addresses, and the System V initial stack. Passing Linux ELF through
`elf2native` would erase the format boundary and would not exercise the Linux
ABI.

## Decision

Linux ELF has a separate bounded validator and load-plan builder. The first
release accepts little-endian x86_64 `ET_EXEC`, rejects `PT_INTERP`, executable
stacks, W+X, overlaps, reserved mappings, malformed arithmetic, and unsupported
headers, and limits program headers and load segments.

Validated `PT_LOAD` regions reuse the existing stable executable source, lazy
file-backed faults, demand-zero BSS, page accounting, and W^X enforcement. The
format-specific validator supplies `LinuxX8664` only after the complete launch
plan is valid.

The Linux initial stack contains `argc`, `argv`, `envp`, and a bounded auxv.
It does not expose `GST4`. Resolved GoOS handles remain kernel-side compatibility
state. `AT_RANDOM` is omitted until trustworthy entropy exists; a deterministic
value is forbidden.

## Consequences

Native image bytes and startup ABI do not change. Shared launch mechanics stay
common, while parsing, address rules, entry state, and startup layout remain
format-specific.
