//go:build uefi

package main

import (
	"goos/kernel/arch/amd64"
	"goos/kernel/clock"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/mm/vm"
	"goos/kernel/stream"
	kernelwait "goos/kernel/wait"
)

// linuxPollCorrelation marks the one wait a task in poll has.
const linuxPollCorrelation = 1

var (
	linuxPollWire    [linuxcompat.LinuxMaxPollFD * linuxcompat.PollFDSize]byte
	linuxPollEntries [linuxcompat.LinuxMaxPollFD]kernelwait.Entry
)

// Poll reports which of a bounded set of descriptors are ready. When none is
// and the timeout allows, the task sleeps on the native readiness of every
// stream it names, with a timer for a finite timeout, and scans again when
// any of them, the timer, or a signal with a handler wakes it. Regular files
// and devices are always ready. A descriptor that is not open is POLLNVAL in
// its entry; the call itself does not fail for it.
func (handler *linuxCallHandler) Poll(request linuxcompat.Request) linuxcompat.Result {
	address, count := request.Arguments[0], request.Arguments[1]
	end, finite := linuxcompat.PollTimeoutNanos(int32(request.Arguments[2]))
	pending, active := linuxcompat.SyscallContinuations.Pending(handler.task)
	if active {
		if pending.Number != request.Number || pending.Arguments != request.Arguments {
			handler.vfsAbandon()
			return linuxErrno(linuxabi.EINVAL)
		}
		waitAnyTable.Cancel(handler.task)
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		if pending.Wake == linuxcompat.WakeInterrupted {
			return linuxErrno(linuxabi.EINTR)
		}
		end = pending.Note
	} else if finite && end != 0 {
		end += nowNanos()
	}
	if count > linuxcompat.LinuxMaxPollFD {
		return linuxErrno(linuxabi.EINVAL)
	}
	wire := linuxPollWire[:count*linuxcompat.PollFDSize]
	if count != 0 && !vm.CopyIn(handler.root, address, wire) {
		return linuxErrno(linuxabi.EFAULT)
	}
	state, found := linuxcompat.FindProcess(handler.root)
	if !found {
		return linuxErrno(linuxabi.EBADF)
	}
	ready, interests := uint64(0), 0
	for index := 0; index < int(count); index++ {
		fd, events := linuxcompat.PollEntry(wire, index)
		revents, entry, watch := pollDescriptor(state, fd, events)
		linuxcompat.SetPollRevents(wire, index, revents)
		if revents != 0 {
			ready++
		} else if watch {
			interests = addPollInterest(interests, entry)
		}
	}
	now := nowNanos()
	expired := finite && (end == 0 || now >= end)
	if ready != 0 || expired {
		if count != 0 && !vm.CopyOut(handler.root, address, wire) {
			return linuxErrno(linuxabi.EFAULT)
		}
		return linuxcompat.Result{Value: ready}
	}
	deadline := uint64(0)
	if finite {
		deadline = amd64.TimerTicks() + linuxcompat.RemainingTicks(end-now, clock.TicksPerSecond, linuxSleepTicks)
	}
	if !linuxcompat.SyscallContinuations.Begin(handler.task, request, nil) ||
		!linuxcompat.SyscallContinuations.Annotate(handler.task, nil, end) ||
		!linuxcompat.SyscallContinuations.Issue(handler.task, 1, linuxPollCorrelation) ||
		(interests != 0 || deadline != 0) && !waitAnyTable.Begin(handler.task, linuxPollEntries[:interests], deadline) ||
		!linuxcompat.SyscallContinuations.Block(handler.task, 1) {
		waitAnyTable.Cancel(handler.task)
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		return linuxErrno(linuxabi.ENOMEM)
	}
	return linuxBlocked
}

// pollDescriptor evaluates one pollfd. watch reports a stream end that is not
// ready yet, with the native readiness that would make it ready.
func pollDescriptor(state *linuxcompat.ProcessState, fd int32, events uint16) (uint16, kernelwait.Entry, bool) {
	if fd < 0 {
		return 0, kernelwait.Entry{}, false
	}
	description, open := state.FDs.Lookup(int(fd))
	if !open {
		return linuxcompat.PollInvalid, kernelwait.Entry{}, false
	}
	var readiness linuxcompat.PollReadiness
	switch description.Kind {
	case linuxcompat.FDStreamReader, linuxcompat.FDStreamWriter:
		streamID, writer, valid := stream.DecodeEndpointObject(description.Object)
		endpoint, live := streamRegistry.Lookup(streamID)
		if !valid || !live {
			readiness.Error = true
			break
		}
		if writer {
			switch endpoint.WriteStatusFor(linuxcompat.LinuxPipeBuf) {
			case stream.StatusOK:
				readiness.Writable = true
			case stream.StatusWouldBlock:
				return 0, kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindStream, Object: streamID, Side: 1},
					Events: kernelwait.Writable | kernelwait.PeerClosed}, true
			default:
				readiness.Error = true
			}
			break
		}
		switch endpoint.ReadStatus() {
		case stream.StatusOK:
			readiness.Readable = true
		case stream.StatusPeerClosed:
			readiness.Hangup = true
		case stream.StatusWouldBlock:
			return 0, kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindStream, Object: streamID, Side: 0},
				Events: kernelwait.Readable | kernelwait.PeerClosed}, true
		default:
			readiness.Error = true
		}
	default:
		// Regular files, directories, and devices never block.
		readiness.Readable, readiness.Writable = true, true
	}
	return linuxcompat.PollRevents(events, readiness), kernelwait.Entry{}, false
}

// addPollInterest adds a native wait entry, merging it with an earlier entry
// for the same stream end, which two descriptors of one pipe can name.
func addPollInterest(count int, entry kernelwait.Entry) int {
	for index := 0; index < count; index++ {
		if linuxPollEntries[index].Key == entry.Key {
			linuxPollEntries[index].Events |= entry.Events
			return count
		}
	}
	linuxPollEntries[count] = entry
	return count + 1
}
