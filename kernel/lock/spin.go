// Package lock provides the spin lock that guards kernel state shared between
// processors.
//
// Disabling interrupts only excludes the local processor. State that another
// processor can reach is guarded by a Spin, taken with local interrupts
// already disabled, so an interrupt on the holding processor cannot try to
// take the same lock. A holder never sleeps, blocks, or switches tasks.
//
// Every lock has a level, and a processor takes locks in increasing level
// order (docs/kernel/smp.md lists the levels). Two locks of one level, such as
// two futex buckets, are taken lower rank first with LockAfter.
// Validation builds check the order and catch recursive acquisition; normal
// builds only spin. Every build counts contention per level and reports a
// spin that lasts longer than the watchdog allows.
package lock

import (
	"goos/internal/validation"
	"sync/atomic"
)

// Level orders the kernel's locks. A processor holding a lock may only take a
// lock of a higher level, or a second lock of the same level and a higher
// rank through LockAfter.
type Level uint8

const (
	// LevelKernel is the kernel-object lock: processes, handles, channels,
	// streams, waits, address-space metadata, and the Linux personality.
	LevelKernel Level = iota + 1
	// LevelLifecycle serializes housekeeping steps and the lifecycle queue.
	LevelLifecycle
	// LevelRegistry is the process registry and the Linux process tables.
	LevelRegistry
	// LevelProcess is one process: its handles, quotas, and Linux state.
	LevelProcess
	// LevelAddressSpace is one address space: its regions and page tables.
	LevelAddressSpace
	// LevelVFS is the Linux VFS bridge: sessions and their owners.
	LevelVFS
	// LevelChannel is the channel registry, every channel, and IPC waits.
	LevelChannel
	// LevelStream is the stream registry, every stream, and stream waits.
	LevelStream
	// LevelFutex is one futex bucket.
	LevelFutex
	// LevelWait is process waits, multi-object waits, IRQ waits, and the
	// Linux call continuations.
	LevelWait
	// LevelShmem is the shared memory objects.
	LevelShmem
	// LevelExecutable is the executable table and its page cache.
	LevelExecutable
	// LevelLog is the kernel log and the diagnostic trace ring.
	LevelLog
	// LevelMemory is the physical page allocator.
	LevelMemory
	// LevelRandom is the kernel's random byte generator.
	LevelRandom
	// LevelScheduler is the run state: task states, run queues, and the
	// processor each task runs on.
	LevelScheduler
	// LevelShootdown serializes TLB shootdowns.
	LevelShootdown
	// LevelConsole serializes kernel output to the serial port and the
	// framebuffer, which a panic may need while holding any other lock.
	LevelConsole
	levelCount
)

// Levels is the number of levels, for tables indexed by level.
const Levels = int(levelCount)

var levelNames = [levelCount]string{
	"none", "kernel", "lifecycle", "registry", "process", "address-space", "vfs",
	"channel", "stream", "futex", "wait", "shmem", "executable", "log", "memory",
	"random", "scheduler", "shootdown", "console",
}

// String names the level in diagnostics.
func (level Level) String() string {
	if level >= levelCount {
		return "unknown"
	}
	return levelNames[level]
}

// MaxCPUs bounds the processors the validation bookkeeping tracks.
const MaxCPUs = 64

// Spin is a test-and-test-and-set lock. The zero value is unlocked.
type Spin struct {
	state atomic.Uint32
	// owner is the holding processor plus one, kept by validation builds.
	owner atomic.Uint32
	level Level
	// rank orders the locks of one level, such as the buckets of a table.
	rank uint32
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
	// Now reads a clock for the watchdog and the spin statistics; nil turns
	// both off.
	Now func() uint64
	// StuckAfter is how long, in Now's units, a spin lasts before Stuck runs.
	StuckAfter uint64
	// Stuck reports a spin that outlasted StuckAfter: the level it waits for
	// and the processor that holds the lock, plus one, when a validation
	// build knows it. It runs once per acquisition.
	Stuck func(level Level, holder uint32)
}

var hooks Hooks

// held records, per processor, the levels it holds; heldCount counts the
// locks of each level, since LockAfter can hold two. Validation builds only.
var (
	held      [MaxCPUs]atomic.Uint32
	heldCount [MaxCPUs][levelCount]uint8
)

// Stats is the contention of one level since boot.
type Stats struct {
	// Contended counts acquisitions that had to spin.
	Contended uint64
	// Spin is the total time those acquisitions spun, in the clock's units.
	Spin uint64
	// Longest is the longest single spin.
	Longest uint64
	// Stuck counts spins the watchdog reported.
	Stuck uint64
}

var stats [levelCount]struct {
	contended atomic.Uint64
	spin      atomic.Uint64
	longest   atomic.Uint64
	stuck     atomic.Uint64
}

// contended counts acquisitions that had to wait, over every level.
var contended atomic.Uint64

// Install sets the hooks. It runs once, on the bootstrap processor.
func Install(installed Hooks) { hooks = installed }

// New returns an unlocked lock of the given level.
func New(level Level) Spin {
	return Spin{level: level}
}

// SetLevel names the level of a lock declared as a zero value.
func (lock *Spin) SetLevel(level Level) { lock.level = level }

// SetRank orders a lock among the locks of its level.
func (lock *Spin) SetRank(rank uint32) { lock.rank = rank }

// Level returns the lock's level.
func (lock *Spin) Level() Level { return lock.level }

