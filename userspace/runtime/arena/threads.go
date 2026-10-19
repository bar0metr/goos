//go:build goos_userspace

package arena

import (
	"sync/atomic"
	"unsafe"

	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"goos/userspace/runtime/tls"
)

// Threads (ADR 0203). A process runs one thread until it starts a second
// with userspace/runtime/thread; from then on every heap operation takes the
// heap lock, and a collection holds every other thread off its processor
// while it marks and sweeps. A process that never starts a thread takes no
// lock and suspends nothing.

// threadState is one thread's runtime state, found from its stack pointer.
// It holds no heap address the collector must not see: the region record
// innermost points to is in the regions list anyway.
type threadState struct {
	// regions and cursor are the thread's entered regions and the
	// allocation position of the innermost one.
	regions   regionStack
	cursor    allocatorPosition
	innermost *Region
	live      bool
	// fast is set while the thread is inside the allocation fast path.
	fast atomic.Uint32
	// cache is the thread's own slabs (cache.go).
	cache threadCache
	// counts is what the thread counted on its own.
	counts threadCounts
}

var (
	threads [tls.Slots]threadState
	// threaded is set once, before the second thread starts.
	threaded atomic.Bool
	// heapLock guards the heap, the regions list, and the counters of
	// every thread; gcLock makes one thread the owner of a collection.
	// gcLock comes first.
	heapLock spinLock
	gcLock   spinLock
	// gcFlag is raised while a collection begins and runs: a thread that
	// sees it does not start a fast-path allocation. gcEpoch counts
	// completed collections.
	gcFlag  atomic.Uint32
	gcEpoch atomic.Uint64
	// snapshots receives the held threads' state for the collection that
	// runs; it is a global, so marking scans it, and it is cleared after.
	snapshots     [kcall.MaxThreadSnapshots * kcall.ThreadSnapshotSize]byte
	snapshotCount int
	// suspendFailures counts collections given up because the other
	// threads could not all be held.
	suspendFailures atomic.Uint64
	suspensions     atomic.Uint64
	// lastRendezvous and maxRendezvous are how long holding the other
	// threads took, in nanoseconds.
	lastRendezvous, maxRendezvous, totalRendezvous uint64
)

// self returns the calling thread's state.
func self() *threadState {
	if !threaded.Load() {
		return &threads[0]
	}
	return &threads[tls.Slot()]
}

// selfSlot returns the calling thread's slot.
func selfSlot() int {
	if !threaded.Load() {
		return 0
	}
	return tls.Slot()
}

// EnableThreads sets the runtime up for a second thread. The thread package
// calls it before it starts one; it is the only change of mode, and only one
// thread exists when it runs.
func EnableThreads() {
	if !ready {
		Init()
	}
	if threaded.Load() {
		return
	}
	threads[0].live = true
	threaded.Store(true)
}

// Threaded reports whether the process has started a second thread.
func Threaded() bool { return threaded.Load() }

// ThreadStarted makes slot's state ready for the thread that runs in it.
func ThreadStarted(slot int) {
	if slot <= 0 || slot >= tls.Slots {
		panic("arena: thread started in an invalid slot")
	}
	t := &threads[slot]
	lockHeap()
	t.regions, t.cursor, t.innermost = regionStack{}, allocatorPosition{}, nil
	t.cache.reset()
	t.counts = threadCounts{}
	t.live = true
	unlockHeap()
}

// ThreadExiting gives back what the calling thread holds of the heap before
// it ends: its own slabs and its counts.
func ThreadExiting() {
	t := self()
	if t.regions.depth != 0 {
		panic("arena: thread ended inside a region")
	}
	lockHeap()
	native.returnCache(&t.cache)
	native.foldCounts(&t.counts)
	t.live = false
	unlockHeap()
}

// threadStack returns the bounds of slot's stack.
func threadStack(slot int) (base, top uintptr) {
	if slot == 0 {
		return stackBase(), stackTop
	}
	top = uintptr(kcall.ThreadStackTop(uint64(slot)))
	return top - uintptr(kcall.ThreadStackSize), top
}

// spinLock guards a short section. A waiter gives up its processor after a
// few tries, so a holder another processor runs, or one that was preempted,
// gets to finish.
type spinLock struct {
	held      atomic.Uint32
	acquired  atomic.Uint64
	contended atomic.Uint64
}

