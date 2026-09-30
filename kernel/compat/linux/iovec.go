package linux

import (
	"encoding/binary"

	"goos/kernel/compat/linux/abi"
)

// IOVecEntry decodes element index of a copied struct iovec array.
func IOVecEntry(wire []byte, index int) (uint64, uint64) {
	offset := index * abi.IOVecSize
	return binary.LittleEndian.Uint64(wire[offset:]), binary.LittleEndian.Uint64(wire[offset+8:])
}

// ValidateIOVec checks a copied vector before any byte moves, in the order
// Linux applies: a length that is negative as ssize_t is EINVAL, and a
// non-empty element outside user memory is EFAULT.
func ValidateIOVec(wire []byte, userLimit uint64) abi.Errno {
	if len(wire)%abi.IOVecSize != 0 || len(wire) > MaxIOVec*abi.IOVecSize {
		return abi.EINVAL
	}
	for index := 0; index < len(wire)/abi.IOVecSize; index++ {
		base, length := IOVecEntry(wire, index)
		if length>>63 != 0 {
			return abi.EINVAL
		}
		if length != 0 && (base >= userLimit || length > userLimit-base) {
			return abi.EFAULT
		}
	}
	return 0
}

// VectorPiece locates byte position of the data a vector describes. It
// returns the address that byte goes to and how many bytes remain in the
// same element. Empty elements are skipped; a position at or past the end of
// the vector has no piece.
func VectorPiece(wire []byte, position uint64) (uint64, uint64, bool) {
	for index := 0; index < len(wire)/abi.IOVecSize; index++ {
		base, length := IOVecEntry(wire, index)
		if position < length {
			return base + position, length - position, true
		}
		position -= length
	}
	return 0, 0, false
}
