// Package lock provides the spin lock that guards kernel state shared between
// processors.
//
// Disabling interrupts only excludes the local processor. State that another
// processor can reach is guarded by a Spin, taken with local interrupts
// already disabled, so an interrupt on the holding processor cannot try to
// take the same lock. A holder never sleeps, blocks, or switches tasks.
//
// Every lock has a level, and a processor takes locks in strictly increasing
// level order (docs/kernel/smp.md lists the levels). Validation builds check
// the order and catch recursive acquisition; normal builds only spin.
package lock

import (
	"goos/internal/validation"
	"sync/atomic"
)

// Level orders the kernel's locks. A processor holding a lock may only take a
// lock of a higher level.
type Level uint8

const (
	// LevelKernel is the kernel-object lock: processes, handles, channels,
	// streams, waits, address-space metadata, and the Linux personality.
	LevelKernel Level = iota + 1
	// LevelMemory is the physical page allocator.
	LevelMemory
	// LevelRandom is the kernel's random byte generator.
	LevelRandom
	// LevelScheduler is the run state: task states, run queues, and the
	// processor each task runs on.
	LevelScheduler
	// LevelShootdown serializes TLB shootdowns that are not issued under the
	// kernel-object lock.
	LevelShootdown
	// LevelConsole serializes kernel output to the serial port and the
	// framebuffer, which a panic may need while holding any other lock.
	LevelConsole
	levelCount
)

// MaxCPUs bounds the processors the validation bookkeeping tracks.
const MaxCPUs = 64

// Spin is a test-and-test-and-set lock. The zero value is unlocked.
type Spin struct {
	state atomic.Uint32
	// owner is the holding processor plus one, kept by validation builds.
	owner atomic.Uint32
	level Level
}

// Hooks connect the lock to the processor it runs on. The architecture layer
// installs them before a second processor starts.
type Hooks struct {
	// Pause is called in every spin iteration.
	Pause func()
	// CPU returns the index of the calling processor.
	CPU func() uint32
	// Fatal stops the machine on a broken lock invariant.
	Fatal func(message string)
}

var hooks Hooks

// held records, per processor, the levels it holds. Validation builds only.
var held [MaxCPUs]atomic.Uint32

// contended counts acquisitions that had to spin.
var contended atomic.Uint64

// Install sets the hooks. It runs once, on the bootstrap processor.
func Install(installed Hooks) { hooks = installed }

// New returns an unlocked lock of the given level.
func New(level Level) Spin {
	return Spin{level: level}
}

// SetLevel names the level of a lock declared as a zero value.
func (lock *Spin) SetLevel(level Level) { lock.level = level }

// Lock spins until the lock is free and takes it.
func (lock *Spin) Lock() {
	cpu := lock.enter()
	if !lock.state.CompareAndSwap(0, 1) {
		contended.Add(1)
		for {
			for lock.state.Load() != 0 {
				if hooks.Pause != nil {
					hooks.Pause()
				}
			}
			if lock.state.CompareAndSwap(0, 1) {
				break
			}
		}
	}
	lock.acquired(cpu)
}

// TryLock takes the lock when it is free and reports whether it did.
func (lock *Spin) TryLock() bool {
	cpu := lock.enter()
	if !lock.state.CompareAndSwap(0, 1) {
		return false
	}
	lock.acquired(cpu)
	return true
}

// Unlock releases a lock the calling processor holds.
func (lock *Spin) Unlock() {
	if validation.Enabled {
		cpu := currentCPU()
		if lock.owner.Load() != cpu+1 {
			fatal("lock released by a processor that does not hold it")
		}
		lock.owner.Store(0)
		if lock.level != 0 && cpu < MaxCPUs {
			clearBit(&held[cpu], uint32(1)<<lock.level)
		}
	}
	if lock.state.Swap(0) != 1 {
		fatal("unlock of a free lock")
	}
}

// Held reports whether any processor holds the lock.
func (lock *Spin) Held() bool { return lock.state.Load() != 0 }

// HeldByCaller reports whether the calling processor holds the lock. Only
// validation builds know; normal builds report whether anyone holds it.
func (lock *Spin) HeldByCaller() bool {
	if !validation.Enabled {
		return lock.Held()
	}
	return lock.owner.Load() == currentCPU()+1
}

// Contended returns how many acquisitions had to wait.
func Contended() uint64 { return contended.Load() }

func (lock *Spin) enter() uint32 {
	if !validation.Enabled {
		return 0
	}
	cpu := currentCPU()
	if lock.owner.Load() == cpu+1 {
		fatal("recursive lock acquisition")
	}
	if lock.level != 0 && cpu < MaxCPUs && held[cpu].Load()>>lock.level != 0 {
		fatal("lock taken out of level order")
	}
	return cpu
}

func (lock *Spin) acquired(cpu uint32) {
	if !validation.Enabled {
		return
	}
	lock.owner.Store(cpu + 1)
	if lock.level != 0 && cpu < MaxCPUs {
		setBit(&held[cpu], uint32(1)<<lock.level)
	}
}

func currentCPU() uint32 {
	if hooks.CPU == nil {
		return 0
	}
	return hooks.CPU()
}

func fatal(message string) {
	if hooks.Fatal != nil {
		hooks.Fatal(message)
	}
	panic(message)
}

// setBit and clearBit change one bit of a word only its own processor writes;
// the compare-and-swap keeps them correct regardless.
func setBit(word *atomic.Uint32, bit uint32) {
	for old := word.Load(); !word.CompareAndSwap(old, old|bit); old = word.Load() {
	}
}

func clearBit(word *atomic.Uint32, bit uint32) {
	for old := word.Load(); !word.CompareAndSwap(old, old&^bit); old = word.Load() {
	}
}
