package lock

import (
	"goos/internal/validation"
	"sync/atomic"
)

// RW is a reader-writer spin lock: any number of readers, or one writer. A
// writer that waits keeps new readers out, so a stream of readers cannot
// starve it. Its level orders it with the other locks; a processor holds at
// most one RW of a level, as reader or as writer.
type RW struct {
	// state is the reader count, with rwWriter set while a writer holds the
	// lock and rwPending while one waits for it.
	state atomic.Uint32
	// owner is the writer's processor plus one, kept by validation builds.
	owner atomic.Uint32
	level Level
}

const (
	rwWriter  = uint32(1) << 31
	rwPending = uint32(1) << 30
	rwReaders = rwPending - 1
)

// SetLevel names the level of a lock declared as a zero value.
func (lock *RW) SetLevel(level Level) { lock.level = level }

// RLock takes the lock for reading.
func (lock *RW) RLock() { lock.RLockUnless(nil) }

// RLockUnless takes the lock for reading, or gives up when the word stop
// points at becomes nonzero, and reports whether it took the lock.
func (lock *RW) RLockUnless(stop *uint64) bool {
	cpu := lock.enter()
	if !lock.tryRead() {
		var wait waiting
		wait.begin(lock.level)
		for !lock.tryRead() {
			if stop != nil && atomic.LoadUint64(stop) != 0 {
				wait.end()
				return false
			}
			wait.pause(&lock.owner)
		}
		wait.end()
	}
	lock.acquired(cpu, false)
	return true
}

func (lock *RW) tryRead() bool {
	state := lock.state.Load()
	return state&(rwWriter|rwPending) == 0 && state&rwReaders != rwReaders && lock.state.CompareAndSwap(state, state+1)
}

// RUnlock releases a read hold.
func (lock *RW) RUnlock() {
	lock.released(false)
	for {
		state := lock.state.Load()
		if state&rwReaders == 0 {
			fatal("read unlock of a lock no reader holds")
			return
		}
		if lock.state.CompareAndSwap(state, state-1) {
			return
		}
	}
}

// Lock takes the lock for writing.
func (lock *RW) Lock() { lock.LockUnless(nil) }

// LockUnless takes the lock for writing, or gives up when the word stop
// points at becomes nonzero, and reports whether it took the lock.
func (lock *RW) LockUnless(stop *uint64) bool {
	cpu := lock.enter()
	if !lock.state.CompareAndSwap(0, rwWriter) {
		var wait waiting
		wait.begin(lock.level)
		for {
			state := lock.state.Load()
			if state&(rwWriter|rwReaders) == 0 {
				// Taking the lock clears the pending mark; another waiting
				// writer sets it again.
				if lock.state.CompareAndSwap(state, rwWriter) {
					break
				}
				continue
			}
			if stop != nil && atomic.LoadUint64(stop) != 0 {
				wait.end()
				return false
			}
			if state&rwPending == 0 {
				lock.state.CompareAndSwap(state, state|rwPending)
			}
			wait.pause(&lock.owner)
		}
		wait.end()
	}
	lock.acquired(cpu, true)
	return true
}

// Unlock releases the write hold.
func (lock *RW) Unlock() {
	lock.released(true)
	for {
		state := lock.state.Load()
		if state&rwWriter == 0 {
			fatal("write unlock of a lock no writer holds")
			return
		}
		if lock.state.CompareAndSwap(state, state&rwPending) {
			return
		}
	}
}

// AssertHeld stops a validation build whose caller holds the lock neither for
// reading nor for writing. A reader is known only by the level it holds.
func (lock *RW) AssertHeld() {
	if !validation.Enabled || hooks.CPU == nil {
		return
	}
	cpu := hooks.CPU()
	if lock.owner.Load() == cpu+1 {
		return
	}
	if cpu < MaxCPUs && lock.level < levelCount && heldCount[cpu][lock.level] != 0 && lock.state.Load()&rwReaders != 0 {
		return
	}
	fatal("kernel state used without its lock: " + lock.level.String())
}

// AssertWriter stops a validation build whose caller does not hold the lock
// for writing.
func (lock *RW) AssertWriter() {
	if validation.Enabled && hooks.CPU != nil && lock.owner.Load() != hooks.CPU()+1 {
		fatal("kernel state changed without its write lock: " + lock.level.String())
	}
}

func (lock *RW) enter() uint32 {
	if !validation.Enabled {
		return 0
	}
	cpu := currentCPU()
	if lock.owner.Load() == cpu+1 {
		fatal("recursive lock acquisition")
	}
	if lock.level != 0 && lock.level < levelCount && cpu < MaxCPUs && held[cpu].Load()>>lock.level != 0 {
		fatal("lock taken out of level order")
	}
	return cpu
}

func (lock *RW) acquired(cpu uint32, writer bool) {
	if !validation.Enabled {
		return
	}
	if writer {
		lock.owner.Store(cpu + 1)
	}
	if lock.level != 0 && lock.level < levelCount && cpu < MaxCPUs {
		heldCount[cpu][lock.level]++
		setBit(&held[cpu], uint32(1)<<lock.level)
	}
}

func (lock *RW) released(writer bool) {
	if !validation.Enabled {
		return
	}
	cpu := currentCPU()
	if writer {
		if lock.owner.Load() != cpu+1 {
			fatal("lock released by a processor that does not hold it")
		}
		lock.owner.Store(0)
	}
	if lock.level != 0 && lock.level < levelCount && cpu < MaxCPUs {
		if heldCount[cpu][lock.level] == 0 {
			fatal("lock released by a processor that does not hold it")
		}
		heldCount[cpu][lock.level]--
		if heldCount[cpu][lock.level] == 0 {
			clearBit(&held[cpu], uint32(1)<<lock.level)
		}
	}
}
