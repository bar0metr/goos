//go:build uefi

package amd64

import (
	"unsafe"

	"goos/internal/validation"
	"goos/kernel/sched"
)

// The kernel side of instruction holds (hold.go). Only validation kernels
// arm one; every check below starts with validation.Enabled, so other
// kernels compile it away. The scheduler lock guards the table.
//
// A thread stops on a debug exception: the entry code saves its registers
// and IRETQ frame as the context a switch restores, which is also the
// context a collection snapshots, and blocks it. While it stands, DR0 holds
// the RIP it stopped at and TF is clear, so a wake that is not a step, such
// as a stale wake left from before it stopped, runs it straight back into
// the same stop. A processor loads a task's DR0 and DR7 when it switches to
// the task, and clears DR7 for any task without a hold.

type taskHold struct {
	armed bool
	// stale marks a hold cancelled while its task ran: the debug exception
	// the old DR7 or TF may still raise resumes the task instead of ending
	// it.
	stale bool
	state uint8
	// quiet stops preemption delivery once the first stop came, so a
	// diversion cannot enter the range in the middle of a step.
	quiet    bool
	lo, hi   uint64
	rip      uint64
	previous uint64
	dr0, dr7 uint64
}

var (
	holds [sched.MaxTasks]taskHold
	// loadedDR0 and loadedDR7 are what each processor's registers hold, so
	// a switch writes them only when they change.
	loadedDR0, loadedDR7 [MaxCPUs]uint64
)

//export goos_read_dr6
func readDR6() uint64

//export goos_write_dr6
func writeDR6(value uint64)

//export goos_write_dr0
func writeDR0(value uint64)

//export goos_write_dr7
func writeDR7(value uint64)

// What goos_user_debug tells the entry code to do with the task.
const (
	debugResume    = 0
	debugHold      = 1
	debugTerminate = 2
)

// userDebug handles a debug exception from user mode, with the task's
// fifteen registers and IRETQ frame saved at context and interrupts off. A
// task without a hold ends as before.
//
//export goos_user_debug
func userDebug(context uintptr) uint64 {
	if !validation.Enabled {
		return debugTerminate
	}
	writeDR6(dr6Clear)
	slot := currentSlot()
	if slot == sched.Idle || slot < kernelTasks {
		return debugTerminate
	}
	// SAFETY: the entry code passes the frame it just saved on the task's
	// own kernel stack, which the task keeps while it runs this handler.
	frame := (*[contextWords]uint64)(unsafe.Pointer(context))
	cpu := CPUIndex()
	schedulerLock.Lock()
	hold := &holds[slot]
	action := uint64(debugHold)
	switch {
	case !hold.armed && hold.stale:
		frame[17] &^= flagTF
		hold.dr0, hold.dr7 = 0, 0
		action = debugResume
	case !hold.armed:
		action = debugTerminate
	case hold.state == holdRunning:
		rip := frame[15]
		hold.rip = rip
		hold.state = holdLeft
		if rip >= hold.lo && rip < hold.hi {
			hold.state = holdStopped
		}
		hold.quiet = true
		frame[17] &^= flagTF | flagRF
		hold.dr0, hold.dr7 = rip, dr7Local0
	}
	loadDebug(cpu, hold.dr0, hold.dr7)
	schedulerLock.Unlock()
	return action
}

// loadDebug makes cpu's DR0 and DR7 hold dr0 and dr7. Interrupts are off.
func loadDebug(cpu int, dr0, dr7 uint64) {
	if dr7 == loadedDR7[cpu] && (dr7 == 0 || dr0 == loadedDR0[cpu]) {
		return
	}
	writeDR7(0)
	if dr7 != 0 {
		writeDR0(dr0)
		loadedDR0[cpu] = dr0
		writeDR7(dr7)
	}
	loadedDR7[cpu] = dr7
}

// switchDebug loads the debug registers of the task cpu switches to. The
// scheduler lock is held.
func switchDebug(cpu int, next int) {
	dr0, dr7 := uint64(0), uint64(0)
	if next != sched.Idle && next < sched.MaxTasks {
		dr0, dr7 = holds[next].dr0, holds[next].dr7
	}
	loadDebug(cpu, dr0, dr7)
}

// holdSuppressesPreempt reports whether a hold keeps preemption from
// diverting slot. The scheduler lock is held.
func holdSuppressesPreempt(slot uint64) bool {
	return slot < sched.MaxTasks && holds[slot].armed && holds[slot].quiet
}

// clearHold forgets a slot's hold when the slot gets a new task.
func clearHold(slot uint64) {
	if validation.Enabled && slot < sched.MaxTasks {
		holds[slot] = taskHold{}
	}
}

// HoldStatus is what TaskHoldStatus reports of one hold.
type HoldStatus struct {
	Armed bool
	// Off reports that the task is on no processor, so its saved context
	// is the one it resumes from.
	Off      bool
	State    int
	RIP, RSP uint64
	Previous uint64
	Lo, Hi   uint64
}

