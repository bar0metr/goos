package linux

import (
	"encoding/binary"
	"testing"

	"goos/kernel/compat/linux/abi"
)

func iovec(entries ...[2]uint64) []byte {
	wire := make([]byte, len(entries)*abi.IOVecSize)
	for index, entry := range entries {
		binary.LittleEndian.PutUint64(wire[index*abi.IOVecSize:], entry[0])
		binary.LittleEndian.PutUint64(wire[index*abi.IOVecSize+8:], entry[1])
	}
	return wire
}

func TestValidateIOVec(t *testing.T) {
	const limit = uint64(1) << 47
	cases := []struct {
		name string
		wire []byte
		want abi.Errno
	}{
		{"two elements", iovec([2]uint64{0x1000, 16}, [2]uint64{0x2000, 32}), 0},
		{"empty element at any base", iovec([2]uint64{^uint64(0), 0}), 0},
		{"ends at the limit", iovec([2]uint64{limit - 8, 8}), 0},
		{"crosses the limit", iovec([2]uint64{limit - 8, 9}), abi.EFAULT},
		{"kernel base", iovec([2]uint64{0xffff800000000000, 1}), abi.EFAULT},
		{"wraps", iovec([2]uint64{0x1000, ^uint64(0) >> 1}), abi.EFAULT},
		{"negative length", iovec([2]uint64{0x1000, 1 << 63}), abi.EINVAL},
		{"truncated wire", make([]byte, abi.IOVecSize+1), abi.EINVAL},
		{"over the vector bound", make([]byte, (MaxIOVec+1)*abi.IOVecSize), abi.EINVAL},
	}
	for _, test := range cases {
		if got := ValidateIOVec(test.wire, limit); got != test.want {
			t.Errorf("%s: errno %d, want %d", test.name, got, test.want)
		}
	}
	if base, length := IOVecEntry(iovec([2]uint64{1, 2}, [2]uint64{3, 4}), 1); base != 3 || length != 4 {
		t.Fatalf("entry 1 = %d,%d", base, length)
	}
}
