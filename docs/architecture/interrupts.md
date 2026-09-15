# Descriptor tables and CPU exceptions

GoOS replaces the firmware GDT and IDT after installing its own page tables.
The bootstrap GDT contains null, ring 0 and ring 3 code/data descriptors, and a
64-bit TSS descriptor.

The IDT contains 256 entries and installs gates for architectural exception
vectors 0 through 31. The breakpoint gate uses DPL 3 so future userspace
debuggers can issue `INT3`; other exception gates are restricted to ring 0.
Exceptions that push a hardware error code use distinct entry stubs so their
stack shape remains unambiguous.

Vector 3 currently writes a fixed serial diagnostic and returns with `IRETQ`.
Boot executes a deliberate `INT3` and requires `EXCEPTION_OK`, `GDT_OK`, and
`IDT_OK`. Other CPU exceptions stop the machine after a serial panic marker.
User-fault recovery policy remains future work; non-breakpoint faults are still
fatal during bootstrap.

## Bootstrap timer

The first timer path remaps the dual 8259 PIC to vectors 32 through 47, masks
every line except IRQ0, and configures PIT channel 0 for 100 Hz. The IRQ0 stub
increments a monotonic 64-bit tick counter, acknowledges the master PIC, and
returns with `IRETQ`. Boot waits for three physical timer interrupts before
emitting `TIMER_OK`.

The PIC/PIT path is a deliberately small bootstrap mechanism for the current
single-CPU QEMU target. Local APIC, IOAPIC, per-CPU timers, and calibrated time
conversion are required before SMP and are not hidden behind the current API.

## Ring transition state

The GDT also contains ring 3 code and data descriptors plus a 64-bit TSS. RSP0
points into a dedicated four-page kernel stack with an unmapped guard page. The
task register is loaded only after the TSS descriptor and stack are complete.

IDT vector `0x80` is a DPL 3 interrupt gate used by the bootstrap native call.
On entry the CPU switches from the user stack to TSS.RSP0. The handler validates
the saved user CS, preserves all general-purpose registers, and invokes the
architecture-independent dispatcher. Returning operations resume with `IRETQ`;
Exit restores the suspended boot context. The gate is a temporary transport;
future `SYSCALL/SYSRET` support must preserve the same native call contract.
