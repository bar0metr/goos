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
Non-breakpoint faults inspect the saved privilege level. Kernel faults remain
fatal. A user fault removes only the current userspace task from the scheduler
and resumes another runnable task.

## Bootstrap timer

The first timer path remaps the dual 8259 PIC to vectors 32 through 47, masks
every line except IRQ0, and configures PIT channel 0 for the rate named by
`kernel/clock`, currently 100 Hz. The PIT divides a fixed 1.193182 MHz input and
is independent of CPU frequency, so the rate is a deliberate trade between
wakeup latency and per-tick interrupt overhead. The IRQ0 stub
increments a monotonic 64-bit tick counter, acknowledges the master PIC, and
returns with `IRETQ`. Boot waits for three physical timer interrupts, then
counts TSC cycles across 100 ms of ticks to calibrate a nanosecond clock
(ADR 0054), before emitting `TIMER_OK`.

The PIC/PIT path remains the bootstrap timer mechanism. For Q35 PCI INTx, the
kernel maps the LAPIC and IOAPIC into supervisor-only pages and routes virtio
GSI 11 to vector 43. The handler coalesces the level-triggered source, masks the
route, wakes the single capability-authorized waiter, and acknowledges the
LAPIC. The driver acknowledges the device ISR; its next wait rearms the route.

The current interrupt routing is deliberately single-CPU and single-device.
ACPI MADT parsing, dynamic GSI routing, per-CPU timers, and calibrated time
conversion are required before SMP.

## Ring transition state

The GDT also contains ring 3 code and data descriptors plus a 64-bit TSS. RSP0
points into a dedicated four-page kernel stack with two unmapped guard pages. The
task register is loaded only after the TSS descriptor and stack are complete.

IDT vector `0x80` is a DPL 3 interrupt gate used by the bootstrap native call.
On entry the CPU switches from the user stack to TSS.RSP0. The handler validates
the saved user CS, preserves all general-purpose registers, and invokes the
architecture-independent dispatcher. Returning operations resume with `IRETQ`;
Exit marks the task non-runnable and schedules its successor. The gate is a
temporary transport;
future `SYSCALL/SYSRET` support must preserve the same native call contract.

## Device interrupts

Three device lines are routed through the IOAPIC on bootstrap CPU 0:

| Line | Vector | Trigger | Owner |
| --- | ---: | --- | --- |
| GSI 11 | 43 | active-low, level | the userspace virtio-blk driver, through `IRQWait` |
| GSI 4 | 36 | active-high, edge | the console UART, handled in the kernel |
| GSI 1 | 33 | active-high, edge | the PS/2 keyboard, handled in the kernel |

The virtio line is masked by its handler and rearmed by the driver's next wait,
so an unserviced device cannot storm. The console line is not: every byte
matters and the handler consumes the condition by reading the device.

The console handler drains every byte the UART has into a 256-byte ring and
returns. It takes no scheduling decision. The kernel loop sees the ring and
publishes readability through the ordinary wait table, so one mechanism
completes a wait and a reader blocks on the console the way it blocks on a
channel (ADR 0075). Draining in the handler is what keeps input from being lost;
the wake can wait for the next tick because the bytes are already safe.

Byte three is diverted into an interrupt flag rather than the ring. Ctrl-C is a
signal, so a shell can ask whether it was typed without consuming the input a
running command is being given.

The keyboard handler reads one i8042 byte and passes it to a bounded scan-code
set 1 decoder. Completed US-layout keys enter the same console ring as UART
bytes. Arrow keys become CSI sequences; keyboard Ctrl-C increments the same
interrupt flag. Controller waits exist only during bounded initialization;
steady-state input is interrupt-driven.

Memory-mapped device registers are reached through assembly helpers. As Go they
were ordinary loads and stores that the optimiser could reorder, merge or
discard, which is not a contract an IOAPIC register select and data write can be
written against.
