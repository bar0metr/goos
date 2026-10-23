//go:build goos_userspace && !goos_goroutines

package lock

import (
	"goos/userspace/runtime/sys"
	"sync/atomic"
)

// spinTries is how many times a contended Lock retries before it yields.
const spinTries = 64

// Mutex is a mutual exclusion lock. The zero value is unlocked.
//
// This is the implementation for native programs without goroutines: one
// word, taken with compare-and-swap. The holder is an OS thread; a contended
// waiter retries spinTries times and then gives up its processor, so a
// holder that was preempted gets to run.
type Mutex struct {
	held atomic.Uint32
}

// Lock acquires the lock, waiting until it is free.
func (l *Mutex) Lock() {
	for tries := 0; !l.held.CompareAndSwap(0, 1); tries++ {
		if tries >= spinTries {
			sys.ThreadYield()
			tries = 0
		}
	}
}

// Unlock releases the lock. Unlocking a Mutex that is not locked is a fault.
func (l *Mutex) Unlock() {
	if l.held.Swap(0) == 0 {
		panic("lock: unlock of unlocked Mutex")
	}
}

// TryLock acquires the lock if it is free and reports whether it did.
func (l *Mutex) TryLock() bool { return l.held.CompareAndSwap(0, 1) }
