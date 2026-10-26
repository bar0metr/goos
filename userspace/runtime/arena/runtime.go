//go:build goos_userspace

// Package arena is the memory runtime every native userspace program links:
// the TinyGo runtime hooks, the reclaimable heap behind them, and scoped
// regions for scratch with an obvious lifetime (ADR 0201).
//
// The heap is anonymous memory the program reserves from the kernel at start
// (ADR 0104): HeapReserve bytes of address space, of which only the pages it
// touches cost memory, charged to its page quota. Ordinary allocations come
// from it and are reclaimed by a stop-the-world, non-moving, conservative mark
// and sweep. A collection runs when the heap grows past a threshold set by the
// last one, when an allocation does not fit, or when the program calls
// runtime.GC. Pages it frees go back to the kernel.
//
// The collector's roots are the program's globals, its stack and registers,
// and the used part of every region. Nothing else is scanned: not page-pool
// pages, shared-memory windows, device or DMA memory, or any other
// reservation. A pointer to a heap object stored only there does not keep the
// object alive, and the object is freed under it. Store indexes or handles in
// such memory, never Go pointers; there is no way to register another root
// range.
//
// Finalizers are not supported: runtime.SetFinalizer records nothing and no
// finalizer ever runs.
package arena

import (
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"goos/userspace/runtime/tls"
	"runtime"
	"unsafe"
)

const (
	// HeapReserve is the address space the heap may grow into. It is
	// address space only; the page quota is what bounds the memory used.
	HeapReserve = uint64(256 << 20)
	heapPages   = uint32(HeapReserve >> pageShift)
	_           = uint(heapPagesMax - heapPages)
	// markStackEntries bounds the collector's mark stack. A graph wider
	// than this still collects: the overflow path rescans the heap.
	markStackEntries = 64 << 10
)

var (
	native       heap
	ready        bool
	initializing bool
	collecting   bool
	exhausted    func()
	// zeroSized is what every zero-byte allocation points to.
	zeroSized uintptr
	// pauses are the durations of collections in nanoseconds.
	lastPause, maxPause, totalPause uint64
)

// Init reserves the heap. A program calls it before anything allocates; the
// runtime startup that would call it is not run, because the entry point
// jumps straight to the program.
func Init() {
	if ready {
		return
	}
	if initializing {
		panic("arena: allocation while the heap is being set up")
	}
	initializing = true
	base := reserve(HeapReserve)
	meta := reserve(uint64(metaBytes(heapPages)))
	work := reserve(markStackEntries * uint64(wordBytes))
	// SAFETY: both reservations were just made for exactly these lengths
	// and stay mapped for the life of the process.
	metadata := unsafe.Slice((*pageMeta)(pointerAt(uintptr(meta))), heapPages)
	stack := unsafe.Slice((*uintptr)(pointerAt(uintptr(work))), markStackEntries)
	native.init(uintptr(base), heapPages, metadata, stack, discard)
	native.pageLimit = pageLimit()
	ready = true
}

// processInfo receives the process's record; a global, so reading it
// allocates nothing.
var processInfo [kcall.ProcessInfoSize]byte

// pageLimit returns the process's page quota in bytes, or zero when the
// kernel does not say. The quota is set at launch and never changes.
func pageLimit() uint64 {
	if sys.ProcessInfo(kcall.ProcessInfoSelf, &processInfo) != kcall.StatusOK {
		return 0
	}
	offset := kcall.ProcessInfoLimitsOffset + 4*kcall.ProcessPages
	limit := uint64(0)
	for index := 3; index >= 0; index-- {
		limit = limit<<8 | uint64(processInfo[offset+index])
	}
	return limit * kcall.SystemInfoPageSize
}

// InitService is Init for a long-running service. A program started from a
// file loads each image page on first use (ADR 0121), through the VFS, FAT32,
// and block services; a page first needed while those services restart faults
// the program. A service outlives such restarts, so InitService reads in its
// whole image, code, constants, and globals, and collects two throwaway
// allocations once so the collector has run before anything depends on it.
func InitService() {
	loadImage()
	Init()
	native.allocObject(64, false)
	native.allocObject(2*pageBytes, true)
	Collect()
}

func reserve(length uint64) uint64 {
	length = (length + uint64(pageBytes) - 1) &^ (uint64(pageBytes) - 1)
	base, status := sys.VMReserve(length, kcall.VMRead|kcall.VMWrite)
	if status != kcall.StatusOK {
		fail()
	}
	return base
}

