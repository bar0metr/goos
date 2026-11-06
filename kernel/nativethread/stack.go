package nativethread

import (
	"goos/kernel/kcall"
	"goos/kernel/mm/region"
)

// A thread's guard is the region package's low guard.
var (
	_ = [kcall.ThreadGuardSize - region.StackGuardBytes]struct{}{}
	_ = [region.StackGuardBytes - kcall.ThreadGuardSize]struct{}{}
)

// StackRegion is the one region a thread in slot holds: its stack with the
// guard below it as the region's lowest ThreadGuardSize bytes. Two regions a
// thread would let 63 threads take 126 of a process's region.MaxRegions.
func StackRegion(slot uint64) region.Region {
	top := kcall.ThreadStackTop(slot)
	return region.Region{Base: top - kcall.ThreadStride, Length: kcall.ThreadStride, Protection: region.Read | region.Write,
		Backing: region.Anonymous, Flags: region.Pinned | region.LowGuard}
}
