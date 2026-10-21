//go:build uefi

package main

// The native thread calls (ADR 0203): threads that share one native
// process, each on its own kernel-made stack, and the suspension a
// collection holds them in.

import (
	"encoding/binary"
	"sync/atomic"

	"goos/kernel/arch/amd64"
	"goos/kernel/kcall"
	"goos/kernel/mm/region"
	"goos/kernel/mm/vm"
	"goos/kernel/nativethread"
	"goos/kernel/process"
	"goos/kernel/sched"
	kernelwait "goos/kernel/wait"
)

// threadCaller returns the caller's process and its thread group, with the
// process's first task adopted as the main thread.
func (handler nativeCallHandler) threadCaller() (*process.Process, *nativethread.Group, sched.TaskID, bool) {
	owner, found := handler.self()
	if !found || owner.ABI() != process.ABINative || owner.TaskCount() == 0 {
		return nil, nil, 0, false
	}
	group := owner.Threads()
	group.Adopt(owner.Tasks()[0])
	return owner, group, amd64.CurrentTask(), true
}

// threadStackRange returns the guard base and the stack top of a slot.
func threadStackRange(slot uint64) (guard uint64, top uint64) {
	top = kcall.ThreadStackTop(slot)
	return top - kcall.ThreadStride, top
}

// executableEntry reports whether entry lies in a region that is readable
// and executable and not writable: a thread never starts in data.
func executableEntry(owner *process.Process, entry uint64) bool {
	if !vm.ValidateUserRange(entry, 1) {
		return false
	}
	found, ok := owner.Regions().Find(entry)
	return ok && found.Protection&region.Execute != 0 && found.Protection&region.Read != 0 && found.Protection&region.Write == 0
}

// ThreadCreate starts a thread of the caller's process at entry. Every step
// that can fail runs before the task starts, and undoes what came before it.
func (handler nativeCallHandler) ThreadCreate(entry uint64, argument uint64, address uint64) kcall.Status {
	local := thisCPU()
	owner, group, caller, found := handler.threadCaller()
	if !found {
		return kcall.StatusPermissionDenied
	}
	if !vm.ValidateUserRange(address, kcall.ThreadInfoSize) || !vm.CanCopyOut(handler.root, address, kcall.ThreadInfoSize) {
		return kcall.StatusInvalidArgument
	}
	if !executableEntry(owner, entry) {
		return kcall.StatusInvalidArgument
	}
	if _, live := group.SlotOf(caller); !live || group.Suspender() != 0 {
		// Only the collecting thread runs while a collection holds the
		// others, and it creates nothing.
		return kcall.StatusPermissionDenied
	}
	slot, reserved := group.Reserve()
	if !reserved {
		return kcall.StatusResourceExhausted
	}
	if owner.TaskCount() >= process.MaxTasksPerProcess || amd64.FreeTaskSlots() == 0 || !owner.ReserveTask() {
		group.Cancel(slot)
		return kcall.StatusResourceExhausted
	}
	guard, top := threadStackRange(slot)
	regions := owner.Regions()
	if regions.Insert(region.Region{Base: guard, Length: kcall.ThreadGuardSize, Backing: region.Guard, Flags: region.Pinned}) != region.OK {
		owner.CancelReservedTask()
		group.Cancel(slot)
		return kcall.StatusResourceExhausted
	}
	if regions.Insert(region.Region{Base: top - kcall.ThreadStackSize, Length: kcall.ThreadStackSize, Protection: region.Read | region.Write, Backing: region.Anonymous, Flags: region.Pinned}) != region.OK {
		regions.Remove(guard, kcall.ThreadGuardSize, true)
		owner.CancelReservedTask()
		group.Cancel(slot)
		return kcall.StatusResourceExhausted
	}
	registers := amd64.UserRegisters{RIP: entry, RSP: top, RDI: argument, RSI: slot}
	task, added := amd64.AddUserTaskWithRegisters(&pageAllocator, handler.root, registers)
	if !added {
		regions.Remove(guard, kcall.ThreadStride, true)
		owner.CancelReservedTask()
		group.Cancel(slot)
		return kcall.StatusResourceExhausted
	}
	taskID := sched.TaskID(task)
	if !owner.CommitReservedTask(taskID) || !processRegistry.NoteOwner(taskID, owner) || !group.Commit(slot, taskID) {
		panicSerial("native thread could not be bound")
	}
	binary.LittleEndian.PutUint64(local.threadInfoWire[0:], uint64(taskID))
	binary.LittleEndian.PutUint64(local.threadInfoWire[8:], slot)
	if !vm.CopyOut(handler.root, address, local.threadInfoWire[:]) {
		panicSerial("validated thread info write failed")
	}
	affinity, _ := amd64.TaskAffinity(caller)
	if !amd64.SetTaskAffinity(taskID, affinity) || !amd64.StartTask(taskID) {
		panicSerial("native thread could not be started")
	}
	return kcall.StatusOK
}

