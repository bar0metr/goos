package goroutine

import (
	"sync/atomic"
	"unsafe"

	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"goos/userspace/runtime/tls"
)

const (
	// MaxGoroutines bounds the goroutine table. Limit, which SetLimit
	// lowers or raises up to it, bounds the goroutines that exist at once.
	MaxGoroutines = 1024
	// DefaultLimit is the goroutines a process may have unless it asks for
	// more.
	DefaultLimit = 256
	// StackSize is the default stack reservation of a goroutine and
	// GuardSize the unmapped range below it. Only touched pages cost
	// memory.
	StackSize = 256 << 10
	GuardSize = 64 << 10
	// SchedulerStackSize is the stack the runtime makes for a thread that
	// came with a goroutine on its own stack: the main thread and those
	// thread.Start makes.
	SchedulerStackSize = 64 << 10
	// PreemptTicks is how many ticks of user time a thread runs between
	// two visits of the preemption entry.
	PreemptTicks = 2
	// queueSize is the capacity of a thread's run queue.
	queueSize = 256
	// MaxWaiters bounds the handle interests goroutines wait on at once:
	// one wait of the kernel covers them and the waiting thread's own
	// wake flag.
	MaxWaiters = kcall.WaitAnyMaxObjects - 1
)

// Goroutine states.
const (
	gFree uint32 = iota
	gRunnable
	gRunning
	// gWaking is gRunning with a wake that came before the goroutine
	// parked: it runs on instead of parking.
	gWaking
	// gRunnableWaking carries the same unconsumed wake through preemption
	// and migration. Only an attempted park consumes it.
	gRunnableWaking
	gParked
	gDead
)

// Goroutine kinds.
const (
	kindGo uint8 = iota
	kindMain
	kindPinned
)

// G is one goroutine. The table holds every G, so the collector finds each
// task through its globals.
type G struct {
	// task is the TinyGo task (internal/task.Task) of the goroutine.
	task   uintptr
	status atomic.Uint32
	// gen counts the uses of the slot; a reference that carries it never
	// names the slot's next goroutine.
	gen uint32
	// sp is the stack pointer saved when the goroutine last switched out.
	sp uintptr
	// lo and hi bound the stack; base and size are its reservation, guard
	// included, when the runtime made it.
	lo, hi     uintptr
	base, size uintptr
	// m is the slot of the thread running the goroutine, or -1.
	m atomic.Int32
	// pinned is the slot of the only thread that runs the goroutine, or -1.
	pinned int32
	// next links the goroutine into the global run queue or the free list,
	// as an index plus one.
	next uint32
	kind uint8
	// sleep counts the goroutine's sleeps; a timer entry that carries an
	// older count is stale.
	sleep uint32
	// wait is the goroutine's handle wait (poll.go).
	wait waitState
	// id is the goroutine's number, for diagnostics.
	id uint64
}

// waitState is a goroutine's wait for handles. state moves from waitIdle to
// waitArmed when the goroutine registers and to waitDone exactly once, by
// whichever completes it: the poller, a timeout, or a cancel.
type waitState struct {
	lock       spin
	state      atomic.Uint32
	generation uint32
	seq        uint32
	mask       uint64
	status     kcall.Status
	cancel     *Cancel
}

func (w *waitState) begin(generation uint32, cancel *Cancel) uint32 {
	w.lock.lock()
	w.generation = generation
	w.seq++
	w.mask, w.status, w.cancel = 0, kcall.StatusOK, cancel
	w.state.Store(waitArmed)
	seq := w.seq
	w.lock.unlock()
	return seq
}

// claim validates the identity and completes the wait under one lock. A
// delayed event from an earlier wait cannot claim a newly armed wait.
func (w *waitState) claim(generation, seq uint32, mask uint64, status kcall.Status) bool {
	w.lock.lock()
	defer w.lock.unlock()
	if w.generation != generation || w.seq != seq || w.state.Load() != waitArmed {
		return false
	}
	w.mask, w.status = mask, status
	w.state.Store(waitDone)
	return true
}

const (
	waitIdle uint32 = iota
	waitArmed
	waitDone
)

// Thread kinds.
const (
	mNone uint32 = iota
	// mWorker runs any goroutine.
	mWorker
	// mPinned runs only the goroutine of its thread.Start function.
	mPinned
)

// Why a goroutine switched to its thread's scheduler stack.
const (
	reasonNone uint8 = iota
	reasonPark
	reasonYield
	reasonPreempt
	reasonSleep
	reasonExit
)

// M is one thread of the process.
type M struct {
	slot int32
	kind atomic.Uint32
	// curG is the goroutine the thread runs, or nil on its scheduler
	// stack.
	curG *G
	// sysSP is the scheduler stack's pointer saved while a goroutine runs;
	// sysLo and sysHi bound that stack, and sysBase is its reservation
	// when the runtime made it.
	sysSP, sysLo, sysHi, sysBase uintptr
	// reason and when tell the scheduler stack why the goroutine switched.
	reason uint8
	when   int64
	queue  runQueue
	// id is the thread's ID, which ThreadWake names. wakePending is set
	// while a wake is on its way, so a thread is woken once however many
	// threads want it.
	id          uint64
	wakePending atomic.Uint32
	// permit is set while the thread may run goroutines; spinning while it
	// holds a permit and looks for work.
	permit, spinning bool
	// wokenSpinning is set by the thread that woke it to look for work.
	wokenSpinning bool
	idle          bool
	idleNext      int32
	// sysTask is the TinyGo task of the scheduler stack.
	sysTask uintptr
	// pinnedG is the goroutine of a pinned thread.
	pinnedG *G
	// dispatches counts the goroutines the thread switched to; seen is its
	// value at the last preemption visit.
	dispatches, seen uint64
	tick             uint32
	counters         mCounters
	// interests is the thread's argument to the kernel's wait.
	interests [kcall.WaitAnyMaxObjects]sys.Interest
}

// mCounters are one thread's counters. Only the thread writes them.
type mCounters struct {
	dispatches, steals, stolen, parks, preemptVisits, preemptions, polls atomic.Uint64
}

var (
	gs [MaxGoroutines]G
	ms [tls.Slots]M
)

// indexOf returns g's index in the table.
func indexOf(g *G) uint32 {
	return uint32((uintptr(unsafe.Pointer(g)) - uintptr(unsafe.Pointer(&gs[0]))) / unsafe.Sizeof(gs[0]))
}

// registration is one handle a goroutine waits for.
type registration struct {
	handle uint64
	events kcall.WaitEvent
	// index is the goroutine's table index plus one, zero for a free entry.
	index, gen, seq uint32
	// bit is the interest's position in the goroutine's own list.
	bit uint8
}

// Cancel ends the waits that name it. Its zero value is ready to use.
type Cancel struct {
	lock    spin
	done    bool
	count   int
	waiters [8]registration
}
