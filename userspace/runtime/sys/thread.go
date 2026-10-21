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

// ThreadWake sets the wake flag of a thread of the calling process: a
// WaitAny of that thread with an interest in kcall.WaitAnyThread returns
// (ADR 0204).
func ThreadWake(thread uint64) kcall.Status { return status(threadWake(thread)) }

// ThreadWakeTake clears the calling thread's wake flag; NotFound means it
// was not set.
func ThreadWakeTake() kcall.Status { return status(threadWakeTake()) }
