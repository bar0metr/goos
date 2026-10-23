package lock

import (
	"goos/internal/validation"
	"sync/atomic"
)

// RW is a reader-writer spin lock: any number of readers, or one writer. A
// writer that waits keeps new readers out, so a stream of readers cannot
// starve it. Its level orders it with the other locks. A processor holds a
// second RW of one level only through RLockAfter or LockAfter, which take
// it above the first in rank order.
type RW struct {
	// state is the reader count, with rwWriter set while a writer holds the
	// lock and rwPending while one waits for it.
	state atomic.Uint32
	// owner is the writer's processor plus one, kept by validation builds.
	owner     atomic.Uint32
	levelWord atomic.Uint32
	// rank orders the locks of one level, such as every process's handle
	// table.
	rank uint32
}

// readHeld records, per processor, the RW locks it holds for reading, so a
// validation build can tell one handle table from another. A processor
// holds at most readSlots of them at once.
const readSlots = 4

var readHeld [MaxCPUs][readSlots]*RW

const (
	rwWriter  = uint32(1) << 31
	rwPending = uint32(1) << 30
	rwReaders = rwPending - 1
)

// SetLevel names the level of a lock declared as a zero value.
func (lock *RW) SetLevel(level Level) {
	if lock.level() != level {
		lock.levelWord.Store(uint32(level))
	}
}

// SetRank orders the lock among the locks of its level.
func (lock *RW) SetRank(rank uint32) { lock.rank = rank }

// Rank returns the lock's rank.
func (lock *RW) Rank() uint32 { return lock.rank }

// level returns the lock's level. It is atomic: types that lock themselves
// name their level each time they take the lock, from any processor.
func (lock *RW) level() Level { return Level(lock.levelWord.Load()) }

// RLock takes the lock for reading.
func (lock *RW) RLock() { lock.RLockUnless(nil) }

// RLockUnless takes the lock for reading, or gives up when the word stop
// points at becomes nonzero, and reports whether it took the lock.
func (lock *RW) RLockUnless(stop *uint64) bool {
	return lock.readUnless(nil, stop)
}

// RLockAfter takes the lock for reading as the second lock of first's level;
// first ranks below it.
func (lock *RW) RLockAfter(first *RW, stop *uint64) bool {
	return lock.readUnless(first, stop)
}

func (lock *RW) readUnless(first *RW, stop *uint64) bool {
	cpu := lock.enter(first)
	if !lock.tryRead() {
		var wait waiting
		wait.begin(lock.level())
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
	return lock.writeUnless(nil, stop)
}

// LockAfter takes the lock for writing as the second lock of first's level;
// first ranks below it.
func (lock *RW) LockAfter(first *RW, stop *uint64) bool {
	return lock.writeUnless(first, stop)
}

func (lock *RW) writeUnless(first *RW, stop *uint64) bool {
	cpu := lock.enter(first)
	if !lock.state.CompareAndSwap(0, rwWriter) {
		var wait waiting
		wait.begin(lock.level())
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
				// A writer that gives up takes its pending mark with it, or
				// new readers would wait for a writer that never comes.
				// Another waiting writer marks the lock again next round.
				for state&rwPending != 0 && !lock.state.CompareAndSwap(state, state&^rwPending) {
					state = lock.state.Load()
				}
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
// reading nor for writing.
func (lock *RW) AssertHeld() {
	if !validation.Enabled || hooks.CPU == nil {
		return
	}
	cpu := hooks.CPU()
	if lock.owner.Load() == cpu+1 {
		return
	}
	if cpu < MaxCPUs {
		for _, held := range readHeld[cpu] {
			if held == lock {
				return
			}
		}
	}
	fatal("kernel state used without its lock: " + lock.level().String() + " rank " + decimal(lock.rank))
}

// AssertWriter stops a validation build whose caller does not hold the lock
// for writing.
func (lock *RW) AssertWriter() {
	if validation.Enabled && hooks.CPU != nil && lock.owner.Load() != hooks.CPU()+1 {
		fatal("kernel state changed without its write lock: " + lock.level().String() + " rank " + decimal(lock.rank))
	}
}

func (lock *RW) enter(first *RW) uint32 {
	if !validation.Enabled {
		return 0
	}
	cpu := currentCPU()
	if lock.owner.Load() == cpu+1 || lock.readBy(cpu) {
		fatal("recursive lock acquisition")
	}
	if lock.level() == 0 || lock.level() >= levelCount || cpu >= MaxCPUs {
		return cpu
	}
	if first != nil {
		if first.level() != lock.level() || first.owner.Load() != cpu+1 && !first.readBy(cpu) || first.rank >= lock.rank {
			fatal("second lock of a level taken out of rank order")
		}
		if held[cpu].Load()>>(lock.level()+1) != 0 {
			fatal("lock taken out of level order")
		}
		return cpu
	}
	if held[cpu].Load()>>lock.level() != 0 {
		fatal("lock taken out of level order")
	}
	return cpu
}

// readBy reports whether processor cpu holds the lock for reading.
func (lock *RW) readBy(cpu uint32) bool {
	if cpu >= MaxCPUs {
		return false
	}
	for _, held := range readHeld[cpu] {
		if held == lock {
			return true
		}
	}
	return false
}

func (lock *RW) acquired(cpu uint32, writer bool) {
	count(lock.level())
	if !validation.Enabled {
		return
	}
	if writer {
		lock.owner.Store(cpu + 1)
	} else if cpu < MaxCPUs {
		recorded := false
		for slot := range readHeld[cpu] {
			if readHeld[cpu][slot] == nil {
				readHeld[cpu][slot], recorded = lock, true
				break
			}
		}
		if !recorded {
			fatal("too many read locks held at once")
		}
	}
	if lock.level() != 0 && lock.level() < levelCount && cpu < MaxCPUs {
		heldCount[cpu][lock.level()]++
		setBit(&held[cpu], uint32(1)<<lock.level())
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
	} else if cpu < MaxCPUs {
		found := false
		for slot := range readHeld[cpu] {
			if readHeld[cpu][slot] == lock {
				readHeld[cpu][slot], found = nil, true
				break
			}
		}
		if !found {
			fatal("read lock released by a processor that does not hold it")
		}
	}
	if lock.level() != 0 && lock.level() < levelCount && cpu < MaxCPUs {
		if heldCount[cpu][lock.level()] == 0 {
			fatal("lock released by a processor that does not hold it")
		}
		heldCount[cpu][lock.level()]--
		if heldCount[cpu][lock.level()] == 0 {
			clearBit(&held[cpu], uint32(1)<<lock.level())
		}
	}
}

// decimal formats a rank for a fatal message.
func decimal(value uint32) string {
	var digits [10]byte
	at := len(digits)
	for {
		at--
		digits[at] = byte('0' + value%10)
		value /= 10
		if value == 0 {
			return string(digits[at:])
		}
	}
}
