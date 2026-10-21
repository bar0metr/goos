package goroutine

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

// threadJoinOps keeps the join adapter's orchestration identical in native
// builds and host tests. The callbacks retain no M-local scratch across a
// park.
type threadJoinOps struct {
	// probe joins without waiting: ThreadJoin with JoinPoll.
	probe func(uint64) (uint64, kcall.Status)
	wait  func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, int64) (uint64, kcall.Status)
}

// joins names the threads parked goroutines join. A goroutine joiner never
// registers with the kernel, which records one joiner task and wakes that
// task, so a second goroutine joining the same thread is refused here the
// way the kernel refuses a second native joiner. Only a running thread is
// claimed, and a process has fewer running joinable threads than slots.
var joins struct {
	lock    spin
	threads [kcall.ThreadSlots]uint64
}

// claimJoin records thread as joined by a goroutine. It returns
// AlreadyExists when another goroutine joins it.
func claimJoin(thread uint64) kcall.Status {
	joins.lock.lock()
	defer joins.lock.unlock()
	free := -1
	for index := range joins.threads {
		claimed := joins.threads[index]
		if claimed == thread {
			return kcall.StatusAlreadyExists
		}
		if claimed == 0 && free < 0 {
			free = index
		}
	}
	if free < 0 {
		return kcall.StatusResourceExhausted
	}
	joins.threads[free] = thread
	return kcall.StatusOK
}

func releaseJoin(thread uint64) {
	joins.lock.lock()
	for index := range joins.threads {
		if joins.threads[index] == thread {
			joins.threads[index] = 0
		}
	}
	joins.lock.unlock()
}

// joinThread polls the join, parks on the thread's end while it runs, and
// polls again for the code. The polls keep the native statuses: Self is
// InvalidArgument, a native joiner already registered is AlreadyExists, and
// an unknown or already joined thread is NotFound.
func joinThread(thread uint64, ops threadJoinOps) (uint64, kcall.Status) {
	code, status := ops.probe(thread)
	if status != kcall.StatusTimeout {
		return code, status
	}
	if status = claimJoin(thread); status != kcall.StatusOK {
		return 0, status
	}
	defer releaseJoin(thread)
	var interests [kcall.WaitAnyMaxObjects]sys.Interest
	interests[0] = sys.Interest{Handle: handle.ID(kcall.ThreadEndInterest(thread)), Events: kcall.WaitCompleted}
	for {
		if _, status = ops.wait(&interests, 1, 0); status != kcall.StatusOK {
			return 0, status
		}
		// Readiness carries no code, and a native joiner may have taken the
		// end first; the poll reports either.
		code, status = ops.probe(thread)
		if status != kcall.StatusTimeout {
			return code, status
		}
	}
}
