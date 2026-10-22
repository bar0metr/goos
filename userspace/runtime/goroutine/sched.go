//go:build goos_userspace && goos_goroutines

package goroutine

import (
	"encoding/binary"
	"sync/atomic"
	"unsafe"

	"goos/internal/cpuinfo"
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/runtime/tls"
)

//go:linkname swap goos_g_swap
func swap(next uintptr, save *uintptr)

//go:linkname entryAddress goos_g_entry_address
func entryAddress() uintptr

//go:linkname mstartAddress goos_g_mstart_address
func mstartAddress() uintptr

//go:linkname stackPointer goos_g_stack_pointer
func stackPointer() uintptr

//go:linkname nanotime goos_runtime_ticks
func nanotime() int64

//go:linkname staticTask goos_task_static
func staticTask(index int) uintptr

// The static tasks (tinygo/goroutines/task_stack.go.txt): one per thread
// slot for its scheduler stack, the main function's, and one per slot for a
// thread.Start function.
const (
	taskMain   = 16
	taskPinned = 17
)

//go:linkname taskG goos_task_g
func taskG(task uintptr) uintptr

//go:linkname setTaskG goos_task_set_g
func setTaskG(task uintptr, g uintptr)

//go:linkname runtimeTimerNext goos_runtime_timer_next
func runtimeTimerNext() int64

//go:linkname runtimeTimerRun goos_runtime_timer_run
func runtimeTimerRun(now int64)

func pin()         { tls.Pin() }
func unpin()       { tls.Unpin() }
func threadYield() { sys.ThreadYield() }

const never = int64(^uint64(0) >> 1)

var sched struct {
	// lock guards the global queue, the idle list, the thread counts, and
	// the free list of the goroutine table.
	lock   spin
	global globalQueue
	// idle is the first idle thread's slot plus one; idleNext links the
	// others.
	idle  int32
	nidle atomic.Int32
	// workers counts the threads that run any goroutine, the main thread
	// included; pinned the threads of thread.Start.
	workers, pinned int32
	createFailed    bool
	maxProcs        atomic.Int32
	// free counts the permits no thread holds. GOMAXPROCS can take it
	// below zero; a thread that sees that gives its permit back.
	free     atomic.Int32
	spinning atomic.Int32
	freeG    uint32
	limit    int32
	live     atomic.Int32
	peak     int32
	created  uint64
	nextID   uint64
	started  atomic.Bool
	// mainGone is set when the main function called runtime.Goexit.
	mainGone bool
	// poller is the slot plus one of the thread waiting on the kernel for
	// handles and timers, and pollUntil the time that wait ends.
	poller    atomic.Int32
	pollUntil atomic.Int64
	// timers holds sleeping goroutines and wait deadlines; earliest is its
	// first entry's time, and runtimeEarliest a bound on the first
	// time.Timer's.
	timers          spin
	heap            sleepHeap
	earliest        atomic.Int64
	runtimeEarliest atomic.Int64
	wakes           atomic.Uint64
	// firing counts the threads between taking due timers off the heap
	// and making their goroutines runnable: a goroutine is then in no queue
	// and on no timer, yet not asleep.
	firing atomic.Int32
	// dispatches counts switches to goroutines on every thread, for the
	// deadlock check's second look.
	dispatches atomic.Uint64
}

func currentM() *M { return (*M)(unsafe.Pointer(tls.Current().M)) }

// ensureStarted starts the scheduler on the main thread the first time a
// goroutine operation runs.
func ensureStarted() {
	if sched.started.Load() {
		return
	}
	if tls.Slot() != 0 {
		fatal("goroutine: the runtime must start on the main thread")
	}
	start()
}

