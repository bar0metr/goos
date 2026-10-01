//go:build uefi

package main

import (
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/mm/vm"
	"goos/kernel/sched"
)

var (
	linuxSignalFrame    [linuxcompat.SignalFrameSize]byte
	linuxSignalUContext [linuxcompat.UContextSize]byte
)

// deliverLinuxSignals runs when a Linux system call is about to return to the
// caller with every effect of the call complete. The next pending signal the
// mask allows is discarded, ends the process, or gets a frame on the user
// stack and becomes the handler the return enters. One handler is entered per
// return; its rt_sigreturn is the next boundary.
func deliverLinuxSignals(frame *linuxabi.Frame, task sched.TaskID) {
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
		base, placed := linuxcompat.SignalFrameAddress(frame.UserStack, vm.UserAddressStart)
		if !valid || !placed {
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
		linuxcompat.EncodeSignalFrame(&linuxSignalFrame, signal, info, &context, previous, action.Restorer)
		if !vm.CopyOut(frame.Root, base, linuxSignalFrame[:]) {
			endLinuxProcessBySignal(frame, linuxabi.SignalSegv)
			return
		}
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

// SigReturn resumes the context a handler's frame saved. The frame is user
// memory: a return address or stack outside the user half, or a frame the
// process cannot read, ends the process with SIGSEGV instead of entering it.
func (handler *linuxCallHandler) SigReturn(request linuxcompat.Request) linuxcompat.Result {
	state, found := linuxcompat.FindProcess(handler.root)
	registers, valid := amd64.LinuxCallRegisters(handler.task)
	if !found || !valid || handler.frame == nil || !vm.CopyIn(handler.root, handler.frame.UserStack, linuxSignalUContext[:]) {
		return linuxSignalFault(handler)
	}
	context, mask := linuxcompat.DecodeSignalContext(&linuxSignalUContext)
	ip, stack := context.Registers[linuxcompat.RegRIP], context.Registers[linuxcompat.RegRSP]
	if !vm.ValidateUserRange(ip, 1) || !vm.ValidateUserRange(stack, 0) {
		return linuxSignalFault(handler)
	}
	registersFromContext(&registers, &context)
	// The return path takes RFLAGS from R11 and keeps only the bits a user
	// program may set.
	registers.R11 = context.Registers[linuxcompat.RegRFlags]
	if !amd64.SetLinuxCallRegisters(handler.task, registers) {
		return linuxSignalFault(handler)
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