// Lock spins until the lock is free and takes it.
func (lock *Spin) Lock() {
	cpu := lock.enter(nil)
	if !lock.state.CompareAndSwap(0, 1) {
		lock.spin(nil)
	}
	lock.acquired(cpu)
}

// LockAfter takes a second lock of first's level. The caller holds first,
// and lock ranks above it, so two processors that need the same pair take
// them in the same order.
func (lock *Spin) LockAfter(first *Spin) {
	cpu := lock.enter(first)
	if !lock.state.CompareAndSwap(0, 1) {
		lock.spin(nil)
	}
	lock.acquired(cpu)
}

// LockUnless spins until the lock is free and takes it, or gives up when the
// word stop points at becomes nonzero, and reports whether it took the lock.
// A task the scheduler is stopping waits for no lock: the processor that
// stopped it may need what it would take.
func (lock *Spin) LockUnless(stop *uint64) bool {
	cpu := lock.enter(nil)
	if !lock.state.CompareAndSwap(0, 1) && !lock.spin(stop) {
		return false
	}
	lock.acquired(cpu)
	return true
}

// spin waits for the lock and takes it. It returns false, without the lock,
// when stop becomes nonzero.
func (lock *Spin) spin(stop *uint64) bool {
	contended.Add(1)
	level := lock.level
	if level >= levelCount {
		level = 0
	}
	stats[level].contended.Add(1)
	start, reported, rounds := uint64(0), false, 0
	if hooks.Now != nil {
		start = hooks.Now()
	}
	for {
		for lock.state.Load() != 0 {
			if stop != nil && atomic.LoadUint64(stop) != 0 {
				lock.spun(level, start)
				return false
			}
			if hooks.Pause != nil {
				hooks.Pause()
			}
			rounds++
			if rounds%1024 == 0 && !reported && hooks.Now != nil && hooks.StuckAfter != 0 && hooks.Now()-start > hooks.StuckAfter {
				reported = true
				stats[level].stuck.Add(1)
				if hooks.Stuck != nil {
					hooks.Stuck(level, lock.owner.Load())
				}
			}
		}
		if lock.state.CompareAndSwap(0, 1) {
			lock.spun(level, start)
			return true
		}
	}
}

func (lock *Spin) spun(level Level, start uint64) {
	if hooks.Now == nil {
		return
	}
	spent := hooks.Now() - start
	stats[level].spin.Add(spent)
	for longest := stats[level].longest.Load(); spent > longest && !stats[level].longest.CompareAndSwap(longest, spent); longest = stats[level].longest.Load() {
	}
}

// TryLock takes the lock when it is free and reports whether it did.
func (lock *Spin) TryLock() bool {
	cpu := lock.enter(nil)
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
		if lock.level != 0 && lock.level < levelCount && cpu < MaxCPUs {
			heldCount[cpu][lock.level]--
			if heldCount[cpu][lock.level] == 0 {
				clearBit(&held[cpu], uint32(1)<<lock.level)
			}
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

// AssertHeld stops a validation build whose caller does not hold the lock.
// It checks nothing before the hooks are installed, as in host tests, which
// run single-threaded without the kernel's locking.
func (lock *Spin) AssertHeld() {
	if validation.Enabled && hooks.CPU != nil && lock.owner.Load() != hooks.CPU()+1 {
		fatal("kernel state used without its lock: " + lock.level.String())
	}
}

// Contended returns how many acquisitions had to wait.
func Contended() uint64 { return contended.Load() }

// Statistics returns the contention of one level.
func Statistics(level Level) Stats {
	if level >= levelCount {
		return Stats{}
	}
	return Stats{
		Contended: stats[level].contended.Load(),
		Spin:      stats[level].spin.Load(),
		Longest:   stats[level].longest.Load(),
		Stuck:     stats[level].stuck.Load(),
	}
}

// enter checks the acquisition order. first, when set, is the lock of the
// same level the caller already holds.
func (lock *Spin) enter(first *Spin) uint32 {
	if !validation.Enabled {
		return 0
	}
	cpu := currentCPU()
	if lock.owner.Load() == cpu+1 {
		fatal("recursive lock acquisition")
	}
	if lock.level == 0 || lock.level >= levelCount || cpu >= MaxCPUs {
		return cpu
	}
	if first != nil {
		if first.level != lock.level || first.owner.Load() != cpu+1 || first.rank >= lock.rank {
			fatal("second lock of a level taken out of rank order")
		}
		if held[cpu].Load()>>(lock.level+1) != 0 {
			fatal("lock taken out of level order")
		}
		return cpu
	}
	if held[cpu].Load()>>lock.level != 0 {
		fatal("lock taken out of level order")
	}
	return cpu
}

func (lock *Spin) acquired(cpu uint32) {
	if !validation.Enabled {
		return
	}
	lock.owner.Store(cpu + 1)
	if lock.level != 0 && lock.level < levelCount && cpu < MaxCPUs {
		heldCount[cpu][lock.level]++
		setBit(&held[cpu], uint32(1)<<lock.level)
	}
}

// HeldLevels returns the levels the calling processor holds, as a bit mask.
// Validation builds only; normal builds return zero.
func HeldLevels() uint32 {
	if !validation.Enabled {
		return 0
	}
	cpu := currentCPU()
	if cpu >= MaxCPUs {
		return 0
	}
	return held[cpu].Load()
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
