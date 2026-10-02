//go:build uefi

package main

import (
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/diag"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/sched"
)

var linuxTIDWire [4]byte

// cloneThread creates a thread: a task in the caller's process that shares
// its address space, descriptors, and signal handlers, and resumes after
// this call with RAX zero on the stack the caller gave. It gets its own FS
// base (CLONE_SETTLS), the caller's x87 and SSE state, and a TID from the
// PID space, written where CLONE_PARENT_SETTID and CLONE_CHILD_SETTID ask.
// Every check that can fail runs before the task exists: a runnable task
// cannot be withdrawn.
func (handler *linuxCallHandler) cloneThread(form linuxcompat.CloneForm) linuxcompat.Result {
	owner, found := processRegistry.ByTask(handler.task)
	if !found || handler.frame == nil {
		return linuxErrno(linuxabi.EIO)
	}
	if !vm.ValidateUserRange(form.Stack, 0) || form.SetTLS && !linuxcompat.UserFSBase(form.TLS, vm.UserAddressLimit) {
		return linuxErrno(linuxabi.EINVAL)
	}
	// The family and global process budgets bound processes; tasks are
	// bounded by the process's task quota and the scheduler's slots.
	if owner.Available(process.ResourceTasks) < 1 || owner.TaskCount() >= process.MaxTasksPerProcess || !linuxcompat.ThreadSlotFree() {
		return linuxErrno(linuxabi.EAGAIN)
	}
	fsBase, fsFound := amd64.TaskFSBase(handler.task)
	registers, registersFound := forkRegisters(handler.task, handler.frame)
	if !fsFound || !registersFound {
		return linuxErrno(linuxabi.EIO)
	}
	registers.RSP = form.Stack
	if form.SetTLS {
		fsBase = form.TLS
	}
	tid := linuxcompat.AllocateTID()
	putUint32(linuxTIDWire[:], uint32(tid))
	if form.SetParentTID && !vm.CopyOut(handler.root, form.ParentTID, linuxTIDWire[:]) ||
		form.SetChildTID && !vm.CopyOut(handler.root, form.ChildTID, linuxTIDWire[:]) {
		return linuxErrno(linuxabi.EFAULT)
	}
	task, added := amd64.AddUserTaskWithRegisters(&pageAllocator, handler.root, registers)
	if !added {
		return linuxErrno(linuxabi.EAGAIN)
	}
	parentFPU, _ := amd64.LinuxUserFPU(handler.task)
	thread, recorded := linuxcompat.AddThread(sched.TaskID(task), handler.root, tid)
	if !recorded || !processRegistry.BindTask(owner.ID(), sched.TaskID(task)) ||
		!amd64.SetTaskFSBase(sched.TaskID(task), fsBase) || !amd64.SetTaskFPU(sched.TaskID(task), parentFPU) {
		panicSerial("Linux thread could not be bound")
	}
	if form.ClearChildTID {
		thread.ClearChildTID = form.ChildTID
	}
	// A thread starts with its creator's blocked mask.
	if state, found := linuxcompat.FindProcess(handler.root); found {
		thread.Mask = state.Signals.SaveThreadMask()
	}
	trace(diag.ProcessSpawned, owner.ID(), task, 0, 0)
	return linuxcompat.Result{Value: uint64(tid)}
}

// execEndedTasks are tasks exec ended in the image it replaced. Their exit
// touches nothing in the process, which now runs the new image.
var execEndedTasks [sched.MaxTasks]sched.TaskID

func isExecEnded(task sched.TaskID) bool {
	for index := range execEndedTasks {
		if execEndedTasks[index] == task {
			return true
		}
	}
	return false
}

func takeExecEnded(task sched.TaskID) bool {
	for index := range execEndedTasks {
		if execEndedTasks[index] == task {
			execEndedTasks[index] = 0
			return true
		}
	}
	return false
}

// endLinuxThread runs for a Linux task that ended while its process lives
// on. The address the thread registered with CLONE_CHILD_CLEARTID or
// set_tid_address is zeroed and one waiter on it woken, which is how
// pthread_join learns the thread is gone. A task that ended abnormally, by
// a fault or a kill, takes the whole process with it, as a fatal signal
// does on Linux.
func endLinuxThread(task sched.TaskID, owner *process.Process, code uint64) {
	thread, isThread := linuxcompat.RemoveThread(task)
	releaseThreadIOBuffer(owner, thread.IOBuffer)
	if takeExecEnded(task) {
		return
	}
	root := owner.AddressSpace().Root
	address := thread.ClearChildTID
	if !isThread {
		// The first thread left with exit while others run.
		if state, found := linuxcompat.FindProcess(root); found {
			address, state.ClearChildTID = state.ClearChildTID, 0
		}
	}
	if address != 0 {
		clear(linuxTIDWire[:])
		if vm.CopyOut(root, address, linuxTIDWire[:]) {
			wakeFutex(root, address, linuxcompat.FutexBitsetAll, 1)
		}
	}
	if linuxcompat.AbnormalExit(code) {
		for _, other := range owner.Tasks() {
			cancelTaskWaits(other)
			amd64.TerminateTask(uint64(other), code)
		}
	}
}

// endOtherLinuxThreads ends every task of the caller's process but the
// caller, before exec replaces the image they run in, and makes the caller
// the process's first thread, whose TID is the PID.
func endOtherLinuxThreads(handler *linuxCallHandler) {
	owner, found := processRegistry.ByTask(handler.task)
	if !found {
		return
	}
	if thread, isThread := linuxcompat.RemoveThread(handler.task); isThread {
		releaseThreadIOBuffer(owner, thread.IOBuffer)
		state, _ := linuxcompat.FindProcess(handler.root)
		linuxcompat.AdoptSignalsAsFirst(state)
	}
	for _, other := range owner.Tasks() {
		if other == handler.task {
			continue
		}
		if thread, isThread := linuxcompat.RemoveThread(other); isThread {
			releaseThreadIOBuffer(owner, thread.IOBuffer)
		}
		for index := range execEndedTasks {
			if execEndedTasks[index] == 0 {
				execEndedTasks[index] = other
				break
			}
		}
		cancelTaskWaits(other)
		amd64.TerminateTask(uint64(other), linuxcompat.SignalExitCode(linuxabi.SignalKill))
	}
}
