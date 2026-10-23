package goroutine

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

// threadJoinOps keeps the join adapter's orchestration identical in native
// builds and host tests. The callbacks retain no M-local scratch across a
// park. A kernel token owns the join independently of the current M.
type threadJoinOps struct {
	claim   func(uint64) (uint64, kcall.Status)
	collect func(uint64, uint64) (uint64, kcall.Status)
	release func(uint64, uint64) kcall.Status
	wait    func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, int64) (uint64, kcall.Status)
}

// joinThread claims the join, polls it, parks on the thread's end while it
// runs, then polls for the code. The kernel reservation excludes other
// goroutine and native joiners across migration, including after the target
// ends. A deferred release covers wait errors and goroutine unwinding; after
// collection the kernel has already retired the claim.
func joinThread(thread uint64, ops threadJoinOps) (uint64, kcall.Status) {
	token, status := ops.claim(thread)
	if status != kcall.StatusOK {
		return 0, status
	}
	defer ops.release(thread, token)
	var interests [kcall.WaitAnyMaxObjects]sys.Interest
	interests[0] = sys.Interest{Handle: handle.ID(kcall.ThreadEndInterest(thread)), Events: kcall.WaitCompleted}
	for {
		code, status := ops.collect(thread, token)
		if status != kcall.StatusTimeout {
			return code, status
		}
		if _, status = ops.wait(&interests, 1, 0); status != kcall.StatusOK {
			return 0, status
		}
	}
}