// start adopts the running main function as the main goroutine and makes
// the main thread a worker with a scheduler stack of its own.
func start() {
	sched.limit = DefaultLimit
	for index := MaxGoroutines - 1; index >= 0; index-- {
		gs[index].next = sched.freeG
		gs[index].m.Store(-1)
		sched.freeG = uint32(index) + 1
	}
	sched.earliest.Store(never)
	sched.runtimeEarliest.Store(never)
	sched.pollUntil.Store(never)
	procs := defaultProcs()
	sched.maxProcs.Store(procs)
	sched.free.Store(procs - 1)
	sched.workers = 1
	m := &ms[0]
	if !makeSchedulerStack(m, 0) {
		fatal("goroutine: no scheduler stack for the main thread")
	}
	g := allocG()
	if g == nil {
		fatal("goroutine: no goroutine slot for main")
	}
	g.kind, g.pinned = kindMain, -1
	g.lo, g.hi = arena.MainStack()
	g.sp = g.hi
	g.task = staticTask(taskMain)
	setTaskG(g.task, uintptr(unsafe.Pointer(g)))
	tls.Current().Task = g.task
	g.m.Store(0)
	m.permit = true
	m.curG = g
	tls.Current().G = uintptr(unsafe.Pointer(g))
	g.status.Store(gRunning)
	sched.live.Store(1)
	sched.peak = 1
	sched.created = 1
	initThread(m, 0, mWorker)
	roots := arena.StackRoots{Current: currentStack, Plan: planCollection, Mark: markPlanned}
	if validation.Enabled {
		roots.Switch = switchAt
	}
	arena.SetStackRoots(roots)
	sys.ThreadPreempt(preemptEntryAddress(), PreemptTicks)
	sched.started.Store(true)
}

// defaultProcs caps CPU parallelism by this process's task quota.
func defaultProcs() int32 {
	var info [kcall.ProcessInfoSize]byte
	limit := int32(1)
	if sys.ProcessInfo(kcall.ProcessInfoSelf, &info) == kcall.StatusOK {
		offset := kcall.ProcessInfoLimitsOffset + 4*kcall.ProcessTasks
		limit = int32(binary.LittleEndian.Uint32(info[offset : offset+4]))
	}
	return parallelism(onlineProcs(), limit)
}

func onlineProcs() int32 {
	var summary [kcall.CPUInfoSize]byte
	if sys.CPUInfo(&summary) == kcall.StatusOK {
		if decoded, ok := cpuinfo.DecodeSummary(summary[:]); ok && decoded.OnlineCPUs > 0 {
			return int32(decoded.OnlineCPUs)
		}
	}
	return 1
}

// initThread makes slot's thread known to the scheduler: its ID, which
// wakes it, and its control block's record. kind is set last, so a
// collection never sees a thread whose stacks are not set.
func initThread(m *M, slot int32, kind uint32) {
	id, _, status := sys.ThreadSelf()
	if status != kcall.StatusOK {
		fatal("goroutine: a thread cannot name itself")
	}
	m.slot, m.id = slot, id
	m.sysTask = staticTask(int(slot))
	m.idleNext = 0
	m.wakePending.Store(0)
	sys.ThreadWakeTake()
	block := tls.Of(int(slot))
	block.M, block.ID = uintptr(unsafe.Pointer(m)), id
	m.kind.Store(kind)
}

// writeFrame lays out at the top of a new stack what goos_g_swap pops:
// the six callee-saved registers, with r12 and r13 given, then the
// address it returns to. It returns the stack pointer to swap to.
func writeFrame(hi, r12, r13, pc uintptr) uintptr {
	sp := hi - 7*8
	frame := (*[7]uintptr)(unsafe.Pointer(sp))
	frame[0], frame[1], frame[2], frame[3] = 0, 0, r12, r13
	frame[4], frame[5], frame[6] = 0, 0, pc
	return sp
}

// makeSchedulerStack gives a thread that came with a goroutine on its own
// stack a scheduler stack that starts in goos_g_mloop.
func makeSchedulerStack(m *M, argument uintptr) bool {
	base, ok := reserveStack(SchedulerStackSize)
	if !ok {
		return false
	}
	m.sysBase = base
	m.sysLo = base + GuardSize
	m.sysHi = m.sysLo + SchedulerStackSize
	m.sysSP = writeFrame(m.sysHi, 0, argument, mstartAddress())
	return true
}

