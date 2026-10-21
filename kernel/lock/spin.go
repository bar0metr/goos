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
	// LevelRegistry is the process registry, the fields of every process
	// but its handles and address space, and the task-to-process map.
	LevelRegistry Level = iota + 1
	// LevelLinux is one bucket of Linux families: the descriptors, signals,
	// threads, and exec staging of the processes that share a thread group or
	// descriptor table.
	LevelLinux
	// LevelLifecycle is the queue of lifecycle messages for PID 1.
	LevelLifecycle
	// LevelHandle is one process's handle table, ranked by process ID. A
	// call that moves handles holds two, the lower rank first.
	LevelHandle
	// LevelStream is the stream registry and every stream.
	LevelStream
	// LevelShmem is the shared memory objects and the image page cache.
	LevelShmem
	// LevelAddressSpace is one address space: its regions and page tables.
	// Copies and faults read it; mapping changes write it.
	LevelAddressSpace
	// LevelExecutable is the executable table. A fault inside a copy reads
	// an image page from it with the address space held.
	LevelExecutable
	// LevelPaging is the table of executable page requests out to file
	// services. A request is sent and recorded under it.
	LevelPaging
	// LevelChannelSlots is the channel registry's slots, held only to find,
	// add, or remove a channel.
	LevelChannelSlots
	// LevelChannel is one channel: its queues and endpoint state. Nothing
	// holds two.
	LevelChannel
	// LevelFutex is one futex bucket.
	LevelFutex
	// LevelWait is the wait tables, each with a lock of its own: channel and
	// stream waits, process waits, multi-object waits, and the Linux call
	// continuations. None is taken while another is held.
	LevelWait
	// LevelDeadline is the index of wait deadlines. The wait tables take it
	// inside their own locks as registrations come and go.
	LevelDeadline
	// LevelLinuxTables is the Linux personality's shared tables: process
	// state and record slots, threads, open descriptions, directories, VFS
	// sessions, and the identities they hand out. The personality takes it
	// itself, for one table operation at a time.
	LevelLinuxTables
	// LevelHandleIndex is the index of which processes hold each channel
	// endpoint, taken briefly by handle tables as they change.
	LevelHandleIndex
	// LevelLog is the kernel log and the diagnostic trace ring.
	LevelLog
	// LevelMemory is the physical page allocator.
	LevelMemory
	// LevelRandom is the kernel's random byte generator.
	LevelRandom
	// LevelScheduler is the run state: task states, run queues, and the
	// processor each task runs on.
	LevelScheduler
	// LevelConsole serializes kernel output to the serial port and the
	// framebuffer, which a panic may need while holding any other lock.
	LevelConsole
	levelCount
)

// Levels is the number of levels, for tables indexed by level.
const Levels = int(levelCount)

