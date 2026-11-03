//go:build uefi

package main

import (
	"goos/kernel/arch/amd64"
	"goos/kernel/clock"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/mm/vm"
)

// Select serves select and pselect6 on the readiness engine of poll: each
// descriptor a set names is evaluated as a pollfd asking for the events that
// set reports, and the task sleeps on the same native readiness, timer, and
// signals. A descriptor in a set that is not open fails the whole call with
// EBADF, as on Linux.
//
// The exceptional set reports POLLPRI. No GoOS descriptor kind has urgent or
// priority data, so the call returns it empty.
//
// Both calls write the time left back to their timeout, select as a timeval
// and pselect6 as a timespec, as the Linux system calls do; a C library
// pselect hides that from its caller. pselect6 may install a temporary
// signal mask, which works as the one of ppoll does.
func (handler *linuxCallHandler) Select(request linuxcompat.Request) linuxcompat.Result {
	local := thisCPU()
	state, found := linuxcompat.FindProcess(handler.root)
	if !found {
		return linuxErrno(linuxabi.EBADF)
	}
	finite := request.Arguments[4] != 0
	var end uint64
	pending, active := linuxcompat.SyscallContinuations.Pending(handler.task)
	if active {
		if pending.Number != request.Number || pending.Arguments != request.Arguments {
			handler.vfsAbandon()
			return linuxErrno(linuxabi.EINVAL)
		}
		waitAnyTable.Cancel(handler.task)
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		end = pending.Note
		if pending.Wake == linuxcompat.WakeInterrupted {
			return handler.selectReturn(state, request, end, linuxErrno(linuxabi.EINTR))
		}
	} else {
		var errno linuxabi.Errno
		if end, errno = handler.selectTimeout(state, request); errno != 0 {
			return linuxErrno(errno)
		}
		if now := nowNanos(); finite && end != 0 {
			if end > ^uint64(0)-now {
				end = ^uint64(0)
			} else {
				end += now
			}
		}
	}
	count, errno := linuxcompat.SelectCount(request.Arguments[0])
	if errno != 0 {
		return handler.selectReturn(state, request, end, linuxErrno(errno))
	}
	size := linuxcompat.FDSetBytes(count)
	sets := &local.linuxSelectSets
	for index := 0; index < 3; index++ {
		wire := local.linuxSelectWire[index*linuxcompat.FDSetSize : index*linuxcompat.FDSetSize+size]
		sets[index] = linuxcompat.FDSet{}
		if address := request.Arguments[1+index]; address != 0 && size != 0 {
			if !vm.CopyIn(handler.root, address, wire) {
				return handler.selectReturn(state, request, end, linuxErrno(linuxabi.EFAULT))
			}
			sets[index] = linuxcompat.DecodeFDSet(wire, count)
		}
		sets[3+index] = linuxcompat.FDSet{}
	}
	for fd := 0; fd < count; fd++ {
		if sets[0].Has(fd) || sets[1].Has(fd) || sets[2].Has(fd) {
			if _, open := state.FDs.Lookup(fd); !open {
				return handler.selectReturn(state, request, end, linuxErrno(linuxabi.EBADF))
			}
		}
	}
	ready, interests := uint64(0), 0
	for fd := 0; fd < count; fd++ {
		read, write, except := sets[0].Has(fd), sets[1].Has(fd), sets[2].Has(fd)
		if !read && !write && !except {
			continue
		}
		var revents uint16
		revents, interests = pollReadiness(state, int32(fd), linuxcompat.SelectEvents(read, write), interests)
		if read && revents&linuxcompat.SelectReadEvents != 0 {
			sets[3].Add(fd)
			ready++
		}
		if write && revents&linuxcompat.SelectWriteEvents != 0 {
			sets[4].Add(fd)
			ready++
		}
		if except && revents&linuxcompat.SelectExceptEvents != 0 {
			sets[5].Add(fd)
			ready++
		}
	}
	// With nothing ready, a handled signal the mask lets through ends the
	// call before the timeout is looked at, as on Linux, and so does one
	// terminal input just raised for the caller's group.
	if ready == 0 && (state.Signals.Interrupting() || state.Signals.Acting()) {
		return handler.selectReturn(state, request, end, linuxErrno(linuxabi.EINTR))
	}
	now := nowNanos()
	if ready != 0 || finite && (end == 0 || now >= end) {
		for index := 0; index < 3; index++ {
			address := request.Arguments[1+index]
			if address == 0 || size == 0 {
				continue
			}
			wire := local.linuxSelectWire[index*linuxcompat.FDSetSize : index*linuxcompat.FDSetSize+size]
			linuxcompat.EncodeFDSet(wire, &sets[3+index])
			if !vm.CopyOut(handler.root, address, wire) {
				return handler.selectReturn(state, request, end, linuxErrno(linuxabi.EFAULT))
			}
		}
		return handler.selectReturn(state, request, end, linuxcompat.Result{Value: ready})
	}
	deadline := uint64(0)
	if finite {
		deadline = amd64.TimerTicks() + linuxcompat.RemainingTicks(end-now, clock.TicksPerSecond, linuxSleepTicks)
	}
	if !linuxcompat.SyscallContinuations.Begin(handler.task, request, nil) ||
		!linuxcompat.SyscallContinuations.Annotate(handler.task, nil, end) ||
		!linuxcompat.SyscallContinuations.Issue(handler.task, 1, linuxPollCorrelation) ||
		(interests != 0 || deadline != 0) && !waitAnyTable.Begin(handler.task, local.linuxPollEntries[:interests], deadline) ||
		!linuxcompat.SyscallContinuations.Block(handler.task, 1) {
		waitAnyTable.Cancel(handler.task)
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		return handler.selectReturn(state, request, end, linuxErrno(linuxabi.ENOMEM))
	}
	return linuxBlocked
}