// allocG takes a free goroutine slot under the limit.
func allocG() *G {
	sched.lock.lock()
	if sched.live.Load() >= sched.limit || sched.freeG == 0 {
		sched.lock.unlock()
		return nil
	}
	index := sched.freeG - 1
	g := &gs[index]
	sched.freeG = g.next
	g.next = 0
	sched.nextID++
	g.id = sched.nextID
	live := sched.live.Add(1)
	if live > sched.peak {
		sched.peak = live
	}
	sched.lock.unlock()
	g.gen++
	g.base, g.size, g.sp, g.lo, g.hi = 0, 0, 0, 0, 0
	g.m.Store(-1)
	g.sleep = 0
	g.wait.state.Store(waitIdle)
	g.wait.cancel = nil
	return g
}

// freeG returns a dead goroutine's slot.
func freeG(g *G) {
	g.task = 0
	g.lo, g.hi, g.sp = 0, 0, 0
	g.status.Store(gFree)
	sched.lock.lock()
	g.next = sched.freeG
	sched.freeG = indexOf(g) + 1
	sched.live.Add(-1)
	sched.lock.unlock()
}

// noteLive counts a new goroutine.
func noteLive() {
	sched.lock.lock()
	sched.created++
	sched.lock.unlock()
}

// switchOut parks the calling goroutine on its thread's scheduler stack for
// reason. The caller has pinned the thread exactly once; the pin passes to
// the scheduler stack, and the scheduler stack's pin passes back when the
// goroutine runs again, possibly on another thread.
func switchOut(m *M, g *G, reason uint8, when int64) {
	if arena.InRegion() {
		fatal("goroutine: cannot park or migrate inside an M-owned arena region")
	}
	if tls.Current().Pins != 1 {
		fatal("goroutine: switch while pinned to the thread (in a system call, an allocation, or a runtime lock)")
	}
	m.reason, m.when = reason, when
	swap(m.sysSP, &g.sp)
}

// execute runs g on m until it switches out.
func execute(m *M, g *G) {
	g.dispatchState()
	g.m.Store(m.slot)
	m.curG = g
	block := tls.Current()
	block.G, block.Task = uintptr(unsafe.Pointer(g)), g.task
	m.dispatches++
	m.counters.dispatches.Add(1)
	sched.dispatches.Add(1)
	swap(g.sp, &m.sysSP)
	afterSwitch(m)
}

// afterSwitch runs on m's scheduler stack once its goroutine switched out.
func afterSwitch(m *M) {
	g := m.curG
	m.curG = nil
	block := tls.Current()
	block.G, block.Task = 0, m.sysTask
	g.m.Store(-1)
	reason := m.reason
	m.reason = reasonNone
	switch reason {
	case reasonPark:
		m.counters.parks.Add(1)
		if g.parkState() {
			enqueue(m, g)
		}
	case reasonYield, reasonPreempt:
		g.rescheduleState()
		globalPut(g)
		wakep()
	case reasonSleep:
		g.sleep++
		seq := g.sleep
		if g.parkState() {
			enqueue(m, g)
		} else {
			addTimer(sleepEntry{when: m.when, index: indexOf(g), gen: g.gen, seq: seq, kind: sleepWake})
		}
	case reasonExit:
		exitG(g)
	default:
		fatal("goroutine: switch without a reason")
	}
}

// exitG frees a goroutine that has ended. Its stack is unused now: the
// thread runs on its scheduler stack.
func exitG(g *G) {
	g.status.Store(gDead)
	if g.base != 0 {
		freeStack(g.base, g.size)
	}
	setTaskG(g.task, 0)
	freeG(g)
	if sched.live.Load() == 0 && sched.mainGone {
		deadlock()
	}
}

// readyG makes a parked goroutine runnable, or tells a running one that is
// about to park to run on.
func readyG(g *G) {
	if g.readyState() {
		enqueue(currentM(), g)
	}
}