func discard(address, length uintptr) bool {
	return sys.VMDiscard(uint64(address), uint64(length)) == kcall.StatusOK
}

// OnExhausted sets what happens when an allocation does not fit even after a
// collection: normally the program reports its own out-of-memory marker and
// stops. If report returns, or none is set, the process spins, since no
// allocation can succeed.
func OnExhausted(report func()) { exhausted = report }

func fail() {
	if ready && !collecting {
		// Whatever reports the failure gets every free page back first.
		lockHeap()
		native.trim()
		unlockHeap()
	}
	if exhausted != nil {
		exhausted()
	}
	for {
	}
}

// Collect runs a collection now, unless a region is active: region scratch
// may hold the only copy of a pointer the program is still using, so nothing
// is collected until the outermost region is left.
//
// The stack below Collect, at most 4 KiB of it and none past the bottom of
// the stack, is zeroed first and the work runs in collect, whose frames
// therefore start out zero. Without that, a slot a frame reserves but
// has not written can still hold an address an earlier, deeper call stored,
// and the scan keeps that allocation alive: memreclaim's unreachable cycle
// survived this way after an unrelated change moved the frames.
//
// With several threads, the caller holds the others off their processors
// for the collection (collectThreaded, ADR 0203); a thread inside a region
// still never collects, and another thread's region is scanned up to its
// position.
func Collect() {
	tls.Pin()
	collectPinned(collectExplicit)
	tls.Unpin()
}

// collectPinned runs a collection for reason and reports whether one ran to
// its end; the caller is pinned to its thread.
func collectPinned(reason collectionReason) bool {
	if !ready || self().regions.depth != 0 {
		return false
	}
	if collecting {
		panic("arena: collection started during a collection")
	}
	if threaded.Load() {
		return collectThreaded(selfSlot(), reason)
	}
	if !planStacks(selfSlot(), stackPointer()) {
		suspendFailures.Add(1)
		native.diag.abandoned[reason]++
		return false
	}
	native.allocators = 1
	base, _ := currentStack()
	scrubStack(base)
	collect(reason)
	return true
}

//go:noinline
func collect(reason collectionReason) {
	collecting = true
	started := nativeTicks()
	native.beginMark()
	native.phase = PhaseGlobals
	markGlobals()
	native.phase = PhaseRegions
	markRegions()
	markThreads()
	markStack()
	native.phase = PhaseNone
	native.finishMark()
	native.sweep()
	// With several threads the collector gives pages back after it lets
	// the others go (collectThreaded): discarding thousands of pages took
	// most of a pause on eight processors.
	if !threaded.Load() {
		native.trimAfter(reason)
		releaseMarkStack()
	}
	pause := uint64(nativeTicks() - started)
	lastPause = pause
	totalPause += pause
	if pause > maxPause {
		maxPause = pause
	}
	native.diag.pauses[histBucket(pause)]++
	native.diag.completed[reason]++
	collecting = false
}

// IdlePending reports whether anything was allocated since the last
// collection, free pages wait to go back to the kernel, or threads own
// slabs an idle collection would take back.
func IdlePending() bool {
	if !ready {
		return false
	}
	lockHeap()
	pending := native.mallocs != native.collectedMallocs || native.dirtyFree != 0 ||
		threaded.Load() && (anyUnfolded() || native.cachedPages != 0)
	unlockHeap()
	return pending
}

// CollectIdle collects if anything was allocated since the last collection,
// then gives every free page back, retained ones included. A service calls
// it when it has nothing to do, so its pages drop to its live heap while it
// waits rather than at the next threshold. With several threads it does
// nothing unless every other thread waits too (IdleEnter): one thread with
// nothing to do does not make its process idle.
func CollectIdle() {
	if !ready {
		return
	}
	tls.Pin()
	lockHeap()
	native.diag.idleRequests++
	waiting := !threaded.Load() || othersIdle(selfSlot())
	allocated := native.mallocs != native.collectedMallocs || threaded.Load() && (anyUnfolded() || native.cachedPages != 0)
	if !waiting {
		native.diag.idleSkipped++
	}
	unlockHeap()
	// An idle collection given up found a thread at work: its free
	// pages stay for that thread.
	if waiting && allocated && !collectPinned(collectIdle) {
		waiting = false
	}
	if waiting && !collecting {
		lockHeap()
		native.trim()
		unlockHeap()
	}
	tls.Unpin()
}