func (l *spinLock) lock() {
	if l.held.CompareAndSwap(0, 1) {
		l.acquired.Add(1)
		return
	}
	l.contended.Add(1)
	for tries := 0; !l.held.CompareAndSwap(0, 1); tries++ {
		if tries >= 32 {
			sys.ThreadYield()
			tries = 0
		}
	}
	l.acquired.Add(1)
}

func (l *spinLock) unlock() { l.held.Store(0) }

// lockHeap takes the heap lock once threads exist.
func lockHeap() {
	if threaded.Load() {
		heapLock.lock()
	}
}

func unlockHeap() {
	if threaded.Load() {
		heapLock.unlock()
	}
}

// collectThreaded is Collect with several threads: the caller becomes the
// collection's owner, holds every other thread off its processor, and
// collects over their roots. A thread that asks while another collects
// waits for that collection and does not run another.
func collectThreaded(slot int) {
	epoch := gcEpoch.Load()
	gcLock.lock()
	if gcEpoch.Load() != epoch {
		gcLock.unlock()
		return
	}
	heapLock.lock()
	gcFlag.Store(1)
	if suspendOthers() {
		base, _ := threadStack(slot)
		scrubStack(base)
		collect()
		clear(snapshots[:snapshotCount*int(kcall.ThreadSnapshotSize)])
		snapshotCount = 0
		if sys.ThreadResume() != kcall.StatusOK {
			panic("arena: held threads could not be released")
		}
		gcEpoch.Add(1)
	} else {
		suspendFailures.Add(1)
	}
	gcFlag.Store(0)
	heapLock.unlock()
	gcLock.unlock()
}

// suspendAttempts bounds how often a collection tries to hold the other
// threads before it gives up.
const suspendAttempts = 4

// suspendOthers holds every other thread and records their snapshots. A
// thread caught inside the allocation fast path is let go to finish it,
// and the hold is taken again: the raised gcFlag keeps it from starting
// another.
func suspendOthers() bool {
	started := nativeTicks()
	for attempt := 0; attempt < suspendAttempts; attempt++ {
		count, status := sys.ThreadSuspend(snapshots[:])
		if status == kcall.StatusOK {
			if !anyInFastPath(count) {
				snapshotCount = count
				suspensions.Add(1)
				noteRendezvous(uint64(nativeTicks() - started))
				return true
			}
			clear(snapshots[:count*int(kcall.ThreadSnapshotSize)])
			if sys.ThreadResume() != kcall.StatusOK {
				panic("arena: held threads could not be released")
			}
		} else if status != kcall.StatusTimeout {
			return false
		}
		sys.ThreadYield()
	}
	return false
}

func noteRendezvous(elapsed uint64) {
	lastRendezvous = elapsed
	totalRendezvous += elapsed
	if elapsed > maxRendezvous {
		maxRendezvous = elapsed
	}
}

// anyInFastPath reports whether a held thread was stopped inside the
// allocation fast path.
func anyInFastPath(count int) bool {
	for index := 0; index < count; index++ {
		slot := int(snapshotWord(index, 1))
		if slot >= 0 && slot < tls.Slots && threads[slot].fast.Load() != 0 {
			return true
		}
	}
	return false
}

// snapshotWord reads word of snapshot index.
func snapshotWord(index, word int) uint64 {
	offset := index*int(kcall.ThreadSnapshotSize) + word*8
	value := uint64(0)
	for shift := 0; shift < 8; shift++ {
		value |= uint64(snapshots[offset+shift]) << (8 * shift)
	}
	return value
}

// markThreads scans every held thread's registers and stack. The stack is
// read from 128 bytes below the saved stack pointer, the red zone a leaf
// function may keep values in, to the stack's top.
func markThreads() {
	native.phase = PhaseRegisters
	for index := 0; index < snapshotCount; index++ {
		record := index * int(kcall.ThreadSnapshotSize)
		start := uintptr(unsafe.Pointer(&snapshots[record+int(kcall.ThreadSnapshotRegisters)]))
		native.markRange(start, start+uintptr(kcall.ThreadSnapshotSize-kcall.ThreadSnapshotRegisters-8))
	}
	native.phase = PhaseStack
	for index := 0; index < snapshotCount; index++ {
		slot := int(snapshotWord(index, 1))
		sp := uintptr(snapshotWord(index, 2))
		if slot < 0 || slot >= tls.Slots {
			panic("arena: held thread in an invalid slot")
		}
		base, top := threadStack(slot)
		if sp < base || sp > top {
			panic("arena: held thread's stack pointer is outside its stack")
		}
		from := sp - 128
		if from < base {
			from = base
		}
		native.markRange(from, top)
	}
}