// enqueue puts a runnable goroutine where a thread will find it: on its
// own thread when it is pinned, on the calling worker's queue, or on the
// global queue, and makes sure a thread looks for it.
func enqueue(m *M, g *G) {
	sched.wakes.Add(1)
	if g.pinned >= 0 {
		wakeThread(&ms[g.pinned])
		return
	}
	if m != nil && m.kind.Load() == mWorker && m.permit {
		if !m.queue.put(indexOf(g)) {
			overflow(m, g)
		}
	} else {
		globalPut(g)
	}
	wakep()
}

// overflow moves half of a full queue and g to the global queue.
func overflow(m *M, g *G) {
	var moved [queueSize / 2]uint32
	count := m.queue.steal(&moved)
	sched.lock.lock()
	for index := 0; index < count; index++ {
		sched.global.push(gs[:], moved[index])
	}
	sched.global.push(gs[:], indexOf(g))
	sched.lock.unlock()
}

func globalPut(g *G) {
	sched.lock.lock()
	sched.global.push(gs[:], indexOf(g))
	sched.lock.unlock()
}

func globalGet() *G {
	if sched.global.length.Load() == 0 {
		return nil
	}
	sched.lock.lock()
	index, ok := sched.global.pop(gs[:])
	sched.lock.unlock()
	if !ok {
		return nil
	}
	return &gs[index]
}

// takePermit claims a permit to run goroutines.
func takePermit() bool {
	for {
		free := sched.free.Load()
		if free <= 0 {
			return false
		}
		if sched.free.CompareAndSwap(free, free-1) {
			return true
		}
	}
}

// dropPermit gives m's permit back, and with it the goroutines m queued.
func dropPermit(m *M) {
	if !m.permit {
		return
	}
	var moved [queueSize / 2]uint32
	for {
		count := m.queue.steal(&moved)
		if count == 0 {
			break
		}
		sched.lock.lock()
		for index := 0; index < count; index++ {
			sched.global.push(gs[:], moved[index])
		}
		sched.lock.unlock()
	}
	if index, ok := m.queue.get(); ok {
		globalPut(&gs[index])
	}
	m.permit = false
	sched.free.Add(1)
}

func (m *M) stopSpinning() {
	if m.spinning {
		m.spinning = false
		sched.spinning.Add(-1)
	}
}

// wakep starts a thread looking for work when a permit is free and no
// thread is looking already.
func wakep() {
	if sched.free.Load() <= 0 || sched.spinning.Load() != 0 || !sched.spinning.CompareAndSwap(0, 1) {
		return
	}
	sched.lock.lock()
	var target *M
	if sched.idle != 0 {
		target = &ms[sched.idle-1]
		sched.idle = target.idleNext
		target.idle = false
		target.wokenSpinning = true
		sched.nidle.Add(-1)
	}
	create := target == nil && !sched.createFailed && sched.workers < sched.maxProcs.Load() && sched.workers+sched.pinned < tls.Slots
	if create {
		sched.workers++
	}
	sched.lock.unlock()
	if target != nil {
		wakeThread(target)
		return
	}
	if create && makeWorker() {
		return
	}
	sched.spinning.Add(-1)
}

// makeWorker starts a thread that runs goroutines, already counted as
// spinning. A thread the task quota refuses lowers the parallelism the
// process gets instead of failing anything.
func makeWorker() bool {
	arena.EnableThreads()
	if _, _, status := sys.WorkerCreate(1); status != kcall.StatusOK {
		sched.lock.lock()
		sched.workers--
		sched.createFailed = true
		sched.lock.unlock()
		return false
	}
	return true
}

