package pci

const (
	VirtioVendor      = uint16(0x1af4)
	LegacyBlockDevice = uint16(0x1001)
)

type Config interface {
	Read32(bus, slot, function, offset uint8) uint32
}

type Device struct {
	Bus, Slot, Function uint8
	Vendor, Device      uint16
	IOBase              uint16
	IOSize              uint16
	IRQ                 uint8
}

func FindLegacyVirtioBlock(config Config) (Device, bool) {
	if config == nil {
		return Device{}, false
	}
	for bus := 0; bus < 256; bus++ {
		for slot := 0; slot < 32; slot++ {
			for function := 0; function < 8; function++ {
				id := config.Read32(uint8(bus), uint8(slot), uint8(function), 0)
				if uint16(id) == 0xffff {
					if function == 0 {
						break
					}
					continue
				}
				if uint16(id) != VirtioVendor || uint16(id>>16) != LegacyBlockDevice {
					continue
				}
				bar := config.Read32(uint8(bus), uint8(slot), uint8(function), 0x10)
				irq := uint8(config.Read32(uint8(bus), uint8(slot), uint8(function), 0x3c))
				if bar&1 == 0 || bar&^uint32(3) > 0xffff || irq == 0 || irq == 0xff {
					return Device{}, false
				}
				return Device{Bus: uint8(bus), Slot: uint8(slot), Function: uint8(function), Vendor: uint16(id), Device: uint16(id >> 16), IOBase: uint16(bar &^ 3), IOSize: 0x40, IRQ: irq}, true
			}
		}
	}
	return Device{}, false
}
