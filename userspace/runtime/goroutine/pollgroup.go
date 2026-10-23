package goroutine

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

// waitGroup rebuilds one original wait from the poller's mixed snapshot.
// Kernel rejection of the aggregate may mean different Gs watch the same
// object; validating each original set preserves alias and event semantics.
func waitGroup(snapshot *[MaxWaiters]registration, first, count int, interests *[kcall.WaitAnyMaxObjects]sys.CursorInterest, bits *[MaxWaiters]uint8) (uint64, int) {
	key := snapshot[first]
	members := uint64(0)
	size := 0
	for index := first; index < count; index++ {
		r := snapshot[index]
		if r.index != key.index || r.gen != key.gen || r.seq != key.seq {
			continue
		}
		interests[size] = sys.CursorInterest{Handle: handle.ID(r.handle), Events: r.events, Cursor: r.cursor}
		bits[size] = r.bit
		members |= uint64(1) << index
		size++
	}
	return members, size
}

func groupReadyMask(mask uint64, bits *[MaxWaiters]uint8, count int) uint64 {
	result := uint64(0)
	for index := 0; index < count; index++ {
		if mask&(uint64(1)<<index) != 0 {
			result |= uint64(1) << bits[index]
		}
	}
	return result
}