var levelNames = [levelCount]string{
	"none", "registry", "linux", "lifecycle", "handle",
	"stream", "shmem", "address-space", "executable", "paging", "channel-slots", "channel", "futex", "wait", "deadline", "linux-tables", "handle-index", "log",
	"memory", "random", "scheduler", "console",
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
	owner     atomic.Uint32
	levelWord atomic.Uint32
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

// HeldLevelsOf returns the levels processor cpu holds locks of, as
// HeldLevels does for the caller, for a stuck-lock report.
func HeldLevelsOf(cpu int) uint32 {
	if cpu < 0 || cpu >= MaxCPUs {
		return 0
	}
	return held[cpu].Load()
}

// Stats is the contention of one level since boot.
type Stats struct {
	// Acquired counts every acquisition.
	Acquired uint64
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

// acquisitions counts each processor's acquisitions per level. Every
// processor adds to its own row, so the count costs no shared cache line.
var acquisitions [MaxCPUs][levelCount]atomic.Uint64

// count records one acquisition of level by the calling processor.
func count(level Level) {
	if hooks.CPU == nil || level >= levelCount {
		return
	}
	if cpu := hooks.CPU(); cpu < MaxCPUs {
		acquisitions[cpu][level].Add(1)
	}
}

// contended counts acquisitions that had to wait, over every level.
var contended atomic.Uint64

// Install sets the hooks. It runs once, on the bootstrap processor.
func Install(installed Hooks) { hooks = installed }

// New returns an unlocked lock of the given level.
func New(level Level) *Spin {
	spin := new(Spin)
	spin.levelWord.Store(uint32(level))
	return spin
}

// SetLevel names the level of a lock declared as a zero value.
func (lock *Spin) SetLevel(level Level) {
	if lock.level() != level {
		lock.levelWord.Store(uint32(level))
	}
}

// level returns the lock's level. It is atomic: types that lock themselves
// name their level each time they take the lock, from any processor.
func (lock *Spin) level() Level { return Level(lock.levelWord.Load()) }

// SetRank orders a lock among the locks of its level.
func (lock *Spin) SetRank(rank uint32) { lock.rank = rank }

// Level returns the lock's level.
func (lock *Spin) Level() Level { return lock.level() }

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
	var wait waiting
	wait.begin(lock.level())
	for {
		for lock.state.Load() != 0 {
			if stop != nil && atomic.LoadUint64(stop) != 0 {
				wait.end()
				return false
			}
			wait.pause(&lock.owner)
		}
		if lock.state.CompareAndSwap(0, 1) {
			wait.end()
			return true
		}
	}
}

// waiting is one acquisition that spins: it counts the contention and runs
// the watchdog.
type waiting struct {
	level    Level
	start    uint64
	rounds   int
	reported bool
}

func (wait *waiting) begin(level Level) {
	if level >= levelCount {
		level = 0
	}
	contended.Add(1)
	stats[level].contended.Add(1)
	wait.level = level
	if hooks.Now != nil {
		wait.start = hooks.Now()
	}
}

// pause spins once. owner is the holder's processor plus one, when known.
func (wait *waiting) pause(owner *atomic.Uint32) {
	if hooks.Pause != nil {
		hooks.Pause()
	}
	wait.rounds++
	if wait.rounds%1024 != 0 || wait.reported || hooks.Now == nil || hooks.StuckAfter == 0 || hooks.Now()-wait.start <= hooks.StuckAfter {
		return
	}
	wait.reported = true
	stats[wait.level].stuck.Add(1)
	if hooks.Stuck != nil {
		hooks.Stuck(wait.level, owner.Load())
	}
}

func (wait *waiting) end() {
	if hooks.Now == nil {
		return
	}
	spent := hooks.Now() - wait.start
	level := wait.level
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
		if lock.level() != 0 && lock.level() < levelCount && cpu < MaxCPUs {
			heldCount[cpu][lock.level()]--
			if heldCount[cpu][lock.level()] == 0 {
				clearBit(&held[cpu], uint32(1)<<lock.level())
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
		fatal("kernel state used without its lock: " + lock.level().String())
	}
}

// Guard names the lock that guards a package's state when the package does
// not own it: the kernel installs the lock at boot, and the package checks it
// at each entry. Host tests install nothing and check nothing.
type Guard struct {
	spin *Spin
	rw   *RW
}

// Set installs the guarding lock.
func (guard *Guard) Set(spin *Spin) { guard.spin = spin }

// Installed reports whether a guarding lock is installed.
func (guard *Guard) Installed() bool { return guard.spin != nil || guard.rw != nil }

// SetRW installs a reader-writer lock as the guard.
func (guard *Guard) SetRW(rw *RW) { guard.rw = rw }

// Check stops a validation build whose caller does not hold the guarding
// lock, for reading at least.
func (guard *Guard) Check() {
	if !validation.Enabled {
		return
	}
	if guard.spin != nil {
		guard.spin.AssertHeld()
	}
	if guard.rw != nil {
		guard.rw.AssertHeld()
	}
}

// CheckWriter stops a validation build whose caller changes the guarded
// state without holding the guarding lock exclusively.
func (guard *Guard) CheckWriter() {
	if !validation.Enabled {
		return
	}
	if guard.spin != nil {
		guard.spin.AssertHeld()
	}
	if guard.rw != nil {
		guard.rw.AssertWriter()
	}
}

// Contended returns how many acquisitions had to wait.
func Contended() uint64 { return contended.Load() }

// Statistics returns the contention of one level.
func Statistics(level Level) Stats {
	if level >= levelCount {
		return Stats{}
	}
	acquired := uint64(0)
	for cpu := range acquisitions {
		acquired += acquisitions[cpu][level].Load()
	}
	return Stats{
		Acquired:  acquired,
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
	if lock.level() == 0 || lock.level() >= levelCount || cpu >= MaxCPUs {
		return cpu
	}
	if first != nil {
		if first.level() != lock.level() || first.owner.Load() != cpu+1 || first.rank >= lock.rank {
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

func (lock *Spin) acquired(cpu uint32) {
	count(lock.level())
	if !validation.Enabled {
		return
	}
	lock.owner.Store(cpu + 1)
	if lock.level() != 0 && lock.level() < levelCount && cpu < MaxCPUs {
		heldCount[cpu][lock.level()]++
		setBit(&held[cpu], uint32(1)<<lock.level())
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
