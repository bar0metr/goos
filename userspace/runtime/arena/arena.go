//go:build goos_userspace

// Package arena is the memory runtime every userspace program links: a bump
// allocator over the heap the linker script reserves (userspace/runtime/amd64.ld),
// and the TinyGo runtime hooks that route allocation to it. It used to be
// repeated in every program (ADR 0095).
//
// Nothing is ever freed one allocation at a time. A program that serves
// requests takes a Mark once its long-lived state is built and resets to it
// before each request, so each request starts from the same empty arena.
package arena

import (
	"runtime"
	"unsafe"
)

//go:extern _heap_start
var heapStartSymbol [0]byte

//go:extern _heap_end
var heapEndSymbol [0]byte

// Mark is a position in the arena to return to.
type Mark uintptr

var (
	next      uintptr
	exhausted func()
)

// Init starts the arena at the beginning of the heap. A program calls it
// before anything allocates; the runtime startup that would call it is not
// run, because the entry point jumps straight to the program.
func Init() { next = uintptr(unsafe.Pointer(&heapStartSymbol)) }

// OnExhausted sets what happens when an allocation does not fit: normally
// the program reports its own out-of-memory marker and stops. If report
// returns, or none is set, the process spins, since no allocation can succeed.
func OnExhausted(report func()) { exhausted = report }

// Current returns the position the next allocation will start at.
func Current() Mark { return Mark(next) }

// Reset discards every allocation made since mark was taken. Nothing that was
// allocated after it may be used again.
func Reset(mark Mark) { next = uintptr(mark) }

//go:linkname initHeap runtime.initHeap
func initHeap() { Init() }

//go:linkname alloc runtime.alloc
func alloc(size uintptr, _ unsafe.Pointer) unsafe.Pointer {
	size = (size + 15) &^ 15
	start, end := next, next+size
	if end < start || end > uintptr(unsafe.Pointer(&heapEndSymbol)) {
		if exhausted != nil {
			exhausted()
		}
		for {
		}
	}
	next = end
	// Go guarantees zeroed allocations, and the compiler relies on it when it
	// skips clearing large composite literals. The arena is reused after every
	// reset, so stale bytes would otherwise leak into fresh values.
	//
	// SAFETY: [start, end) lies inside the linker-reserved heap, checked just
	// above, and belongs to no live allocation: everything past next is free.
	cleared := unsafe.Slice((*byte)(unsafe.Pointer(start)), size)
	for index := range cleared {
		cleared[index] = 0
	}
	return unsafe.Pointer(start)
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
