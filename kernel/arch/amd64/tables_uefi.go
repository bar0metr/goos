//go:build uefi && amd64

package amd64

import (
	"goos/kernel/mm/pmm"
	"goos/kernel/mm/vm"
	"unsafe"
)

const (
	lapicVirtual  = uintptr(0x3fffd000)
	ioapicVirtual = uintptr(0x3fffe000)
)

const (
	exceptionCount    = 32
	timerVector       = 32
	keyboardIRQVector = 33
	// GSI 4 is the legacy UART line. Vector numbering follows the IOAPIC
	// convention this bootstrap already uses for GSI 11.
	serialIRQVector = 36
	// The PICs' spurious vectors, IRQ 7 and IRQ 15 after the remap.
	picSpuriousMaster = 39
	picSpuriousSlave  = 47
	virtioIRQVector   = 43
	// localTimerVector is every processor's own preemption tick. The three
	// vectors after it are interrupts one processor sends another: look at
	// the run queue, drop translations, and stop after a panic.
	localTimerVector = 0x30
	rescheduleVector = 0x31
	shootdownVector  = 0x32
	stopVector       = 0x33
	// netIRQVector is the network line's, whichever ISA line it is: a vector
	// outside the remapped PICs' range cannot be confused with theirs.
	netIRQVector       = 0x34
	userCallVector     = 0x80
	yieldVector        = 0x81
	apicSpuriousVector = 0xff
	tssBytes           = 104
	// tssStride spaces the processors' TSSs; each fits in its stride.
	tssStride = 128
	// tssIST1 is the offset of the first interrupt stack pointer in the TSS.
	tssIST1           = 36
	doubleFaultVector = 8
	// gdtFixed is the null, kernel code and data, and user data and code
	// descriptors. Processor n's 16-byte TSS descriptor follows at index
	// gdtFixed+2n, selector tssSelector+16n.
	gdtFixed = 5
)

// Positions of the handlers after the exceptions in goos_interrupt_table.
const (
	handlerTimer = exceptionCount + iota
	handlerUserCall
	handlerVirtio
	handlerYield
	handlerSerial
	handlerKeyboard
	handlerLocalTimer
	handlerReschedule
	handlerShootdown
	handlerStop
	handlerNet
	handlerSpurious
	handlerCount
)

var (
	gdt = [gdtFixed + 2*MaxCPUs]uint64{
		0,
		0x00af9a000000ffff,
		0x00af92000000ffff,
		0x00cff2000000ffff,
		0x00affa000000ffff,
	}
	idt [256]IDTEntry
	// tss holds every processor's TSS. RSP0 names the kernel-entry stack of
	// the task the processor runs; IST1 its own double-fault stack.
	tss [MaxCPUs][tssStride]byte
)

//go:extern goos_interrupt_table
var interruptTable [handlerCount]uintptr

//go:extern goos_linux_entry_mapping_table
var linuxEntryMappingTable [3]uintptr

//go:extern goos_smp_ready
var smpReady uint64

// EntryProbeAddress returns one mapped higher-half trampoline address for
// supervisor-isolation validation.
func EntryProbeAddress() uint64 {
	address, _ := vm.KernelAlias(uint64(interruptTable[handlerUserCall]))
	return address
}

// gates lists the IDT vectors past the exceptions and their handlers.
var gates = [...]struct {
	vector  uint8
	handler int
	user    bool
}{
	{timerVector, handlerTimer, false},
	{keyboardIRQVector, handlerKeyboard, false},
	{serialIRQVector, handlerSerial, false},
	{picSpuriousMaster, handlerSpurious, false},
	{virtioIRQVector, handlerVirtio, false},
	{picSpuriousSlave, handlerSpurious, false},
	{localTimerVector, handlerLocalTimer, false},
	{rescheduleVector, handlerReschedule, false},
	{shootdownVector, handlerShootdown, false},
	{stopVector, handlerStop, false},
	{netIRQVector, handlerNet, false},
	{userCallVector, handlerUserCall, true},
	{yieldVector, handlerYield, false},
	{apicSpuriousVector, handlerSpurious, false},
}

