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

// linuxVforkCorrelation marks the one wait a suspended vfork parent has.
const linuxVforkCorrelation = 1

// Fork serves fork, vfork, and the process forms of clone. A vfork creates the
// same private child as fork and then suspends the caller until the child
// replaces its image or exits. The parent resumes with the child's PID.
func (handler *linuxCallHandler) Fork(request linuxcompat.Request) linuxcompat.Result {
	// A wake that did not come from the child leaves the parent suspended.
	if pending, active := linuxcompat.SyscallContinuations.Pending(handler.task); active && pending.Phase == linuxcompat.ContinuationWaiting &&
		pending.Number == request.Number && pending.Arguments == request.Arguments {
		return linuxBlocked
	}
	pending, active, errno := handler.vfsStage(request)
	if errno != 0 {
		return linuxErrno(errno)
	}
	if active {
		linuxcompat.SyscallContinuations.Finish(handler.task)
		return linuxcompat.Result{Value: pending.Note}
	}
	form, errno := linuxcompat.ProcessCloneForm(request)
	if errno == 0 && form.Stack != 0 && !vm.ValidateUserRange(form.Stack, 0) {
		errno = linuxabi.EINVAL
	}
	if errno != 0 {
		return linuxErrno(errno)
	}
	pid, child, errno := handler.forkChild(form.Stack)
	if errno != 0 {
		return linuxErrno(errno)
	}
	if !form.Suspend {
		return linuxcompat.Result{Value: uint64(pid)}
	}
	// The child cannot run before this call returns, so it cannot exec or
	// exit before the parent is recorded as suspended.
	if child == nil || !linuxcompat.SyscallContinuations.Begin(handler.task, request, nil) ||
		!linuxcompat.SyscallContinuations.Annotate(handler.task, nil, uint64(pid)) ||
		!linuxcompat.SyscallContinuations.Issue(handler.task, 1, linuxVforkCorrelation) ||
		!linuxcompat.SyscallContinuations.Block(handler.task, 1) {
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		return linuxcompat.Result{Value: uint64(pid)}
	}
	child.HoldVforkParent(handler.task)
	return linuxBlocked
}

// releaseVforkParent resumes the parent a vfork suspended for state, once.
func releaseVforkParent(state *linuxcompat.ProcessState) {
	if parent := state.ReleaseVforkParent(); parent != 0 {
		resumeLinuxSyscall(parent, linuxVforkCorrelation, linuxcompat.WakeMessage)
	}
}

// forkChild creates a child Process from the caller: a private copy of every
// page it has touched, its region map, descriptors that share the parent's
// open-file descriptions, and the registers of this call with RAX zero. A
// nonzero stack replaces the child's stack pointer. The child gets no
// authority the parent does not hold. Every step that can fail runs before the
// child's task exists, and a failure leaves no trace of the child.
func (handler *linuxCallHandler) forkChild(stack uint64) (int32, *linuxcompat.ProcessState, linuxabi.Errno) {
	parent, found := processRegistry.ByTask(handler.task)
	compat, compatFound := linuxcompat.FindProcess(handler.root)
	if !found || !compatFound || handler.frame == nil || parent.TaskCount() != 1 || kernelPageTable == 0 {
		return 0, nil, linuxabi.EIO
	}
	vfsAuthority, vfsFound := parent.Handles().Lookup(compat.VFSHandle, handle.TypeChannel, capability.Read|capability.Write|capability.Wait)
	fsBase, fsFound := amd64.TaskFSBase(handler.task)
	registers, registersFound := forkRegisters(handler.task, handler.frame)
	if !vfsFound || !fsFound || !registersFound {
		return 0, nil, linuxabi.EIO
	}
	if stack != 0 {
		registers.RSP = stack
	}
	pid := nextProcessID
	record, status := linuxcompat.RegisterChild(pid, compat.PID, processRegistry.Count())
	if status != linuxcompat.ChildRegistrationOK {
		return 0, nil, linuxabi.EAGAIN
	}
	child := freeProcessSlot()
	if child == nil {
		linuxcompat.RollbackProcessRecord(pid)
		return 0, nil, linuxabi.EAGAIN
	}
	pagesBefore := pageAllocator.Statistics().FreePages
	root, created := vm.CreateUserAddressSpace(&pageAllocator, kernelPageTable)
	if !created {
		linuxcompat.RollbackProcessRecord(pid)
		return 0, nil, linuxabi.ENOMEM
	}
	if !process.InitChild(child, pid, parent, process.AddressSpace{Root: root}) {
		abandonFork(pid, root, child, false)
		return 0, nil, linuxabi.ENOMEM
	}
	if !vm.CopyUserRange(&pageAllocator, handler.root, root, vm.UserAddressStart, vm.UserAddressLimit-vm.UserAddressStart) {
		abandonFork(pid, root, child, false)
		return 0, nil, linuxabi.ENOMEM
	}
	// The child names the same VFS endpoint through its own handle. The
	// family context, not any one process, decides when the endpoint closes.
	vfsHandle, inserted := child.Handles().Insert(handle.TypeChannel, vfsAuthority)
	if !inserted || !linuxcompat.ForkProcess(handler.root, root, record, vfsHandle) {
		abandonFork(pid, root, child, false)
		return 0, nil, linuxabi.ENOMEM
	}
	if !processRegistry.Add(child) {
		abandonFork(pid, root, child, false)
		return 0, nil, linuxabi.EAGAIN
	}
	if used := pagesBefore - pageAllocator.Statistics().FreePages; used > uint64(^uint32(0)) || used != 0 && !child.Charge(process.ResourcePages, uint32(used)) {
		abandonFork(pid, root, child, true)
		return 0, nil, linuxabi.ENOMEM
	}
	if id := parent.Executable(); id != 0 {
		object, found := executables.Find(id)
		if !found {
			abandonFork(pid, root, child, true)
			return 0, nil, linuxabi.ENOMEM
		}
		object.Retain()
		child.SetExecutable(id)
	}
	task, added := amd64.AddUserTaskWithRegisters(&pageAllocator, root, registers)
	if !added {
		abandonFork(pid, root, child, true)
		return 0, nil, linuxabi.ENOMEM
	}
	// A runnable task cannot be withdrawn. Its process ID is consumed either
	// way so it is never handed out twice.
	nextProcessID++
	if !processRegistry.BindTask(pid, sched.TaskID(task)) || !amd64.SetTaskFSBase(sched.TaskID(task), fsBase) {
		panicSerial("forked Linux task could not be bound")
	}
	trace(diag.ProcessSpawned, pid, task, 0, 0)
	state, _ := linuxcompat.FindProcess(root)
	return record.PID, state, 0
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
	releaseVforkParent(state)
	linuxcompat.ExitProcess(root, linuxcompat.TerminationFromCode(code, exited.CPUTicks()), releaseLinuxDescriptionReference)
	if _, shared := linuxcompat.VFSContext(familyID); shared {
		exited.Handles().Close(vfsHandle)
	}
}