// SetRetained sets how many free pages a collection or an explicit free
// leaves for the allocations after it instead of giving them back, up to
// 256. Zero, the default, gives every page back at once; without a call, a
// collection that several threads' allocations started keeps some
// (retention). An idle wait and a failed allocation give back the retained
// pages too. Retained pages stay
// charged to the process's page quota, and nothing gives them back when a
// page touched past the quota stops the process: the kernel charges a page
// at that touch, after the allocator has returned (ADR 0202).
func SetRetained(pages uint32) {
	if !ready {
		Init()
	}
	lockHeap()
	native.setRetained(pages)
	unlockHeap()
}

// releaseMarkStack gives back the mark stack pages a wide graph touched,
// keeping the first.
func releaseMarkStack() {
	used := uintptr(native.workHigh) * wordBytes
	if used <= pageBytes {
		return
	}
	first := uintptr(unsafe.Pointer(&native.work[0])) + pageBytes
	end := (uintptr(unsafe.Pointer(&native.work[0])) + used + pageBytes - 1) &^ (pageBytes - 1)
	if discard(first, end-first) {
		native.workHigh = int(pageBytes / wordBytes)
	}
}

//go:linkname initHeap runtime.initHeap
func initHeap() { Init() }

// pointerFree reports whether a TinyGo 0.42 layout says an object holds no
// pointers. The compiler passes the layout gc.precise reads
// (runtime/gc_precise.go, internal/gclayout): an odd value carries the
// pointer bitmap inline above sizeFieldBits+1 = 7 bits, and an all-zero
// bitmap means no word is a pointer. Any other value, including the nil the
// runtime passes for maps, channels, and C allocations, is scanned.
// memreclaim checks this contract on every validation boot.
func pointerFree(layout unsafe.Pointer) bool {
	value := uintptr(layout)
	return value&1 != 0 && value>>7 == 0
}

// alloc keeps the calling goroutine on its thread for the whole
// allocation: the thread's caches, region cursor, and counts are the
// thread's, not the goroutine's (ADR 0204).
//
//go:linkname alloc runtime.alloc
func alloc(size uintptr, layout unsafe.Pointer) unsafe.Pointer {
	tls.Pin()
	pointer := allocPinned(size, layout)
	tls.Unpin()
	return pointer
}

func allocPinned(size uintptr, layout unsafe.Pointer) unsafe.Pointer {
	t := self()
	if t.regions.depth != 0 {
		return regionAlloc(t, size)
	}
	if size == 0 {
		return unsafe.Pointer(&zeroSized)
	}
	if !ready {
		Init()
	}
	if threaded.Load() {
		if collecting {
			panic("arena: allocation during a collection")
		}
		if size <= maxSmallSize && threadCaches {
			return pointerAt(allocCached(t, size, pointerFree(layout)))
		}
		return pointerAt(allocThreaded(t, size, pointerFree(layout)))
	}
	if native.liveBytes >= native.nextCollection && !collecting {
		collectPinned(collectAutomatic)
	}
	noPointers := pointerFree(layout)
	address := native.allocObject(size, noPointers)
	if address == 0 && !collecting {
		collectPinned(collectPressure)
		native.trim()
		address = native.allocObject(size, noPointers)
	}
	if address == 0 {
		fail()
	}
	return pointerAt(address)
}

// allocThreaded is alloc once threads exist. A collection never starts
// while the heap lock is held: the lock is let go, the collection runs, and
// the allocation starts again. The thread's own counts are folded first, so
// the goal is compared with what it allocated from its slabs too.
func allocThreaded(t *threadState, size uintptr, noPointers bool) uintptr {
	heapLock.lock()
	t.fold(&native)
	if native.liveBytes >= native.nextCollection && !collecting {
		heapLock.unlock()
		collectPinned(collectAutomatic)
		heapLock.lock()
	}
	address := native.allocObject(size, noPointers)
	if address == 0 && !collecting {
		heapLock.unlock()
		collectPinned(collectPressure)
		heapLock.lock()
		native.trim()
		address = native.allocObject(size, noPointers)
	}
	if address != 0 {
		t.allocated = true
	}
	heapLock.unlock()
	if address == 0 {
		fail()
	}
	return address
}

// free returns an allocation at once. Freeing a pointer the heap did not hand
// out, or one already freed, stops the process: the metadata can no longer be
// trusted. Region memory is reclaimed only by clearing its region.
//
//go:linkname free runtime.free
func free(pointer unsafe.Pointer) {
	tls.Pin()
	freePinned(pointer)
	tls.Unpin()
}

