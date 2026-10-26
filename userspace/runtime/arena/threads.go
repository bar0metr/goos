//go:build goos_userspace

package arena

import (
	"sync/atomic"
	"unsafe"

	"goos/internal/validation"
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
	// threadAlloc is the fast path's state (window.go).
	threadAlloc
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
	// gcEpoch counts completed collections; gcFlag is in window.go.
	gcEpoch atomic.Uint64
	// snapshots receives the held threads' state for the collection that
	// runs; it is a global, so marking scans it, and it is cleared after.
	snapshots     [kcall.MaxThreadSnapshots * kcall.ThreadSnapshotSize]byte
	snapshotCount int
	// suspendFailures counts collections given up because the other
	// threads could not all be held.
	suspendFailures atomic.Uint64
	suspensions     atomic.Uint64
	// allocWindowRetries and regionWindowRetries count holds let go
	// because a held thread was inside an allocator window or a region
	// transition.
	allocWindowRetries  atomic.Uint64
	regionWindowRetries atomic.Uint64
	// lastRendezvous and maxRendezvous are how long holding the other
	// threads took, in nanoseconds.
	lastRendezvous, maxRendezvous, totalRendezvous uint64
	// peakThreads is the most threads that were live at once.
	peakThreads uint32
	// threadCaches turns the owned slabs on; with it off every small
	// allocation takes the heap lock (UseThreadCaches).
	threadCaches = true
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

// UseThreadCaches turns thread-owned slabs on or off. It takes effect only
// before the second thread starts; benchmarks use it to compare the heap
// lock alone with the owned slabs.
func UseThreadCaches(on bool) {
	if !threaded.Load() {
		threadCaches = on
	}
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
	native.returnCache(&t.cache, &t.counts)
	t.fold(&native)
	t.allocated = false
	t.idle.Store(0)
	t.live = true
	live := uint32(0)
	for index := range threads {
		if threads[index].live {
			live++
		}
	}
	peakThreads = max(peakThreads, live)
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
	native.returnCache(&t.cache, &t.counts)
	t.fold(&native)
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
	// The holder stays on its thread until it lets go: a goroutine
	// switched away while it held the heap lock would stop every other
	// goroutine of its thread that allocates (ADR 0204).
	tls.Pin()
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

func (l *spinLock) unlock() {
	l.held.Store(0)
	tls.Unpin()
}

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

// collectThreaded is a collection with several threads: the caller becomes
// the collection's owner, holds every other thread off its processor, and
// collects over their roots. A thread that asks while another collects
// waits for that collection and does not run another.
func collectThreaded(slot int, reason collectionReason) {
	epoch := gcEpoch.Load()
	gcLock.lock()
	if gcEpoch.Load() != epoch {
		gcLock.unlock()
		return
	}
	heapLock.lock()
	gcFlag.Store(1)
	completed := false
	switch {
	case !suspendOthers():
		suspendFailures.Add(1)
	case reason == collectIdle && !othersIdle(slot):
		// A thread found work after CollectIdle looked: the process is
		// not idle, and an idle collection would take back the slabs it
		// allocates from.
		native.diag.idleAbandons++
		releaseHeld()
	default:
		// From here an allocation would wait for the heap lock this
		// thread holds; alloc stops the process instead.
		collecting = true
		allocators := prepareThreads(reason)
		// A stack pointer in no known stack means the roots are not all
		// known: nothing is marked or swept (ADR 0204).
		completed = planStacks(slot, stackPointer())
		if completed {
			if validation.Enabled {
				noteSwitchHolds()
			}
			native.allocators = allocators
			base, _ := currentStack()
			scrubStack(base)
			collect(reason)
			for index := range threads {
				native.checkCache(&threads[index].cache, uint32(index+1))
				threads[index].allocated = false
			}
		}
		collecting = false
		releaseHeld()
		if completed {
			gcEpoch.Add(1)
		} else {
			suspendFailures.Add(1)
		}
	}
	if !completed {
		native.diag.abandoned[reason]++
	}
	gcFlag.Store(0)
	heapLock.unlock()
	gcLock.unlock()
}

// prepareThreads makes the held threads' heap state ready for marking and
// returns how many live threads allocated since the last collection, at
// least one. Every thread's counts are folded and every remote free is
// folded into its slab, so the mark bitmaps are clear. An automatic
// collection leaves each thread that allocated its slabs; any other
// collection, and a thread that did not allocate, gives them back. Every
// thread is held outside the fast path, or is the caller.
//
// Each step leaves the heap consistent, so a collection given up after it
// leaves a heap the next one can start from.
func prepareThreads(reason collectionReason) uint32 {
	allocators := uint32(0)
	for index := range threads {
		t := &threads[index]
		active := t.live && (t.allocated || t.counts.allocs != 0)
		if active {
			allocators++
		}
		if active && reason == collectAutomatic {
			native.diag.preserved += native.drainCache(&t.cache, &t.counts)
		} else {
			native.diag.flushed += native.returnCache(&t.cache, &t.counts)
		}
		t.fold(&native)
	}
	native.foldStrayFrees()
	return max(allocators, 1)
}

// othersIdle reports whether every live thread but slot waits in an idle
// wait.
func othersIdle(slot int) bool {
	return idleQuorum(slot, len(threads), func(index int) *threadAlloc { return &threads[index].threadAlloc })
}

// releaseHeld lets the held threads go.
func releaseHeld() {
	clear(snapshots[:snapshotCount*int(kcall.ThreadSnapshotSize)])
	snapshotCount = 0
	if sys.ThreadResume() != kcall.StatusOK {
		panic("arena: held threads could not be released")
	}
}

// IdleEnter marks the calling thread as waiting with nothing to run, until
// IdleLeave. wait.Any and the goroutine scheduler's idle wait call it, and
// the scheduler clears it whenever it runs a goroutine. A thread marked
// idle while it still runs only makes an idle collection more eager; roots
// and marking do not depend on it.
func IdleEnter() {
	tls.Pin()
	self().idle.Store(1)
	tls.Unpin()
}

// IdleLeave clears the calling thread's idle mark.
func IdleLeave() {
	tls.Pin()
	if t := self(); t.idle.Load() != 0 {
		t.idle.Store(0)
	}
	tls.Unpin()
}

// suspendAttempts bounds how often a collection tries to hold the other
// threads before it gives up.
const suspendAttempts = 4

// suspendOthers holds every other thread and records their snapshots. A
// thread caught inside an allocation or region-transition window is let
// go to finish it before another hold. The raised gcFlag keeps allocation
// fast paths out; region transitions may require another bounded retry.
func suspendOthers() bool {
	started := nativeTicks()
	for attempt := 0; attempt < suspendAttempts; attempt++ {
		count, status := sys.ThreadSuspend(snapshots[:])
		if status == kcall.StatusOK {
			switch heldWindow(count) {
			case windowNone:
				snapshotCount = count
				suspensions.Add(1)
				noteRendezvous(uint64(nativeTicks() - started))
				return true
			case windowRegion:
				regionWindowRetries.Add(1)
			default:
				allocWindowRetries.Add(1)
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
	native.diag.rendezvous[histBucket(elapsed)]++
	lastRendezvous = elapsed
	totalRendezvous += elapsed
	if elapsed > maxRendezvous {
		maxRendezvous = elapsed
	}
}

// heldWindow reports the window a held thread has unpublished allocator or
// region state in: windowRegion when any is inside a region transition,
// otherwise windowAlloc when any is inside an allocator window.
func heldWindow(count int) int {
	found := windowNone
	for index := 0; index < count; index++ {
		slot := int(snapshotWord(index, 1))
		if slot >= 0 && slot < tls.Slots {
			found = max(found, threads[slot].window())
		}
	}
	return found
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
	if stackRoots.Mark != nil {
		stackRoots.Mark(markStackRange)
		return
	}
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

// allocCached is a small allocation once threads exist. It takes a slot
// from the thread's own slabs without a lock; when they are full, or a
// collection is starting, it takes the heap lock and refills. The window
// the fast path runs in is in window.go.
func allocCached(t *threadState, size uintptr, noPointers bool) uintptr {
	class := native.classOf[(size+15)>>4]
	if address := t.allocFast(&native, class, size, noPointers); address != 0 {
		return address
	}
	owner := uint32(selfSlot() + 1)
	heapLock.lock()
	t.fold(&native)
	if native.liveBytes >= native.nextCollection && !collecting {
		heapLock.unlock()
		collectPinned(collectAutomatic)
		heapLock.lock()
	}
	address := uintptr(0)
	if native.refill(&t.cache, &t.counts, class, owner, cacheTarget(class)) {
		address = native.cacheAlloc(&t.cache, &t.counts, class, noPointers)
	}
	if address == 0 && !collecting {
		heapLock.unlock()
		collectPinned(collectPressure)
		heapLock.lock()
		native.trim()
		// One slab: a heap that ran out keeps what it has for others.
		if native.refill(&t.cache, &t.counts, class, owner, 1) {
			address = native.cacheAlloc(&t.cache, &t.counts, class, noPointers)
		}
	}
	// The allocation after the fold counted again, under the lock.
	t.noteUnfolded()
	heapLock.unlock()
	if address == 0 {
		fail()
	}
	return address
}

// anyUnfolded reports whether some thread counted what it has not folded:
// allocations or frees since its last refill.
func anyUnfolded() bool {
	for index := range threads {
		if threads[index].unfolded.Load() != 0 {
			return true
		}
	}
	return false
}

// freeThreaded is free once threads exist. A slot of a slab some thread
// owns is freed without the heap lock: into the slab by its owner, into its
// remote-free bits by anyone else. Everything else takes the lock.
func freeThreaded(t *threadState, address uintptr) {
	owner := uint32(selfSlot() + 1)
	// The reservation's bounds never change, unlike the frontier the heap
	// lock guards; a page past the frontier has no owner and takes the
	// locked path, which checks it.
	if threadCaches && ready && address >= native.base && address-native.base < uintptr(native.capacity)<<pageShift &&
		t.freeFast(&native, address, owner) {
		return
	}
	heapLock.lock()
	if inRegion(address) {
		heapLock.unlock()
		return
	}
	if !ready || !native.contains(address) {
		heapLock.unlock()
		panic("arena: free of a pointer outside the heap")
	}
	// The owner may have changed since the try above; under the lock
	// only an owned slab's owner still changes it without the lock.
	t.noteUnfolded()
	if !t.freeOwned(&native, address, owner) {
		native.freeAt(address)
	}
	heapLock.unlock()
}

// threadDiagnostics fills the thread part of d. The caller holds the heap
// lock. The thread counters in d are the folded ones (foldThread).
func threadDiagnostics(d *Diagnostics) {
	d.ThreadsLive, d.ThreadsPeak = 0, peakThreads
	for index := range threads {
		if threads[index].live {
			d.ThreadsLive++
		}
	}
	d.HeapLocks, d.HeapContended = heapLock.acquired.Load(), heapLock.contended.Load()
	d.GCLocks, d.GCContended = gcLock.acquired.Load(), gcLock.contended.Load()
	d.Suspensions, d.SuspendFailures = suspensions.Load(), suspendFailures.Load()
	d.AllocWindowRetries, d.RegionWindowRetries = allocWindowRetries.Load(), regionWindowRetries.Load()
	d.RendezvousLast, d.RendezvousMax, d.RendezvousTotal = lastRendezvous, maxRendezvous, totalRendezvous
	d.PauseLast, d.PauseMax, d.PauseTotal = lastPause, maxPause, totalPause
	d.UnfoldedThreads = 0
	for index := range threads {
		if threads[index].unfolded.Load() != 0 {
			d.UnfoldedThreads++
		}
	}
}
