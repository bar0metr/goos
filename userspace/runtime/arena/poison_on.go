//go:build !goos_userspace || goos_validation

package arena

import (
	"math/bits"
	"sync/atomic"
)

// poisonFreed makes sweep fill what it frees with freedPoison. Validation
// images and host tests keep it, so a collection that frees a live object
// corrupts it at once rather than when its slot is reused. Other images
// compile every call out (poison_off.go).
const poisonFreed = true

// freedPoison is no canonical address, so following a pointer read from a
// freed object faults, and no checksum a test stores matches it.
const freedPoison = uintptr(0xa5a5a5a5a5a5a5a5)

// poisonedBytes counts what sweep poisoned. It holds no address.
var poisonedBytes atomic.Uint64

// PoisonedBytes reports how many freed bytes sweep overwrote: zero in
// images that do not poison.
func PoisonedBytes() uint64 { return poisonedBytes.Load() }

// poisonSlots fills the slots of the slab at page that the sweep frees: the
// allocated ones left unmarked. Allocation clears a reused slot first, so
// only code that kept a freed object reads the poison.
func (h *heap) poisonSlots(page uint32, size uintptr) {
	m := &h.meta[page]
	base := h.pageAddress(page)
	for word := 0; word < slotWords; word++ {
		for dead := m.alloc[word] &^ m.mark[word]; dead != 0; dead &= dead - 1 {
			slot := uintptr(word*64 + bits.TrailingZeros64(dead))
			h.poison(base+slot*size, size)
		}
	}
}

// poison fills length bytes at address with freedPoison. Sweep poisons a
// run in its first page only: the rest may never have been touched, and
// poisoning it would make the kernel supply it.
func (h *heap) poison(address, length uintptr) {
	for offset := uintptr(0); offset+wordBytes <= length; offset += wordBytes {
		*(*uintptr)(pointerAt(address + offset)) = freedPoison
	}
	poisonedBytes.Add(uint64(length))
}
