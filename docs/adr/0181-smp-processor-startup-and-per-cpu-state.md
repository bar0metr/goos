# ADR 0181: Processors start through a low trampoline and own their entry state

## Status

Accepted.

## Context

ADR 0180 inventories every processor the firmware lists but runs on the
bootstrap processor alone. A second processor needs a way in: an x86
application processor starts in real mode at an address below 1 MiB, and the
kernel identity-maps nothing there. Once it runs, every piece of state the
hardware reads per processor has to be its own: the TSS that names RSP0 and
the double-fault stack, the task register, the SYSCALL MSRs and the block
behind `KERNEL_GS_BASE`, the local APIC and its timer. The entry code also
needs to know which processor it is on before it can trust a register.

## Decision

`kernel/cpu.Table` keeps the live state of each inventoried processor:
present, started, online, or failed, moving forward only. The dense index is
the identity everywhere; the APIC ID is used only to address interrupts. An
APIC ID above 254 does not fit an xAPIC destination, so that processor stays
offline and boot says so. GoOS does not enable x2APIC.

The bootstrap processor starts the others one at a time, after its own
scheduler and clocks run. It copies a small trampoline from `io.S` into a
usable page below 1 MiB from the firmware memory map, which stays reserved,
and builds a start-up root that shares the kernel's tables except over the
first 2 MiB, where private copies also map the trampoline page read-only and
executable. For each processor it prepares a guarded stack, a double-fault
stack, and a TSS, then sends INIT and up to two start-up interrupts with the
architectural delays. The trampoline enters long mode on the start-up root
and jumps to `goos_ap_entry`, which loads the kernel root and calls
`goos_ap_main` on the new stack.

Both sides advance one compare-and-swap state word per processor. A processor
that misses its 100 ms start-up deadline, or its join deadline, is cancelled;
if it arrives later it finds the cancellation and halts. Boot never waits
without a bound and continues with the processors that answered.

Started processors park until boot hands the machine to userspace. They then
join under the kernel lock: each starts its local APIC timer, enters the
scheduler's online set, and runs its idle loop on its start-up stack. The
CPU inventory marks a processor online only after it joined, so `lscpu`
reports processors that run tasks.

Per-processor state:

- One shared GDT holds a TSS descriptor per processor; processor n loads
  task register `0x28 + 16n`. Entry code reads the selector with `STR` to
  find its processor, which needs no register and no memory.
- A 256-byte data block per processor holds the current slot, the reschedule
  and kill requests, the root it last returned to, an entry counter, and
  counters for diagnostics.
- A 32-byte SYSCALL entry block per processor, behind that processor's
  `KERNEL_GS_BASE`, names the current task's root, kernel-entry stack, and
  slot.
- The IDT is shared and immutable after boot.
- Each processor has an idle slot after the 64 task slots, with a context,
  an FPU area, and a tick count of its idle time.

Every process root maps the GDT, the IDT, the TSS array, and the entry block
array supervisor-only, as it mapped the single copies before.

A kernel panic stops the other processors with a dedicated vector and
reports the processor's index and APIC ID; a fatal exception does the same
from assembly.

## Consequences

The bootstrap processor is processor 0 under the same abstractions as the
others. A single-processor machine runs the same code with an online set of
one. A low page and four page-table pages are spent on start-up; the page
tables are freed when every processor arrived. Processors above the xAPIC
destination range, hotplug, and offlining a running processor are out of
scope.
