# ADR 0075: Interrupt-driven console

## Status

Accepted. Extends ADR 0051 and closes the defect ADR 0072 recorded.

## Context

`ConsoleRead` polled the UART: it answered `NotFound` when no byte had arrived,
and the shell immediately asked again. Waiting for someone to type therefore
consumed a CPU. Per-process accounting made it measurable rather than
theoretical: the shell was charged about 7,200 of the 7,500 timer ticks in a
session, roughly ninety-five percent of the machine.

Nothing broke, because round-robin still ran everything and there was nothing
else to run. It defeated the idle task, it contradicted the move away from
polling in ADR 0051, and it is the kind of cost that stops being free the moment
there is a second CPU or a power budget.

The obvious cheap fix — sleeping a tick between retries — trades the spin for
lost input. The UART FIFO holds sixteen bytes, and console input here arrives in
whole lines as fast as the host can write them, so a reader that looks once per
tick loses characters.

## Decision

**The UART raises an interrupt and the handler drains it.** GSI 4 is routed to
vector 36, the received-data-available interrupt is enabled, and the handler
reads every byte the device has into a 256-byte ring. Draining in the handler is
what makes the fix safe: bytes are taken at the speed they arrive, so no reader
has to be running for input to be kept.

**The interrupt is enabled only after its gate exists.** `COM1.Init` runs long
before the IDT is installed, so enabling the interrupt there would let the first
byte arrive with no handler. It is turned on after descriptor tables and IOAPIC
routing are in place.

**The console is a waitable object.** `WaitAny` accepts the console device
handle with `WaitReadable`, and the shell waits on it alongside its PID 1
channel. Read authority on the console is what a reader already needs to take a
byte, so waiting for one needs nothing more. The shell no longer retries; it
blocks.

**The wake travels the ordinary path.** The handler does not touch the
scheduler. The kernel loop sees the ring and publishes readability through the
same wait table every other readiness uses, so one mechanism completes a wait.
The cost is up to one tick of latency on a keystroke, which is imperceptible,
and the bytes are already safe in the ring by then.

**Ctrl-C is a signal, not input.** The handler diverts byte three into a flag
instead of the ring, and `ConsoleInterrupt` consumes that flag. The shell asks
whether an interrupt was typed without reading anything.

This part was forced by the ring. Watching for Ctrl-C while a command owns the
console previously meant reading bytes and discarding the ones that were not
Ctrl-C, which took input away from the process the user was typing to. It
happened to work because the UART buffered only sixteen bytes and the running
utility usually won the race. A 256-byte ring made the shell win instead, and
`readline` received `payload` where `bounded stdin payload` had been typed.
Diverting the interrupt removes the race rather than re-tuning it.

**Overruns are reported.** Input arriving faster than it is read overruns the
ring. Nothing can recover those bytes, so the count is logged once: a reader
that has fallen behind will keep falling behind, and repeating the warning would
only add to what it is behind on.

**Memory-mapped device registers are reached through assembly.** As Go, the
IOAPIC accessors were ordinary loads and stores that the optimiser was free to
reorder, merge or discard. Two register writes survived; adding the two this
change needs did not, and the virtio route came out corrupted — storage stopped
working while the console change appeared to be at fault. An IOAPIC access is a
register select followed by a data transfer that must reach the device in that
order and exactly once, which is what `outb` and its siblings are already in
assembly for.

## Alternatives

**Sleeping between retries.** Three lines, no interrupt, no new waitable object,
and it removes ninety-nine percent of the spin. It also loses input: a line
pasted into the console is more than the sixteen bytes the FIFO holds, and a
reader that looks once per tick does not come back in time.

**Draining the UART from the timer interrupt.** No new vector, no routing, and
the ring still fills without a reader running. It samples at the tick rate, so
it has the same overrun problem as sleeping, only inside the kernel.

**Giving the shell an IRQ capability.** `IRQWait` exists and the shell could
wait on GSI 4 directly. It makes the shell a device driver, which is what the
architecture keeps drivers in userspace to avoid, and it puts a raw interrupt
line in the hands of an interactive session.

**Waking the waiter from the handler.** The IRQ 11 path records a task slot and
marks it runnable from assembly, and the console could do the same. It would
save up to a tick of latency and it would put a second mechanism for completing
a wait beside the wait table, which is the one thing every other object already
agrees on.

**A line discipline in the kernel.** Buffering lines, echoing, and delivering
Ctrl-C to a foreground group is what a real terminal layer does, and it would
subsume the interrupt question. It is a subsystem, not a fix, and the TTY
service is where it belongs when there is one.

## Consequences

The shell is charged two timer ticks over a twenty-second session instead of
7,174. The boot test asserts its total stays in three digits, so the spin cannot
come back unnoticed, and that it never reports dropped input.

`make test-latency` is unchanged in its command medians. The in-process
`bench vfs` figure recovered to its baseline, because the machine is no longer
spending most of its time in a process that is waiting.

Vector 36 has a gate and GSI 4 is routed and unmasked. The handler saves four
registers, drains, and returns; it takes no scheduling decision and touches
nothing the kernel loop owns.

`ConsoleRead` can no longer return byte three, and the shell's line editor no
longer sees one. Nothing else wanted a literal Ctrl-C.

Fixing the IOAPIC accessors also fixed a latent defect in the existing virtio
routing: it worked by luck rather than by construction, and the next device
added to that function would have hit the same corruption.