// InstallDescriptorTables replaces firmware GDT/IDT state on the bootstrap
// processor. All architectural exception vectors receive a gate; vector 3 is
// deliberately callable from ring 3 for debugger-compatible breakpoints. The
// IDT is shared by every processor: its gates never change after boot, and
// every handler finds its processor through the task register.
func InstallDescriptorTables(allocator *pmm.Allocator, pageTable uint64) bool {
	for vector := 0; vector < exceptionCount; vector++ {
		attributes := interruptGate
		if vector == 3 {
			attributes = breakpointGate
		}
		if interruptTable[vector] == 0 {
			return false
		}
		handler, ok := vm.KernelAlias(uint64(interruptTable[vector]))
		if !ok {
			return false
		}
		idt[vector] = NewIDTEntry(uintptr(handler), attributes)
	}
	for index := range gates {
		gate := gates[index]
		if interruptTable[gate.handler] == 0 {
			return false
		}
		handler, ok := vm.KernelAlias(uint64(interruptTable[gate.handler]))
		if !ok {
			return false
		}
		attributes := interruptGate
		if gate.user {
			attributes = userCallGate
		}
		idt[gate.vector] = NewIDTEntry(uintptr(handler), attributes)
	}
	// A double fault switches to the TSS's first interrupt stack, so it can be
	// reported when the stack that faulted has run into its guard (ADR 0091).
	idt[doubleFaultVector].IST = 1

	allocation, ok := allocator.AllocateContiguous(kernelStackPages + stackGuardPages)
	if !ok || !unmapGuardPages(pageTable, allocation) {
		return false
	}
	rsp0, ok := vm.KernelAlias(allocation + (kernelStackPages+stackGuardPages)*pmm.PageSize - 64)
	if !ok || !prepareCPUTables(allocator, pageTable, 0, rsp0) {
		return false
	}
	return loadCPUTables(0)
}

// prepareCPUTables gives processor cpu its TSS, with rsp0 as the initial
// kernel-entry stack and a fresh guarded double-fault stack, and the GDT
// descriptor that names it.
func prepareCPUTables(allocator *pmm.Allocator, pageTable uint64, cpu int, rsp0 uint64) bool {
	if cpu < 0 || cpu >= MaxCPUs {
		return false
	}
	fault, ok := allocator.AllocateContiguous(faultStackPages + stackGuardPages)
	if !ok || !unmapGuardPages(pageTable, fault) {
		return false
	}
	faultTop, ok := vm.KernelAlias(fault + (faultStackPages+stackGuardPages)*pmm.PageSize - 64)
	if !ok {
		return false
	}
	segment := &tss[cpu]
	for index := range segment {
		segment[index] = 0
	}
	putUint64(segment[4:12], rsp0)
	putUint64(segment[tssIST1:tssIST1+8], faultTop)
	segment[102], segment[103] = byte(tssBytes), 0
	tssBase, ok := vm.KernelAlias(uint64(uintptr(unsafe.Pointer(&segment[0]))))
	if !ok {
		return false
	}
	cpuData[cpu][cpuTSSRSP0] = tssBase + 4
	gdt[gdtFixed+2*cpu], gdt[gdtFixed+2*cpu+1] = tssDescriptor(uintptr(tssBase), tssBytes-1)
	return true
}

// loadCPUTables loads the GDT with processor cpu's task register and the IDT
// on the calling processor.
func loadCPUTables(cpu int) bool {
	gdtBase, gdtOK := vm.KernelAlias(uint64(uintptr(unsafe.Pointer(&gdt[0]))))
	idtBase, idtOK := vm.KernelAlias(uint64(uintptr(unsafe.Pointer(&idt[0]))))
	if !gdtOK || !idtOK {
		return false
	}
	loadGDT((*uint64)(unsafe.Pointer(uintptr(gdtBase))), uint16(len(gdt)*8-1), tssSelector+uint16(16*cpu))
	loadIDT((*IDTEntry)(unsafe.Pointer(uintptr(idtBase))), uint16(len(idt)*16-1))
	return true
}

