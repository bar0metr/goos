package pci

import "testing"

type fakeConfig map[uint32]uint32

func (f fakeConfig) Read32(bus, slot, function, offset uint8) uint32 {
	key := uint32(bus)<<24 | uint32(slot)<<16 | uint32(function)<<8 | uint32(offset)
	if value, ok := f[key]; ok {
		return value
	}
	return 0xffffffff
}

func TestFindLegacyVirtioBlock(t *testing.T) {
	f := fakeConfig{
		uint32(3)<<24 | uint32(4)<<16:        uint32(LegacyBlockDevice)<<16 | uint32(VirtioVendor),
		uint32(3)<<24 | uint32(4)<<16 | 0x10: 0xc041,
		uint32(3)<<24 | uint32(4)<<16 | 0x3c: 11,
	}
	device, ok := FindLegacyVirtioBlock(f)
	if !ok || device.Bus != 3 || device.Slot != 4 || device.IOBase != 0xc040 || device.IRQ != 11 {
		t.Fatalf("unexpected device: %+v", device)
	}
}
