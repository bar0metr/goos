package device

// Fixed device objects. Kind-encoded objects below use the top byte, so these
// small values cannot collide with them.
const (
	ConsoleObject       = uint64(1)
	SystemControlObject = uint64(2)
)

const (
	kindPort = uint64(1)
	kindDMA  = uint64(2)
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

func IRQObject(line uint8) (uint64, bool) {
	if line == 0 || line == 0xff {
		return 0, false
	}
	return uint64(line), true
}
