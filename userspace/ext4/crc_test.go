package ext4

import (
	"hash/crc32"
	"math/rand"
	"testing"
)

// TestCRC32CMatchesCastagnoli checks the eight-byte fold against the
// standard library over every length up to 64 bytes and random seeds.
// The library inverts the register on the way in and out; ext4 keeps it raw.
func TestCRC32CMatchesCastagnoli(t *testing.T) {
	table := crc32.MakeTable(crc32.Castagnoli)
	random := rand.New(rand.NewSource(9))
	data := make([]byte, 4096+7)
	random.Read(data)
	for length := 0; length <= 64; length++ {
		for _, offset := range []int{0, 1, 3, 7} {
			seed := random.Uint32()
			chunk := data[offset : offset+length]
			if got, want := crc32c(seed, chunk), ^crc32.Update(^seed, table, chunk); got != want {
				t.Fatalf("length %d offset %d: %#x, want %#x", length, offset, got, want)
			}
		}
	}
	if got, want := crc32c(^uint32(0), data[:4096]), ^crc32.Update(0, table, data[:4096]); got != want {
		t.Fatalf("block: %#x, want %#x", got, want)
	}
}
