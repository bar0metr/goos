//go:build goos_userspace

package thread

import (
	"goos/kernel/kcall"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
)

// Start runs fn(argument) on a new thread and returns its ID.
func Start(fn func(uintptr), argument uintptr) (ID, Status) {
	if fn == nil {
		return 0, kcall.StatusInvalidArgument
	}
	arena.EnableThreads()
	index, ok := claim(fn, argument)
	if !ok {
		return 0, kcall.StatusResourceExhausted
	}
	id, _, status := sys.ThreadCreate(uintptr(index))
	if status != kcall.StatusOK {
		take(index)
		return 0, status
	}
	return ID(id), kcall.StatusOK
}

// Join waits for a thread to end and returns its exit code: zero when its
// function returned.
func Join(id ID) (uint64, Status) { return sys.ThreadJoin(uint64(id), false) }

// Done reports, without waiting, whether a thread ended; when it did, it is
// joined and its code returned.
func Done(id ID) (uint64, bool, Status) {
	code, status := sys.ThreadJoin(uint64(id), true)
	if status == kcall.StatusTimeout {
		return 0, false, kcall.StatusOK
	}
	return code, status == kcall.StatusOK, status
}

// Self returns the calling thread's ID.
func Self() ID {
	id, _, _ := sys.ThreadSelf()
	return ID(id)
}

// Yield gives up the processor.
func Yield() { sys.ThreadYield() }

func yield() { sys.ThreadYield() }

// Exit ends the calling thread with code. The process goes on while another
// of its threads lives.
func Exit(code uint64) {
	arena.ThreadExiting()
	sys.ThreadExit(code)
}

// main is where every thread but the first runs: goos_thread_entry calls it
// on the thread's stack with the start entry and the slot.
//
//export goos_thread_main
func main(index uint64, slot uint64) {
	arena.ThreadStarted(int(slot))
	fn, argument := take(int(index))
	if fn != nil {
		fn(argument)
	}
	Exit(0)
}
