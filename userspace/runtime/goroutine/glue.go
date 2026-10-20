//go:build goos_userspace && goos_goroutines

package goroutine

import (
	"unsafe"

	"goos/kernel/kcall"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/runtime/tls"
)

// The functions the TinyGo runtime and task package of the goroutine
// toolchain root call (tinygo/goroutines), and those the runtime's assembly
// calls. Each is exported under the symbol its caller names.

// start runs a go statement: a new goroutine that calls fn(args), the
// compiler's start wrapper and its argument bundle. It panics in the caller
// when the goroutine limit is reached or no stack can be reserved.
//
//export goos_g_start
func goStart(task uintptr, fn uintptr, args uintptr, stackSize uintptr) {
	ensureStarted()
	g := allocG()
	if g == nil {
		panic("goroutine: limit of goroutines reached")
	}
	size := uintptr(StackSize)
	if stackSize > size {
		size = (stackSize + pageSize - 1) &^ (pageSize - 1)
	}
	base, ok := reserveStack(size)
	if !ok {
		g.status.Store(gDead)
		freeG(g)
		panic("goroutine: no memory for a goroutine stack")
	}
	g.kind, g.pinned = kindGo, -1
	g.base, g.size = base, size
	g.lo, g.hi = base+GuardSize, base+GuardSize+size
	g.sp = writeFrame(g.hi, fn, args, entryAddress())
	g.task = task
	setTaskG(task, uintptr(unsafe.Pointer(g)))
	noteLive()
	pin()
	g.status.Store(gRunnable)
	enqueue(currentM(), g)
	unpin()
}

// begin is the first thing a new goroutine runs: the scheduler stack's pin
// came with the switch, and the goroutine's own code runs unpinned.
//
//export goos_g_begin
func begin() { unpin() }

// currentTaskSlow is task.Current on a thread whose control block names no
// task yet: the main thread before the scheduler starts, which becomes the
// main goroutine, and a thread before the runtime set it up.
//
//export goos_g_current_task_slow
func currentTaskSlow() uintptr {
	block := tls.Current()
	slot := int(block.Slot)
	if block.M != 0 {
		return staticTask(slot)
	}
	if slot == 0 {
		return staticTask(taskMain)
	}
	return staticTask(taskPinned + slot)
}

// park switches the running goroutine out until it is made runnable.
//
//export goos_g_park
func park() {
	ensureStarted()
	pin()
	m := currentM()
	g := m.curG
	if g == nil {
		fatal("goroutine: blocking operation on a scheduler stack")
	}
	if arena.InRegion() {
		fatal("goroutine: blocking operation inside an arena region")
	}
	switchOut(m, g, reasonPark, 0)
	unpin()
}

// parkForever parks the goroutine with nothing that can wake it.
//
//export goos_g_park_forever
func parkForever() { park() }

// ready makes the goroutine of task runnable.
//
//export goos_g_ready
func ready(task uintptr) {
	g := (*G)(unsafe.Pointer(taskG(task)))
	if g == nil {
		return
	}
	pin()
	readyG(g)
	unpin()
}

// yield lets other goroutines run. On a scheduler stack it does nothing.
//
//export goos_g_yield
func yield() {
	ensureStarted()
	pin()
	m := currentM()
	g := m.curG
	if g == nil || g.pinned >= 0 {
		unpin()
		if g != nil {
			sys.ThreadYield()
		}
		return
	}
	switchOut(m, g, reasonYield, 0)
	unpin()
}

// sleep parks the goroutine for at least duration nanoseconds.
//
//export goos_g_sleep
func sleep(duration int64) {
	ensureStarted()
	when := nanotime() + duration
	pin()
	m := currentM()
	g := m.curG
	if g == nil {
		unpin()
		waitOwn(m, when)
		return
	}
	switchOut(m, g, reasonSleep, when)
	unpin()
}

// exit ends the running goroutine. goexit is set for runtime.Goexit.
//
//export goos_g_exit
func exit(goexit bool) {
	pin()
	m := currentM()
	g := m.curG
	switch {
	case g == nil:
		fatal("goroutine: exit on a scheduler stack")
	case g.kind == kindPinned:
		unpin()
		exitPinned(m)
		arena.ThreadExiting()
		sys.ThreadExit(0)
	case g.kind == kindMain:
		if !goexit {
			sys.Exit(0)
		}
		sched.mainGone = true
	}
	switchOut(m, g, reasonExit, 0)
	fatal("goroutine: a goroutine ran after it ended")
}

// mainReturned ends the process when the main function returns, whatever
// other goroutines still do.
//
//export goos_main_returned
func mainReturned() { sys.Exit(0) }

//export goos_g_num_cpu
func numCPU() int {
	ensureStarted()
	return int(onlineProcs())
}

// maxProcs sets how many threads may run goroutines at once when n is
// positive, and returns the previous value.
//
//export goos_g_max_procs
func maxProcs(n int) int {
	ensureStarted()
	if n <= 0 {
		return int(sched.maxProcs.Load())
	}
	n = min(n, int(defaultProcs()))
	sched.lock.lock()
	previous := int(sched.maxProcs.Load())
	sched.maxProcs.Store(int32(n))
	sched.free.Add(int32(n - previous))
	if n > previous {
		sched.createFailed = false
	}
	sched.lock.unlock()
	pin()
	for count := previous; count < n; count++ {
		wakep()
	}
	unpin()
	return previous
}

//export goos_g_count
func count() int { return int(sched.live.Load()) }

// timerAdded notes a time.Timer due at when.
//
//export goos_g_timer_added
func timerAdded(when int64) {
	for {
		current := sched.runtimeEarliest.Load()
		if when >= current || sched.runtimeEarliest.CompareAndSwap(current, when) {
			break
		}
	}
	noteDeadline(when)
}

//export goos_g_lock
func lock(word *uint32) { spinAt(word).lock() }

//export goos_g_unlock
func unlock(word *uint32) { spinAt(word).unlock() }

//export goos_g_thread_yield
func osYield() { sys.ThreadYield() }

const pageSize = 4096

// currentStack returns the bounds of the stack the caller runs on.
func currentStack() (uintptr, uintptr) {
	sp := stackPointer()
	slot := tls.Slot()
	if lo, hi, _, ok := locate(slot, sp, arena.NativeStack); ok {
		return lo, hi
	}
	return arena.NativeStack(slot)
}

// planCollection is the collector's Plan hook (roots.go).
func planCollection(self arena.HeldThread, held []arena.HeldThread) bool {
	var others [tls.Slots]HeldThread
	count := 0
	for index := range held {
		if count < len(others) {
			others[count] = HeldThread{Slot: held[index].Slot, SP: held[index].SP}
			count++
		}
	}
	return planStacks(HeldThread{Slot: self.Slot, SP: self.SP}, others[:count], arena.NativeStack)
}

var _ = kcall.StatusOK
