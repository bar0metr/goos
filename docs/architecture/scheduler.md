# Scheduler

GoOS separates schedulable Tasks from Processes. A Task owns execution state
and a kernel stack. A Process owns an address space, handles, metadata, and one
or more Task identities. Userspace tasks record their owning process and CR3;
kernel tasks use the kernel page-table root.

`kernel/sched.Core` holds the run state of every task and processor: 64 task
slots with generation-safe IDs, a run queue and a current slot per
processor, and the set of tasks on a processor. The amd64 layer calls it
under the scheduler lock. Processors, placement, stealing, and the locks are
described in [SMP](../kernel/smp.md) (ADR 0182). Each task also has an
affinity mask: it runs only on the online processors the mask allows, and
the kernel's own tasks stay on the housekeeping processor (ADR 0188).

## x86_64 context switch

Every switch saves all 15 general-purpose registers above the interrupt frame,
records that stack pointer as the task context, saves the SSE and x87 state into
the slot's 512-byte FXSAVE area, and asks the scheduler for the next slot on
this processor. On the incoming stack it releases the outgoing task to other
processors, then restores the incoming FXSAVE area, its FS base, this
processor's TSS.RSP0 and SYSCALL entry block, and resumes it with `IRETQ`,
loading the task's CR3 on the way to ring 3. A slot's FXSAVE area is reset to
a clean image, all exceptions masked, when the scheduler starts and when an
exited task's slot is reclaimed (ADR 0056). The switch points are:

- the local APIC timer, the preemption point, once per tick on each processor;
- a reschedule interrupt from another processor;
- a native call that blocks, rewound over its `INT` so it retries on wake;
- a native call that exits or faults;
- a native or Linux call that returns while this processor's reschedule
  request is set;
- a user page fault that blocked its task;
- IRQ11 when it woke the driver's waiter;
- vector 0x81, which kernel tasks use to give up the processor without
  advancing time. It is an interrupt gate with DPL 0, so ring 3 cannot invoke
  it.

Selection is round-robin over the processor's queue from the slot after the
current one. A processor with nothing queued steals one task from another
queue, and with nothing to steal runs its idle loop, which has a slot of its
own after the 64 task slots.

New kernel tasks receive four contiguous stack pages, a synthetic interrupt
frame, and two unmapped guard pages below them; the build bounds every kernel
frame by that guard (ADR 0088). The frame preserves Win64 entry alignment
and home space required by compiler-generated functions. Userspace tasks receive
the same guarded kernel stack plus a ring 3 frame naming their RX entry point
and RW+NX user stack.

A task can block itself and be woken by another task on any processor.
Native Exit retires the current user task and schedules its successor without
returning through a global boot context.

`WaitAny` registers up to 16 readiness interests against the full generation-safe
Task ID. A matching channel, stream, or process event completes the operation and
sets the task's runnable bit.

## Wakeups and safe-return rescheduling

A wake marks the task runnable, records a pending wake, and places the task
on a run queue; it never switches from inside the kernel code that published
the event. The processor that should run the task gets a reschedule request:
its own return path checks the flag, and another processor gets an
interrupt. A native or Linux call whose work is complete and that finds the
request set is parked with its result already in RAX and its instruction
pointer after the `INT` or `SYSCALL`, exactly as if the timer had preempted
it on return. Delivery of a message, a stream write, a process exit, or a
block-device interrupt therefore does not wait for the next tick.

A task that blocks with a wake pending stays runnable. No wake is lost
between a task's last check and its sleep, whichever processor publishes it.

## Kernel tasks

Slot 0 runs the kernel's housekeeping loop (ADR 0053) on the bootstrap
processor, to which the kernel tasks are pinned. Each step runs with
interrupts disabled, takes the locks of the state it touches (ADR 0184), and
ends by sleeping through vector 0x81; a wake that came during
the step keeps it running. It is woken by every tick, which
expires the waits the deadline index reports due (ADR 0189) and retries
retained lifecycle messages; by a user task exit
or fault; and by a lifecycle request from PID 1. Slots 1 and 2, the boot-time
scheduling demonstration, sleep permanently once the demonstration completes.
Slot 3 is reserved and never runs; each processor's idle loop has its own
slot.

The live userspace scenario schedules two processes with distinct CR3 roots.
One first waits out a deadline longer than its peer's, so the peer's timed-out
wait is observed before any message arrives, then remains in ring 3 long enough
to be preempted, performs native calls, and exits. The other executes an invalid opcode; the exception path terminates only
that task, after which kernel tasks and the boot sequence continue.

After exit delivery, the dead task's guarded kernel stack is returned to the
physical allocator and its architecture slot can be reused. An exit is
published only once the task's processor has left its stack. Task IDs combine a generation with their reusable slot; IPC wakeups
and exit teardown validate both fields, so stale events cannot target a new
task. A dead slot becomes reusable only after its exit record has been delivered
and its kernel stack reclaimed, because installing a new task bumps the slot
generation: reusing a slot with an undelivered exit would orphan that record and
block every waiter on the exited process forever. Priorities are not
implemented.
