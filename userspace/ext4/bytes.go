package ext4

// Every on-disk structure is decoded from a slice whose length the caller
// has already checked against the structure it reads, so these helpers only
// assemble bytes. ext4 is little-endian; the JBD2 journal is big-endian and
// has its own helpers.

func le16(data []byte, offset int) uint16 {
	return uint16(data[offset]) | uint16(data[offset+1])<<8
}

func le32(data []byte, offset int) uint32 {
	return uint32(data[offset]) | uint32(data[offset+1])<<8 |
		uint32(data[offset+2])<<16 | uint32(data[offset+3])<<24
}

func putLE32(data []byte, value uint32) {
	data[0] = byte(value)
	data[1] = byte(value >> 8)
	data[2] = byte(value >> 16)
	data[3] = byte(value >> 24)
}

func be32(data []byte, offset int) uint32 {
	return uint32(data[offset])<<24 | uint32(data[offset+1])<<16 |
		uint32(data[offset+2])<<8 | uint32(data[offset+3])
}

func be64(data []byte, offset int) uint64 {
	return uint64(be32(data, offset))<<32 | uint64(be32(data, offset+4))
}

// join64 combines the low and high halves ext4 splits wide fields into.
func join64(low uint32, high uint32) uint64 { return uint64(high)<<32 | uint64(low) }

// mulOK multiplies two values and reports whether the product fits.
func mulOK(a uint64, b uint64) (uint64, bool) {
	if a != 0 && b > ^uint64(0)/a {
		return 0, false
	}
	return a * b, true
}

// addOK adds two values and reports whether the sum fits.
func addOK(a uint64, b uint64) (uint64, bool) {
	sum := a + b
	return sum, sum >= a
}
