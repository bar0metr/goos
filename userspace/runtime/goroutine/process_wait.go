package goroutine

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

// processWaitOps keeps the process adapter's orchestration identical in native
// builds and host tests. The callbacks retain no M-local scratch across a park.
type processWaitOps struct {
	probe func(handle.ID, uint64) (uint64, kcall.Status)
	now   func() int64
	wait  func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, int64) (uint64, kcall.Status)
}

func waitForProcess(process handle.ID, timeout uint64, ops processWaitOps) (uint64, kcall.Status) {
	// The raw probe preserves the process syscall's capability.Wait check,
	// object errors and exit code, even when the timeout cannot fit.
	code, status := ops.probe(process, 0)
	if status != kcall.StatusTimeout || timeout == 0 {
		return code, status
	}
	// Unlike WaitAny, ProcessWait has no infinite-timeout sentinel.
	deadline, valid := deadlineFromTicks(ops.now(), timeout)
	if !valid || timeout == kcall.WaitAnyInfiniteTimeout {
		return 0, kcall.StatusResourceExhausted
	}
	var interests [kcall.WaitAnyMaxObjects]sys.Interest
	interests[0] = sys.Interest{Handle: process, Events: kcall.WaitCompleted}
	for {
		if _, status = ops.wait(&interests, 1, deadline); status != kcall.StatusOK {
			return 0, status
		}
		// Readiness does not supply the exit code. Re-probe through the native
		// operation, with no descriptor borrowed from a previous thread.
		code, status = ops.probe(process, 0)
		if status != kcall.StatusTimeout {
			return code, status
		}
		if ops.now() >= deadline {
			return 0, kcall.StatusTimeout
		}
	}
}
