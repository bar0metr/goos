package goroutine

import (
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

// plainWaitOps keeps native plain-mode validation ahead of the poller's
// cursor-format conversion in both native code and host tests.
type plainWaitOps struct {
	probe func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, uint64) (uint64, kcall.Status)
	park  func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, int64, *Cancel) (uint64, kcall.Status)
}

func waitPlain(interests *[kcall.WaitAnyMaxObjects]sys.Interest, count int, deadline int64, cancel *Cancel, ops plainWaitOps) (uint64, kcall.Status) {
	if interests == nil || count < 0 || count > MaxWaiters {
		return 0, kcall.StatusInvalidArgument
	}
	// A plain log interest must return Unsupported, even though the poller
	// could represent it in cursor format with cursor zero. Validate the
	// original set before registration so alias fallback cannot change its
	// native mode or accept an otherwise invalid combination.
	mask, status := ops.probe(interests, count, 0)
	if status != kcall.StatusTimeout {
		return mask, status
	}
	return ops.park(interests, count, deadline, cancel)
}
