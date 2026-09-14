//go:build uefi && amd64

package amd64

const (
	com1Base       = uint16(0x3f8)
	lineStatusPort = com1Base + 5
	transmitReady  = byte(0x20)
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

// EnableNXAndWriteProtect enables EFER.NXE and CR0.WP. After this call the CPU
// enforces both non-executable mappings and supervisor read-only mappings.
//
//export goos_enable_nx_wp
func EnableNXAndWriteProtect()

// LoadPageTable installs the physical PML4 address in CR3.
//
//export goos_load_cr3
func LoadPageTable(address uint64)

// StackPointer returns the current stack pointer for bootstrap validation.
//
//export goos_read_rsp
func StackPointer() uint64
