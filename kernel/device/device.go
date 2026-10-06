package device

// Fixed device objects. Kind-encoded objects below use the top byte, so these
// small values cannot collide with them.
const (
	ConsoleObject       = uint64(1)
	SystemControlObject = uint64(2)
	// LogObject authorizes reading the kernel log ring. It is separate from the
	// console so that the right to read the log is not the right to read what
	// is typed, which belongs to the terminal service alone (ADR 0082).
	LogObject = uint64(3)
	// LoaderObject authorizes creating executable objects, which a launch can
	// name instead of an extent of the system image (ADR 0097). Only PID 1
	// holds it; it is not transferable.
	LoaderObject = uint64(4)
	// ClockObject is the future realtime-setting authority. Reading either
	// clock needs no handle; no ClockObject handle is issued yet.
	ClockObject = uint64(5)
	// SerialConsoleObject and GraphicalConsoleObject are the two terminal
	// consoles (ADR 0125): COM1, and the framebuffer with the PS/2 keyboard.
	// Each is written, read, waited on and sized on its own, so each has its
	// own terminal service. ConsoleObject stays the logical console, whose
	// writes reach both: the kernel, PID 1 and the system services report
	// there, and it has no input.
	SerialConsoleObject    = uint64(6)
	GraphicalConsoleObject = uint64(7)
	// InertObject stands in a fixed handle slot for a device that is absent.
	// It authorizes nothing: no call accepts it.
	InertObject = uint64(8)
)

// The DMA domains, one per discovered device. A domain names the device whose
// reset and interrupt teardown runs before its driver's DMA pages are freed.
const (
	DMADomainBlock = uint16(1)
	DMADomainNet   = uint16(2)
)

// InertIRQObject is the IRQ placeholder for an absent device. It is not a
// line, so no route matches it.
const InertIRQObject = uint64(0x100)

// IsConsole reports whether object is the logical console or a terminal
// console; any of them authorizes writing kernel log records.
func IsConsole(object uint64) bool {
	return object == ConsoleObject || IsTerminalConsole(object)
}

// IsTerminalConsole reports whether object is a console with input of its own.
func IsTerminalConsole(object uint64) bool {
	return object == SerialConsoleObject || object == GraphicalConsoleObject
}

const (
	kindPort  = uint64(1)
	kindDMA   = uint64(2)
	kindImage = uint64(3)
)

func PortObject(base, size uint16) (uint64, bool) {
	if base == 0 || size == 0 || uint32(base)+uint32(size) > 1<<16 {
		return 0, false
	}
	return kindPort<<56 | uint64(size)<<16 | uint64(base), true
}

func DecodePortObject(object uint64) (uint16, uint16, bool) {
	if object>>56 != kindPort {
		return 0, 0, false
	}
	base, size := uint16(object), uint16(object>>16)
	return base, size, base != 0 && size != 0 && uint32(base)+uint32(size) <= 1<<16
}

func DMAObject(domain uint16) (uint64, bool) {
	if domain == 0 {
		return 0, false
	}
	return kindDMA<<56 | uint64(domain), true
}

func DecodeDMAObject(object uint64) (uint16, bool) {
	if object>>56 != kindDMA || uint16(object) == 0 {
		return 0, false
	}
	return uint16(object), true
}

// ImageObject names the read-only boot system image. Holding it authorizes
// mapping that image, and nothing else; the filesystem service needs it to
// serve /system, and no other process is given it.
func ImageObject(generation uint16) (uint64, bool) {
	if generation == 0 {
		return 0, false
	}
	return kindImage<<56 | uint64(generation), true
}

func DecodeImageObject(object uint64) (uint16, bool) {
	if object>>56 != kindImage || uint16(object) == 0 {
		return 0, false
	}
	return uint16(object), true
}

func IRQObject(line uint8) (uint64, bool) {
	if line == 0 || line == 0xff {
		return 0, false
	}
	return uint64(line), true
}
