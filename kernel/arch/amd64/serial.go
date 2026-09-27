//go:build uefi && amd64

package amd64

const (
	com1Base       = uint16(0x3f8)
	lineStatusPort = com1Base + 5
	transmitReady  = byte(0x20)
	receiveReady   = byte(0x01)
)

// COM1 is the first legacy UART. QEMU exposes it as the serial console.
type COM1 struct{}

func NewCOM1() COM1 { return COM1{} }

func (COM1) Init() {
	outb(com1Base+1, 0x00)
	outb(com1Base+3, 0x80)
	outb(com1Base, 0x03)
	outb(com1Base+1, 0x00)
	outb(com1Base+3, 0x03)
	outb(com1Base+2, 0xc7)
	outb(com1Base+4, 0x0b)
}

func (COM1) WriteByte(value byte) {
	for inb(lineStatusPort)&transmitReady == 0 {
		cpuPause()
	}
	outb(com1Base, value)
}

func (COM1) TryReadByte() (byte, bool) {
	if inb(lineStatusPort)&receiveReady == 0 {
		return 0, false
	}
	return inb(com1Base), true
}

func (serial COM1) WriteString(value string) {
	for index := 0; index < len(value); index++ {
		if value[index] == '\n' {
			serial.WriteByte('\r')
		}
		serial.WriteByte(value[index])
	}
}

func (serial COM1) WriteUint(value uint64) {
	var digits [20]byte
	position := len(digits)
	if value == 0 {
		serial.WriteByte('0')
		return
	}
	for value != 0 {
		position--
		digits[position] = byte(value%10) + '0'
		value /= 10
	}
	for ; position < len(digits); position++ {
		serial.WriteByte(digits[position])
	}
}

func (serial COM1) WriteHex(value uint64) {
	const digits = "0123456789abcdef"
	serial.WriteString("0x")
	started := false
	for shift := 60; shift >= 0; shift -= 4 {
		nibble := byte(value >> uint(shift) & 0xf)
		if nibble != 0 || started || shift == 0 {
			started = true
			serial.WriteByte(digits[nibble])
		}
	}
}

// The following declarations enter the only privileged assembly in the
// bootstrap. Ports are compile-time-selected by this package, and callers do
// not receive raw memory or kernel pointers.
//
//export goos_outb
func outb(port uint16, value byte)

//export goos_inb
func inb(port uint16) byte

//export goos_outw
func outw(port uint16, value uint16)

//export goos_inw
func inw(port uint16) uint16

//export goos_outl
func outl(port uint16, value uint32)

//export goos_inl
func inl(port uint16) uint32

func PortRead(port uint16, width uint8) (uint32, bool) {
	switch width {
	case 1:
		return uint32(inb(port)), true
	case 2:
		return uint32(inw(port)), true
	case 4:
		return inl(port), true
	default:
		return 0, false
	}
}

func PortWrite(port uint16, width uint8, value uint32) bool {
	switch width {
	case 1:
		outb(port, byte(value))
	case 2:
		outw(port, uint16(value))
	case 4:
		outl(port, value)
	default:
		return false
	}
	return true
}

func PCIConfigRead32(bus, slot, function, offset uint8) uint32 {
	address := uint32(1<<31) | uint32(bus)<<16 | uint32(slot&0x1f)<<11 | uint32(function&7)<<8 | uint32(offset&0xfc)
	outl(0xcf8, address)
	return inl(0xcfc)
}

func PCIConfigWrite16(bus, slot, function, offset uint8, value uint16) {
	address := uint32(1<<31) | uint32(bus)<<16 | uint32(slot&0x1f)<<11 | uint32(function&7)<<8 | uint32(offset&0xfc)
	outl(0xcf8, address)
	outw(0xcfc+uint16(offset&2), value)
}

//export goos_pause
func cpuPause()

// Halt waits for an interrupt without consuming a core. Interrupts remain in
// the state established by firmware until the kernel installs its own IDT.
//
//export goos_halt
func Halt()

// HasNX reports whether the CPU implements the execute-disable page-table bit.
//
//export goos_has_nx
func HasNX() bool

// EnableNXAndWriteProtect enables EFER.NXE, CR0.WP, and CR4.OSFXSR/OSXMMEXCPT.
// After this call the CPU enforces non-executable and supervisor read-only
// mappings, and FXSAVE covers the SSE registers the scheduler switches.
//
//export goos_enable_nx_wp
func EnableNXAndWriteProtect()

// SystemReset restarts the machine through the chipset reset control register,
// then the keyboard controller, then a forced triple fault.
//
//export goos_system_reset
func SystemReset()

// LoadPageTable installs the physical PML4 address in CR3.
//
//export goos_load_cr3
func LoadPageTable(address uint64)

// SetKernelPageTable gives interrupt entry code the root to install before it
// touches kernel state while returning from ring 3.
//
//export goos_set_kernel_root
func SetKernelPageTable(address uint64)

// UserCallEntryCount reports the number of ring-3 native calls observed by
// the low handler after the entry trampoline changed CR3.
//
//export goos_user_call_entry_count
func UserCallEntryCount() uint64

// UserCallEntryDrift reports how often a task entered the low native-call
// handler with an RSP different from its first observed entry.
//
//export goos_user_call_entry_drift
func UserCallEntryDrift() uint64

// StackPointer returns the current stack pointer for bootstrap validation.
//
//export goos_read_rsp
func StackPointer() uint64

// ProbeAddressSpaces reads one virtual byte through each supplied CR3 and
// returns A in bits 0..7 and B in bits 8..15.
//
//export goos_probe_address_spaces
func ProbeAddressSpaces(rootA uint64, rootB uint64, kernelRoot uint64, address uint64) uint64
