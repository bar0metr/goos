# ADR 0004: Kernel-owned GDT and IDT

Status: Accepted

## Context

Firmware descriptor tables are not part of the post-UEFI kernel contract. A
page fault or invalid instruction without a valid kernel IDT can escalate into
a double fault and reset the guest, hiding the original failure.

## Decision

Install a minimal long-mode GDT and a 256-entry IDT after switching to
kernel-owned page tables. Populate exception vectors 0 through 31 with assembly
stubs that distinguish hardware-error-code exceptions. Give only `INT3` a DPL 3
gate. Validate the returning exception path during every boot with a deliberate
breakpoint.

## Alternatives

Keeping firmware tables leaves critical CPU state under unspecified ownership.
Calling Go directly from interrupt stubs is deferred because stack alignment,
register frames, IST stacks, and the TinyGo calling convention need an explicit
ABI before asynchronous interrupts are enabled.

## Consequences

GoOS can identify fatal CPU exceptions through serial output and can safely
return from a breakpoint. Rich diagnostics, TSS/IST protection, double-fault
stacks, and Go-level exception dispatch remain required before ring 3 and
preemptive scheduling.
