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
	case linuxabi.SysGetpid:
		return linuxcompat.Result{Value: uint64(state.PID)}
	case linuxabi.SysGettid:
		// The first thread's TID is the PID; a clone-created thread has
		// its own.
		return linuxcompat.Result{Value: uint64(linuxcompat.TaskTID(handler.task, state))}
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
	case linuxabi.SysGettimeofday:
		return handler.gettimeofday(arguments[0], arguments[1])
	case linuxabi.SysTime:
		return handler.time(arguments[0])
	case linuxabi.SysSysinfo:
		return handler.sysinfo(arguments[0])
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
	case linuxabi.SysUmask:
		return linuxcompat.Result{Value: state.SetUmask(arguments[0])}
	case linuxabi.SysSetRobustList:
		if arguments[1] != linuxcompat.RobustListHeadSize {
			return linuxErrno(linuxabi.EINVAL)
		}
		return linuxcompat.Refuse(request.Number)
	case linuxabi.SysFutex:
		return handler.futex(request)
	case linuxabi.SysSchedYield:
		// The call returns through a scheduling point, so a thread that
		// spins on a lock another holds lets the holder run.
		amd64.RequestReschedule()
		return linuxcompat.Result{}
	case linuxabi.SysSchedGetaffinity:
		return handler.schedGetaffinity(state, arguments)
	case linuxabi.SysGetcpu:
		return handler.getcpu(arguments)
	}
	return linuxErrno(linuxabi.ENOSYS)
}

// linuxCPUMaskSize is the kernel's CPU mask: one 64-bit word covers every
// processor GoOS runs.
const linuxCPUMaskSize = 8

var linuxCPUMask [linuxCPUMaskSize]byte

// schedGetaffinity reports every online processor for the caller's own
// process or thread: a thread runs wherever the scheduler places it. The
// result is the size of the mask written, as Linux returns it.
func (handler *linuxCallHandler) schedGetaffinity(state *linuxcompat.ProcessState, arguments *[6]uint64) linuxcompat.Result {
	pid, size := int32(arguments[0]), arguments[1]
	if pid != 0 && pid != state.PID && pid != linuxcompat.TaskTID(handler.task, state) {
		return linuxErrno(linuxabi.ESRCH)
	}
	if size < linuxCPUMaskSize || size%8 != 0 {
		return linuxErrno(linuxabi.EINVAL)
	}
	putUint64(linuxCPUMask[:], amd64.OnlineMask())
	if !vm.CopyOut(handler.root, arguments[2], linuxCPUMask[:]) {
		return linuxErrno(linuxabi.EFAULT)
	}
	return linuxcompat.Result{Value: linuxCPUMaskSize}
}

var linuxCPUWire [4]byte

// getcpu reports the processor the caller runs on, which may change before
// it uses the answer, and node zero.
func (handler *linuxCallHandler) getcpu(arguments *[6]uint64) linuxcompat.Result {
	if arguments[0] != 0 {
		putUint32(linuxCPUWire[:], uint32(amd64.CPUIndex()))
		if !vm.CopyOut(handler.root, arguments[0], linuxCPUWire[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
	}
	if arguments[1] != 0 {
		clear(linuxCPUWire[:])
		if !vm.CopyOut(handler.root, arguments[1], linuxCPUWire[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
	}
	return linuxcompat.Result{}
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

// getrandom returns bytes from the kernel's generator (ADR 0185), which boot
// seeds before the first process starts, so the call never blocks.
func (handler *linuxCallHandler) getrandom(request linuxcompat.Request) linuxcompat.Result {
	address, length := request.Arguments[0], request.Arguments[1]
	if errno := linuxcompat.RandomFlags(uint64(uint32(request.Arguments[2]))); errno != 0 {
		return linuxErrno(errno)
	}
	stirRandom()
	if length > linuxcompat.MaxRandomBytes {
		length = linuxcompat.MaxRandomBytes
	}
	if length == 0 {
		return linuxcompat.Result{}
	}
	if length = handler.accessible(address, length, true); length == 0 {
		return linuxErrno(linuxabi.EFAULT)
	}
	filled := randomBytes(linuxRandom[:length])
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

var (
	linuxTimeval  [linuxcompat.TimevalSize]byte
	linuxTimezone [linuxcompat.TimezoneSize]byte
)

// gettimeofday reads the realtime clock in microseconds. Either pointer may
// be null. The timezone is UTC with no daylight saving, as Linux reports
// when nothing set one.
func (handler *linuxCallHandler) gettimeofday(timeval uint64, timezone uint64) linuxcompat.Result {
	if timeval != 0 {
		if !monotonicClock.Calibrated() {
			return linuxErrno(linuxabi.EINVAL)
		}
		value, set := realtimeClock.Now(&monotonicClock, amd64.ReadTSC())
		if !set {
			return linuxErrno(linuxabi.EINVAL)
		}
		linuxcompat.EncodeTimeval(&linuxTimeval, value)
		if !vm.CopyOut(handler.root, timeval, linuxTimeval[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
	}
	if timezone != 0 && !vm.CopyOut(handler.root, timezone, linuxTimezone[:]) {
		return linuxErrno(linuxabi.EFAULT)
	}
	return linuxcompat.Result{}
}

var linuxTime [8]byte

// time returns the realtime clock in whole seconds and also stores it where
// tloc points, when it points anywhere.
func (handler *linuxCallHandler) time(tloc uint64) linuxcompat.Result {
	if !monotonicClock.Calibrated() {
		return linuxErrno(linuxabi.EINVAL)
	}
	value, set := realtimeClock.Now(&monotonicClock, amd64.ReadTSC())
	if !set {
		return linuxErrno(linuxabi.EINVAL)
	}
	seconds := uint64(value.Seconds)
	if tloc != 0 {
		putUint64(linuxTime[:], seconds)
		if !vm.CopyOut(handler.root, tloc, linuxTime[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
	}
	return linuxcompat.Result{Value: seconds}
}

var linuxSysinfo [linuxcompat.SysinfoSize]byte

// sysinfo reports uptime, the managed RAM, and the free pages. Programs
// size their buffers from totalram; 7-Zip picks its dictionary limit there.
func (handler *linuxCallHandler) sysinfo(address uint64) linuxcompat.Result {
	statistics := pageAllocator.Statistics()
	linuxcompat.EncodeSysinfo(&linuxSysinfo, linuxcompat.Sysinfo{
		UptimeSeconds: nowNanos() / 1_000_000_000,
		TotalRAM:      statistics.ManagedPages * vm.PageSize,
		FreeRAM:       statistics.FreePages * vm.PageSize,
		Processes:     uint16(processRegistry.Count()),
	})
	if !vm.CopyOut(handler.root, address, linuxSysinfo[:]) {
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
