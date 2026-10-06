package pci

const (
	VirtioVendor      = uint16(0x1af4)
	LegacyBlockDevice = uint16(0x1001)
	LegacyNetDevice   = uint16(0x1000)
)

// Configuration-space offsets and the bits discovery reads or changes.
const (
	offsetID        = 0x00
	offsetCommand   = 0x04
	offsetBAR0      = 0x10
	offsetInterrupt = 0x3c
	barIO           = uint32(1)
	barIOMask       = ^uint32(3)
	// minimumIOSize covers the legacy virtio header and the start of the
	// device configuration both drivers read.
	minimumIOSize = 0x20
	// CommandIO and CommandBusMaster are the command-register bits a driver
	// needs; CommandInterruptDisable masks INTx at the function.
	CommandIO               = uint16(1 << 0)
	CommandBusMaster        = uint16(1 << 2)
	CommandInterruptDisable = uint16(1 << 10)
)

type Config interface {
	Read32(bus, slot, function, offset uint8) uint32
	Write32(bus, slot, function, offset uint8, value uint32)
}

type Device struct {
	Bus, Slot, Function uint8
	Vendor, Device      uint16
	IOBase              uint16
	IOSize              uint16
	IRQ                 uint8
	// Pin is the INTx pin, 1 for INTA through 4 for INTD.
	Pin uint8
}

// Problem says why discovery refused a device.
type Problem uint8

const (
	ProblemNone Problem = iota
	ProblemAbsent
	// ProblemBAR is an I/O BAR that is not I/O space, is zero, sizes to less
	// than the registers a driver reads, or does not fit 16-bit port space.
	ProblemBAR
	// ProblemInterrupt is a missing or out-of-range INTx pin or line.
	ProblemInterrupt
	// ProblemDuplicate is a second function matching the same identity:
	// which one a driver gets would be an accident of enumeration.
	ProblemDuplicate
)

// Overlaps reports whether the I/O ranges of two devices share a port.
func (device Device) Overlaps(other Device) bool {
	if device.IOSize == 0 || other.IOSize == 0 {
		return false
	}
	start, end := uint32(device.IOBase), uint32(device.IOBase)+uint32(device.IOSize)
	otherStart, otherEnd := uint32(other.IOBase), uint32(other.IOBase)+uint32(other.IOSize)
	return start < otherEnd && otherStart < end
}

// FindLegacyVirtioBlock returns the first legacy virtio block function.
func FindLegacyVirtioBlock(config Config) (Device, bool) {
	device, problem := find(config, LegacyBlockDevice, false)
	return device, problem == ProblemNone
}

// FindLegacyVirtioNet returns the one legacy virtio network function, and
// refuses a second one rather than choosing between them.
func FindLegacyVirtioNet(config Config) (Device, Problem) {
	return find(config, LegacyNetDevice, true)
}

func find(config Config, deviceID uint16, unique bool) (Device, Problem) {
	if config == nil {
		return Device{}, ProblemAbsent
	}
	var found Device
	problem := ProblemAbsent
	for bus := 0; bus < 256; bus++ {
		for slot := 0; slot < 32; slot++ {
			for function := 0; function < 8; function++ {
				id := config.Read32(uint8(bus), uint8(slot), uint8(function), offsetID)
				if uint16(id) == 0xffff {
					if function == 0 {
						break
					}
					continue
				}
				if uint16(id) != VirtioVendor || uint16(id>>16) != deviceID {
					continue
				}
				if problem != ProblemAbsent {
					return Device{}, ProblemDuplicate
				}
				found, problem = describe(config, uint8(bus), uint8(slot), uint8(function), id)
				if !unique || problem != ProblemNone {
					return found, problem
				}
			}
		}
	}
	return found, problem
}

// describe validates one function's I/O BAR and interrupt. The BAR is sized
// by the standard write of all ones, and its original value is written back
// before anything else reads it.
func describe(config Config, bus, slot, function uint8, id uint32) (Device, Problem) {
	original := config.Read32(bus, slot, function, offsetBAR0)
	config.Write32(bus, slot, function, offsetBAR0, ^uint32(0))
	probe := config.Read32(bus, slot, function, offsetBAR0)
	config.Write32(bus, slot, function, offsetBAR0, original)
	base := original & barIOMask
	if probe>>16 == 0 {
		// A function may decode only 16 address bits of I/O space and read
		// the upper half of its BAR as zero.
		probe |= 0xffff0000
	}
	size := ^(probe & barIOMask) + 1
	if original&barIO == 0 || probe&barIO == 0 || base == 0 || base > 0xffff || size&(size-1) != 0 ||
		size < minimumIOSize || size > 0x10000 || base+size > 0x10000 || base&(size-1) != 0 {
		return Device{}, ProblemBAR
	}
	interrupt := config.Read32(bus, slot, function, offsetInterrupt)
	line, pin := uint8(interrupt), uint8(interrupt>>8)
	if line == 0 || line >= 16 || pin == 0 || pin > 4 {
		return Device{}, ProblemInterrupt
	}
	return Device{Bus: bus, Slot: slot, Function: function, Vendor: uint16(id), Device: uint16(id >> 16),
		IOBase: uint16(base), IOSize: uint16(size), IRQ: line, Pin: pin}, ProblemNone
}

// EnableCommand returns command with I/O decoding and bus mastering on and
// INTx unmasked at the function, leaving every other bit as it was.
func EnableCommand(command uint16) uint16 {
	return command&^CommandInterruptDisable | CommandIO | CommandBusMaster
}