// InstallLinuxSyscallEntry enables the x86_64 SYSCALL instruction and binds it
// to the supervisor-only higher-half trampoline. User GS stays zero; SWAPGS
// selects the mapped entry-data page until the IRETQ return prefix swaps back.
func InstallLinuxSyscallEntry() bool {
	if linuxEntryMappingTable[0] == 0 || linuxEntryMappingTable[1] == 0 || linuxEntryMappingTable[2] == 0 {
		return false
	}
	return installLinuxSyscallFor(0)
}

// installLinuxSyscallFor binds SYSCALL on the calling processor, which is
// processor cpu, with its own entry block behind KERNEL_GS_BASE.
func installLinuxSyscallFor(cpu int) bool {
	entry, entryOK := vm.KernelAlias(uint64(linuxEntryMappingTable[0]))
	data, dataOK := vm.KernelAlias(uint64(uintptr(unsafe.Pointer(&linuxEntryData[cpu][0]))))
	return entryOK && dataOK && installLinuxSyscall(entry, data, linuxSTAR, linuxFMASK)
}

//export goos_install_linux_syscall
func installLinuxSyscall(entry uint64, entryData uint64, star uint64, fmask uint64) bool

// ConfigureDeviceIRQs maps the architectural APIC pages and routes virtio and
// both legacy console-input lines to the bootstrap processor, whatever its
// APIC ID. Device interrupts stay there when other processors start; a
// handler that wakes a task elsewhere interrupts that processor.
func ConfigureDeviceIRQs(pageTable uint64) bool {
	if !vm.MapKernelDevicePage(pageTable, uint64(lapicVirtual), 0xfee00000) ||
		!vm.MapKernelDevicePage(pageTable, uint64(ioapicVirtual), 0xfec00000) {
		return false
	}
	invalidatePage(uint64(lapicVirtual))
	invalidatePage(uint64(ioapicVirtual))
	enableLocalAPIC()
	destination := lapicRead(lapicIDRegister) & 0xff000000
	cpuAPIC[0] = destination >> 24
	ioapicWrite(0x10+2*11+1, destination)
	ioapicWrite(0x10+2*11, uint32(virtioIRQVector)|1<<13|1<<15)
	ioapicWrite(0x10+2*4+1, destination)
	ioapicWrite(0x10+2*4, uint32(serialIRQVector))
	ioapicWrite(0x10+2*1+1, destination)
	ioapicWrite(0x10+2*1, uint32(keyboardIRQVector))
	smpReady = 1
	return true
}

//go:extern goos_net_irq_select
var netIRQSelect uint64

//go:extern goos_net_irq_route
var netIRQRoute uint64

// The IOAPIC redirection bits the device routes use: level-triggered,
// active-low as the PCI INTx lines are, and masked.
const (
	routeActiveLow = 1 << 13
	routeLevel     = 1 << 15
	routeMasked    = 1 << 16
)

// ConfigureNetIRQ routes ISA line to the network vector on the bootstrap
// processor, masked: the driver's first wait unmasks it. It runs once, on the
// boot processor, before any other processor starts, so the asm state it
// writes is published before a handler or an arm reads it.
func ConfigureNetIRQ(line uint8) bool {
	if line == 0 || line >= 16 || netIRQSelect != 0 {
		return false
	}
	selector := uint32(0x10 + 2*uint32(line))
	route := uint32(netIRQVector) | routeActiveLow | routeLevel
	ioapicWrite(selector+1, cpuAPIC[0]<<24)
	ioapicWrite(selector, route|routeMasked)
	netIRQRoute = uint64(route)
	netIRQSelect = uint64(selector)
	return true
}

// NetIRQCount reports how many network interrupts have been taken.
//
//go:extern goos_net_irq_count
var netIRQCount uint64

func NetIRQCount() uint64 { return netIRQCount }

// TakeNetIRQ consumes the network line's pending event.
//
//export goos_take_net_irq
func TakeNetIRQ() bool

