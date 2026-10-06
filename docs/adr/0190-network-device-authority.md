# ADR 0190: Network device authority and IRQ waits

## Status

Accepted. Extends the device authority of ADR 0034 and ADR 0035 to a second
device, adds an IRQ interest to the multi-object wait of ADR 0051, and fixes
the launch path of ADR 0024 for consumed memory grants.

## Context

The kernel knew one PCI device. Discovery matched only the legacy virtio-blk
function and took its I/O BAR size on trust. GSI 11, its pending bit and its
single waiter belonged to the disk, and a driver could block on its line only
through `IRQWait`, which waits for nothing else. `WaitAny` refused IRQ
handles. Closing any DMA capability at process exit wrote zero to the disk's
status register, whichever driver had exited.

A network driver has to wait for two things at once: an interrupt from its
device and a message from the network service. Blocking in `IRQWait` would
leave the service's messages queued until the next frame arrived, and polling
the channel would spend CPU for nothing on an idle link. The kernel also had
to stay out of the protocol: frames, addresses and routes belong to a
userspace service (ADR 0191).

## Decision

### Discovery

`pci.FindLegacyVirtioNet` looks for the transitional virtio-net function,
vendor `1af4`, device `1000`. It walks every bus, slot and function and
refuses a second match with `ProblemDuplicate`: which of two NICs a driver
got would otherwise depend on enumeration order.

Both device searches now size their I/O BAR. Discovery reads BAR 0, writes
all ones, reads the result, and writes the original value back before
anything else reads it. A function that decodes only 16 address bits reads
the upper half as zero, and the probe treats that half as set. The BAR must
be I/O space in both readings, with a nonzero base, a power-of-two size of
at least 32 bytes (the legacy header and the start of the device
configuration), an aligned base, and a range that ends inside 16-bit port
space. Anything else is `ProblemBAR`. The interrupt register must give an
INTx pin from 1 to 4 and a line from 1 to 15, or the function is
`ProblemInterrupt`. Block discovery keeps returning the first block function
it finds, now with its probed size.

`discoverNetwork` runs after the disk's discovery and refuses, with a line on
the console, a function whose port range overlaps the disk's or whose line is
not one of 3, 5, 6, 9, 10, 12 and 14. The excluded lines are the disk's 11,
the consoles' 1 and 4, the PICs' spurious lines 7 and 15, and the timer,
cascade, RTC and FPU lines. A shared line would need a handler that asks
every device on it whether it interrupted, which this kernel does not have.
For an accepted function `pci.EnableCommand` sets I/O decoding and bus
mastering, clears the INTx disable bit, and keeps every other bit of the
command register. A missing or refused function leaves the system without a
network and changes nothing else.

`make run` and the network test place the NIC at PCI slot 4. The Q35
firmware routes that slot's INTA to line 10, which keeps it off the disk's 11.

### A vector and a lock

`amd64.ConfigureNetIRQ` routes the accepted line, active low and level
triggered as PCI INTx is, to vector `0x34` on the bootstrap processor. It
leaves the route masked; the driver's first wait unmasks it. The vector lies
outside the remapped PICs' range, so it cannot be mistaken for a legacy line.
The function runs once, before the application processors start, and writes
the route's IOAPIC selector and value where the assembly reads them.

`goos_irq_net` masks its route, sets a pending flag, counts the interrupt,
wakes the kernel loop in task slot 0, and acknowledges the LAPIC. It parses
nothing and touches no wait table. The kernel loop publishes the readiness
through the ordinary `WaitAny` table, as it does for console input
(ADR 0075). The driver's read of the device ISR deasserts the line.

An IOAPIC register access is two writes: a select, then data. The handlers
run on the bootstrap processor, while a driver on any processor can arm or
quiesce its route. `goos_ioapic_lock`, a spinlock taken with interrupts off,
now brackets every select and data pair: in the network handler and in its
arm and quiesce routines, and also in the IRQ 11 handler and its rearm and
quiesce routines, which had relied on running on one processor.

