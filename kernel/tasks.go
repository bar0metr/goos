//go:build uefi

package main

// Kernel-side tasks and the dispatch entry points the scheduler calls.

import (
	"goos/internal/logfmt"
	"goos/internal/validation"
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/sched"
)

var nativeCallABIRejections uint64
var linuxSyscallEntries uint64
var linuxSyscallABIRejections uint64
var linuxSyscallFrameRejections uint64

// authorizeNativeCall rejects the native gate before any operation-specific
// dispatcher observes registers belonging to another process ABI. The first
// rejection is logged and the counter remains bounded to one machine word.
//
//export goos_authorize_native_call
func authorizeNativeCall(root uint64) uint64 {
	if processRegistry.AllowsABI(root, process.ABINative) {
		return uint64(kcall.StatusOK)
	}
	nativeCallABIRejections++
	if nativeCallABIRejections == 1 {
		kernelLogUint(logfmt.Warn, "native call ABI rejected", "root", root)
	}
	return uint64(kcall.StatusPermissionDenied)
}

// dispatchLinuxSyscall validates the architectural return state and Process
// personality before the compatibility subsystem observes the request. The
// assembly caller owns frame on the current task's guarded kernel stack.
//
//export goos_dispatch_linux_syscall
func dispatchLinuxSyscall(frame *linuxabi.Frame) {
	if frame == nil {
		return
	}
	frame.Return = linuxabi.Negative(linuxabi.EINVAL)
	frame.Action = linuxabi.ActionReturn
	if !vm.ValidateUserRange(frame.UserIP, 1) || !vm.ValidateUserRange(frame.UserStack, 0) {
		linuxSyscallFrameRejections++
		if linuxSyscallFrameRejections == 1 {
			kernelLogUint(logfmt.Warn, "Linux syscall frame rejected", "ip", frame.UserIP)
		}
		amd64.SetCurrentTaskExitCode(uint64(1)<<63 | 13)
		frame.Action = linuxabi.ActionExit
		return
	}
	if !processRegistry.AllowsABI(frame.Root, process.ABILinuxX8664) {
		linuxSyscallABIRejections++
		if linuxSyscallABIRejections == 1 {
			kernelLogUint(logfmt.Warn, "Linux syscall ABI rejected", "root", frame.Root)
		}
		frame.Return = linuxabi.Negative(linuxabi.EPERM)
		return
	}
	linuxSyscallEntries++
	state := amd64.ReadSchedulerState()
	linuxHandler.root = frame.Root
	linuxHandler.frame = frame
	linuxHandler.task = sched.TaskID(amd64.TaskIDForSlot(state.Current))
	// The call works with, and returns under, its own thread's signal mask.
	if process, found := linuxcompat.FindProcess(frame.Root); found {
		linuxcompat.SelectThreadSignals(process, linuxHandler.task)
	}
	result := linuxcompat.Dispatch(frame, &linuxHandler)
	if result.Errno == linuxabi.EFAULT {
		if executablePagePending(linuxHandler.task) {
			// The copy touched a page of the program's own image that is
			// still on its way from the VFS. The fault path asked for it
			// and parked the task; the call is retried when it arrives,
			// before it has changed anything.
			linuxcompat.SyscallContinuations.Cancel(linuxHandler.task)
			ipcWaitSet.Cancel(linuxHandler.task)
			result = linuxcompat.Result{Action: linuxabi.ActionBlock}
		} else {
			linuxcompat.NoteBadPointer(linuxabi.Syscall(frame.Number))
		}
	}
	if result.Action == linuxabi.ActionExec {
		if result.Entry == 0 || result.Stack == 0 || result.Root == 0 {
			result = linuxcompat.Result{Errno: linuxabi.EIO}
		} else {
			frame.UserIP, frame.UserStack, frame.Root = result.Entry, result.Stack, result.Root
		}
	}
	if result.Errno == linuxabi.EPIPE && (frame.Number == uint64(linuxabi.SysWrite) || frame.Number == uint64(linuxabi.SysWritev)) {
		// A write to a pipe nobody reads raises SIGPIPE as well as failing.
		// Linux reports it as sent by the writer itself.
		if state, found := linuxcompat.FindProcess(frame.Root); found {
			state.Signals.Raise(linuxabi.SignalPipe, linuxcompat.SignalInfo{Code: linuxabi.SignalCodeUser, PID: state.PID, UID: linuxcompat.StatUID})
		}
	}
	frame.Return, frame.Action = result.Register(), result.Action
	if result.Restart && frame.Action == linuxabi.ActionReturn && frame.UserIP >= 2 {
		// The return enters a handler, or comes straight back, at the
		// SYSCALL instruction with the call number, as Linux restarts a call.
		frame.UserIP -= 2
		frame.Return = frame.Number
	}
	if frame.Action == linuxabi.ActionReturn {
		deliverLinuxSignals(frame, linuxHandler.task)
		// A temporary mask no handler took over ends with the call.
		if state, found := linuxcompat.FindProcess(frame.Root); found {
			state.Signals.EndMaskSwap()
		}
	}
	if frame.Action == linuxabi.ActionBlock && (frame.UserIP < 2 || !vm.ValidateUserRange(frame.UserIP-2, 2)) {
		linuxSyscallFrameRejections++
		amd64.SetCurrentTaskExitCode(uint64(1)<<63 | 13)
		frame.Action = linuxabi.ActionExit
	}
}

// resumeLinuxSyscall accepts the exact continuation correlation before waking
// its task. A stale reply therefore cannot make a reused task slot runnable.
func resumeLinuxSyscall(task sched.TaskID, correlation uint64, reason linuxcompat.WakeReason) bool {
	if !linuxcompat.SyscallContinuations.Resume(task, correlation, reason) {
		return false
	}
	if amd64.WakeTaskID(uint64(task)) {
		return true
	}
	linuxcompat.SyscallContinuations.Cancel(task)
	return false
}

// dispatchKernelCall is the architecture-neutral native-call boundary. The
// packed return keeps the assembly bridge simple: action occupies the upper
// half and the typed native status occupies the lower half.
//
//export goos_dispatch_kernel_call
func dispatchKernelCall(number uint64, arg0 uint64, arg1 uint64, root uint64) uint64 {
	// The handler is reused through a pointer: a struct passed as an interface
	// would be boxed, and the kernel leaks every heap allocation.
	dispatchHandler.root = root
	result := kcall.Dispatch(kcall.Request{Operation: kcall.Operation(number), Arg0: arg0, Arg1: arg1, Arg2: amd64.KernelCallArg2()}, &dispatchHandler)
	return uint64(result.Action)<<32 | uint64(result.Status)
}

//export goos_demo_task_a
func runDemoTaskA() {
	for !validation.Enabled {
		amd64.KernelSleep()
	}
	for demoTaskA < 3 {
		demoTaskA++
		amd64.Halt()
	}
	demoABlocked = 1
	amd64.BlockCurrentTask()
	amd64.Halt()
	demoAResumed = 1
	demoTaskA++
	for {
		amd64.KernelSleep()
	}
}

//export goos_demo_task_b
func runDemoTaskB() {
	for !validation.Enabled {
		amd64.KernelSleep()
	}
	for {
		demoTaskB++
		if demoABlocked != 0 && demoAResumed == 0 && demoTaskB >= 6 {
			amd64.WakeTask(1)
		}
		if demoAResumed != 0 {
			break
		}
		amd64.Halt()
	}
	for {
		amd64.KernelSleep()
	}
}

//export goos_idle_task
func runIdleTask() {
	for {
		amd64.Halt()
	}
}
