//go:build goos_userspace && !goos_validation

package arena

// poisonFreed turns sweep poisoning off outside validation images; the
// calls below are compiled out.
const poisonFreed = false

func PoisonedBytes() uint64 { return 0 }

func (h *heap) poisonSlots(page uint32, size uintptr) {}

func (h *heap) poison(address, length uintptr) {}
