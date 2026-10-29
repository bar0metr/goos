//go:build uefi

package main

import (
	"goos/kernel/clock"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/mm/vm"
)

// linuxWaitCorrelation marks the one wait a task blocked in wait4 has.
const linuxWaitCorrelation = 1

// Wait4 reaps a zombie child of the caller, or with WUNTRACED or WCONTINUED
// reports a child that stopped or continued, or blocks until a matching child
// does one of those. The status and rusage destinations are checked before
// the report is consumed, so a bad pointer costs the caller no child.
func (handler *linuxCallHandler) Wait4(request linuxcompat.Request) linuxcompat.Result {
	local := thisCPU()
	// A wake that no child exit caused leaves the caller waiting.
	if pending, active := linuxcompat.SyscallContinuations.Pending(handler.task); active && pending.Phase == linuxcompat.ContinuationWaiting &&
		pending.Number == request.Number && pending.Arguments == request.Arguments {
		return linuxBlocked
	}
	pending, active, errno := handler.vfsStage(request)
	if errno != 0 {
		return linuxErrno(errno)
	}
	// A child that ended wins over the signal that woke the wait, as on Linux.
	interrupted := active && pending.Wake == linuxcompat.WakeInterrupted
	if active {
		linuxcompat.SyscallContinuations.Finish(handler.task)
	}
	state, found := linuxcompat.FindProcess(handler.root)
	if !found {
		return linuxErrno(linuxabi.ECHILD)
	}
	target, options, errno := linuxcompat.WaitRequest(request.Arguments[0], request.Arguments[2])
	if errno != 0 {
		return linuxErrno(errno)
	}
	statusAddress, usageAddress := request.Arguments[1], request.Arguments[3]
	if statusAddress != 0 && !vm.CanCopyOut(handler.root, statusAddress, uint64(len(local.linuxWaitStatus))) ||
		usageAddress != 0 && !vm.CanCopyOut(handler.root, usageAddress, linuxabi.RusageSize) {
		return linuxErrno(linuxabi.EFAULT)
	}
	zombie, any := linuxcompat.WaitableChild(state.PID, target, options)
	if !any {
		return linuxErrno(linuxabi.ECHILD)
	}
	if zombie.State == linuxcompat.ProcessRunning {
		reported, ok := linuxcompat.TakeChildReport(state.PID, zombie.PID)
		if !ok {
			return linuxErrno(linuxabi.ECHILD)
		}
		putUint32(local.linuxWaitStatus[:], reported.WaitStatus())
		clear(local.linuxRusage[:])
		if statusAddress != 0 && !vm.CopyOut(handler.root, statusAddress, local.linuxWaitStatus[:]) ||
			usageAddress != 0 && !vm.CopyOut(handler.root, usageAddress, local.linuxRusage[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		return linuxcompat.Result{Value: uint64(reported.PID)}
	}
	if zombie.State == linuxcompat.ProcessZombie {
		reaped, ok := linuxcompat.ReapChild(state.PID, zombie.PID)
		if !ok {
			return linuxErrno(linuxabi.ECHILD)
		}
		putUint32(local.linuxWaitStatus[:], reaped.WaitStatus())
		linuxRusageFrom(reaped.CPUTicks)
		if statusAddress != 0 && !vm.CopyOut(handler.root, statusAddress, local.linuxWaitStatus[:]) ||
			usageAddress != 0 && !vm.CopyOut(handler.root, usageAddress, local.linuxRusage[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		return linuxcompat.Result{Value: uint64(reaped.PID)}
	}
	if options.NoHang {
		return linuxcompat.Result{}
	}
	if interrupted {
		return handler.interrupted(true)
	}
	if !linuxcompat.SyscallContinuations.Begin(handler.task, request, nil) ||
		!linuxcompat.SyscallContinuations.Issue(handler.task, 1, linuxWaitCorrelation) ||
		!linuxcompat.SyscallContinuations.Block(handler.task, 1) {
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		return linuxErrno(linuxabi.EIO)
	}
	state.HoldChildWaiter(handler.task)
	return linuxBlocked
}

// linuxRusageFrom fills struct rusage with what GoOS measures: the child's
// run time in timer ticks, reported as user time. The kernel samples which
// task a tick interrupted and cannot split user from system time, so system
// time and every other field stay zero.
func linuxRusageFrom(ticks uint64) {
	local := thisCPU()
	clear(local.linuxRusage[:])
	seconds := ticks / clock.TicksPerSecond
	microseconds := ticks % clock.TicksPerSecond * 1000000 / clock.TicksPerSecond
	putUint64(local.linuxRusage[0:8], seconds)
	putUint64(local.linuxRusage[8:16], microseconds)
}

// wakeChildWaiter resumes the parent blocked in wait4, if any, after one of
// its children became a zombie.
func wakeChildWaiter(parentPID int32) {
	if task := linuxcompat.ReleaseChildWaiter(parentPID); task != 0 {
		resumeLinuxSyscall(task, linuxWaitCorrelation, linuxcompat.WakeMessage)
	}
}
