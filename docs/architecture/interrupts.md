# Descriptor tables and CPU exceptions

GoOS replaces the firmware GDT and IDT after installing its own page tables.
The bootstrap GDT contains null, ring 0 code, and ring 0 data descriptors. TSS
and ring 3 descriptors are intentionally deferred until userspace transition is
implemented.

The IDT contains 256 entries and installs gates for architectural exception
vectors 0 through 31. The breakpoint gate uses DPL 3 so future userspace
debuggers can issue `INT3`; other exception gates are restricted to ring 0.
Exceptions that push a hardware error code use distinct entry stubs so their
stack shape remains unambiguous.

Vector 3 currently writes a fixed serial diagnostic and returns with `IRETQ`.
Boot executes a deliberate `INT3` and requires `EXCEPTION_OK`, `GDT_OK`, and
`IDT_OK`. Other CPU exceptions stop the machine after a serial panic marker.
Structured register frames and recovery policy will be added before userspace.

## Bootstrap timer

The first timer path remaps the dual 8259 PIC to vectors 32 through 47, masks
every line except IRQ0, and configures PIT channel 0 for 100 Hz. The IRQ0 stub
increments a monotonic 64-bit tick counter, acknowledges the master PIC, and
returns with `IRETQ`. Boot waits for three physical timer interrupts before
emitting `TIMER_OK`.

The PIC/PIT path is a deliberately small bootstrap mechanism for the current
single-CPU QEMU target. Local APIC, IOAPIC, per-CPU timers, and calibrated time
conversion are required before SMP and are not hidden behind the current API.
