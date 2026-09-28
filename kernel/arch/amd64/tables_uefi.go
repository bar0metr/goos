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
	virtioIRQVector = 43
	userCallVector  = 0x80
	yieldVector     = 0x81
	tssBytes        = 104
	// tssIST1 is the offset of the first interrupt stack pointer in the TSS.
	tssIST1           = 36
	doubleFaultVector = 8
)

var (
	gdt = [7]uint64{
		0,
		0x00af9a000000ffff,
		0x00af92000000ffff,
		0x00cff2000000ffff,
		0x00affa000000ffff,
		0,
		0,
	}
	idt [256]IDTEntry
	tss [tssBytes]byte
)

//go:extern goos_interrupt_table
var interruptTable [exceptionCount + 6]uintptr

// EntryProbeAddress returns one mapped higher-half trampoline address for
// supervisor-isolation validation.
func EntryProbeAddress() uint64 {
	address, _ := vm.KernelAlias(uint64(interruptTable[exceptionCount+1]))
	return address
}

// InstallDescriptorTables replaces firmware GDT/IDT state. All architectural
// exception vectors receive a gate; vector 3 is deliberately callable from
// ring 3 for debugger-compatible breakpoints.
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
	if interruptTable[timerVector] == 0 {
		return false
	}
	timer, ok := vm.KernelAlias(uint64(interruptTable[timerVector]))
	if !ok {
		return false
	}
	idt[timerVector] = NewIDTEntry(uintptr(timer), interruptGate)
	if interruptTable[exceptionCount+2] == 0 {
		return false
	}
	virtio, ok := vm.KernelAlias(uint64(interruptTable[exceptionCount+2]))
	if !ok {
		return false
	}
	idt[virtioIRQVector] = NewIDTEntry(uintptr(virtio), interruptGate)
	if interruptTable[exceptionCount+1] == 0 {
		return false
	}
	userCall, ok := vm.KernelAlias(uint64(interruptTable[exceptionCount+1]))
	if !ok {
		return false
	}
	idt[userCallVector] = NewIDTEntry(uintptr(userCall), userCallGate)
	if interruptTable[exceptionCount+3] == 0 {
		return false
	}
	yield, ok := vm.KernelAlias(uint64(interruptTable[exceptionCount+3]))
	if !ok {
		return false
	}
	idt[yieldVector] = NewIDTEntry(uintptr(yield), interruptGate)
	if interruptTable[exceptionCount+4] == 0 {
		return false
	}
	serial, ok := vm.KernelAlias(uint64(interruptTable[exceptionCount+4]))
	if !ok {
		return false
	}
	idt[serialIRQVector] = NewIDTEntry(uintptr(serial), interruptGate)
	if interruptTable[exceptionCount+5] == 0 {
		return false
	}
	keyboard, ok := vm.KernelAlias(uint64(interruptTable[exceptionCount+5]))
	if !ok {
		return false
	}
	idt[keyboardIRQVector] = NewIDTEntry(uintptr(keyboard), interruptGate)

	allocation, ok := allocator.AllocateContiguous(kernelStackPages + stackGuardPages)
	if !ok || !unmapGuardPages(pageTable, allocation) {
		return false
	}
	rsp0, ok := vm.KernelAlias(allocation + (kernelStackPages+stackGuardPages)*pmm.PageSize - 64)
	if !ok {
		return false
	}
	putUint64(tss[4:12], rsp0)
	// A double fault switches to the TSS's first interrupt stack, so it can be
	// reported when the stack that faulted has run into its guard (ADR 0091).
	fault, ok := allocator.AllocateContiguous(faultStackPages + stackGuardPages)
	if !ok || !unmapGuardPages(pageTable, fault) {
		return false
	}
	faultTop, ok := vm.KernelAlias(fault + (faultStackPages+stackGuardPages)*pmm.PageSize - 64)
	if !ok {
		return false
	}
	putUint64(tss[tssIST1:tssIST1+8], faultTop)
	idt[doubleFaultVector].IST = 1
	tssBase, ok := vm.KernelAlias(uint64(uintptr(unsafe.Pointer(&tss[0]))))
	if !ok {
		return false
	}
	setTSSStackAddress((*byte)(unsafe.Pointer(uintptr(tssBase + 4))))
	tss[102], tss[103] = byte(tssBytes), 0
	gdt[5], gdt[6] = tssDescriptor(uintptr(tssBase), tssBytes-1)
	gdtBase, gdtOK := vm.KernelAlias(uint64(uintptr(unsafe.Pointer(&gdt[0]))))
	idtBase, idtOK := vm.KernelAlias(uint64(uintptr(unsafe.Pointer(&idt[0]))))
	if !gdtOK || !idtOK {
		return false
	}
	loadGDT((*uint64)(unsafe.Pointer(uintptr(gdtBase))), uint16(len(gdt)*8-1))
	loadIDT((*IDTEntry)(unsafe.Pointer(uintptr(idtBase))), uint16(len(idt)*16-1))
	return true
}

// ConfigureDeviceIRQs maps the architectural APIC pages and routes virtio and
// both legacy console-input lines on bootstrap CPU 0.
func ConfigureDeviceIRQs(pageTable uint64) bool {
	if !vm.MapKernelDevicePage(pageTable, uint64(lapicVirtual), 0xfee00000) ||
		!vm.MapKernelDevicePage(pageTable, uint64(ioapicVirtual), 0xfec00000) {
		return false
	}
	invalidatePage(uint64(lapicVirtual))
	invalidatePage(uint64(ioapicVirtual))
	mmioWrite32(lapicVirtual+0xf0, mmioRead32(lapicVirtual+0xf0)|1<<8|0xff)
	ioapicWrite(0x10+2*11+1, 0)
	ioapicWrite(0x10+2*11, uint32(virtioIRQVector)|1<<13|1<<15)
	ioapicWrite(0x10+2*4+1, 0)
	ioapicWrite(0x10+2*4, uint32(serialIRQVector))
	ioapicWrite(0x10+2*1+1, 0)
	ioapicWrite(0x10+2*1, uint32(keyboardIRQVector))
	return true
}

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
func loadGDT(base *uint64, limit uint16)

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

// KernelCallArg2 returns the third native-call argument captured by the
// interrupt entry path. The bootstrap kernel has one CPU.
//
//export goos_kernel_call_arg2_read
func KernelCallArg2() uint64

// ConsoleTake removes one byte from the console input ring. The result carries
// the byte in its low eight bits with bit eight set, or zero when the ring is
// empty, so one return value distinguishes an empty ring from a NUL byte.
//
//export goos_console_take
func ConsoleTake() uint64

// ConsolePending reports whether the console ring holds a byte.
//
//export goos_console_pending
func ConsolePending() bool

// ConsoleTakeInterrupt consumes a pending console interrupt. Ctrl-C never
// enters the input ring, so asking whether it was typed does not consume the
// bytes a running command is owed.
//
//export goos_console_take_interrupt
func ConsoleTakeInterrupt() bool

// ConsoleDropped counts bytes that arrived with the ring already full.
//
//export goos_console_dropped_read
func ConsoleDropped() uint64

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

//export goos_set_tss_stack_address
func setTSSStackAddress(address *byte)
