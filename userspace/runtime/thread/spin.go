package thread

import "sync/atomic"

// spin is a lock for a few instructions of shared state. A waiter yields
// its processor after a few tries.
type spin struct{ held atomic.Uint32 }

func (s *spin) lock() {
	for tries := 0; !s.held.CompareAndSwap(0, 1); tries++ {
		if tries >= 64 {
			yield()
			tries = 0
		}
	}
}

func (s *spin) unlock() { s.held.Store(0) }