// The states TaskHoldStatus reports.
const (
	HoldRunning = holdRunning
	HoldStopped = holdStopped
	HoldLeft    = holdLeft
)

// ArmTaskHold arms a hold of task at the first boundary of [lo, hi). A
// processor that runs the task is asked to switch, which loads the
// breakpoint. It refuses the caller, kernel tasks, and dead tasks.
func ArmTaskHold(task sched.TaskID, lo, hi uint64) bool {
	if !validation.Enabled {
		return false
	}
	cpu := CPUIndex()
	flags := lockScheduler()
	slot, ok := runState.Alive(task)
	ok = ok && slot >= kernelTasks && runState.Current(cpu) != slot
	target := sched.Idle
	if ok {
		holds[slot] = taskHold{armed: true, state: holdRunning, lo: lo, hi: hi, dr0: lo, dr7: dr7Local0}
		target = runState.RunningOn(slot)
	}
	schedulerLock.Unlock()
	if target != sched.Idle {
		kickCPU(target, cpu)
	}
	RestoreInterrupts(flags)
	return ok
}

// TaskHoldStatus reads task's hold and, once the task stopped and left its
// processor, the stack pointer it stopped with.
func TaskHoldStatus(task sched.TaskID) (HoldStatus, bool) {
	if !validation.Enabled {
		return HoldStatus{}, false
	}
	flags := lockScheduler()
	slot, ok := runState.Alive(task)
	var status HoldStatus
	if ok && slot >= kernelTasks {
		hold := &holds[slot]
		status = HoldStatus{Armed: hold.armed, State: int(hold.state), RIP: hold.rip, Previous: hold.previous, Lo: hold.lo, Hi: hold.hi}
		if context, held := stoppedContext(slot); held {
			status.Off, status.RSP = true, context[18]
		}
	}
	unlockScheduler(flags)
	return status, ok
}

// stoppedContext returns the saved context of a slot whose hold stopped it
// and which is on no processor. The scheduler lock is held.
func stoppedContext(slot int) (*[contextWords]uint64, bool) {
	hold := &holds[slot]
	if !hold.armed || hold.state != holdStopped && hold.state != holdLeft || runState.RunningOn(slot) != sched.Idle {
		return nil, false
	}
	address := taskContexts[slot]
	if address == 0 {
		return nil, false
	}
	// SAFETY: a task off every processor keeps its context on its own
	// kernel stack, and the scheduler lock keeps it from resuming while
	// the frame changes.
	context := (*[contextWords]uint64)(unsafe.Pointer(uintptr(address)))
	if context[16] != uint64(userCodeSelector) || context[15] != hold.rip {
		return nil, false
	}
	return context, true
}

// StepTaskHold lets a stopped task run to its next stop: a single step, or
// a run to breakpoint when it is nonzero. The task must stand at a stop and
// be off its processor.
func StepTaskHold(task sched.TaskID, breakpoint uint64) bool {
	if !validation.Enabled {
		return false
	}
	flags := lockScheduler()
	slot, ok := runState.Alive(task)
	var context *[contextWords]uint64
	if ok && slot >= kernelTasks {
		context, ok = stoppedContext(slot)
	} else {
		ok = false
	}
	if ok {
		hold := &holds[slot]
		hold.previous, hold.state = hold.rip, holdRunning
		if breakpoint == 0 {
			context[17] |= flagTF
			hold.dr0, hold.dr7 = 0, 0
		} else {
			context[17] &^= flagTF
			hold.dr0, hold.dr7 = breakpoint, dr7Local0
		}
	}
	unlockScheduler(flags)
	if ok {
		WakeTaskID(uint64(task))
	}
	return ok
}

// EndTaskHold disarms task's hold and lets it run on with TF and its
// breakpoint clear. A task that stopped is woken; one on no processor gets
// TF cleared in its saved flags; one that runs is asked to switch, which
// clears its processor's DR7. A debug exception still raised by the old
// state resumes the task.
func EndTaskHold(task sched.TaskID) bool {
	if !validation.Enabled {
		return false
	}
	cpu := CPUIndex()
	flags := lockScheduler()
	slot, ok := runState.Alive(task)
	ok = ok && slot >= kernelTasks && holds[slot].armed
	target, wake := sched.Idle, false
	if ok {
		hold := &holds[slot]
		if context, stopped := stoppedContext(slot); stopped {
			context[17] &^= flagTF
			wake = true
		} else if target = runState.RunningOn(slot); target == sched.Idle {
			// SAFETY: as in stoppedContext, a task off every processor
			// keeps its saved context on its own kernel stack.
			if address := taskContexts[slot]; address != 0 {
				context := (*[contextWords]uint64)(unsafe.Pointer(uintptr(address)))
				if context[16] == uint64(userCodeSelector) {
					context[17] &^= flagTF
				}
			}
		}
		*hold = taskHold{stale: true}
	}
	schedulerLock.Unlock()
	if target != sched.Idle {
		kickCPU(target, cpu)
	}
	RestoreInterrupts(flags)
	if wake {
		WakeTaskID(uint64(task))
	}
	return ok
}