// workerMain is where a thread makeWorker started runs, on the stack the
// kernel made for it, which is its scheduler stack.
//
//export goos_worker_main
func workerMain(argument uintptr, slot uint64) {
	pin()
	tls.Current().Task = staticTask(int(slot))
	arena.ThreadStarted(int(slot))
	m := &ms[slot]
	m.sysLo, m.sysHi = arena.NativeStack(int(slot))
	m.sysBase = 0
	m.queue = runQueue{}
	initThread(m, int32(slot), mWorker)
	sys.ThreadPreempt(preemptEntryAddress(), PreemptTicks)
	m.spinning = argument&1 != 0
	workerLoop(m)
}

// mloop is where a scheduler stack the runtime made starts: the first time
// the goroutine of the main thread, or of a thread.Start thread, parks.
//
//export goos_g_mloop
func mloop(argument uintptr) {
	m := currentM()
	if m.curG != nil {
		afterSwitch(m)
	}
	if m.kind.Load() == mPinned {
		pinnedLoop(m)
	}
	workerLoop(m)
}

func workerLoop(m *M) {
	for {
		execute(m, findRunnable(m))
	}
}

// findRunnable returns the next goroutine m runs, waiting for one when
// there is none.
func findRunnable(m *M) *G {
	for {
		if m.permit && sched.free.Load() < 0 {
			// GOMAXPROCS went down: give the permit back.
			dropPermit(m)
		}
		if !m.permit {
			if !takePermit() {
				m.stopSpinning()
				idle(m)
				continue
			}
			m.permit = true
		}
		m.tick++
		if m.tick%8 == 0 || m.queue.length() == 0 {
			checkTimers()
		}
		// Readiness must progress even when a yielding producer keeps
		// both runnable queues nonempty.
		if m.tick%16 == 0 {
			pollNow(m)
		}
		if m.tick%61 == 0 {
			if g := globalGet(); g != nil {
				return found(m, g)
			}
		}
		if index, ok := m.queue.get(); ok {
			return found(m, &gs[index])
		}
		if g := globalGet(); g != nil {
			return found(m, g)
		}
		if !m.spinning && 2*sched.spinning.Load() < sched.maxProcs.Load()-sched.free.Load() {
			m.spinning = true
			sched.spinning.Add(1)
		}
		if m.spinning {
			if g := steal(m); g != nil {
				return found(m, g)
			}
		}
		if pollNow(m) {
			continue
		}
		dropPermit(m)
		m.stopSpinning()
		if anyWork() {
			continue
		}
		idle(m)
	}
}

// found hands g to m. A thread that found work while it was the one
// looking wakes another to look, so parallelism follows the work.
func found(m *M, g *G) *G {
	if m.spinning {
		m.stopSpinning()
		wakep()
	}
	return g
}

// steal takes half of another worker's queue.
func steal(m *M) *G {
	var stolen [queueSize / 2]uint32
	start := int(m.tick) % tls.Slots
	for round := 0; round < 2; round++ {
		for offset := 0; offset < tls.Slots; offset++ {
			victim := &ms[(start+offset)%tls.Slots]
			if victim == m || victim.kind.Load() != mWorker {
				continue
			}
			count := victim.queue.steal(&stolen)
			if count == 0 {
				continue
			}
			m.counters.steals.Add(1)
			m.counters.stolen.Add(uint64(count))
			for index := 1; index < count; index++ {
				if !m.queue.put(stolen[index]) {
					globalPut(&gs[stolen[index]])
				}
			}
			return &gs[stolen[0]]
		}
	}
	return nil
}

// anyWork reports whether a runnable goroutine waits anywhere.
func anyWork() bool {
	if sched.global.length.Load() != 0 {
		return true
	}
	for slot := range ms {
		if ms[slot].kind.Load() == mWorker && ms[slot].queue.length() != 0 {
			return true
		}
	}
	return false
}