// selectTimeout reads the timeout of select or pselect6 and installs the
// temporary mask of pselect6, in the order Linux checks them: the pselect6
// mask argument, the timeout, then the mask itself. It returns the timeout in
// nanoseconds from now; zero with a timeout given means return at once.
func (handler *linuxCallHandler) selectTimeout(state *linuxcompat.ProcessState, request linuxcompat.Request) (uint64, linuxabi.Errno) {
	local := thisCPU()
	if request.Number == linuxabi.SysSelect {
		if request.Arguments[4] == 0 {
			return 0, 0
		}
		if !vm.CopyIn(handler.root, request.Arguments[4], local.linuxTimespec[:]) {
			return 0, linuxabi.EFAULT
		}
		return linuxcompat.SelectTimevalNanos(&local.linuxTimespec)
	}
	maskAddress, maskSize := uint64(0), uint64(0)
	if request.Arguments[5] != 0 {
		if !vm.CopyIn(handler.root, request.Arguments[5], local.linuxSelectArguments[:]) {
			return 0, linuxabi.EFAULT
		}
		maskAddress, maskSize = linuxcompat.DecodeSelectArguments(&local.linuxSelectArguments)
	}
	nanos := uint64(0)
	if request.Arguments[4] != 0 {
		if !vm.CopyIn(handler.root, request.Arguments[4], local.linuxTimespec[:]) {
			return 0, linuxabi.EFAULT
		}
		if _, errno := linuxcompat.SleepTicks(&local.linuxTimespec, clock.TicksPerSecond, linuxSleepTicks); errno != 0 {
			return 0, errno
		}
		nanos = linuxcompat.SleepNanos(&local.linuxTimespec)
	}
	if maskAddress != 0 {
		if maskSize != linuxcompat.SignalSetSize {
			return 0, linuxabi.EINVAL
		}
		if !vm.CopyIn(handler.root, maskAddress, local.linuxSignalSet[:]) {
			return 0, linuxabi.EFAULT
		}
		state.Signals.SwapMask(linuxcompat.DecodeSignalSet(&local.linuxSignalSet))
	}
	return nanos, 0
}

// selectReturn ends a select or pselect6 that does not sleep. Unless the
// timeout was zero, the time left goes back to it; a timeout in read-only
// memory does not fail a call that completed, as on Linux. pselect6 gives
// back the mask it replaced unless a handler is about to interrupt it.
func (handler *linuxCallHandler) selectReturn(state *linuxcompat.ProcessState, request linuxcompat.Request, end uint64, result linuxcompat.Result) linuxcompat.Result {
	local := thisCPU()
	if request.Number == linuxabi.SysPselect6 && result.Errno != linuxabi.EINTR {
		state.Signals.EndMaskSwap()
	}
	if request.Arguments[4] == 0 || end == 0 {
		return result
	}
	remaining, now := uint64(0), nowNanos()
	if end > now {
		remaining = end - now
	}
	if request.Number == linuxabi.SysSelect {
		linuxcompat.EncodeRemainingTimeval(&local.linuxTimespec, remaining)
	} else {
		linuxcompat.EncodeRemainingTimespec(&local.linuxTimespec, remaining)
	}
	vm.CopyOut(handler.root, request.Arguments[4], local.linuxTimespec[:])
	return result
}
