//go:build uefi

package main

// Linux identity, time, and limit calls. None of them reaches a service:
// they answer from the process's compatibility state and the kernel clocks.

import (
	"goos/internal/timekeeping"
	"goos/internal/version"
	"goos/kernel/arch/amd64"
	"goos/kernel/clock"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	linuxelf "goos/kernel/compat/linux/elf"
	"goos/kernel/mm/vm"
)

var (
	linuxTimespec [linuxcompat.TimespecSize]byte
	linuxUname    [linuxcompat.UnameSize]byte
	linuxRlimit   [linuxcompat.RlimitSize]byte
)

const (
	// linuxSleepCorrelation identifies the one wait a sleeping task has. A
	// sleep ends by its deadline alone, so nothing else could answer it.
	linuxSleepCorrelation = 1
	// linuxSleepTicks bounds a sleep far below where a deadline would wrap.
	linuxSleepTicks = uint64(1) << 40
)

func (handler *linuxCallHandler) System(request linuxcompat.Request) linuxcompat.Result {
	state, found := linuxcompat.FindProcess(handler.root)
	if !found {
		return linuxErrno(linuxabi.ESRCH)
	}
	arguments := &request.Arguments
	switch request.Number {
	case linuxabi.SysGetpid, linuxabi.SysGettid:
		// One task per process: the thread ID is the process ID.
		return linuxcompat.Result{Value: uint64(state.PID)}
	case linuxabi.SysGetppid:
		return linuxcompat.Result{Value: linuxcompat.ParentPID}
	case linuxabi.SysGetuid, linuxabi.SysGeteuid:
		return linuxcompat.Result{Value: uint64(linuxcompat.StatUID)}
	case linuxabi.SysGetgid, linuxabi.SysGetegid:
		return linuxcompat.Result{Value: uint64(linuxcompat.StatGID)}
	case linuxabi.SysSetuid:
		// A process may set the identity it already has. It has no other to
		// switch to and no privilege that would let it.
		if arguments[0] != uint64(linuxcompat.StatUID) {
			return linuxErrno(linuxabi.EPERM)
		}
		return linuxcompat.Result{}
	case linuxabi.SysSetgid:
		if arguments[0] != uint64(linuxcompat.StatGID) {
			return linuxErrno(linuxabi.EPERM)
		}
		return linuxcompat.Result{}
	case linuxabi.SysGetgroups:
		// No supplementary groups exist. A negative size is the one bad
		// argument left.
		if int32(arguments[0]) < 0 {
			return linuxErrno(linuxabi.EINVAL)
		}
		return linuxcompat.Result{}
	case linuxabi.SysUname:
		linuxcompat.EncodeUname(&linuxUname, version.Release)
		if !vm.CopyOut(handler.root, arguments[0], linuxUname[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		return linuxcompat.Result{}
	case linuxabi.SysClockGettime:
		return handler.clockGettime(arguments[0], arguments[1])
	case linuxabi.SysNanosleep:
		return handler.nanosleep(request)
	case linuxabi.SysPrlimit64:
		return handler.prlimit(state, arguments)
	}
	return linuxErrno(linuxabi.ENOSYS)
}

// clockGettime reads a kernel clock directly. It asks no service and cannot
// block.
func (handler *linuxCallHandler) clockGettime(id uint64, address uint64) linuxcompat.Result {
	source, errno := linuxcompat.ClockOf(id)
	if errno != 0 || !monotonicClock.Calibrated() {
		return linuxErrno(linuxabi.EINVAL)
	}
	value := timekeeping.FromNanos(nowNanos())
	if source == timekeeping.Realtime {
		var set bool
		if value, set = realtimeClock.Now(&monotonicClock, amd64.ReadTSC()); !set {
			return linuxErrno(linuxabi.EINVAL)
		}
	}
	linuxcompat.EncodeTimespec(&linuxTimespec, value)
	if !vm.CopyOut(handler.root, address, linuxTimespec[:]) {
		return linuxErrno(linuxabi.EFAULT)
	}
	return linuxcompat.Result{}
}

// nanosleep blocks the task until a timer deadline. No signal can interrupt
// a GoOS Linux process, so the sleep always runs to its end and the
// remaining-time argument is never written. Killing the process cancels the
// wait with the task's other waits.
func (handler *linuxCallHandler) nanosleep(request linuxcompat.Request) linuxcompat.Result {
	_, active, errno := handler.vfsStage(request)
	if errno != 0 {
		waitAnyTable.Cancel(handler.task)
		return linuxErrno(errno)
	}
	if active {
		waitAnyTable.Take(handler.task)
		linuxcompat.SyscallContinuations.Finish(handler.task)
		return linuxcompat.Result{}
	}
	if !vm.CopyIn(handler.root, request.Arguments[0], linuxTimespec[:]) {
		return linuxErrno(linuxabi.EFAULT)
	}
	ticks, errno := linuxcompat.SleepTicks(&linuxTimespec, clock.TicksPerSecond, linuxSleepTicks)
	if errno != 0 || ticks == 0 {
		return linuxErrno(errno)
	}
	if !linuxcompat.SyscallContinuations.Begin(handler.task, request, nil) ||
		!linuxcompat.SyscallContinuations.Issue(handler.task, 1, linuxSleepCorrelation) ||
		!waitAnyTable.Begin(handler.task, nil, amd64.TimerTicks()+ticks) ||
		!linuxcompat.SyscallContinuations.Block(handler.task, 1) {
		waitAnyTable.Cancel(handler.task)
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		return linuxErrno(linuxabi.EINVAL)
	}
	return linuxBlocked
}

// prlimit reports the fixed limits of the calling process. It changes none:
// a new limit is refused with EPERM, which is what Linux answers a process
// that may not raise its limits.
func (handler *linuxCallHandler) prlimit(state *linuxcompat.ProcessState, arguments *[6]uint64) linuxcompat.Result {
	pid, resource, replacement, previous := arguments[0], arguments[1], arguments[2], arguments[3]
	if pid != 0 && pid != uint64(state.PID) {
		return linuxErrno(linuxabi.ESRCH)
	}
	limit, errno := linuxcompat.ResourceLimit(resource, linuxelf.DefaultStackReserve)
	if errno != 0 {
		return linuxErrno(errno)
	}
	if replacement != 0 {
		return linuxErrno(linuxabi.EPERM)
	}
	if previous != 0 {
		putUint64(linuxRlimit[0:8], limit)
		putUint64(linuxRlimit[8:16], limit)
		if !vm.CopyOut(handler.root, previous, linuxRlimit[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
	}
	return linuxcompat.Result{}
}
