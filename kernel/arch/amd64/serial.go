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
