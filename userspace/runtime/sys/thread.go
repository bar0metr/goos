package sys

import (
	"goos/kernel/kcall"
	"goos/userspace/runtime/tls"
	"unsafe"
)

// Native threads (ADR 0203). Programs use userspace/runtime/thread; these
// are its calls into the kernel.

// ThreadCreate starts a thread of the calling process at the runtime's
// thread trampoline with argument, and returns its ID and slot.
func ThreadCreate(argument uintptr) (uint64, uint64, kcall.Status) {
	w := current()
	defer tls.Unpin()
	w.threadWire = [2]uint64{}
	result := status(threadCreate(threadEntryAddress(), argument, unsafe.Pointer(&w.threadWire[0])))
	if result != kcall.StatusOK {
		return 0, 0, result
	}
	return w.threadWire[0], w.threadWire[1], kcall.StatusOK
}

// ThreadExit ends the calling thread with code. It does not return.
func ThreadExit(code uint64) { threadExit(code) }

// ThreadJoin waits for thread to end and returns its exit code. With poll it
// answers Timeout rather than wait. A wait goes to the installed
// ThreadJoinHandler first, which may park only the calling goroutine.
func ThreadJoin(thread uint64, poll bool) (uint64, kcall.Status) {
	if !poll {
		if handler := threadJoinHandler.Load(); handler != nil {
			if code, status, handled := (*handler)(thread); handled {
				return code, status
			}
		}
	}
	w := current()
	defer tls.Unpin()
	w.countWire = 0
	flags := uint64(0)
	if poll {
		flags = kcall.JoinPoll
	}
	result := status(threadJoin(thread, flags, unsafe.Pointer(&w.countWire)))
	return w.countWire, result
}

// ThreadSelf returns the calling thread's ID and slot.
func ThreadSelf() (uint64, uint64, kcall.Status) {
	w := current()
	defer tls.Unpin()
	w.threadWire = [2]uint64{}
	result := status(threadSelf(unsafe.Pointer(&w.threadWire[0])))
	return w.threadWire[0], w.threadWire[1], result
}

// ThreadYield gives up the processor.
func ThreadYield() { threadYield() }

// ThreadSuspend holds every other thread of the process and writes their
// snapshots, kcall.ThreadSnapshotSize bytes each, into snapshots. It returns
// how many it wrote. Only the collector calls it.
func ThreadSuspend(snapshots []byte) (int, kcall.Status) {
	capacity := uint64(len(snapshots)) / kcall.ThreadSnapshotSize
	if capacity == 0 {
		return 0, kcall.StatusInvalidArgument
	}
	if capacity > kcall.MaxThreadSnapshots {
		capacity = kcall.MaxThreadSnapshots
	}
	w := current()
	defer tls.Unpin()
	w.countWire = 0
	result := status(threadSuspend(address(snapshots), capacity, unsafe.Pointer(&w.countWire)))
	return int(w.countWire), result
}

// ThreadResume releases the threads ThreadSuspend holds.
func ThreadResume() kcall.Status { return status(threadResume()) }

// WorkerCreate starts a thread at the goroutine runtime's worker entry with
// argument and returns its ID and slot (ADR 0204).
func WorkerCreate(argument uintptr) (uint64, uint64, kcall.Status) {
	w := current()
	defer tls.Unpin()
	w.threadWire = [2]uint64{}
	result := status(threadCreate(workerEntryAddress(), argument, unsafe.Pointer(&w.threadWire[0])))
	if result != kcall.StatusOK {
		return 0, 0, result
	}
	return w.threadWire[0], w.threadWire[1], kcall.StatusOK
}

// ThreadSetBase sets the calling thread's FS base. The runtime's entry code
// already points it at the thread's control block; nothing else should
// move it.
func ThreadSetBase(base uintptr) kcall.Status { return status(threadSetBase(base)) }

// ThreadPreempt asks the kernel to divert the calling thread to entry after
// every ticks ticks of user time, or with a zero entry to stop (ADR 0204).
func ThreadPreempt(entry uintptr, ticks uint64) kcall.Status {
	return status(threadPreempt(entry, ticks))
}

// ProbeVectorSpin fills XMM0 to XMM14 with a pattern and loops without
// calls until *stop is nonzero. It returns how many times a register had
// lost the pattern: a preemption that does not keep the SSE state of the code
// it interrupts shows up here. Only goprobe calls it.
func ProbeVectorSpin(stop *uint32) uint64 { return probeVectorSpin(stop) }

// ThreadWake sets the wake flag of a thread of the calling process: a
// WaitAny of that thread with an interest in kcall.WaitAnyThread returns
// (ADR 0204).
func ThreadWake(thread uint64) kcall.Status { return status(threadWake(thread)) }

// ThreadWakeTake clears the calling thread's wake flag; NotFound means it
// was not set.
func ThreadWakeTake() kcall.Status { return status(threadWakeTake()) }

// ThreadHoldRange arms an instruction hold of another thread of the process
// at the first boundary of [lo, hi) (ADR 0204). Only validation kernels
// carry it out; others answer Unsupported. Only goprobe calls it.
func ThreadHoldRange(thread uint64, lo, hi uintptr) kcall.Status {
	return status(threadHoldRange(thread, lo, hi))
}

// HoldStop is where a held thread stopped.
type HoldStop struct {
	RIP, SP uintptr
	// State is kcall.HoldStopped, or kcall.HoldDone once the thread left
	// the range.
	State uint64
}

// ThreadHoldWait reports where a held thread stopped. Timeout means it
// still runs toward its next stop.
func ThreadHoldWait(thread uint64) (HoldStop, kcall.Status) {
	w := current()
	defer tls.Unpin()
	w.holdWire = [3]uint64{}
	result := status(threadHoldWait(thread, unsafe.Pointer(&w.holdWire[0])))
	return HoldStop{RIP: uintptr(w.holdWire[0]), State: w.holdWire[1], SP: uintptr(w.holdWire[2])}, result
}

// ThreadHoldStep runs a stopped thread to its next instruction boundary in
// its range, stepping over calls.
func ThreadHoldStep(thread uint64) kcall.Status { return status(threadHoldStep(thread)) }

// ThreadHoldCancel disarms a thread's hold and lets it run on.
func ThreadHoldCancel(thread uint64) kcall.Status { return status(threadHoldCancel(thread)) }

// ThreadJoinClaim reserves a join independently of the calling native
// thread and returns its token. The reservation excludes native joiners and
// other claims until collection or release. It never blocks.
func ThreadJoinClaim(thread uint64) (uint64, kcall.Status) {
	return threadJoinToken(thread, kcall.JoinClaim, 0)
}

// ThreadJoinCollect polls a reserved join, returning Timeout while the
// target runs. A successful collection consumes the token and returns the
// exit code. The caller may use a different native thread than the claimant.
func ThreadJoinCollect(thread, token uint64) (uint64, kcall.Status) {
	return threadJoinToken(thread, kcall.JoinCollect, token)
}

// ThreadJoinRelease relinquishes an uncollected reservation. It never
// blocks. NotFound means the claim no longer exists, including after a
// successful collection.
func ThreadJoinRelease(thread, token uint64) kcall.Status {
	_, result := threadJoinToken(thread, kcall.JoinRelease, token)
	return result
}

func threadJoinToken(thread, flags, token uint64) (uint64, kcall.Status) {
	w := current()
	defer tls.Unpin()
	w.countWire = token
	result := status(threadJoin(thread, flags, unsafe.Pointer(&w.countWire)))
	if result != kcall.StatusOK {
		return 0, result
	}
	return w.countWire, result
}
