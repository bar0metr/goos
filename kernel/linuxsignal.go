//go:build uefi

package main

import (
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/mm/vm"
	"goos/kernel/sched"
)

// deliverLinuxSignals runs when a Linux system call is about to return to the
// caller with every effect of the call complete. The next pending signal the
// mask allows is discarded, ends the process, or gets a frame on the user
// stack and becomes the handler the return enters. One handler is entered per
// return; its rt_sigreturn is the next boundary.
func deliverLinuxSignals(frame *linuxabi.Frame, task sched.TaskID) {
	local := thisCPU()
	state, found := linuxcompat.FindProcess(frame.Root)
	if !found {
		return
	}
	for {
		signal, info, pending := state.Signals.Take()
		if !pending {
			return
		}
		switch state.Signals.Outcome(signal) {
		case linuxcompat.SignalDiscard:
			continue
		case linuxcompat.SignalTerminate:
			endLinuxProcessBySignal(frame, signal)
			return
		}
		action := state.Signals.ActionOf(signal)
		if action.Flags&linuxabi.SignalActionRestorer == 0 || !vm.ValidateUserRange(action.Restorer, 1) || !vm.ValidateUserRange(action.Handler, 1) {
			// x86_64 handlers return through the restorer the program
			// supplies. Without a usable one, Linux ends the process.
			endLinuxProcessBySignal(frame, linuxabi.SignalSegv)
			return
		}
		registers, valid := amd64.LinuxCallRegisters(task)
		fpu, fpuFound := amd64.LinuxUserFPU(task)
		base, fpuBase, placed := linuxcompat.SignalFrameAddress(frame.UserStack, vm.UserAddressStart)
		if !valid || !fpuFound || !placed {
			endLinuxProcessBySignal(frame, linuxabi.SignalSegv)
			return
		}
		var context linuxcompat.UserContext
		contextFromRegisters(&context, &registers)
		context.Registers[linuxcompat.RegRAX] = frame.Return
		context.Registers[linuxcompat.RegRIP] = frame.UserIP
		context.Registers[linuxcompat.RegRSP] = frame.UserStack
		context.Registers[linuxcompat.RegRFlags] = registers.R11
		previous := state.Signals.EnterHandler(signal, action)
		linuxcompat.EncodeSignalFrame(&local.linuxSignalFrame, signal, info, &context, previous, action.Restorer, fpuBase)
		if !vm.CopyOut(frame.Root, base, local.linuxSignalFrame[:]) || !vm.CopyOut(frame.Root, fpuBase, fpu[:]) {
			endLinuxProcessBySignal(frame, linuxabi.SignalSegv)
			return
		}
		// The handler starts from a clean x87 and SSE state, as on Linux.
		// The interrupted state waits in the frame.
		amd64.InitialFPUArea(fpu)
		registers.RDI = signal
		registers.RSI = linuxcompat.SignalFrameInfo(base)
		registers.RDX = linuxcompat.SignalFrameUContext(base)
		if !amd64.SetLinuxCallRegisters(task, registers) {
			endLinuxProcessBySignal(frame, linuxabi.SignalSegv)
			return
		}
		frame.UserIP, frame.UserStack, frame.Return = action.Handler, base, 0
		return
	}
}

