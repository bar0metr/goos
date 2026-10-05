# Symmetric multiprocessing

GoOS runs tasks on every processor the firmware lists, up to 64. This page
describes how the processors start, what each one owns, and which lock or
protocol guards each piece of shared kernel state. ADR 0181 (start-up and
per-processor state), ADR 0182 (scheduler and locks), and ADR 0183 (TLB
shootdown) record the decisions.

Disabling interrupts excludes only the local processor. Shared state needs a
lock or an atomic protocol as well, and every mutable kernel structure below
names its own.

## Processors

`kernel/cpu.Table` holds the live state of each processor in the boot
inventory (ADR 0180), indexed by its dense logical number. Processor 0 is
the bootstrap processor. The APIC ID is a hardware name and never an index.
A processor moves forward only: present, started, online; or present and
failed. A processor whose APIC ID does not fit an xAPIC destination (above
254) stays present and offline, and boot says why.

The bootstrap processor starts the others after its own scheduler runs:

1. It copies the trampoline in `io.S` to a free page below 1 MiB, found in
   the firmware memory map (`boot.LowPage`). The page stays reserved.
2. It builds a start-up root: the kernel root's top level with private
   copies of the tables over the first 2 MiB, which also map the trampoline
   page read-only and executable at its physical address.
3. For one processor at a time it allocates a guarded stack, a double-fault
   stack, and a TSS, fills the trampoline's parameter block, and sends INIT,
   then up to two start-up interrupts, with the architectural delays.
4. The processor goes from real mode through protected mode to long mode,
   loads the kernel root, and calls `goos_ap_main` on its stack. That loads
   the shared GDT with its own task register, the shared IDT, the SYSCALL
   MSRs with its own entry block, the PAT, and its local APIC, then reports
   started and parks.

A processor that misses a deadline stays offline. Each side moves a
per-processor state word with compare-and-swap, so a processor that arrives
after the bootstrap processor gave up finds the cancellation and parks
instead of joining. Boot goes on with the processors that answered.

Started processors join when boot hands the machine to userspace, under the
kernel lock: each one starts its local timer, enters the scheduler's online
set, and runs its idle loop on its start-up stack. Only then does the
bootstrap processor mark it online in the CPU inventory, which is what
`lscpu` reports.

## What each processor owns

| State | Where |
| --- | --- |
| TSS: RSP0 of the running task, IST1 double-fault stack | `tss[n]`, selector `0x28 + 16n` |
| Data block: current slot, reschedule request, active root, entry count, kill request, counters | `goos_cpu_data[n]`, 256 bytes |
| SYSCALL entry block: process root, kernel-entry stack, user RSP, current slot | `goos_linux_entry_data[n]`, behind `KERNEL_GS_BASE` |
| Idle loop: context, FPU area, tick count | slot `64 + n` |
| Local APIC timer at the scheduler rate | the processor's own APIC |
| Line buffer of kernel console output | `console.lines[n]` |

The GDT is shared: entries 5 and up hold one TSS descriptor per processor,
and processor n loads task register `0x28 + 16n`. Entry code computes its
processor from that selector with `STR`, before it can trust any register,
and finds its data block there. The IDT is shared and never changes after
boot. Every process root maps the GDT, the IDT, the TSS array, and the entry
block array supervisor-only, because the processor reads them on a ring
transition before the kernel root is loaded.

## Interrupts

| Vector | Source | Processors |
| ---: | --- | --- |
| 32 | PIT IRQ0, only while boot calibrates the clocks | bootstrap |
| 33, 36, 43 | PS/2, COM1, virtio-blk through the IOAPIC | bootstrap |
| 0x30 | local APIC timer, the scheduler tick | each |
| 0x31 | reschedule request | each |
| 0x32 | TLB shootdown | each |
| 0x33 | stop after a panic | each |
| 0xff, 39, 47 | spurious | each |

Boot measures the TSC and the bootstrap processor's APIC timer over the same
100 ms of PIT ticks, then masks the PIT and starts the APIC timer at the
scheduler rate. Every processor uses that count. Only the bootstrap
processor's tick advances kernel time and wakes the housekeeping task, so
four processors still tick time at 100 Hz.

Device interrupts stay on the bootstrap processor, routed to its APIC ID.
A handler that wakes a task on another processor asks that processor to
reschedule. The processor that receives an interrupt acknowledges it at its
own APIC.

## Scheduling

`kernel/sched.Core` is the run state, a pure structure with host tests. The
amd64 layer calls it under the scheduler lock. It keeps the global task table
of 64 slots with generation-safe IDs, and, per processor, a run queue (the
`home` mask) and the current slot. A live task belongs to exactly one queue.
A task in the `running` mask is on a processor from the moment one picks it
until that processor has left its stack, so no other processor can pick a
task whose registers are still being saved.

A switch has two halves. `goos_sched_switch` runs on the outgoing task's
stack after the entry code saved its context and FPU state: it retires the
task for the given reason and picks the next one. `goos_sched_finish` runs on
the incoming task's stack: it clears the outgoing task from the running set,
publishes its exit if it ended, and moves it to an idle processor if it is
still runnable. A dead task's exit is published only there, so housekeeping
never frees a stack a processor still stands on.

