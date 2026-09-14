# ADR 0005: PIC/PIT bootstrap timer

Status: Accepted

## Context

Preemptive scheduling requires a hardware interrupt source. The first release
targets one x86_64 CPU under QEMU; SMP and production hardware support are out
of scope for this milestone.

## Decision

Use the legacy 8259 PIC and PIT channel 0 as a bootstrap timer. Remap IRQs away
from CPU exception vectors, mask every line except IRQ0, configure 100 Hz, and
acknowledge each interrupt explicitly. Require three delivered interrupts in
the boot test.

## Alternatives

Local APIC timers and the IOAPIC are the long-term mechanism but require ACPI
table parsing, APIC discovery, calibration, and per-CPU state. Retaining UEFI
timer events after `ExitBootServices` is invalid.

## Consequences

The kernel has an actual timer-driven interrupt source suitable for beginning
the scheduler. The mechanism is single-CPU and QEMU-oriented. APIC support must
replace it before SMP or broader hardware support; the scheduler must depend on
a timer abstraction rather than PIC ports.
