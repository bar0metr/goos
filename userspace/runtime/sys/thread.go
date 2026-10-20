package sys

import (
	"goos/kernel/kcall"
	"unsafe"
)

// Native threads (ADR 0203). Programs use userspace/runtime/thread; these
// are its calls into the kernel.

// ThreadCreate starts a thread of the calling process at the runtime's
// thread trampoline with argument, and returns its ID and slot.
func ThreadCreate(argument uintptr) (uint64, uint64, kcall.Status) {
	w := current()
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
// answers Timeout rather than wait.
func ThreadJoin(thread uint64, poll bool) (uint64, kcall.Status) {
	w := current()
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
	w.countWire = 0
	result := status(threadSuspend(address(snapshots), capacity, unsafe.Pointer(&w.countWire)))
	return int(w.countWire), result
}

// ThreadResume releases the threads ThreadSuspend holds.
func ThreadResume() kcall.Status { return status(threadResume()) }