A processor picks from its own queue round-robin, and when that is empty
steals one task from another queue. A woken or new task stays where it ran if
that processor idles, then goes to an idle processor, then to the least
loaded one, and the chosen processor is asked to reschedule. The request is a
flag plus an interrupt, and a pending flag covers later requests. The
kernel's own tasks (slots 0 to 3) are pinned to the bootstrap processor,
which is the housekeeping processor: deadline expiry, exit delivery, and
lifecycle retries run there once.

A wake sets the task runnable and records a pending wake. A task that blocks
with a wake pending stays runnable. That replaces the old rule that no wake
can arrive between a task's last check and its sleep because interrupts are
off: on two processors the wake can come at any time, and the pending bit
keeps it.

A new task is installed not runnable. Its creator sets its process binding,
thread record, TLS base, FPU state, and TID writes, then calls `StartTask`.

Terminating a task that runs on another processor marks it killed and
interrupts that processor, which ends it at its next scheduling point. A
task held by `HoldTasks` stays off every processor until released; a Linux
fork holds the caller's other threads while it copies the address space.
`QuiesceTasks` waits until tasks are off their processors; exec uses it
before it destroys the old address space.

## Locks

Locks are taken in increasing level order, with local interrupts disabled.
Validation builds record the holder and check the order and recursion.

| Level | Lock | Guards |
| ---: | --- | --- |
| 1 | kernel | every kernel object below |
| 2 | memory | the physical page allocator |
| 3 | scheduler | `sched.Core`, slot tables, kill and hold requests |
| 5 | console | serial and framebuffer output |

The kernel lock serializes the kernel's object layer. Every native call,
Linux system call, user page-fault resolution, and housekeeping step runs
under it. Tasks run in parallel in user mode; their kernel calls run one at a
time. The scheduler, timers, IPIs, TLB shootdown, and the page allocator do
not depend on it. A task waiting for the kernel lock gives up when it is
killed or held, retrying its call if it ever runs again, so a processor that
holds the lock while it waits for that task to leave cannot deadlock.

| State | Protection |
| --- | --- |
| Physical page allocator | memory lock |
| Run state, slot contexts, roots, FS bases, exit codes | scheduler lock |
| Process registry, PID allocation, completion records | kernel lock |
| Per-process quotas, handle tables, region maps, thread records | kernel lock |
| IPC channels, streams, waiter tables, `WaitAny` | kernel lock |
| Shared memory, executable objects, image page cache | kernel lock |
| Linux process state, signals, futexes, continuations | kernel lock |
| VFS bridge, lifecycle queues, kernel log | kernel lock |
| Process page tables, CopyIn and CopyOut walks | kernel lock, plus TLB shootdown |
| Kernel root mappings after boot (stack guard pages) | kernel lock, plus a lazy flush |
| Console input rings | single producer (bootstrap processor's IRQ), single consumer under the kernel lock |
| IRQ 11 pending flag and waiter | exchanged atomically |
| Kernel console output | console lock, one line at a time |
| CPU inventory online state | written by the bootstrap processor under the kernel lock |
| Per-processor data blocks, TSS, idle slots | local to their processor |
| GDT, IDT, trampoline layout | written before the processor that uses them starts |

## TLB shootdown

A processor holds translations of a process root only while it runs in ring 3
on that root: every kernel entry loads the kernel root, and GoOS maps no
global pages and uses no PCIDs, so the CR3 write drops the old root's
translations. The kernel reaches user memory through the direct map.

So a shootdown only has to make each processor that may be in ring 3 on the
root enter the kernel once. Each processor stores the root it is about to
return to before the CR3 write, and counts its kernel entries after the CR3
write at entry. The initiator, holding the kernel lock, changes the page
tables, fences, reads which processors run the root, interrupts them, and
waits until each has entered the kernel or left the root. Entry code clears
the active root, so a processor spinning on a lock with interrupts off is
never waited for.

`UnmapUserRange` clears the leaves first and keeps each owned page's address
in the cleared entry, runs the shootdown, then frees the pages. No stale
translation can reach a page after the allocator hands it to someone else.
`ProtectUserRange` runs one after it changes permissions. A fault on a page
whose entry already allows the access came from a translation another
processor replaced, and the resolver retries it.

Kernel root changes after boot are the guard pages of new task stacks. A
processor flushes its TLB at its next switch when the kernel's generation
counter moved; only the processor that runs a task touches its stack, and it
switches to the task first.

## Diagnostics

In a diagnostic build, `diag` prints the run state (running and online
masks, switches, steals, migrations, kills), the most tasks of one address
space that ran on distinct processors at once (`parallel-peak`), lock
contention, shootdown counts, and per processor its ticks, idle ticks, and
received reschedule and shootdown interrupts. Nothing is logged per switch.

## Panics

A kernel panic stops the other processors with the stop vector, then prints
the message and the processor's index and APIC ID. A fatal exception does the
same from assembly. AP start-up failures are not panics: the processor stays
offline.

## Tests

`make test-smp` boots four processors and runs the Linux probe's `smp` mode,
which uses `getcpu` to prove that workers ran on every processor and that
two computed on distinct processors within one window shorter than half a
tick.
It then runs the thread, FPU, fork, exec, pipe, and signal probes with their
tasks spread over the processors. `SMP_MATRIX=1` adds one, two, and eight
processors. Every boot target takes `SMP=N` to run its own scenario on N
processors.
