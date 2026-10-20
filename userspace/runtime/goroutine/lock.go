package goroutine

import (
	"sync/atomic"
	"unsafe"
)

// spin is the runtime's lock for a few instructions of shared state. Its
// holder is pinned to its thread, so it is never switched out while
// another goroutine of the same thread waits for it; a waiter gives up its
// processor after a few tries, so a holder that the kernel preempted gets
// to finish. No spin lock is held across a system call that can block, an
// allocation, or a stack switch.
type spin struct{ held atomic.Uint32 }

func (s *spin) lock() {
	pin()
	for tries := 0; !s.held.CompareAndSwap(0, 1); tries++ {
		if tries >= 64 {
			threadYield()
			tries = 0
		}
	}
}

func (s *spin) unlock() {
	s.held.Store(0)
	unpin()
}

// spinAt treats a uint32 the TinyGo runtime owns as a spin lock.
func spinAt(word *uint32) *spin { return (*spin)(unsafe.Pointer(word)) }
