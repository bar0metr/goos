//go:build uefi && amd64

package amd64

const (
	ps2DataPort       = uint16(0x60)
	ps2StatusPort     = uint16(0x64)
	ps2CommandPort    = uint16(0x64)
	ps2OutputFull     = byte(1 << 0)
	ps2InputFull      = byte(1 << 1)
	ps2ResponseACK    = byte(0xfa)
	ps2WaitIterations = 100000
)

// InitPS2Keyboard performs a bounded i8042 first-port setup. Translation is
// enabled so the interrupt path receives scan-code set 1. Failure leaves the
// graphical console output-only and does not affect serial input.
func InitPS2Keyboard() bool {
	if !ps2WriteCommand(0xad) { // Disable the first port while configuring it.
		return false
	}
	ps2DrainOutput()
	if !ps2WriteCommand(0x20) {
		return false
	}
	configuration, ok := ps2ReadData()
	if !ok {
		return false
	}
	configuration |= 1 << 0 // IRQ1.
	configuration |= 1 << 6 // Translate the device stream to set 1.
	configuration &^= 1 << 4
	if !ps2WriteCommand(0x60) || !ps2WriteData(configuration) || !ps2WriteCommand(0xae) {
		return false
	}
	ps2DrainOutput()
	if !ps2WriteData(0xf4) { // Enable keyboard scanning.
		return false
	}
	response, ok := ps2ReadData()
	return ok && response == ps2ResponseACK
}

func ps2DrainOutput() {
	for attempts := 0; attempts < 256 && inb(ps2StatusPort)&ps2OutputFull != 0; attempts++ {
		_ = inb(ps2DataPort)
	}
}

func ps2WriteCommand(command byte) bool {
	if !ps2WaitInputEmpty() {
		return false
	}
	outb(ps2CommandPort, command)
	return true
}

func ps2WriteData(value byte) bool {
	if !ps2WaitInputEmpty() {
		return false
	}
	outb(ps2DataPort, value)
	return true
}

func ps2WaitInputEmpty() bool {
	for attempts := 0; attempts < ps2WaitIterations; attempts++ {
		if inb(ps2StatusPort)&ps2InputFull == 0 {
			return true
		}
		cpuPause()
	}
	return false
}

func ps2ReadData() (byte, bool) {
	for attempts := 0; attempts < ps2WaitIterations; attempts++ {
		if inb(ps2StatusPort)&ps2OutputFull != 0 {
			return inb(ps2DataPort), true
		}
		cpuPause()
	}
	return 0, false
}
