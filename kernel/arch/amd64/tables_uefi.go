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
	exceptionCount  = 32
	timerVector     = 32
	virtioIRQVector = 43
	userCallVector  = 0x80
	tssBytes        = 104
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
var interruptTable [exceptionCount + 3]uintptr

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
		idt[vector] = NewIDTEntry(interruptTable[vector], attributes)
	}
	if interruptTable[timerVector] == 0 {
		return false
	}
	idt[timerVector] = NewIDTEntry(interruptTable[timerVector], interruptGate)
	if interruptTable[exceptionCount+2] == 0 {
		return false
	}
	idt[virtioIRQVector] = NewIDTEntry(interruptTable[exceptionCount+2], interruptGate)
	if interruptTable[exceptionCount+1] == 0 {
		return false
	}
	idt[userCallVector] = NewIDTEntry(interruptTable[exceptionCount+1], userCallGate)

	allocation, ok := allocator.AllocateContiguous(kernelStackPages + stackGuardPages)
	if !ok || !unmapGuardPage(pageTable, allocation) {
		return false
	}
	rsp0 := allocation + (kernelStackPages+stackGuardPages)*pmm.PageSize - 64
	putUint64(tss[4:12], rsp0)
	setTSSStackAddress(&tss[4])
	tss[102], tss[103] = byte(tssBytes), 0
	// SAFETY: tss is static kernel storage with a stable address for the entire
	// lifetime of the loaded task-register descriptor.
	gdt[5], gdt[6] = tssDescriptor(uintptr(unsafe.Pointer(&tss[0])), tssBytes-1)
	loadGDT(&gdt[0], uint16(len(gdt)*8-1))
	loadIDT(&idt[0], uint16(len(idt)*16-1))
	return true
}

// ConfigureVirtioIRQ maps the architectural APIC pages and routes GSI 11 to
// vector 43 as an active-low, level-triggered interrupt on bootstrap CPU 0.
func ConfigureVirtioIRQ(pageTable uint64) bool {
	if !vm.MapKernelDevicePage(pageTable, uint64(lapicVirtual), 0xfee00000) ||
		!vm.MapKernelDevicePage(pageTable, uint64(ioapicVirtual), 0xfec00000) {
		return false
	}
	invalidatePage(uint64(lapicVirtual))
	invalidatePage(uint64(ioapicVirtual))
	mmioWrite32(lapicVirtual+0xf0, mmioRead32(lapicVirtual+0xf0)|1<<8|0xff)
	ioapicWrite(0x10+2*11+1, 0)
	ioapicWrite(0x10+2*11, uint32(virtioIRQVector)|1<<13|1<<15)
	return true
}

func ioapicWrite(register uint32, value uint32) {
	mmioWrite32(ioapicVirtual, register)
	mmioWrite32(ioapicVirtual+0x10, value)
}

func mmioRead32(address uintptr) uint32         { return *(*uint32)(unsafe.Pointer(address)) }
func mmioWrite32(address uintptr, value uint32) { *(*uint32)(unsafe.Pointer(address)) = value }

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

// StartBootstrapTimer remaps the legacy PIC, unmasks IRQ0, configures the PIT
// for 100 Hz, and enables maskable interrupts.
//
//export goos_start_bootstrap_timer
func StartBootstrapTimer()

// TimerTicks returns the number of IRQ0 deliveries observed by the kernel.
//
//export goos_timer_ticks_read
func TimerTicks() uint64

// KernelCallArg2 returns the third native-call argument captured by the
// interrupt entry path. The bootstrap kernel has one CPU.
//
//export goos_kernel_call_arg2_read
func KernelCallArg2() uint64

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