// SigReturn resumes the context a handler's frame saved: the registers, the
// signal mask, and the x87 and SSE state. The frame is user memory: a return
// address, stack, or floating-point image outside the user half, or a frame
// the process cannot read, ends the process with SIGSEGV instead of entering
// it. Reserved MXCSR bits are cleared, as on Linux. A null fpstate restores
// the initial state.
func (handler *linuxCallHandler) SigReturn(request linuxcompat.Request) linuxcompat.Result {
	local := thisCPU()
	state, found := linuxcompat.FindProcess(handler.root)
	registers, valid := amd64.LinuxCallRegisters(handler.task)
	if !found || !valid || handler.frame == nil || !vm.CopyIn(handler.root, handler.frame.UserStack, local.linuxSignalUContext[:]) {
		return linuxSignalFault(handler)
	}
	context, mask, fpuAddress := linuxcompat.DecodeSignalContext(&local.linuxSignalUContext)
	ip, stack := context.Registers[linuxcompat.RegRIP], context.Registers[linuxcompat.RegRSP]
	fpu, fpuFound := amd64.LinuxUserFPU(handler.task)
	if !vm.ValidateUserRange(ip, 1) || !vm.ValidateUserRange(stack, 0) || !fpuFound {
		return linuxSignalFault(handler)
	}
	// The image is copied whole into kernel memory and made safe there
	// before anything changes, so the restored state never comes from a
	// pointer the program can still move.
	if fpuAddress != 0 && (!vm.ValidateUserRange(fpuAddress, linuxcompat.FPUStateSize) ||
		!vm.CopyIn(handler.root, fpuAddress, local.linuxSignalFPU[:])) {
		return linuxSignalFault(handler)
	}
	if fpuAddress != 0 {
		linuxcompat.SanitizeFPUState(&local.linuxSignalFPU, fpu)
	}
	registersFromContext(&registers, &context)
	// The return path takes RFLAGS from R11 and keeps only the bits a user
	// program may set.
	registers.R11 = context.Registers[linuxcompat.RegRFlags]
	if !amd64.SetLinuxCallRegisters(handler.task, registers) {
		return linuxSignalFault(handler)
	}
	if fpuAddress == 0 {
		amd64.InitialFPUArea(fpu)
	} else {
		*fpu = local.linuxSignalFPU
	}
	state.Signals.RestoreMask(mask)
	handler.frame.UserIP, handler.frame.UserStack = ip, stack
	return linuxcompat.Result{Value: context.Registers[linuxcompat.RegRAX]}
}

func linuxSignalFault(handler *linuxCallHandler) linuxcompat.Result {
	amd64.SetCurrentTaskExitCode(linuxcompat.SignalExitCode(linuxabi.SignalSegv))
	return linuxcompat.Result{Action: linuxabi.ActionExit}
}

// endLinuxProcessBySignal ends the calling process with the status a signal's
// default action reports.
func endLinuxProcessBySignal(frame *linuxabi.Frame, signal uint64) {
	amd64.SetCurrentTaskExitCode(linuxcompat.SignalExitCode(signal))
	frame.Action = linuxabi.ActionExit
}

// raiseChildSignal sends SIGCHLD to the Linux parent of a child that ended.
func raiseChildSignal(parentPID int32, childPID int32, termination linuxcompat.Termination) {
	parent, found := linuxcompat.FindProcessByPID(parentPID)
	if !found {
		return
	}
	info := linuxcompat.SignalInfo{Code: linuxabi.SignalCodeChildExit, PID: childPID, UID: linuxcompat.StatUID, Status: int32(termination.Status)}
	if termination.Signal != 0 {
		info.Code, info.Status = linuxabi.SignalCodeChildKill, termination.Signal
	}
	parent.Signals.Raise(linuxabi.SignalChild, info)
	interruptLinuxSleeper(parent)
}

func contextFromRegisters(context *linuxcompat.UserContext, registers *amd64.UserRegisters) {
	context.Registers[linuxcompat.RegR8], context.Registers[linuxcompat.RegR9] = registers.R8, registers.R9
	context.Registers[linuxcompat.RegR10], context.Registers[linuxcompat.RegR11] = registers.R10, registers.R11
	context.Registers[linuxcompat.RegR12], context.Registers[linuxcompat.RegR13] = registers.R12, registers.R13
	context.Registers[linuxcompat.RegR14], context.Registers[linuxcompat.RegR15] = registers.R14, registers.R15
	context.Registers[linuxcompat.RegRDI], context.Registers[linuxcompat.RegRSI] = registers.RDI, registers.RSI
	context.Registers[linuxcompat.RegRBP], context.Registers[linuxcompat.RegRBX] = registers.RBP, registers.RBX
	context.Registers[linuxcompat.RegRDX], context.Registers[linuxcompat.RegRCX] = registers.RDX, registers.RCX
}

func registersFromContext(registers *amd64.UserRegisters, context *linuxcompat.UserContext) {
	registers.R8, registers.R9 = context.Registers[linuxcompat.RegR8], context.Registers[linuxcompat.RegR9]
	registers.R10, registers.R11 = context.Registers[linuxcompat.RegR10], context.Registers[linuxcompat.RegR11]
	registers.R12, registers.R13 = context.Registers[linuxcompat.RegR12], context.Registers[linuxcompat.RegR13]
	registers.R14, registers.R15 = context.Registers[linuxcompat.RegR14], context.Registers[linuxcompat.RegR15]
	registers.RDI, registers.RSI = context.Registers[linuxcompat.RegRDI], context.Registers[linuxcompat.RegRSI]
	registers.RBP, registers.RBX = context.Registers[linuxcompat.RegRBP], context.Registers[linuxcompat.RegRBX]
	registers.RDX, registers.RCX = context.Registers[linuxcompat.RegRDX], context.Registers[linuxcompat.RegRCX]
	registers.RAX = context.Registers[linuxcompat.RegRAX]
}

