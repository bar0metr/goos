package amd64

const (
	kernelCodeSelector = uint16(0x08)
	interruptGate      = uint8(0x8e)
	breakpointGate     = uint8(0xee)
)

type IDTEntry struct {
	OffsetLow  uint16
	Selector   uint16
	IST        uint8
	Attributes uint8
	OffsetMid  uint16
	OffsetHigh uint32
	Reserved   uint32
}

func NewIDTEntry(handler uintptr, attributes uint8) IDTEntry {
	return IDTEntry{
		OffsetLow:  uint16(handler),
		Selector:   kernelCodeSelector,
		Attributes: attributes,
		OffsetMid:  uint16(handler >> 16),
		OffsetHigh: uint32(handler >> 32),
	}
}

func (entry IDTEntry) Handler() uintptr {
	return uintptr(entry.OffsetLow) |
		uintptr(entry.OffsetMid)<<16 |
		uintptr(entry.OffsetHigh)<<32
}
