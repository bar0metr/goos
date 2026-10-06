//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/process"
	"goos/kernel/sched"
)

// ProcessKill stops a process the caller holds a writable process handle for.
// Its tasks are retired the way a fault retires one, so the normal teardown
// delivers the exit event, releases the handles, and frees the address space.
func (handler nativeCallHandler) ProcessKill(processHandle uint64) kcall.Status {
	owner, found := handler.self()
	if !found {
		return kcall.StatusPermissionDenied
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(processHandle), handle.TypeProcess, capability.Write)
	if !allowed {
		return kcall.StatusPermissionDenied
	}
	pid := process.ID(authority.Object())
	target, exists := processRegistry.ByID(pid)
	if !exists || pid == 1 || target == owner {
		return kcall.StatusPermissionDenied
	}
	stopped := false
	for _, taskID := range target.Tasks() {
		cancelTaskWaits(taskID)
		if amd64.TerminateTask(uint64(taskID), kcall.ExitKilled) {
			stopped = true
		}
	}
	killLinuxFamily(target.AddressSpace().Root)
	if !stopped {
		return kcall.StatusNotFound
	}
	return kcall.StatusOK
}

// cancelTaskWaits removes every registration a task may be blocked on, so no
// later wake refers to a slot that has been retired.
//
// An exec the task has in progress is not cancelled here: the task may be
// preparing it on another processor under its family's lock. Its exit, which
// follows, cancels it (deliverTaskExit).
func cancelTaskWaits(taskID sched.TaskID) {
	forgetFutexWaiter(taskID)
	wakeVFSContextWaiter(linuxcompat.CancelVFSTask(taskID))
	linuxcompat.SyscallContinuations.Cancel(taskID)
	ipcWaitSet.Cancel(taskID)
	streamWaitSet.Cancel(taskID)
	processWaitSet.Cancel(taskID)
	waitAnyTable.Cancel(taskID)
	slot, _, valid := taskID.Parts()
	if valid && uint64(slot) < uint64(len(ipcWaitStatus)) {
		ipcWaitStatus.set(uint64(slot), kcall.StatusOK)
		processWaitStatus.set(uint64(slot), kcall.StatusOK)
		streamWaitStatus.set(uint64(slot), kcall.StatusOK)
	}
}
