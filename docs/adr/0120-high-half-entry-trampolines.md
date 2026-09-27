# ADR 0120: High-half entry trampolines

## Status

Accepted.

## Context

The TinyGo UEFI kernel is a fixed, non-relocatable image linked at low virtual
addresses. Process roots previously retained the low kernel mapping because
interrupt entry, kernel stacks, and normal handlers depended on it. That
reserved ordinary ELF addresses such as `0x00400000` for the kernel.

An attempted conversion pointed IDT gates at high aliases of the existing
handlers and continued through normal TinyGo code at those aliases. TinyGo had
linked every internal reference for the low address. Executing the same bytes
at a different virtual address violated that assumption, grew the entry stack
without bound in `runtime.alloc`, and ended in a double fault. Increasing the
stack only delayed the failure.

Low kernel-entry stacks also stopped working once process roots dropped the low
kernel mapping. Guard pages had to remain absent in both the kernel root and
every process root. Stack reclamation could no longer infer a physical
allocation from a high RSP value.

## Decision

Use a high assembly trampoline, high per-task kernel stacks, and low link-time
TinyGo execution.

Every user-reachable exception, IRQ, yield, and native-call gate points to a
small high alias of assembly entry code. The CPU enters on the task's high
kernel stack. The trampoline loads the kernel CR3 before transferring to the
ordinary low handler address. No Go or TinyGo code runs through a high alias.

The return path stays on the high stack, transfers to a high assembly return
prefix, loads the process CR3 there, restores registers, and executes `iretq`.
It executes no Go or TinyGo code after loading the process CR3.

Process roots contain only the supervisor mappings needed before and after the
CR3 changes:

- high assembly entry and return pages, executable and read-only;
- descriptor and transition metadata, writable and NX;
- the current task's high kernel stack, writable and NX;
- the shared direct-map PML4 slot.

They do not map the low kernel image or low kernel stacks. The kernel root keeps
its low mapping for the linked image and a complete high alias for transition
code and high stacks.

The scheduler keeps the kernel CR3 while it restores a saved context. The
return prefix changes to the selected process CR3. Each scheduler slot records
the stack allocation's physical base, high virtual base, and top. Reclamation
uses that metadata and never derives ownership from RSP.

The guard page below each stack remains unmapped through both roots. Process
address-space teardown frees private supervisor page-table branches without
freeing the kernel pages they map.

## Consequences

Ordinary low virtual addresses are available to userspace. The validation
image executes at `0x00402000`, maps data at `0x00404000`, and verifies that a
ring-3 read from the mapped high trampoline page faults. Repeated native calls
also verify a stable per-task entry RSP.

The design preserves the current linker model and avoids unsupported TinyGo
runtime relocation assumptions. It keeps transition mappings small and
supervisor-only.

Entry and return now cross low and high virtual addresses, which adds assembly
and page-table complexity. Every new user-reachable entry path must use the
same trampoline rules. A truly higher-half-linked, relocatable kernel remains
separate work. This decision does not describe the current kernel as fully
higher-half relocated.
