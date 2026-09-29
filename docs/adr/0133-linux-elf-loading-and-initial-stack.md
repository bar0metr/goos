# ADR 0133: Linux ELF loading and initial stack

## Status

Accepted for GoOS 0.2.0. The bounded ELF64 parser and load-plan validation are
implemented in Phase 3; mapping and the initial stack remain later phases.

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

The parser reads the existing stable executable-source abstraction, caps the
program-header table at 64 entries and `PT_LOAD` at 16 segments, and records
`PT_PHDR`, `PT_TLS`, `PT_GNU_STACK`, and `PT_GNU_RELRO` metadata. Load ranges
must be page-congruent, remain in the configured user range, avoid configured
reserved pages, and contain both the program-header table and executable entry
where required. It performs no mapping and cannot select a Process ABI by
itself.

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