### Waiting on the line

`WaitAny` accepts a `TypeIRQ` handle with Wait for the readable event, and
only for the routed network line. The disk keeps `IRQWait`, which owns its
line's one waiter; a `WaitAny` interest on any other IRQ object is
`Unsupported`. Readiness is the pending flag. `WaitAny` reports it and does
not clear it.

The order inside a wait closes the window between check and sleep. The
kernel checks readiness, registers the waiter, unmasks the route, and checks
readiness again before the task blocks. An interrupt that fires as soon as
the route opens finds a registered waiter, and one that came before
registration shows up in the recheck.

`IRQTake` (operation 55, `sys.IRQTake`) consumes the pending event without
blocking. It needs the same handle and right, answers `NotFound` when nothing
is pending, and takes the device lock set that `IRQWait` takes. The driver
calls it after it reads the device ISR, so an interrupt raised after that
read stays pending for the next wait.

### DMA domains per device

A DMA capability names a domain, and a domain now names one discovered
device: `device.DMADomainBlock` is 1, `device.DMADomainNet` is 2.
`DMAAllocate` refuses a domain whose device was not discovered, before it
reads its descriptor. The quota and the fixed window of ADR 0107 are
unchanged.

When a process that holds a DMA capability exits, its handles close before
its address space goes, and closing the capability quiesces that domain's
device. For the disk that is IRQ 11 masked, then the status register at
offset 18 written to zero. For the network function the route is masked,
the status register reset, the route masked again and the pending flag
cleared, so the reset cannot leave an event behind. The DMA pages are freed
with the address space after that. A network driver's exit never writes a
disk port or touches IRQ 11.

### PID 1's handles

The kernel installs the network function's ports, line and DMA domain in
PID 1's table at slots `0x1c`, `0x1d` and `0x1e`, after the terminal console
handles and the second exec area, and panics if they land elsewhere. With a
usable function they carry Read, Write and Transfer on the ports, Wait and
Transfer on the line, and Write and Transfer on the DMA domain. Without one
the slots still exist and hold `device.InertObject` (ports and DMA) and
`device.InertIRQObject` (the line), which no call accepts and which carry no
Transfer. PID 1 reads its port slot with `HandleInfo` and starts a driver
only when that handle has Transfer.

### Consumed memory grants

A launch grant marked `Consume` closed PID 1's handle once the child had its
copy. For a memory object the child's copy takes a reference of its own, so
closing PID 1's handle without dropping its reference left the object
alive after the child had gone. The launch path now drops that reference
when it closes a consumed memory handle. A handle moved over IPC takes no
reference and drops none. The network test found this: PID 1's page count
grew by six on every driver restart, because the two frame pools it created
for the old driver were never freed.

## Consequences

`make test-network` boots with the NIC at slot 4 and gets frames
through the driver, kills the driver twice, and requires PID 1's, the
service's and the new driver's resource counts after the second restart to
equal those after the first. The disk keeps working across both kills, and a
file written before the first reads back after it. A boot with no NIC
reaches normal mode with storage and the shell. `kernel/pci` host tests
cover discovery independent of the disk, a second function, each BAR and
interrupt refusal, the 16-bit BAR, overlap, and the command bits;
`kernel/kcall` checks operation 55. The register, unmask and recheck order
and the teardown order have no host test of their own; the VM gate
exercises both.

Limits of this version:

- one network function, and only legacy virtio-net;
- no shared interrupt lines, and the line must be one of the seven above;
- no MSI or MSI-X, and every device interrupt goes to the bootstrap
  processor;
- the network line is the only IRQ `WaitAny` accepts.

There is no IOMMU. The network driver programs a bus-mastering device and
can make it write any physical address, as the block driver can, so a
malicious network driver can corrupt the kernel. The capability scoping
above contains accidental faults and removes ambient port access. Until an
IOMMU confines DMA the network driver is part of the effective trusted
computing base.
