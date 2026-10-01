//go:build uefi

package main

// Linux identity, time, limit, signal-state, and random calls. None of them
// reaches a service: they answer from the process's compatibility state, the
// kernel clocks, and the CPU's random generator.

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
	// linuxSignalAction and linuxSignalPrevious hold the new and the old
	// disposition of one rt_sigaction call.
	linuxSignalAction   [linuxcompat.SignalActionSize]byte
	linuxSignalPrevious [linuxcompat.SignalActionSize]byte
	linuxSignalSet      [linuxcompat.SignalSetSize]byte
	linuxRandom         [linuxcompat.MaxRandomBytes]byte
)

const (
	// linuxSleepCorrelation identifies the one wait a sleeping task has. A
	// sleep ends by its deadline alone, so nothing else could answer it.
	linuxSleepCorrelation = 1
	// linuxSleepTicks bounds a sleep far below where a deadline would wrap.
	linuxSleepTicks = uint64(1) << 40
	// linuxSleepRounds bounds how often one sleep waits again because its
	// tick deadline passed before the time had.
	linuxSleepRounds = uint8(200)
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
		return linuxcompat.Result{Value: uint64(linuxcompat.ParentPIDOf(state.PID))}
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
	case linuxabi.SysRtSigaction:
		return handler.signalAction(state, arguments)
	case linuxabi.SysRtSigprocmask:
		return handler.signalMask(state, arguments)
	case linuxabi.SysGetrandom:
		return handler.getrandom(request)
	}
	return linuxErrno(linuxabi.ENOSYS)
}

