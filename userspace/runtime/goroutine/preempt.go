//go:build goos_userspace && goos_goroutines

package goroutine

import (
	"unsafe"

	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/tls"
)

//go:linkname preemptEntryAddress goos_preempt_entry_address
func preemptEntryAddress() uintptr

// preemptVisit runs on the interrupted stack each time the kernel diverts
// a worker thread to goos_preempt_entry, every PreemptTicks ticks of user
// time. It switches the goroutine out only at a point where that is safe:
// the thread is not pinned (no system call descriptors, allocator state,
// or runtime lock in use), the goroutine runs on its own stack, and the
// thread is outside the allocation fast path and every region. The
// goroutine is switched out when the same one ran across a whole interval
// and another goroutine waits to run, or a timer or a handle wait is due.
//
//export goos_g_preempt
func preemptVisit() {
	// Pin before reading any M-local pointer. A nested delivery before
	// this instruction may migrate the G; the loads below then use its
	// current M rather than a TCB captured on the previous M.
	pin()
	block := tls.Current()
	// A value above one means the interrupted code already held a pin.
	if block.Pins != 1 || block.M == 0 || block.G == 0 {
		unpin()
		return
	}
	m := (*M)(unsafe.Pointer(block.M))
	g := (*G)(unsafe.Pointer(block.G))
	sp := stackPointer()
	if m.kind.Load() != mWorker || g.pinned >= 0 || sp < g.lo || sp > g.hi || !arena.Preemptible(int(m.slot)) {
		unpin()
		return
	}
	m.counters.preemptVisits.Add(1)
	if pollPending() && pollNow(m) {
		// A goroutine became runnable: let it run.
		m.seen = m.dispatches
		m.counters.preemptions.Add(1)
		switchOut(m, g, reasonPreempt, 0)
		unpin()
		return
	}
	due := timersDue() || sched.free.Load() < 0
	if m.dispatches != m.seen && !due {
		m.seen = m.dispatches
		unpin()
		return
	}
	m.seen = m.dispatches
	if !due && !anyWork() {
		unpin()
		return
	}
	m.counters.preemptions.Add(1)
	switchOut(m, g, reasonPreempt, 0)
	unpin()
}
