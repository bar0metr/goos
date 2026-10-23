package goroutine

import "math/bits"

const stacksPerBatch = 32

func popcount32(value uint32) int { return bits.OnesCount32(value) }

// stackBatch tracks slots in one VM reservation. The stacks lock guards it.
type stackBatch struct {
	base uintptr
	used uint32
}

func (b *stackBatch) take() (uintptr, bool) {
	if b.base == 0 || b.used == ^uint32(0) {
		return 0, false
	}
	slot := bits.TrailingZeros32(^b.used)
	b.used |= uint32(1) << slot
	return b.base + uintptr(slot)*(GuardSize+StackSize), true
}

func (b *stackBatch) release(base uintptr) bool {
	const stride = GuardSize + StackSize
	if b.base == 0 || base < b.base || base-b.base >= stacksPerBatch*stride || (base-b.base)%stride != 0 {
		return false
	}
	slot := (base - b.base) / stride
	bit := uint32(1) << slot
	if b.used&bit == 0 {
		return false
	}
	b.used &^= bit
	return true
}