// signalAction records and reports a signal disposition. A handler stored
// here runs when its signal is delivered at a system call return.
func (handler *linuxCallHandler) signalAction(state *linuxcompat.ProcessState, arguments *[6]uint64) linuxcompat.Result {
	signal, action, previous := uint64(uint32(arguments[0])), arguments[1], arguments[2]
	if arguments[3] != linuxcompat.SignalSetSize {
		return linuxErrno(linuxabi.EINVAL)
	}
	var replacement *[linuxcompat.SignalActionSize]byte
	if action != 0 {
		if !vm.CopyIn(handler.root, action, linuxSignalAction[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		replacement = &linuxSignalAction
	}
	if errno := state.Signals.Action(signal, replacement, &linuxSignalPrevious); errno != 0 {
		return linuxErrno(errno)
	}
	if previous != 0 && !vm.CopyOut(handler.root, previous, linuxSignalPrevious[:]) {
		return linuxErrno(linuxabi.EFAULT)
	}
	return linuxcompat.Result{}
}

// signalMask records and reports the blocked mask. A signal it unblocks that
// is pending is delivered as this call returns.
func (handler *linuxCallHandler) signalMask(state *linuxcompat.ProcessState, arguments *[6]uint64) linuxcompat.Result {
	how, set, previous := uint64(uint32(arguments[0])), arguments[1], arguments[2]
	if arguments[3] != linuxcompat.SignalSetSize {
		return linuxErrno(linuxabi.EINVAL)
	}
	requested := uint64(0)
	if set != 0 {
		if !vm.CopyIn(handler.root, set, linuxSignalSet[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		requested = linuxcompat.DecodeSignalSet(&linuxSignalSet)
	}
	before, errno := state.Signals.Mask(how, requested, set != 0)
	if errno != 0 {
		return linuxErrno(errno)
	}
	if previous != 0 {
		linuxcompat.EncodeSignalSet(&linuxSignalSet, before)
		if !vm.CopyOut(handler.root, previous, linuxSignalSet[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
	}
	return linuxcompat.Result{}
}

// getrandom returns bytes from the CPU's RDRAND generator. A machine without
// one gets ENOSYS, which the C library reports as a missing call: the kernel
// has no other source it could call random without lying. The refusal is
// counted with the unsupported calls so a diagnostic run shows it.
func (handler *linuxCallHandler) getrandom(request linuxcompat.Request) linuxcompat.Result {
	address, length := request.Arguments[0], request.Arguments[1]
	if errno := linuxcompat.RandomFlags(uint64(uint32(request.Arguments[2]))); errno != 0 {
		return linuxErrno(errno)
	}
	if !amd64.HasHardwareRandom() {
		return linuxcompat.Refuse(request.Number)
	}
	if length > linuxcompat.MaxRandomBytes {
		length = linuxcompat.MaxRandomBytes
	}
	if length == 0 {
		return linuxcompat.Result{}
	}
	if length = handler.accessible(address, length, true); length == 0 {
		return linuxErrno(linuxabi.EFAULT)
	}
	filled := amd64.FillRandom(linuxRandom[:length])
	copied := filled && vm.CopyOut(handler.root, address, linuxRandom[:length])
	// The kernel keeps no copy of bytes a program may use as a key.
	clear(linuxRandom[:length])
	switch {
	case !filled:
		return linuxErrno(linuxabi.EIO)
	case !copied:
		return linuxErrno(linuxabi.EFAULT)
	}
	return linuxcompat.Result{Value: length}
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

// nanosleep blocks the task until a timer deadline. A signal with a handler
// ends the sleep early with EINTR and writes the time left to the
// remaining-time argument; SA_RESTART does not restart it. Killing the
// process cancels the wait with the task's other waits.
//
// The deadline counts timer ticks, and ticks are no clock: a host that
// delivers them late delivers them in a burst, and three can arrive in a
// millisecond. The call therefore also records when the sleep must end by
// the monotonic clock, and a wake that comes before then waits again for
// what is left.
func (handler *linuxCallHandler) nanosleep(request linuxcompat.Request) linuxcompat.Result {
	pending, active, errno := handler.vfsStage(request)
	if errno != 0 {
		waitAnyTable.Cancel(handler.task)
		return linuxErrno(errno)
	}
	stage, ticks := uint8(1), uint64(0)
	if active {
		waitAnyTable.Take(handler.task)
		now := nowNanos()
		if pending.Wake == linuxcompat.WakeInterrupted {
			// A handler is about to run. nanosleep fails with EINTR whatever
			// SA_RESTART says and reports the time left in rem.
			linuxcompat.SyscallContinuations.Finish(handler.task)
			remaining := uint64(0)
			if pending.Note > now {
				remaining = pending.Note - now
			}
			if request.Arguments[1] != 0 {
				putUint64(linuxTimespec[0:8], remaining/uint64(timekeeping.NanosPerSecond))
				putUint64(linuxTimespec[8:16], remaining%uint64(timekeeping.NanosPerSecond))
				if !vm.CopyOut(handler.root, request.Arguments[1], linuxTimespec[:]) {
					return linuxErrno(linuxabi.EFAULT)
				}
			}
			return linuxErrno(linuxabi.EINTR)
		}
		if pending.Note <= now || pending.Stage >= linuxSleepRounds ||
			!linuxcompat.SyscallContinuations.Advance(handler.task, pending.Stage, pending.Stage+1, 0) {
			linuxcompat.SyscallContinuations.Finish(handler.task)
			return linuxcompat.Result{}
		}
		stage, ticks = pending.Stage+1, linuxcompat.RemainingTicks(pending.Note-now, clock.TicksPerSecond, linuxSleepTicks)
	} else {
		if !vm.CopyIn(handler.root, request.Arguments[0], linuxTimespec[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		if ticks, errno = linuxcompat.SleepTicks(&linuxTimespec, clock.TicksPerSecond, linuxSleepTicks); errno != 0 || ticks == 0 {
			return linuxErrno(errno)
		}
		// The note is the monotonic time the sleep ends at, or zero before
		// the clock is calibrated, when the tick deadline alone decides.
		end := uint64(0)
		if monotonicClock.Calibrated() {
			if end = nowNanos() + linuxcompat.SleepNanos(&linuxTimespec); end < linuxcompat.SleepNanos(&linuxTimespec) {
				end = ^uint64(0)
			}
		}
		if errno := handler.vfsBegin(request, nil, nil, end); errno != 0 {
			return linuxErrno(errno)
		}
	}
	if !linuxcompat.SyscallContinuations.Issue(handler.task, stage, linuxSleepCorrelation) ||
		!waitAnyTable.Begin(handler.task, nil, amd64.TimerTicks()+ticks) ||
		!linuxcompat.SyscallContinuations.Block(handler.task, stage) {
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
