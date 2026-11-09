package ext4

// ext4 checksums use two CRCs. metadata_csum covers structures with CRC32C
// (Castagnoli, reflected polynomial 0x82F63B78) and keeps the raw register:
// no final inversion, and every chain starts from either ~0 or the
// filesystem seed. The older uninit_bg group descriptor checksum is CRC16
// with the reflected 0x8005 polynomial (0xA001), started from ~0.

// crc32cTable is filled on first use rather than by a package initializer:
// GoOS services do not run initializers that need the heap.
var (
	crc32cTable  [256]uint32
	crc32cFilled bool
)

func fillCRC32CTable() {
	for index := range crc32cTable {
		value := uint32(index)
		for bit := 0; bit < 8; bit++ {
			if value&1 != 0 {
				value = value>>1 ^ 0x82F63B78
			} else {
				value >>= 1
			}
		}
		crc32cTable[index] = value
	}
	crc32cFilled = true
}

// crc32c continues the raw CRC32C register crc over data.
func crc32c(crc uint32, data []byte) uint32 {
	if !crc32cFilled {
		fillCRC32CTable()
	}
	table := &crc32cTable
	for _, value := range data {
		crc = table[byte(crc)^value] ^ crc>>8
	}
	return crc
}

// crc32cLE32 continues crc over the little-endian encoding of value.
func crc32cLE32(crc uint32, value uint32) uint32 {
	var buffer [4]byte
	putLE32(buffer[:], value)
	return crc32c(crc, buffer[:])
}

func crc32cZeros(crc uint32, count int) uint32 {
	var zero [8]byte
	for count > 0 {
		chunk := count
		if chunk > len(zero) {
			chunk = len(zero)
		}
		crc = crc32c(crc, zero[:chunk])
		count -= chunk
	}
	return crc
}

func crc16(crc uint16, data []byte) uint16 {
	for _, value := range data {
		crc ^= uint16(value)
		for bit := 0; bit < 8; bit++ {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}
