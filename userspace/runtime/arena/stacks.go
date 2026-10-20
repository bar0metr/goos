//go:build goos_userspace

package arena

import "goos/userspace/runtime/tls"

// Stacks a goroutine runtime adds (ADR 0204). Without one, every thread runs
// on the stack the kernel made for it, and a collection scans each held
// thread from its stack pointer to that stack's top. A runtime that switches
// a thread between stacks of its own registers StackRoots: it then names the
// stack the caller runs on and every stack a collection must scan, parked
// goroutines' included.

// HeldThread is a thread a collection holds, or the collecting thread
// itself: its slot and stack pointer.
type HeldThread struct {
	Slot int
	SP   uintptr
}

// StackRoots is a goroutine runtime's view of the process's stacks. Every
// function runs during a collection with the other threads held: it must
// not allocate, take a lock another thread may hold, or switch stacks.
type StackRoots struct {
	// Current returns the bounds of the stack the caller runs on.
	Current func() (lo, hi uintptr)
	// Plan decides what the next mark scans, before anything is marked.
	// self is the collecting thread, held the others. It answers false
	// when a stack pointer lies in no stack the runtime knows; the
	// collection is then given up and nothing is swept.
	Plan func(self HeldThread, held []HeldThread) bool
	// Mark marks every range Plan chose, except the stack the collector
	// runs on, which markStack scans from its own frame up.
	Mark func(mark func(from, to uintptr))
}

var (
	stackRoots StackRoots
	// held lists the threads of the collection that runs, the collector
	// first.
	held      [tls.Slots]HeldThread
	heldCount int
	// planFailures counts collections given up because a stack pointer lay
	// in no known stack.
	planFailures uint64
)

// SetStackRoots registers a goroutine runtime's stacks. It is called once,
// before the runtime switches any thread to a stack of its own.
func SetStackRoots(roots StackRoots) { stackRoots = roots }

// currentStack returns the bounds of the stack the caller runs on.
func currentStack() (lo, hi uintptr) {
	if stackRoots.Current != nil {
		return stackRoots.Current()
	}
	return threadStack(selfSlot())
}

// markStackRange marks a range a goroutine runtime names.
func markStackRange(from, to uintptr) { native.markRange(from, to) }

// planStacks lets the goroutine runtime check the stack pointers of a
// collection: the collector's own, sp, and those of the held threads. It
// reports whether the collection may go on.
func planStacks(slot int, sp uintptr) bool {
	if stackRoots.Plan == nil {
		return true
	}
	held[0] = HeldThread{Slot: slot, SP: sp}
	heldCount = 1
	for index := 0; index < snapshotCount && heldCount < len(held); index++ {
		held[heldCount] = HeldThread{Slot: int(snapshotWord(index, 1)), SP: uintptr(snapshotWord(index, 2))}
		heldCount++
	}
	ok := stackRoots.Plan(held[0], held[1:heldCount])
	if !ok {
		planFailures++
	}
	return ok
}

// Preemptible reports whether the thread in slot may be switched to another
// goroutine where it stands: it is outside the allocation fast path and
// every region. The heap and collection locks pin their holder instead.
func Preemptible(slot int) bool {
	if slot < 0 || slot >= tls.Slots {
		return false
	}
	t := &threads[slot]
	if !threaded.Load() {
		t = &threads[0]
	}
	return t.fast.Load() == 0 && t.regions.depth == 0
}

// InRegion reports whether the calling thread is inside a region. A region
// belongs to the thread, so a goroutine inside one must not park.
func InRegion() bool { return self().regions.depth != 0 }

// MainStack returns the bounds of the main thread's stack, which the kernel
// made.
func MainStack() (lo, hi uintptr) { return stackBase(), stackTop }

// NativeStack returns the bounds of the stack the kernel made for the
// thread in slot.
func NativeStack(slot int) (lo, hi uintptr) {
	if slot < 0 || slot >= tls.Slots {
		return 0, 0
	}
	return threadStack(slot)
}