// Kill serves kill, tkill, and tgkill for the caller and its descendants. A
// positive PID names one process; a single-task process has one thread whose
// ID is its PID, so tgkill needs both IDs equal. Process-group and broadcast
// targets wait for process groups and are EINVAL. Signal 0 checks the target
// and sends nothing, and a zombie target accepts a signal it never acts on.
func (handler *linuxCallHandler) Kill(request linuxcompat.Request) linuxcompat.Result {
	state, found := linuxcompat.FindProcess(handler.root)
	if !found {
		return linuxErrno(linuxabi.ESRCH)
	}
	target, signal, code := int64(int32(request.Arguments[0])), request.Arguments[1], linuxabi.SignalCodeUser
	switch request.Number {
	case linuxabi.SysTkill:
		code = linuxabi.SignalCodeThreadKill
	case linuxabi.SysTgkill:
		thread := int64(int32(request.Arguments[1]))
		signal, code = request.Arguments[2], linuxabi.SignalCodeThreadKill
		if target <= 0 || thread <= 0 {
			return linuxErrno(linuxabi.EINVAL)
		}
		if thread != target {
			if group, isThread := linuxcompat.ThreadGroupOf(int32(thread)); !isThread || int64(group) != target {
				return linuxErrno(linuxabi.ESRCH)
			}
		}
	}
	// A thread's TID names its process: the threads share one signal state.
	if group, isThread := linuxcompat.ThreadGroupOf(int32(target)); isThread {
		target = int64(group)
	}
	if target <= 0 || signal > linuxabi.SignalCount || signal != 0 && !linuxcompat.DeliverableSignal(signal) {
		return linuxErrno(linuxabi.EINVAL)
	}
	record, errno := linuxcompat.SignalTarget(state.PID, int32(target))
	if errno != 0 {
		return linuxErrno(errno)
	}
	if signal == 0 || record.State != linuxcompat.ProcessRunning {
		return linuxcompat.Result{}
	}
	receiver, found := linuxcompat.FindProcessByPID(record.PID)
	if !found {
		return linuxcompat.Result{}
	}
	info := linuxcompat.SignalInfo{Code: code, PID: state.PID, UID: linuxcompat.StatUID}
	// A signal the target would die of ends it now, even while it sleeps in
	// a system call. The caller's own fatal signal acts as this call returns.
	if receiver != state && receiver.Signals.Fatal(signal) {
		if owner, alive := processRegistry.ByRoot(receiver.Root); alive {
			for _, task := range owner.Tasks() {
				cancelTaskWaits(task)
				amd64.TerminateTask(uint64(task), linuxcompat.SignalExitCode(signal))
			}
		}
		return linuxcompat.Result{}
	}
	receiver.Signals.Raise(signal, info)
	if receiver != state {
		interruptLinuxSleeper(receiver)
	}
	return linuxcompat.Result{}
}

// interrupted ends a call whose wait a signal broke. A restartable call, a
// pipe read or write or wait4, restarts after the handler when the signal
// asked for SA_RESTART; any other call fails with EINTR.
func (handler *linuxCallHandler) interrupted(restartable bool) linuxcompat.Result {
	if state, found := linuxcompat.FindProcess(handler.root); found && restartable && state.Signals.RestartsInterrupted() {
		return linuxcompat.Result{Restart: true}
	}
	return linuxErrno(linuxabi.EINTR)
}

// interruptLinuxSleeper wakes the task of state from an interruptible wait
// once a signal with a handler is pending for it: a stream read or write,
// wait4, nanosleep, poll, ppoll, rt_sigsuspend, or pause. A VFS exchange, a queued VFS request,
// and a vfork parent keep waiting, as Linux file I/O and vfork do. A signal
// that would end the process has already ended it.
func interruptLinuxSleeper(state *linuxcompat.ProcessState) {
	if !state.Signals.Interrupting() {
		return
	}
	owner, found := processRegistry.ByRoot(state.Root)
	if !found {
		return
	}
	// A process-directed signal goes to one thread: the first one found in
	// an interruptible wait. A thread that is running takes it at its next
	// system call return instead.
	for _, task := range owner.Tasks() {
		if interruptLinuxTask(state, task) {
			return
		}
	}
}

