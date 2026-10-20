//go:build uefi

package amd64

import (
	"unsafe"

	"goos/kernel/sched"
)

// Preemption entries (ADR 0204). The native goroutine runtime switches
// goroutines only at points where its own state is consistent, so a
// goroutine that computes without calling the runtime would keep its thread
// forever. A thread that registers an entry asks the kernel to divert it
// there after it has run user code for an interval of ticks: the entry saves
// every register on the stack it was interrupted on and lets the runtime
// decide whether to switch.
//
// Delivery changes only the task's saved context, which lies in kernel
// memory: RIP becomes the entry, RSP drops by the red zone, and the
// interrupted RIP waits in preemptRIP until the entry takes it with
// ThreadPreemptTake. Until then no second delivery happens, so the entry's
// prologue is never interrupted by itself. The scheduler lock guards all of
// it.
var (
	preemptEntry [slotCount]uint64
	preemptTicks [slotCount]uint64
	preemptCount [slotCount]uint64
	preemptRIP   [slotCount]uint64
	preemptArmed [slotCount]bool
	// preemptDeliveries counts the diversions made.
	preemptDeliveries uint64
)

// preemptRedZone matches kcall.PreemptRedZone.
const preemptRedZone = 128

// SetTaskPreempt registers entry for task, delivered after every ticks
// ticks of user time; a zero entry turns delivery off.
func SetTaskPreempt(task sched.TaskID, entry, ticks uint64) bool {
	flags := lockScheduler()
	slot, ok := runState.Alive(task)
	if ok {
		preemptEntry[slot], preemptTicks[slot], preemptCount[slot] = entry, ticks, 0
		preemptRIP[slot], preemptArmed[slot] = 0, false
	}
	unlockScheduler(flags)
	return ok
}

// TakeTaskPreempt returns the instruction pointer the last delivery to task
// interrupted and allows the next delivery. It answers false when no
// delivery is waiting.
func TakeTaskPreempt(task sched.TaskID) (uint64, bool) {
	flags := lockScheduler()
	slot, ok := runState.Alive(task)
	rip := uint64(0)
	if ok && preemptArmed[slot] {
		rip, preemptRIP[slot], preemptArmed[slot] = preemptRIP[slot], 0, false
	} else {
		ok = false
	}
	unlockScheduler(flags)
	return rip, ok
}

// PreemptDeliveries returns how many diversions the kernel made.
func PreemptDeliveries() uint64 {
	flags := lockScheduler()
	count := preemptDeliveries
	unlockScheduler(flags)
	return count
}

// clearPreempt forgets a slot's entry when the slot gets a new task.
func clearPreempt(slot uint64) {
	preemptEntry[slot], preemptTicks[slot], preemptCount[slot] = 0, 0, 0
	preemptRIP[slot], preemptArmed[slot] = 0, false
}

// notePreemptTick runs for every tick that interrupted slot, with the
// scheduler lock held and slot's context saved. It diverts the task when it
// interrupted user code, the task has an entry, its interval has passed,
// and no earlier delivery is waiting.
func notePreemptTick(slot uint64) {
	if slot < kernelTasks || slot >= slotCount || preemptEntry[slot] == 0 || preemptArmed[slot] {
		return
	}
	address := taskContexts[slot]
	if address == 0 {
		return
	}
	// SAFETY: the tick saved this context on the task's own kernel stack
	// just before the scheduler was called, and the scheduler lock keeps the
	// task from resuming while the frame changes.
	context := (*[contextWords]uint64)(unsafe.Pointer(uintptr(address)))
	if context[16] != uint64(userCodeSelector) {
		return
	}
	preemptCount[slot]++
	if preemptCount[slot] < preemptTicks[slot] {
		return
	}
	preemptCount[slot] = 0
	rsp := context[18]
	if rsp < preemptRedZone+16 {
		return
	}
	preemptRIP[slot], preemptArmed[slot] = context[15], true
	context[15] = preemptEntry[slot]
	context[18] = rsp - preemptRedZone
	preemptDeliveries++
}