// idle puts m on the idle list and waits until a wake, a handle, or a timer
// gives it something to do. The check after joining the list closes the
// race with a thread that made work runnable just before: either that
// thread found m on the list and woke it, or m sees the work here.
func idle(m *M) {
	sched.lock.lock()
	if !m.idle {
		m.idle = true
		m.idleNext = sched.idle
		sched.idle = m.slot + 1
		sched.nidle.Add(1)
	}
	sched.lock.unlock()
	if !anyWork() && !timersDue() {
		wait(m)
	}
	sched.lock.lock()
	if m.idle {
		removeIdle(m)
	}
	spinning := m.wokenSpinning
	m.wokenSpinning = false
	sched.lock.unlock()
	if spinning {
		m.spinning = true
	}
}

// removeIdle takes m off the idle list. The scheduler lock is held.
func removeIdle(m *M) {
	link := &sched.idle
	for *link != 0 {
		other := &ms[*link-1]
		if other == m {
			*link = m.idleNext
			break
		}
		link = &other.idleNext
	}
	m.idle = false
	sched.nidle.Add(-1)
}

// wakeThread sets the kernel's level-triggered wake flag. Do not skip the
// call based on wakePending: the target may have consumed the kernel flag
// without clearing that diagnostic mirror yet.
func wakeThread(t *M) {
	t.wakePending.Store(1)
	if status := sys.ThreadWake(t.id); status != kcall.StatusOK {
		t.wakePending.Store(0)
	}
}

// drainWake takes m's wake flag. wakePending is diagnostic only.
func drainWake(m *M) {
	sys.ThreadWakeTake()
	m.wakePending.Store(0)
}

// wakeInterest is a wait's interest in the thread's own wake flag.
var wakeInterest = sys.CursorInterest{Handle: handle.ID(kcall.WaitAnyThread), Events: kcall.WaitReadable}

// wakeIdle wakes one idle thread, so it takes the poller's place.
func wakeIdle() {
	if sched.nidle.Load() == 0 {
		return
	}
	sched.lock.lock()
	var target *M
	self := currentM()
	for link := sched.idle; link != 0; link = ms[link-1].idleNext {
		candidate := &ms[link-1]
		if candidate != self {
			target = candidate
			break
		}
	}
	sched.lock.unlock()
	if target != nil {
		wakeThread(target)
	}
}

// pinnedLoop runs a thread.Start thread's goroutine whenever it is
// runnable, and waits for it otherwise.
func pinnedLoop(m *M) {
	g := m.pinnedG
	for {
		if state := g.status.Load(); state == gRunnable || state == gRunnableWaking {
			execute(m, g)
			continue
		}
		waitOwn(m, never)
	}
}

// waitOwn waits for m's wake flag until deadline.
func waitOwn(m *M, deadline int64) {
	m.interests[0] = wakeInterest
	mask, _ := sys.WaitAnyCursor(&m.interests, 1, ticksUntil(deadline))
	if mask&1 != 0 {
		drainWake(m)
	}
}

// deadlock reports that every goroutine waits for another and ends the
// process, as Go does, after the state of every goroutine and thread.
func deadlock() {
	println("fatal error: all goroutines are asleep - deadlock!")
	dump()
	sys.Exit(2)
}

// dump prints the scheduler's state for a fatal report.
func dump() {
	println("goroutine: free", sched.free.Load(), "spinning", sched.spinning.Load(), "idle", sched.nidle.Load(),
		"workers", sched.workers, "global", sched.global.length.Load(), "live", sched.live.Load())
	for slot := range ms {
		m := &ms[slot]
		if m.kind.Load() == mNone {
			continue
		}
		current := uint64(0)
		if m.curG != nil {
			current = m.curG.id
		}
		println("thread", slot, "kind", m.kind.Load(), "idle", m.idle, "permit", m.permit, "spinning", m.spinning,
			"g", current, "queue", m.queue.length(), "wake", m.wakePending.Load())
	}
	for index := range gs {
		g := &gs[index]
		status := g.status.Load()
		if status == gFree {
			continue
		}
		println("goroutine", g.id, "status", status, "kind", g.kind, "thread", g.m.Load(), "wait", g.wait.state.Load(), "sleep", g.sleep)
	}
}

func fatal(message string) {
	println("fatal error:", message)
	sys.Exit(2)
}
