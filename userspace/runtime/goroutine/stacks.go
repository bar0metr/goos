//go:build goos_userspace && goos_goroutines

package goroutine

import (
	"sync/atomic"

	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

// Default stacks share VM reservations with repeating guards (ADR 0204).
// The kernel accounts for committed pages on demand. Returning a slot
// discards all its pages before another goroutine can claim it.
var stacks struct {
	lock     spin
	batches  [(MaxGoroutines + stacksPerBatch - 1) / stacksPerBatch]stackBatch
	reserved atomic.Uint64
}

func reserveStack(size uintptr) (uintptr, bool) {
	if size == StackSize {
		stacks.lock.lock()
		defer stacks.lock.unlock()
		for i := range stacks.batches {
			if base, ok := stacks.batches[i].take(); ok {
				return base, true
			}
		}
		for i := range stacks.batches {
			b := &stacks.batches[i]
			if b.base != 0 {
				continue
			}
			// VM reservation does not allocate from the Go heap or wait
			// for another userspace thread. The collector never takes this
			// lock when it scans stacks.
			base, status := sys.VMReserveStacks(stacksPerBatch)
			if status != kcall.StatusOK {
				return 0, false
			}
			b.base = uintptr(base)
			stacks.reserved.Add(stacksPerBatch * (GuardSize + StackSize))
			return b.take()
		}
		return 0, false
	}
	base, status := sys.VMReserve(uint64(GuardSize+size), kcall.VMRead|kcall.VMWrite)
	if status != kcall.StatusOK {
		return 0, false
	}
	if sys.VMProtect(base, GuardSize, 0) != kcall.StatusOK {
		sys.VMUnmap(base, uint64(GuardSize+size))
		return 0, false
	}
	stacks.reserved.Add(uint64(GuardSize + size))
	return uintptr(base), true
}

func freeStack(base, size uintptr) {
	if size == StackSize {
		if sys.VMDiscard(uint64(base+GuardSize), uint64(size)) != kcall.StatusOK {
			fatal("goroutine: stack discard failed")
		}
		stacks.lock.lock()
		for i := range stacks.batches {
			if stacks.batches[i].release(base) {
				stacks.lock.unlock()
				return
			}
		}
		stacks.lock.unlock()
		fatal("goroutine: stack slot returned twice or outside its batch")
	}
	if sys.VMUnmap(uint64(base), uint64(GuardSize+size)) != kcall.StatusOK {
		fatal("goroutine: stack unmap failed")
	}
	stacks.reserved.Add(^uint64(GuardSize + size - 1))
}
