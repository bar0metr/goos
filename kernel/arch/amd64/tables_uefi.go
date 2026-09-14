//go:build uefi && amd64

package amd64

const (
	exceptionCount = 32
	timerVector    = 32
)

var (
	gdt = [3]uint64{
		0,
		0x00af9a000000ffff,
		0x00af92000000ffff,
	}
	idt [256]IDTEntry
)

//go:extern goos_interrupt_table
var interruptTable [exceptionCount + 1]uintptr

// InstallDescriptorTables replaces firmware GDT/IDT state. All architectural
// exception vectors receive a gate; vector 3 is deliberately callable from
// ring 3 for debugger-compatible breakpoints.
func InstallDescriptorTables() bool {
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
	loadGDT(&gdt[0], uint16(len(gdt)*8-1))
	loadIDT(&idt[0], uint16(len(idt)*16-1))
	return true
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
