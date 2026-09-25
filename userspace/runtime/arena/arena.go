//go:build goos_userspace

// Package arena is the memory runtime every userspace program links: a bump
// allocator over the heap, and the TinyGo runtime hooks that route allocation
// to it. It used to be repeated in every program (ADR 0095).
//
// The heap is anonymous memory the program reserves from the kernel at start
// (ADR 0104): HeapReserve bytes of address space, of which only the pages it
// touches cost memory, charged to its page quota.
//
// Nothing is ever freed one allocation at a time. A program that serves
// requests takes a Mark once its long-lived state is built and resets to it
// before each request, so each request starts from the same empty arena. A
// reset that leaves more than trimThreshold of touched memory above the mark
// gives those pages back to the kernel.
package arena

import (
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"runtime"
	"unsafe"
)

const (
	// HeapReserve is the address space the heap may grow into. It is
	// address space only; the page quota is what bounds the memory used.
	HeapReserve = uint64(256 << 20)
	pageSize    = uintptr(4096)
	// trimThreshold is how much touched memory above its mark a reset keeps
	// before returning it.
	trimThreshold = uintptr(256 << 10)
)

// Mark is a position in the arena to return to.
type Mark uintptr

var (
	start, next, end uintptr
	// high is the highest position reached since the last trim.
	high      uintptr
	exhausted func()
)

// Init reserves the heap. A program calls it before anything allocates; the
// runtime startup that would call it is not run, because the entry point
// jumps straight to the program.
func Init() {
	if start != 0 {
		return
	}
	base, status := sys.VMReserve(HeapReserve, kcall.VMRead|kcall.VMWrite)
	if status != kcall.StatusOK {
		fail()
	}
	start, next, high, end = uintptr(base), uintptr(base), uintptr(base), uintptr(base+HeapReserve)
}

// OnExhausted sets what happens when an allocation does not fit: normally
// the program reports its own out-of-memory marker and stops. If report
// returns, or none is set, the process spins, since no allocation can succeed.
func OnExhausted(report func()) { exhausted = report }

// Current returns the position the next allocation will start at.
func Current() Mark { return Mark(next) }

// Reset discards every allocation made since mark was taken. Nothing that was
// allocated after it may be used again.
func Reset(mark Mark) {
	next = uintptr(mark)
	if high > next && high-next > trimThreshold {
		from := (next + pageSize - 1) &^ (pageSize - 1)
		to := (high + pageSize - 1) &^ (pageSize - 1)
		if to > from {
			sys.VMDiscard(uint64(from), uint64(to-from))
		}
		high = next
	}
}

// Touched is how many bytes of the heap have been allocated at their highest.
func Touched() uint64 { return uint64(high - start) }

func fail() {
	if exhausted != nil {
		exhausted()
	}
	for {
	}
}

//go:linkname initHeap runtime.initHeap
func initHeap() { Init() }

//go:linkname alloc runtime.alloc
func alloc(size uintptr, _ unsafe.Pointer) unsafe.Pointer {
	size = (size + 15) &^ 15
	if start == 0 {
		Init()
	}
	first, last := next, next+size
	if last < first || last > end {
		fail()
	}
	next = last
	if next > high {
		high = next
	}
	// Go guarantees zeroed allocations, and the compiler relies on it when it
	// skips clearing large composite literals. The arena is reused after every
	// reset, so stale bytes would otherwise leak into fresh values.
	//
	// SAFETY: [first, last) lies inside the reserved heap, checked just above,
	// and belongs to no live allocation: everything past next is free.
	cleared := unsafe.Slice((*byte)(unsafe.Pointer(first)), size)
	for index := range cleared {
		cleared[index] = 0
	}
	return unsafe.Pointer(first)
}

//go:linkname free runtime.free
func free(unsafe.Pointer) {}

//go:linkname markRoots runtime.markRoots
func markRoots(uintptr, uintptr) {}

//go:linkname gc runtime.GC
func gc() {}

//go:linkname setFinalizer runtime.SetFinalizer
func setFinalizer(interface{}, interface{}) {}

//go:linkname readMemStats runtime.ReadMemStats
func readMemStats(*runtime.MemStats) {}
