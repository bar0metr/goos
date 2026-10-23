//go:build goos_userspace && goos_goroutines

package goroutine

import (
	"unsafe"

	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/thread"
	"goos/userspace/runtime/tls"
)

// thread.Start threads (ADR 0204). A function thread.Start runs keeps its
// thread: it runs as a goroutine pinned to the thread, on the thread's own
// stack, so it can block on channels and locks like any goroutine. While it
// is parked its thread waits for its wake flag, on a scheduler stack the
// runtime makes for it. Pinned threads run outside GOMAXPROCS.

func installThreadHooks() {
	thread.SetHooks(thread.Hooks{Starting: ensureStarted, Run: runPinned, Exiting: exitPinnedSelf})
}

// runPinned runs fn(argument) as the pinned goroutine of the thread in slot.
func runPinned(fn func(uintptr), argument uintptr, slot int) {
	m := &ms[slot]
	m.queue = runQueue{}
	if !makeSchedulerStack(m, 0) {
		fatal("goroutine: no scheduler stack for a thread")
	}
	g := allocG()
	if g == nil {
		fatal("goroutine: limit of goroutines reached by a thread")
	}
	g.kind, g.pinned = kindPinned, int32(slot)
	g.lo, g.hi = arena.NativeStack(slot)
	g.sp = g.hi
	g.task = staticTask(taskPinned + slot)
	setTaskG(g.task, uintptr(unsafe.Pointer(g)))
	g.m.Store(int32(slot))
	m.pinnedG, m.curG = g, g
	block := tls.Current()
	block.G, block.Task = uintptr(unsafe.Pointer(g)), g.task
	g.status.Store(gRunning)
	noteLive()
	sched.lock.lock()
	sched.pinned++
	sched.lock.unlock()
	initThread(m, int32(slot), mPinned)
	fn(argument)
	exitPinned(m)
}

// exitPinnedSelf is thread.Exit's hook.
func exitPinnedSelf() {
	if m := currentM(); m != nil && m.kind.Load() == mPinned {
		exitPinned(m)
	}
}

// exitPinned forgets the calling pinned thread and its goroutine before the
// thread ends.
func exitPinned(m *M) {
	g := m.pinnedG
	g.status.Store(gDead)
	setTaskG(g.task, 0)
	freeG(g)
	m.kind.Store(mNone)
	m.curG, m.pinnedG = nil, nil
	block := tls.Current()
	block.G, block.M, block.Task = 0, 0, 0
	freeStack(m.sysBase, SchedulerStackSize)
	m.sysBase, m.sysLo, m.sysHi, m.sysSP = 0, 0, 0, 0
	sched.lock.lock()
	sched.pinned--
	sched.lock.unlock()
}