// ThreadExit ends the calling thread with code. Its process goes on while
// another of its threads lives.
func (handler nativeCallHandler) ThreadExit(code uint64) kcall.Status {
	_, group, caller, found := handler.threadCaller()
	if found {
		group.Leave(caller, code)
	}
	amd64.SetCurrentTaskExitCode(code)
	return kcall.StatusOK
}

// ThreadJoin waits for a thread of the caller's process to end and writes
// its code. The joiner registered here is woken by the thread's end, under
// the same lock, so the wake cannot come between the check and the block.
func (handler nativeCallHandler) ThreadJoin(thread uint64, flags uint64, address uint64) (kcall.Status, bool) {
	local := thisCPU()
	_, group, caller, found := handler.threadCaller()
	if !found {
		return kcall.StatusPermissionDenied, false
	}
	if !vm.ValidateUserRange(address, 8) || !vm.CanCopyOut(handler.root, address, 8) {
		return kcall.StatusInvalidArgument, false
	}
	code, status := group.Join(caller, sched.TaskID(thread), flags&kcall.JoinPoll == 0)
	switch status {
	case nativethread.Joined:
		binary.LittleEndian.PutUint64(local.threadCodeWire[:], code)
		if !vm.CopyOut(handler.root, address, local.threadCodeWire[:]) {
			panicSerial("validated join result write failed")
		}
		return kcall.StatusOK, false
	case nativethread.Waiting:
		if flags&kcall.JoinPoll != 0 {
			return kcall.StatusTimeout, false
		}
		return kcall.StatusOK, true
	case nativethread.Busy:
		return kcall.StatusAlreadyExists, false
	case nativethread.Self:
		return kcall.StatusInvalidArgument, false
	}
	return kcall.StatusNotFound, false
}

