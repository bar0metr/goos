package amd64

const (
	kernelCodeSelector = uint16(0x08)
	kernelDataSelector = uint16(0x10)
	userDataSelector   = uint16(0x1b)
	userCodeSelector   = uint16(0x23)
	tssSelector        = uint16(0x28)
	interruptGate      = uint8(0x8e)
	breakpointGate     = uint8(0xee)
	userCallGate       = uint8(0xee)
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

func tssDescriptor(base uintptr, limit uint32) (uint64, uint64) {
	low := uint64(limit&0xffff) |
		uint64(base&0xffffff)<<16 |
		uint64(0x89)<<40 |
		uint64((limit>>16)&0xf)<<48 |
		uint64((base>>24)&0xff)<<56
	return low, uint64(base >> 32)
}