func freePinned(pointer unsafe.Pointer) {
	address := uintptr(pointer)
	if pointer == nil || pointer == unsafe.Pointer(&zeroSized) {
		return
	}
	if threaded.Load() {
		freeThreaded(self(), address)
		return
	}
	if inRegion(address) {
		unlockHeap()
		return
	}
	if !ready || !native.contains(address) {
		unlockHeap()
		panic("arena: free of a pointer outside the heap")
	}
	native.freeAt(address)
	unlockHeap()
}

// markRoots marks what a range of words points to. TinyGo calls it only from
// stack scanners this target does not build; Collect finds every root itself.
//
//go:linkname markRoots runtime.markRoots
func markRoots(start, end uintptr) {
	if collecting {
		native.markRange(start, end)
	}
}

//go:linkname gc runtime.GC
func gc() { Collect() }

//go:linkname setFinalizer runtime.SetFinalizer
func setFinalizer(interface{}, interface{}) {}

// readMemStats keeps the meaning runtime.MemStats gives each field:
// HeapReleased is what is idle and given back now, not the cumulative
// Released, which can count one page many times.
//
//go:linkname readMemStats runtime.ReadMemStats
func readMemStats(stats *runtime.MemStats) {
	s := HeapStats()
	*stats = runtime.MemStats{
		Alloc: s.Live, TotalAlloc: s.TotalAlloc, Sys: s.Reserved + s.Metadata + markStackEntries*uint64(wordBytes),
		Mallocs: s.Mallocs, Frees: s.Frees,
		HeapAlloc: s.Live, HeapSys: s.Reserved, HeapIdle: s.Idle, HeapInuse: s.InUse,
		HeapReleased: s.Idle - (s.Held - s.InUse), HeapObjects: s.Objects,
		GCSys: s.Metadata + markStackEntries*uint64(wordBytes), NumGC: s.Collections,
	}
}

// HeapStats returns the state of the heap. With several threads it counts
// what every thread has folded: each folds at every refill of its own slabs,
// at its exit, and at every collection, so between collections a thread's
// last allocations from its slabs may be missing.
func HeapStats() Stats {
	if !ready {
		return Stats{Reserved: HeapReserve}
	}
	lockHeap()
	stats := native.stats()
	unlockHeap()
	return stats
}

// Pauses returns the duration of the last collection and of the longest, in
// nanoseconds.
func Pauses() (last, longest uint64) {
	lockHeap()
	last, longest = lastPause, maxPause
	unlockHeap()
	return last, longest
}

// PauseTotal returns the cumulative duration of every collection in
// nanoseconds.
func PauseTotal() uint64 {
	lockHeap()
	total := totalPause
	unlockHeap()
	return total
}

// ReadDiagnostics fills d with the allocator's counters and a snapshot of its
// metadata. It allocates nothing, so d is best a global the caller keeps.
func ReadDiagnostics(d *Diagnostics) {
	if ready {
		lockHeap()
		native.diagnostics(d)
		threadDiagnostics(d)
		unlockHeap()
	}
}

// FreeBytes returns the allocation a slice starts at, as runtime.free does.
// The slice must start at the allocation's first byte and nothing may use the
// allocation afterwards.
func FreeBytes(data []byte) {
	if cap(data) != 0 {
		free(unsafe.Pointer(unsafe.SliceData(data)))
	}
}

// PointerFree reports whether the collector treats the allocation a value lies
// in as holding no pointers. Validation programs use it to check the layout
// contract with the compiler.
func PointerFree[T any](value *T) bool {
	if !ready {
		return false
	}
	lockHeap()
	page, slot, _, ok := native.find(uintptr(unsafe.Pointer(value)))
	result := false
	if ok && slot == noPage {
		result = native.meta[page].pointerFree
	} else if ok {
		result = native.meta[page].noscan[slot/64]&(1<<(slot%64)) != 0
	}
	unlockHeap()
	return result
}

// PointerFreeString is PointerFree for the bytes of a string.
func PointerFreeString(text string) bool {
	if len(text) == 0 {
		return false
	}
	return PointerFree(unsafe.StringData(text))
}

// InHeap reports whether a value lies in the collected heap rather than a
// region, a global, or the stack.
func InHeap[T any](value *T) bool {
	if !ready {
		return false
	}
	lockHeap()
	inside := native.contains(uintptr(unsafe.Pointer(value)))
	unlockHeap()
	return inside
}