// ThreadSelf writes the caller's ID and slot.
func (handler nativeCallHandler) ThreadSelf(address uint64) kcall.Status {
	local := thisCPU()
	_, group, caller, found := handler.threadCaller()
	if !found {
		return kcall.StatusPermissionDenied
	}
	slot, live := group.SlotOf(caller)
	if !live || !vm.ValidateUserRange(address, kcall.ThreadInfoSize) || !vm.CanCopyOut(handler.root, address, kcall.ThreadInfoSize) {
		return kcall.StatusInvalidArgument
	}
	binary.LittleEndian.PutUint64(local.threadInfoWire[0:], uint64(caller))
	binary.LittleEndian.PutUint64(local.threadInfoWire[8:], slot)
	if !vm.CopyOut(handler.root, address, local.threadInfoWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// ThreadYield gives up the processor at the call's return.
func (handler nativeCallHandler) ThreadYield() kcall.Status {
	amd64.RequestReschedule()
	return kcall.StatusOK
}

// ThreadSuspend holds every other live thread of the caller's process off
// its processor and writes a snapshot of each: its ID, slot, stack pointer,
// instruction pointer, general-purpose and XMM registers. A thread that does
// not leave its processor within the bound, or whose saved state is not a
// user context, fails the call; everything held is released first, so a
// collection never runs on part of the roots.
func (handler nativeCallHandler) ThreadSuspend(address uint64, capacity uint64, countAddress uint64) kcall.Status {
	local := thisCPU()
	_, group, caller, found := handler.threadCaller()
	if !found {
		return kcall.StatusPermissionDenied
	}
	if _, live := group.SlotOf(caller); !live {
		return kcall.StatusPermissionDenied
	}
	if group.Suspender() != 0 {
		return kcall.StatusAlreadyExists
	}
	count := group.Others(caller, local.threadTasks[:])
	if uint64(count) > capacity {
		return kcall.StatusInvalidArgument
	}
	length := uint64(count) * kcall.ThreadSnapshotSize
	if !vm.ValidateUserRange(countAddress, 8) || !vm.CanCopyOut(handler.root, countAddress, 8) ||
		count != 0 && (!vm.ValidateUserRange(address, length) || !vm.CanCopyOut(handler.root, address, length)) {
		return kcall.StatusInvalidArgument
	}
	held := local.threadTasks[:count]
	amd64.HoldTasks(held)
	if !amd64.QuiesceTasks(held) {
		amd64.ReleaseTasks(held)
		threadSuspendFailures++
		return kcall.StatusTimeout
	}
	written := uint64(0)
	for index := range held {
		if !amd64.TaskLive(held[index]) {
			// It has ended; its stack holds nothing that is still used.
			continue
		}
		slot, _ := group.SlotOf(held[index])
		if !amd64.SnapshotHeldTask(held[index], &local.threadSnapshot) {
			amd64.ReleaseTasks(held)
			threadSuspendFailures++
			return kcall.StatusTimeout
		}
		encodeThreadSnapshot(&local.threadSnapshotWire, held[index], slot, &local.threadSnapshot)
		if !vm.CopyOut(handler.root, address+written*kcall.ThreadSnapshotSize, local.threadSnapshotWire[:]) {
			amd64.ReleaseTasks(held)
			return kcall.StatusInvalidArgument
		}
		written++
	}
	if !group.BeginSuspend(caller, held) {
		amd64.ReleaseTasks(held)
		return kcall.StatusAlreadyExists
	}
	binary.LittleEndian.PutUint64(local.threadCodeWire[:], written)
	if !vm.CopyOut(handler.root, countAddress, local.threadCodeWire[:]) {
		panicSerial("validated suspension count write failed")
	}
	threadSuspensions++
	return kcall.StatusOK
}

// ThreadResume releases the threads the caller's ThreadSuspend holds.
func (handler nativeCallHandler) ThreadResume() kcall.Status {
	local := thisCPU()
	_, group, caller, found := handler.threadCaller()
	if !found {
		return kcall.StatusPermissionDenied
	}
	count, ok := group.EndSuspend(caller, local.threadTasks[:])
	if !ok {
		return kcall.StatusNotFound
	}
	amd64.ReleaseTasks(local.threadTasks[:count])
	return kcall.StatusOK
}

// Suspension counters for the diagnostics dump.
var threadSuspensions, threadSuspendFailures uint64

// encodeThreadSnapshot lays a snapshot out as kcall documents it.
func encodeThreadSnapshot(wire *[kcall.ThreadSnapshotSize]byte, task sched.TaskID, slot uint64, snapshot *amd64.UserSnapshot) {
	registers := &snapshot.Registers
	words := [...]uint64{
		uint64(task), slot, registers.RSP, registers.RIP,
		registers.RAX, registers.RBX, registers.RCX, registers.RDX, registers.RSI, registers.RDI, registers.RBP,
		registers.R8, registers.R9, registers.R10, registers.R11, registers.R12, registers.R13, registers.R14, registers.R15,
	}
	for index := range words {
		binary.LittleEndian.PutUint64(wire[index*8:], words[index])
	}
	for index := range snapshot.Vectors {
		binary.LittleEndian.PutUint64(wire[kcall.ThreadSnapshotVectors+uint64(index)*8:], snapshot.Vectors[index])
	}
	binary.LittleEndian.PutUint64(wire[kcall.ThreadSnapshotSize-8:], 0)
}

// nativeThreadEnding records a native thread's end before its process hears
// of it, and returns the code the process ends with if this was its last
// task: the first code an Exit, fault, or kill of any thread set, or else the
// task's own.
func nativeThreadEnding(owner *process.Process, task sched.TaskID, code uint64) (nativethread.Ending, bool, uint64) {
	group := owner.Threads()
	group.Forget(task)
	ending, found := group.Ended(task, code)
	if found && !ending.Leaving {
		group.SetExitCode(code)
	}
	if found {
		// The group already records the end, under the registry's write
		// lock, so a waiter's recheck sees it or this finds its
		// registration.
		publishWaitAny(kernelwait.Key{Kind: kernelwait.KindThreadEnd, Object: uint64(task)}, kernelwait.Exited)
	}
	if recorded, set := group.ExitCode(); set {
		return ending, found, recorded
	}
	return ending, found, code
}

// nativeThreadEnded finishes a native thread's end while its process lives
// on: the threads a suspension it made holds go free, its joiner wakes, its
// stack goes back, and an end that was not its own ThreadExit takes the
// other threads with it.
func nativeThreadEnded(owner *process.Process, task sched.TaskID, ending nativethread.Ending) {
	local := thisCPU()
	group := owner.Threads()
	if count, ok := group.EndSuspend(task, local.threadTasks[:]); ok {
		amd64.ReleaseTasks(local.threadTasks[:count])
	}
	if ending.Slot != 0 {
		guard, top := threadStackRange(ending.Slot)
		if owner.Regions().Remove(guard, kcall.ThreadStride, true) == region.OK {
			releasePages(owner, vm.UnmapUserRange(&pageAllocator, owner.AddressSpace().Root, top-kcall.ThreadStackSize, kcall.ThreadStackSize))
		}
	}
	if ending.Leaving {
		if ending.Joiner != 0 {
			amd64.WakeTaskID(uint64(ending.Joiner))
		}
		return
	}
	count := group.Others(task, local.threadTasks[:])
	for _, other := range local.threadTasks[:count] {
		cancelTaskWaits(other)
		amd64.TerminateTask(uint64(other), kcall.ExitKilled)
	}
}

// ThreadSetBase sets the calling thread's FS base. The native runtime points
// it at the thread's control block (ADR 0204). Any canonical user address or
// zero is accepted: the base only changes what the thread's own FS-relative
// loads read, and a load through an unmapped base faults in that thread.
func (handler nativeCallHandler) ThreadSetBase(base uint64) kcall.Status {
	_, _, caller, found := handler.threadCaller()
	if !found {
		return kcall.StatusPermissionDenied
	}
	if base != 0 && !vm.ValidateUserRange(base, 8) {
		return kcall.StatusInvalidArgument
	}
	if !amd64.SetTaskFSBase(caller, base) {
		return kcall.StatusNotFound
	}
	return kcall.StatusOK
}

// ThreadPreempt registers the calling thread's preemption entry, delivered
// after every ticks ticks of user time, or turns delivery off with a zero
// entry. The entry must lie in code, as a thread's start does.
func (handler nativeCallHandler) ThreadPreempt(entry uint64, ticks uint64) kcall.Status {
	owner, _, caller, found := handler.threadCaller()
	if !found {
		return kcall.StatusPermissionDenied
	}
	if entry != 0 && !executableEntry(owner, entry) {
		return kcall.StatusInvalidArgument
	}
	if !amd64.SetTaskPreempt(caller, entry, ticks) {
		return kcall.StatusNotFound
	}
	return kcall.StatusOK
}

// ThreadPreemptTake writes the instruction pointer the last delivery
// interrupted and allows the next one. NotFound means no delivery waits.
func (handler nativeCallHandler) ThreadPreemptTake(address uint64) kcall.Status {
	local := thisCPU()
	_, _, caller, found := handler.threadCaller()
	if !found {
		return kcall.StatusPermissionDenied
	}
	if !vm.ValidateUserRange(address, 8) || !vm.CanCopyOut(handler.root, address, 8) {
		return kcall.StatusInvalidArgument
	}
	rip, taken := amd64.TakeTaskPreempt(caller)
	if !taken {
		return kcall.StatusNotFound
	}
	binary.LittleEndian.PutUint64(local.threadCodeWire[:], rip)
	if !vm.CopyOut(handler.root, address, local.threadCodeWire[:]) {
		panicSerial("validated preemption result write failed")
	}
	return kcall.StatusOK
}

// threadWakes holds, per task slot, the task ID of a thread whose wake flag
// is set. Holding the ID rather than a bit makes a flag left by a thread that
// ended mean nothing to the next task in its slot.
var threadWakes [sched.MaxTasks]atomic.Uint64

// ThreadWake sets the wake flag of a thread of the caller's process and
// completes a WaitAny of that thread on WaitAnyThread (ADR 0204). It
// confers nothing: the flag only ends a wait.
func (handler nativeCallHandler) ThreadWake(thread uint64) kcall.Status {
	_, group, _, found := handler.threadCaller()
	if !found {
		return kcall.StatusPermissionDenied
	}
	target := sched.TaskID(thread)
	slot, _, valid := target.Parts()
	if !valid {
		return kcall.StatusNotFound
	}
	if _, live := group.SlotOf(target); !live {
		return kcall.StatusNotFound
	}
	threadWakes[slot].Store(thread)
	publishWaitAny(kernelwait.Key{Kind: kernelwait.KindThread, Object: thread}, kernelwait.Readable)
	return kcall.StatusOK
}

// ThreadWakeTake clears the caller's wake flag. It answers NotFound when the
// flag was not set.
func (handler nativeCallHandler) ThreadWakeTake() kcall.Status {
	_, _, caller, found := handler.threadCaller()
	if !found {
		return kcall.StatusPermissionDenied
	}
	slot, _, valid := caller.Parts()
	if !valid || !threadWakes[slot].CompareAndSwap(uint64(caller), 0) {
		return kcall.StatusNotFound
	}
	return kcall.StatusOK
}

// threadWakeSet reports whether the wake flag of task is set.
func threadWakeSet(task uint64) bool {
	slot, _, valid := sched.TaskID(task).Parts()
	return valid && threadWakes[slot].Load() == task
}
