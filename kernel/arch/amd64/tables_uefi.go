//go:build uefi && amd64

package amd64

import (
	"goos/kernel/mm/pmm"
	"unsafe"
)

const (
	exceptionCount = 32
	timerVector    = 32
	userCallVector = 0x80
	tssBytes       = 104
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
var interruptTable [exceptionCount + 2]uintptr

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

//export goos_set_tss_stack_address
func setTSSStackAddress(address *byte)
