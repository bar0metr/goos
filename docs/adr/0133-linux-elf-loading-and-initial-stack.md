# ADR 0133: Linux ELF loading and initial stack

## Status

Accepted for GoOS 0.2.0. The bounded ELF64 parser and load-plan validation are
implemented in Phase 3. Phase 4 maps validated loads through the common lazy
executable-source path and reserves the Linux stack. Phase 5 implements the
bounded System V initial-stack image and its page installer.

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

The mapper preserves ELF page congruence: a non-page-aligned `p_vaddr` records
the page-aligned source offset and includes the leading file bytes in that
page. Bytes through `p_filesz` come from the executable source; the remainder
of `p_memsz` is demand-zero. Loads are confined to the low image area, so
ordinary `0x400000` executables work while fixed GoOS user windows cannot be
overwritten. Existing regions are collision checked before the map changes.

Linux receives an 8 MiB writable, non-executable, demand-zero stack
reservation below `vm.UserStackTop`, with a pinned 64 KiB guard below it.
Phase 4 maps no initial stack page and writes no Linux startup data.

The Linux initial stack contains `argc`, `argv`, `envp`, and a bounded auxv.
It does not expose `GST4`. Resolved GoOS handles remain kernel-side compatibility
state. `AT_RANDOM` is omitted until trustworthy entropy exists; a deterministic
value is forbidden.

The builder accepts at most 256 arguments, 256 environment entries, and 64 KiB
for the complete occupied stack image. It validates every NUL-free string,
emits NUL-terminated vectors, and aligns initial RSP to 16 bytes. Auxv contains
`AT_PHDR`, `AT_PHENT`, `AT_PHNUM`, `AT_PAGESZ`, `AT_ENTRY`, `AT_EXECFN`,
`AT_UID`, `AT_EUID`, `AT_GID`, `AT_EGID`, `AT_SECURE`, `AT_CLKTCK`, optional
`AT_PLATFORM`, and `AT_NULL`. UID and GID are the synthetic unprivileged value
1000; `AT_CLKTCK` is 100.

Only pages covering the built image are materialized writable and NX. Launch
page accounting includes those owned pages and page tables; the unused stack
reservation still grows by demand-zero faults. PID 1 classification and Linux
process creation remain Phase 6.

## Consequences

Native image bytes and startup ABI do not change. Shared launch mechanics stay
common, while parsing, address rules, entry state, and startup layout remain
format-specific.