// NetIRQPending reports the pending event without consuming it.
//
//export goos_net_irq_is_pending
func NetIRQPending() bool

// RearmNetIRQ unmasks the network route as its driver begins a wait.
//
//export goos_rearm_net_irq
func RearmNetIRQ()

// QuiesceNetIRQ masks the network route and forgets its pending event.
//
//export goos_quiesce_net_irq
func QuiesceNetIRQ()

func ioapicWrite(register uint32, value uint32) {
	mmioWrite32(ioapicVirtual, register)
	mmioWrite32(ioapicVirtual+0x10, value)
}

// Device registers are reached through assembly. As Go, these were ordinary
// memory operations that the optimiser was free to reorder, merge or discard,
// and an IOAPIC access is a register select followed by a data transfer that
// must reach the device in that order and exactly once.
//
//export goos_mmio_read32
func mmioRead32(address uintptr) uint32

//export goos_mmio_write32
func mmioWrite32(address uintptr, value uint32)

func putUint64(target []byte, value uint64) {
	for index := 0; index < 8; index++ {
		target[index] = byte(value >> uint(index*8))
	}
}

// TriggerBreakpoint executes INT3 to validate the installed exception path.
//
//export goos_trigger_breakpoint
func TriggerBreakpoint()

//export goos_load_gdt
func loadGDT(base *uint64, limit uint16, selector uint16)

//export goos_load_idt
func loadIDT(base *IDTEntry, limit uint16)

// StartBootstrapTimer remaps the legacy PIC, unmasks IRQ0, programs the PIT
// with the given reload value, and enables maskable interrupts.
//
//export goos_start_bootstrap_timer
func StartBootstrapTimer(divisor uint16)

// TimerTicks returns the number of IRQ0 deliveries observed by the kernel.
//
//export goos_timer_ticks_read
func TimerTicks() uint64

// KernelCallArg2 returns the third native-call argument, which the entry path
// leaves in the calling processor's data block.
func KernelCallArg2() uint64 { return cpuData[CPUIndex()][cpuCallArg2] }

// The input rings: COM1's and the PS/2 keyboard's, one per console
// (ADR 0125).
const (
	InputSerial   = uint64(0)
	InputKeyboard = uint64(1)
)

// InputTake removes one byte from an input ring. The result carries the byte
// in its low eight bits with bit eight set, or zero when the ring is empty, so
// one return value distinguishes an empty ring from a NUL byte.
//
//export goos_input_take
func InputTake(input uint64) uint64

// InputPending reports whether an input ring holds a byte or an interrupt.
//
//export goos_input_pending
func InputPending(input uint64) bool

// InputTakeInterrupt consumes a ring's pending interrupt. Ctrl-C never enters
// the ring, so asking whether it was typed does not consume the bytes a
// running command is owed.
//
//export goos_input_take_interrupt
func InputTakeInterrupt(input uint64) bool

// InputDropped counts bytes that arrived with a ring already full.
//
//export goos_input_dropped
func InputDropped(input uint64) uint64

// PS2KeyboardEventTake consumes the validation indication set only after an
// IRQ1 byte or interrupt has entered the logical console path.
//
//export goos_ps2_keyboard_event_take
func PS2KeyboardEventTake() bool

// EnableConsoleInterrupt turns on the UART's received-data-available interrupt.
// It must run after the IDT gate for the serial vector exists, or the first
// byte would arrive with no handler installed.
//
//export goos_enable_console_interrupt
func EnableConsoleInterrupt()

// TakeIRQ11 consumes one pending device interrupt.
//
//export goos_take_irq_11
func TakeIRQ11() bool

// SetIRQ11Waiter records a one-based scheduler slot for the next IRQ11.
//
//export goos_set_irq_11_waiter
func SetIRQ11Waiter(slot uint64)

// RearmIRQ11 unmasks the IOAPIC route when a driver begins its next wait.
//
//export goos_rearm_irq_11
func RearmIRQ11()

// QuiesceIRQ11 masks the route and forgets pending/waiter state during driver teardown.
//
//export goos_quiesce_irq_11
func QuiesceIRQ11()
