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
	// counts is what the thread counted on its own. Only the thread
	// changes it, inside the fast path or under the heap lock; it reaches
	// the heap's counters only through foldThread, and nothing else reads
	// it. unfolded is set while counts holds anything foldThread has not
	// folded.
	counts   threadCounts
	unfolded atomic.Uint32
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
	foldThread(t)
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
	foldThread(t)
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
		// From here an allocation would wait for the heap lock this
		// thread holds; alloc stops the process instead.
		collecting = true
		// Every other thread is held outside the fast path: their slabs
		// and counts come back, and the mark bitmaps hold marks again.
		for index := range threads {
			native.returnCache(&threads[index].cache, &threads[index].counts)
			foldThread(&threads[index])
		}
		native.foldStrayFrees()
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

// allocCached is a small allocation once threads exist. It takes a slot
// from the thread's own slab without a lock; when the slab is full, or a
// collection is starting, it takes the heap lock and refills.
//
// The fast path announces itself in t.fast before it reads gcFlag, and
// clears the announcement after the bitmap write. The collector raises
// gcFlag before it holds the threads and reads the announcements after, so
// a held thread either finished its write or is seen inside and let go to
// finish (suspendOthers).
func allocCached(t *threadState, size uintptr, noPointers bool) uintptr {
	class := native.classOf[(size+15)>>4]
	noteUnfolded(t)
	// The counts change only inside the announced window or under the
	// heap lock: a collection folds and clears them while the thread is
	// held, and must not find an update half done.
	t.fast.Store(1)
	if sizeProfile {
		bucket := (size - 1) >> 4
		t.counts.sizeCounts[bucket]++
		t.counts.sizeBytes[bucket] += uint64(size)
	}
	if gcFlag.Load() == 0 {
		if address := native.cacheAlloc(&t.cache, &t.counts, class, noPointers); address != 0 {
			t.fast.Store(0)
			return address
		}
	}
	t.fast.Store(0)
	owner := uint32(selfSlot() + 1)
	heapLock.lock()
	foldThread(t)
	if native.liveBytes >= native.nextCollection && !collecting {
		heapLock.unlock()
		Collect()
		heapLock.lock()
	}
	address := uintptr(0)
	if native.refill(&t.cache, &t.counts, class, owner) {
		address = native.cacheAlloc(&t.cache, &t.counts, class, noPointers)
	}
	if address == 0 && !collecting {
		heapLock.unlock()
		Collect()
		heapLock.lock()
		native.trim()
		if native.refill(&t.cache, &t.counts, class, owner) {
			address = native.cacheAlloc(&t.cache, &t.counts, class, noPointers)
		}
	}
	// The allocation after the fold counted again.
	noteUnfolded(t)
	heapLock.unlock()
	if address == 0 {
		fail()
	}
	return address
}

// noteUnfolded marks that t's counts hold something to fold. Only t calls
// it; the store happens once per fold.
func noteUnfolded(t *threadState) {
	if t.unfolded.Load() == 0 {
		t.unfolded.Store(1)
	}
}

// foldThread publishes t's counts into the heap's. The caller holds the heap
// lock and is t's thread, or t's thread is held or has ended.
func foldThread(t *threadState) {
	native.foldCounts(&t.counts)
	t.unfolded.Store(0)
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
	// The reservation's bounds never change, unlike the frontier the heap
	// lock guards; a page past the frontier has no owner and takes the
	// locked path, which checks it.
	if threadCaches && ready && address >= native.base && address-native.base < uintptr(native.capacity)<<pageShift {
		noteUnfolded(t)
		t.fast.Store(1)
		if gcFlag.Load() == 0 && freeOwned(t, address) {
			t.fast.Store(0)
			return
		}
		t.fast.Store(0)
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
	noteUnfolded(t)
	if !freeOwned(t, address) {
		native.freeAt(address)
	}
	heapLock.unlock()
}

// freeOwned frees address if it is a slot of an owned slab, and reports
// whether it was.
func freeOwned(t *threadState, address uintptr) bool {
	page, slot, owner, ok := native.ownedSlot(address)
	if owner == 0 {
		return false
	}
	if ok && owner == uint32(selfSlot()+1) {
		ok = native.cacheFree(&t.counts, page, slot)
	} else if ok {
		ok = native.remoteFree(&t.counts, page, slot)
	}
	if !ok {
		t.fast.Store(0)
		panic("arena: double free or free of a non-allocation")
	}
	return true
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
	d.RendezvousLast, d.RendezvousMax, d.RendezvousTotal = lastRendezvous, maxRendezvous, totalRendezvous
	d.PauseLast, d.PauseMax, d.PauseTotal = lastPause, maxPause, totalPause
}
