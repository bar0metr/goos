//go:build uefi

package amd64

import (
	"unsafe"

	"goos/kernel/sched"
)

// UserSnapshot is the user state of a task held off every processor: what a
// collector scans for the task's roots (ADR 0203).
type UserSnapshot struct {
	Registers UserRegisters
	// Vectors holds XMM0 to XMM15, two words each.
	Vectors [32]uint64
}

// SnapshotHeldTask copies the user state of a task that HoldTasks holds and
// QuiesceTasks saw leave its processor. Every way a user task leaves a
// processor, a timer or reschedule interrupt, a native call that blocks,
// retries or yields, or a fault that reschedules, saves its fifteen
// general-purpose registers and IRETQ frame as the context the scheduler
// restores, R15 lowest. The snapshot is taken only from such a context, one
// whose CS is the user selector; any other answers false.
func SnapshotHeldTask(task sched.TaskID, into *UserSnapshot) bool {
	flags := lockScheduler()
	ok := snapshotLocked(task, into)
	unlockScheduler(flags)
	return ok
}

func snapshotLocked(task sched.TaskID, into *UserSnapshot) bool {
	slot, ok := runState.Alive(task)
	if !ok || slot < kernelTasks || runState.RunningOn(slot) != sched.Idle {
		return false
	}
	address := taskContexts[slot]
	if address == 0 {
		return false
	}
	// SAFETY: a task off every processor keeps its context on its own
	// kernel stack, which stays mapped for the task's life; the scheduler
	// lock keeps it from being resumed while the frame is read.
	context := (*[contextWords]uint64)(unsafe.Pointer(uintptr(address)))
	if context[16] != uint64(userCodeSelector) {
		return false
	}
	registers := &into.Registers
	registers.R15, registers.R14, registers.R13, registers.R12 = context[0], context[1], context[2], context[3]
	registers.R11, registers.R10, registers.R9, registers.R8 = context[4], context[5], context[6], context[7]
	registers.RDI, registers.RSI, registers.RBP = context[8], context[9], context[10]
	registers.RDX, registers.RCX, registers.RBX, registers.RAX = context[11], context[12], context[13], context[14]
	registers.RIP, registers.RFlags, registers.RSP = context[15], context[17], context[18]
	// The FXSAVE layout keeps XMM0 to XMM15 from byte 160.
	area := &taskFPU[slot]
	for index := range into.Vectors {
		offset := 160 + index*8
		into.Vectors[index] = uint64(area[offset]) | uint64(area[offset+1])<<8 | uint64(area[offset+2])<<16 | uint64(area[offset+3])<<24 |
			uint64(area[offset+4])<<32 | uint64(area[offset+5])<<40 | uint64(area[offset+6])<<48 | uint64(area[offset+7])<<56
	}
	return true
}
