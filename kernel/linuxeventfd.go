//go:build uefi

package main

// eventfd and eventfd2: a 64-bit counter behind a descriptor. curl's
// resolver thread wakes its poll loop through one. The counter lives in the
// compatibility layer; a blocked read or write sleeps on its wait key
// through the syscall continuation, as poll does.

import (
	"encoding/binary"

	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/mm/vm"
	kernelwait "goos/kernel/wait"
)

const (
	eventfdSemaphore = uint64(0x1)
	eventfdCloseExec = uint64(linuxabi.OpenCloseExec)
	eventfdNonBlock  = uint64(linuxabi.OpenNonBlock)
	// linuxEventCorrelation marks the one wait a task in an eventfd read or
	// write has.
	linuxEventCorrelation = 1
	// linuxEventRounds bounds the retries of a call whose counter changed
	// between its check and its sleep.
	linuxEventRounds = 4
)

// eventfdCreate serves eventfd and eventfd2. eventfd takes no flags.
func (handler *linuxCallHandler) eventfdCreate(state *linuxcompat.ProcessState, request linuxcompat.Request) linuxcompat.Result {
	flags := uint64(0)
	if request.Number == linuxabi.SysEventfd2 {
		flags = request.Arguments[1]
	}
	if flags&^(eventfdSemaphore|eventfdCloseExec|eventfdNonBlock) != 0 {
		return linuxErrno(linuxabi.EINVAL)
	}
	object, errno := linuxcompat.CreateEventCounter(state.FamilyID, uint32(request.Arguments[0]), flags&eventfdSemaphore != 0)
	if errno != 0 {
		return linuxErrno(errno)
	}
	status := uint32(linuxabi.OpenReadWrite)
	if flags&eventfdNonBlock != 0 {
		status |= linuxabi.OpenNonBlock
	}
	fd, errno := linuxcompat.AllocateFD(handler.root, linuxcompat.FileDescription{Kind: linuxcompat.FDEvent, Flags: status, Object: object}, flags&eventfdCloseExec != 0)
	if errno != 0 {
		linuxcompat.ReleaseEventCounterObject(object)
		return linuxErrno(errno)
	}
	return linuxcompat.Result{Value: uint64(fd)}
}

func eventKey(object uint64) kernelwait.Key {
	return kernelwait.Key{Kind: kernelwait.KindLinuxEvent, Object: object}
}

// eventTransfer serves read and write on an eventfd: eight bytes, the value
// taken or added. A read of an empty counter or a write that does not fit
// waits, or is EAGAIN on a nonblocking description.
//
//go:noinline
func (handler *linuxCallHandler) eventTransfer(request linuxcompat.Request, description *linuxcompat.FileDescription, write bool, vector bool) linuxcompat.Result {
	local := thisCPU()
	state := handler.state
	if state == nil {
		return linuxErrno(linuxabi.EBADF)
	}
	if vector {
		// No supported program reads or writes a counter through a vector.
		return linuxErrno(linuxabi.EINVAL)
	}
	address, count, object := request.Arguments[1], request.Arguments[2], description.Object
	if count < 8 {
		return linuxErrno(linuxabi.EINVAL)
	}
	if pending, active := linuxcompat.SyscallContinuations.Pending(handler.task); active {
		waitAnyTable.Cancel(handler.task)
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		if pending.Number != request.Number || pending.Arguments != request.Arguments {
			return linuxErrno(linuxabi.EINVAL)
		}
		if pending.Wake == linuxcompat.WakeInterrupted {
			return handler.interrupted(true)
		}
	}
	var word [8]byte
	for round := 0; round < linuxEventRounds; round++ {
		if write {
			if !vm.CopyIn(handler.root, address, word[:]) {
				return linuxErrno(linuxabi.EFAULT)
			}
			ready, errno := linuxcompat.WriteEventCounter(state.FamilyID, object, binary.LittleEndian.Uint64(word[:]))
			if errno != 0 {
				return linuxErrno(errno)
			}
			if ready {
				publishWaitAny(eventKey(object), kernelwait.Readable)
				return linuxcompat.Result{Value: 8}
			}
		} else {
			value, ready, valid := linuxcompat.ReadEventCounter(state.FamilyID, object)
			if !valid {
				return linuxErrno(linuxabi.EBADF)
			}
			if ready {
				binary.LittleEndian.PutUint64(word[:], value)
				if !vm.CopyOut(handler.root, address, word[:]) {
					// The value goes back, so a copy the dispatcher runs
					// again once the page is in loses nothing.
					linuxcompat.WriteEventCounter(state.FamilyID, object, value)
					return linuxErrno(linuxabi.EFAULT)
				}
				publishWaitAny(eventKey(object), kernelwait.Writable)
				return linuxcompat.Result{Value: 8}
			}
		}
		if description.Flags&linuxabi.OpenNonBlock != 0 {
			return linuxErrno(linuxabi.EAGAIN)
		}
		if state.Signals.Interrupting() || state.Signals.Acting() {
			return handler.interrupted(true)
		}
		want := kernelwait.Readable
		if write {
			want = kernelwait.Writable
		}
		entries := local.linuxPollEntries[:1]
		entries[0] = kernelwait.Entry{Key: eventKey(object), Events: want}
		if !linuxcompat.SyscallContinuations.Begin(handler.task, request, nil) ||
			!linuxcompat.SyscallContinuations.Issue(handler.task, 1, linuxEventCorrelation) ||
			!waitAnyTable.Begin(handler.task, entries, 0) {
			waitAnyTable.Cancel(handler.task)
			linuxcompat.SyscallContinuations.Cancel(handler.task)
			return linuxErrno(linuxabi.ENOMEM)
		}
		// Registered first, checked again after: a change in between is
		// seen here or wakes the wait.
		readable, writable, valid := linuxcompat.EventCounterReady(state.FamilyID, object)
		if !valid || !write && readable || write && writable {
			waitAnyTable.Cancel(handler.task)
			linuxcompat.SyscallContinuations.Cancel(handler.task)
			if !valid {
				return linuxErrno(linuxabi.EBADF)
			}
			continue
		}
		if !linuxcompat.SyscallContinuations.Block(handler.task, 1) {
			waitAnyTable.Cancel(handler.task)
			linuxcompat.SyscallContinuations.Cancel(handler.task)
			return linuxErrno(linuxabi.ENOMEM)
		}
		return linuxBlocked
	}
	return linuxErrno(linuxabi.EAGAIN)
}

// pollEvent evaluates an eventfd for poll and select: readable while the
// counter is nonzero, writable while one more unit fits.
func pollEvent(state *linuxcompat.ProcessState, description linuxcompat.FileDescription, events uint16) (uint16, kernelwait.Entry, bool) {
	readable, writable, valid := linuxcompat.EventCounterReady(state.FamilyID, description.Object)
	readiness := linuxcompat.PollReadiness{Readable: readable, Writable: writable, Error: !valid}
	if revents := linuxcompat.PollRevents(events, readiness); revents != 0 || !valid {
		return revents, kernelwait.Entry{}, false
	}
	return 0, kernelwait.Entry{Key: eventKey(description.Object), Events: kernelwait.Readable | kernelwait.Writable}, true
}
