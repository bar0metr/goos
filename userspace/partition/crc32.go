package partition

// crc32Update continues the IEEE CRC-32 (reflected polynomial 0xedb88320)
// that GPT uses, as hash/crc32.Update does: start from zero, feed the data in
// any number of pieces. It is computed bit by bit: a GPT is checked once per
// scan, and a table would cost a kilobyte in every process that links this.
func crc32Update(crc uint32, data []byte) uint32 {
	crc = ^crc
	for _, value := range data {
		crc ^= uint32(value)
		for bit := 0; bit < 8; bit++ {
			crc = crc>>1 ^ 0xedb88320&-(crc&1)
		}
	}
	return ^crc
}