// interruptLinuxTask ends the interruptible wait of one task for a signal,
// and reports whether the task was in one.
func interruptLinuxTask(state *linuxcompat.ProcessState, task sched.TaskID) bool {
	pending, active := linuxcompat.SyscallContinuations.Pending(task)
	if !active || pending.Phase != linuxcompat.ContinuationWaiting {
		return false
	}
	switch pending.Number {
	case linuxabi.SysRead, linuxabi.SysReadv, linuxabi.SysWrite, linuxabi.SysWritev:
		if !streamWaitSet.Cancel(task) {
			return false
		}
	case linuxabi.SysWait4:
		linuxcompat.ReleaseChildWaiter(state.PID)
	case linuxabi.SysNanosleep, linuxabi.SysPoll, linuxabi.SysPpoll, linuxabi.SysRtSigsuspend, linuxabi.SysPause:
		waitAnyTable.Cancel(task)
	case linuxabi.SysFutex:
		return interruptFutexWaiter(task)
	default:
		return false
	}
	return resumeLinuxSyscall(task, pending.Correlation, linuxcompat.WakeInterrupted)
}

// killLinuxFamily ends every other member of the family a launch root leads,
// as SIGKILL would. Ending the root from the native shell ends the job, and
// no descendant outlives it. Each member's exit then follows the ordinary
// path: zombie records, SIGCHLD, and wakes.
func killLinuxFamily(root uint64) {
	local := thisCPU()
	state, found := linuxcompat.FindProcess(root)
	if !found || state.Forked {
		return
	}
	count := linuxcompat.FamilyRoots(state.FamilyID, local.linuxFamilyRoots[:])
	for index := 0; index < count; index++ {
		member, alive := processRegistry.ByRoot(local.linuxFamilyRoots[index])
		if !alive || local.linuxFamilyRoots[index] == root {
			continue
		}
		for _, task := range member.Tasks() {
			cancelTaskWaits(task)
			amd64.TerminateTask(uint64(task), linuxcompat.SignalExitCode(linuxabi.SignalKill))
		}
	}
}

// linuxSuspendCorrelation marks the one wait a task in rt_sigsuspend has.
const linuxSuspendCorrelation = 1

// SigSuspend serves rt_sigsuspend and pause. rt_sigsuspend replaces the
// signal mask; pause keeps it. Either sleeps until a signal with a handler
// arrives and fails with EINTR. Under rt_sigsuspend the handler runs with the
// temporary mask, and its return restores the mask the call replaced. A
// signal whose default action ends the process ends it in the sleep.
func (handler *linuxCallHandler) SigSuspend(request linuxcompat.Request) linuxcompat.Result {
	local := thisCPU()
	state, found := linuxcompat.FindProcess(handler.root)
	if !found {
		return linuxErrno(linuxabi.EINVAL)
	}
	if pending, active := linuxcompat.SyscallContinuations.Pending(handler.task); active {
		if pending.Number != request.Number || pending.Arguments != request.Arguments {
			handler.vfsAbandon()
			return linuxErrno(linuxabi.EINVAL)
		}
		if pending.Phase == linuxcompat.ContinuationWaiting {
			return linuxBlocked
		}
		linuxcompat.SyscallContinuations.Finish(handler.task)
	} else if request.Number == linuxabi.SysRtSigsuspend {
		if request.Arguments[1] != linuxcompat.SignalSetSize {
			return linuxErrno(linuxabi.EINVAL)
		}
		if !vm.CopyIn(handler.root, request.Arguments[0], local.linuxSignalSet[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		state.Signals.SwapMask(linuxcompat.DecodeSignalSet(&local.linuxSignalSet))
	}
	if state.Signals.Interrupting() {
		return linuxErrno(linuxabi.EINTR)
	}
	if !linuxcompat.SyscallContinuations.Begin(handler.task, request, nil) ||
		!linuxcompat.SyscallContinuations.Issue(handler.task, 1, linuxSuspendCorrelation) ||
		!linuxcompat.SyscallContinuations.Block(handler.task, 1) {
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		state.Signals.EndMaskSwap()
		return linuxErrno(linuxabi.ENOMEM)
	}
	return linuxBlocked
}
