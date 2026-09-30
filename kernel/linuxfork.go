//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/diag"
	"goos/kernel/handle"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/sched"
)

// Fork creates a child Process from the caller: a private copy of every page
// it has touched, its region map, descriptors that share the parent's open-file
// descriptions, and the registers of this call with RAX zero. The child gets no
// authority the parent does not hold. Every step that can fail runs before the
// child's task exists, and a failure leaves no trace of the child.
func (handler *linuxCallHandler) Fork(request linuxcompat.Request) linuxcompat.Result {
	parent, found := processRegistry.ByTask(handler.task)
	compat, compatFound := linuxcompat.FindProcess(handler.root)
	if !found || !compatFound || handler.frame == nil || parent.TaskCount() != 1 || kernelPageTable == 0 {
		return linuxErrno(linuxabi.EIO)
	}
	vfsAuthority, vfsFound := parent.Handles().Lookup(compat.VFSHandle, handle.TypeChannel, capability.Read|capability.Write|capability.Wait)
	fsBase, fsFound := amd64.TaskFSBase(handler.task)
	registers, registersFound := forkRegisters(handler.task, handler.frame)
	if !vfsFound || !fsFound || !registersFound {
		return linuxErrno(linuxabi.EIO)
	}
	pid := nextProcessID
	record, status := linuxcompat.RegisterChild(pid, compat.PID, processRegistry.Count())
	if status != linuxcompat.ChildRegistrationOK {
		return linuxErrno(linuxabi.EAGAIN)
	}
	child := freeProcessSlot()
	if child == nil {
		linuxcompat.RollbackProcessRecord(pid)
		return linuxErrno(linuxabi.EAGAIN)
	}
	pagesBefore := pageAllocator.Statistics().FreePages
	root, created := vm.CreateUserAddressSpace(&pageAllocator, kernelPageTable)
	if !created {
		linuxcompat.RollbackProcessRecord(pid)
		return linuxErrno(linuxabi.ENOMEM)
	}
	if !process.InitChild(child, pid, parent, process.AddressSpace{Root: root}) {
		abandonFork(pid, root, child, false)
		return linuxErrno(linuxabi.ENOMEM)
	}
	if !vm.CopyUserRange(&pageAllocator, handler.root, root, vm.UserAddressStart, vm.UserAddressLimit-vm.UserAddressStart) {
		abandonFork(pid, root, child, false)
		return linuxErrno(linuxabi.ENOMEM)
	}
	// The child names the same VFS endpoint through its own handle. The
	// family context, not any one process, decides when the endpoint closes.
	vfsHandle, inserted := child.Handles().Insert(handle.TypeChannel, vfsAuthority)
	if !inserted || !linuxcompat.ForkProcess(handler.root, root, record, vfsHandle) {
		abandonFork(pid, root, child, false)
		return linuxErrno(linuxabi.ENOMEM)
	}
	if !processRegistry.Add(child) {
		abandonFork(pid, root, child, false)
		return linuxErrno(linuxabi.EAGAIN)
	}
	if used := pagesBefore - pageAllocator.Statistics().FreePages; used > uint64(^uint32(0)) || used != 0 && !child.Charge(process.ResourcePages, uint32(used)) {
		abandonFork(pid, root, child, true)
		return linuxErrno(linuxabi.ENOMEM)
	}
	if id := parent.Executable(); id != 0 {
		object, found := executables.Find(id)
		if !found {
			abandonFork(pid, root, child, true)
			return linuxErrno(linuxabi.ENOMEM)
		}
		object.Retain()
		child.SetExecutable(id)
	}
	task, added := amd64.AddUserTaskWithRegisters(&pageAllocator, root, registers)
	if !added {
		abandonFork(pid, root, child, true)
		return linuxErrno(linuxabi.ENOMEM)
	}
	// A runnable task cannot be withdrawn. Its process ID is consumed either
	// way so it is never handed out twice.
	nextProcessID++
	if !processRegistry.BindTask(pid, sched.TaskID(task)) || !amd64.SetTaskFSBase(sched.TaskID(task), fsBase) {
		panicSerial("forked Linux task could not be bound")
	}
	trace(diag.ProcessSpawned, pid, task, 0, 0)
	return linuxcompat.Result{Value: uint64(record.PID)}
}

// forkRegisters is the caller's register state at this call, as the child
// resumes it: after the SYSCALL instruction, with a zero result.
func forkRegisters(task sched.TaskID, frame *linuxabi.Frame) (amd64.UserRegisters, bool) {
	registers, found := amd64.LinuxCallRegisters(task)
	registers.RAX = 0
	registers.RIP, registers.RFlags, registers.RSP = frame.UserIP, frame.UserFlags, frame.UserStack
	return registers, found
}

// abandonFork releases a child whose task never existed: its compatibility
// state and identity, the references its handles and executable hold, its
// registry entry, and its address space. Handle release closes no channel,
// so the parent's VFS endpoint stays open.
func abandonFork(pid process.ID, root uint64, child *process.Process, registered bool) {
	if !linuxcompat.RemoveProcess(root, releaseLinuxDescriptionReference) {
		linuxcompat.RollbackProcessRecord(pid)
	}
	child.Handles().CloseAll(abandonHandle)
	if id := child.Executable(); id != 0 {
		dropExecutable(id)
	}
	if registered {
		processRegistry.Remove(pid)
	}
	child.Reset()
	if !vm.DestroyUserAddressSpace(&pageAllocator, root) {
		addressSpaceReclaimFailures++
	}
}

// exitLinuxProcess records a Linux exit. While other family members still
// use the family VFS context, the exited process gives up its handle to the
// shared endpoint without closing it.
func exitLinuxProcess(exited *process.Process, code uint64) {
	root := exited.AddressSpace().Root
	state, found := linuxcompat.FindProcess(root)
	if !found {
		return
	}
	vfsHandle, familyID := state.VFSHandle, state.FamilyID
	linuxcompat.ExitProcess(root, code, 0, releaseLinuxDescriptionReference)
	if _, shared := linuxcompat.VFSContext(familyID); shared {
		exited.Handles().Close(vfsHandle)
	}
}
