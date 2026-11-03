//go:build uefi

package main

import (
	"goos/internal/timekeeping"
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

// Poll serves poll and ppoll. It reports which of a bounded set of
// descriptors are ready. When none is and the timeout allows, the task sleeps
// on the native readiness of every stream it names, with a timer for a finite
// timeout, and scans again when any of them, the timer, or a signal with a
// handler wakes it. Regular files and devices are always ready. A descriptor
// that is not open is POLLNVAL in its entry; the call itself does not fail
// for it.
//
// ppoll takes its timeout as a timespec and may install a temporary signal
// mask for the call. The mask comes back when the call returns, except on
// EINTR: the handler that interrupted the call runs under the temporary mask
// and its return gives the old one back.
func (handler *linuxCallHandler) Poll(request linuxcompat.Request) linuxcompat.Result {
	if request.Number == linuxabi.SysSelect || request.Number == linuxabi.SysPselect6 {
		return handler.Select(request)
	}
	local := thisCPU()
	state, found := linuxcompat.FindProcess(handler.root)
	if !found {
		return linuxErrno(linuxabi.EBADF)
	}
	masked := request.Number == linuxabi.SysPpoll
	address, count := request.Arguments[0], request.Arguments[1]
	var end uint64
	finite := true
	pending, active := linuxcompat.SyscallContinuations.Pending(handler.task)
	if active {
		if pending.Number != request.Number || pending.Arguments != request.Arguments {
			handler.vfsAbandon()
			return linuxErrno(linuxabi.EINVAL)
		}
		waitAnyTable.Cancel(handler.task)
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		end, finite = pending.Note, request.Arguments[2] != 0
		if !masked {
			_, finite = linuxcompat.PollTimeoutNanos(int32(request.Arguments[2]))
		}
		if pending.Wake == linuxcompat.WakeInterrupted {
			return handler.pollReturn(state, request, end, linuxErrno(linuxabi.EINTR))
		}
	} else {
		var errno linuxabi.Errno
		if end, finite, errno = handler.pollTimeout(state, request); errno != 0 {
			return linuxErrno(errno)
		}
		if finite && end != 0 {
			end += nowNanos()
		}
	}
	if count > linuxcompat.LinuxMaxPollFD {
		return handler.pollReturn(state, request, end, linuxErrno(linuxabi.EINVAL))
	}
	wire := local.linuxPollWire[:count*linuxcompat.PollFDSize]
	if count != 0 && !vm.CopyIn(handler.root, address, wire) {
		return handler.pollReturn(state, request, end, linuxErrno(linuxabi.EFAULT))
	}
	ready, interests := uint64(0), 0
	for index := 0; index < int(count); index++ {
		fd, events := linuxcompat.PollEntry(wire, index)
		var revents uint16
		revents, interests = pollReadiness(state, fd, events, interests)
		linuxcompat.SetPollRevents(wire, index, revents)
		if revents != 0 {
			ready++
		}
	}
	now := nowNanos()
	expired := finite && (end == 0 || now >= end)
	if ready != 0 || expired {
		if count != 0 && !vm.CopyOut(handler.root, address, wire) {
			return handler.pollReturn(state, request, end, linuxErrno(linuxabi.EFAULT))
		}
		return handler.pollReturn(state, request, end, linuxcompat.Result{Value: ready})
	}
	// A handled signal the mask lets through ends the call before it sleeps,
	// and so does one terminal input just raised for the caller's group.
	if state.Signals.Interrupting() || state.Signals.Acting() {
		return handler.pollReturn(state, request, end, linuxErrno(linuxabi.EINTR))
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
		return handler.pollReturn(state, request, end, linuxErrno(linuxabi.ENOMEM))
	}
	return linuxBlocked
}

// pollTimeout reads the timeout of poll or ppoll and installs the temporary
// mask of ppoll. It returns the timeout in nanoseconds from now, or finite
// false for no limit.
func (handler *linuxCallHandler) pollTimeout(state *linuxcompat.ProcessState, request linuxcompat.Request) (uint64, bool, linuxabi.Errno) {
	local := thisCPU()
	if request.Number != linuxabi.SysPpoll {
		end, finite := linuxcompat.PollTimeoutNanos(int32(request.Arguments[2]))
		return end, finite, 0
	}
	if request.Arguments[3] != 0 {
		if request.Arguments[4] != linuxcompat.SignalSetSize {
			return 0, false, linuxabi.EINVAL
		}
		if !vm.CopyIn(handler.root, request.Arguments[3], local.linuxSignalSet[:]) {
			return 0, false, linuxabi.EFAULT
		}
	}
	if request.Arguments[2] == 0 {
		if request.Arguments[3] != 0 {
			state.Signals.SwapMask(linuxcompat.DecodeSignalSet(&local.linuxSignalSet))
		}
		return 0, false, 0
	}
	if !vm.CopyIn(handler.root, request.Arguments[2], local.linuxTimespec[:]) {
		return 0, false, linuxabi.EFAULT
	}
	if _, errno := linuxcompat.SleepTicks(&local.linuxTimespec, clock.TicksPerSecond, linuxSleepTicks); errno != 0 {
		return 0, false, errno
	}
	if request.Arguments[3] != 0 {
		state.Signals.SwapMask(linuxcompat.DecodeSignalSet(&local.linuxSignalSet))
	}
	return linuxcompat.SleepNanos(&local.linuxTimespec), true, 0
}

// pollReturn ends a poll or ppoll that does not sleep. ppoll writes the time
// left to its timespec and, unless a handler is about to interrupt it, gives
// back the mask it replaced.
func (handler *linuxCallHandler) pollReturn(state *linuxcompat.ProcessState, request linuxcompat.Request, end uint64, result linuxcompat.Result) linuxcompat.Result {
	local := thisCPU()
	if request.Number != linuxabi.SysPpoll {
		return result
	}
	if result.Errno != linuxabi.EINTR {
		state.Signals.EndMaskSwap()
	}
	if request.Arguments[2] != 0 {
		remaining, now := uint64(0), nowNanos()
		if end > now {
			remaining = end - now
		}
		putUint64(local.linuxTimespec[0:8], remaining/uint64(timekeeping.NanosPerSecond))
		putUint64(local.linuxTimespec[8:16], remaining%uint64(timekeeping.NanosPerSecond))
		if !vm.CopyOut(handler.root, request.Arguments[2], local.linuxTimespec[:]) && result.Errno == 0 {
			return linuxErrno(linuxabi.EFAULT)
		}
	}
	return result
}

// pollReadiness evaluates one descriptor for poll or select, which asks for
// events. A descriptor that is not ready adds the native waits that would
// change it to the interests, of which there are count so far, and the new
// count comes back with its poll events.
func pollReadiness(state *linuxcompat.ProcessState, fd int32, events uint16, count int) (uint16, int) {
	if description, open := state.FDs.Lookup(int(fd)); fd >= 0 && open && description.Kind == linuxcompat.FDTerminal {
		revents := pollTerminal(state, linuxcompat.TerminalID(description.Object), events)
		if revents == 0 {
			count = addTerminalInterests(count, linuxcompat.TerminalID(description.Object), events)
		}
		return revents, count
	}
	revents, entry, watch := pollDescriptor(state, fd, events)
	if revents == 0 && watch {
		count = addPollInterest(count, entry)
	}
	return revents, count
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
// for the same stream end, which two descriptors of one pipe can name. A
// terminal adds several entries, so a full table of descriptors with one
// among them can ask for more waits than the table holds; the rest are
// dropped, and the call still wakes on the waits it kept and on its timer.
func addPollInterest(count int, entry kernelwait.Entry) int {
	local := thisCPU()
	for index := 0; index < count; index++ {
		if local.linuxPollEntries[index].Key == entry.Key {
			local.linuxPollEntries[index].Events |= entry.Events
			return count
		}
	}
	if count >= len(local.linuxPollEntries) {
		return count
	}
	local.linuxPollEntries[count] = entry
	return count + 1
}

// pollTerminal evaluates a terminal for poll after processing what waits for
// it: readable once a read would complete, hung up once its input has ended,
// writable while its output has room.
func pollTerminal(state *linuxcompat.ProcessState, id linuxcompat.TerminalID, events uint16) uint16 {
	pumpTerminal(id, state)
	snapshot, found := linuxcompat.Terminal(id)
	if !found {
		return linuxcompat.PollRevents(events, linuxcompat.PollReadiness{Error: true})
	}
	var readiness linuxcompat.PollReadiness
	readiness.Readable = linuxcompat.TerminalReadable(id, 1)
	readiness.Hangup = snapshot.Hangup
	if output, _, live := terminalStream(snapshot.Endpoints.Output); live {
		switch output.WriteStatusFor(linuxcompat.LinuxPipeBuf) {
		case stream.StatusOK:
			readiness.Writable = true
		case stream.StatusWouldBlock:
		default:
			readiness.Error = true
		}
	}
	return linuxcompat.PollRevents(events, readiness)
}

// addTerminalInterests adds the waits that change a terminal's readiness:
// its input, geometry, and processed input for a reader, its output for a
// writer.
func addTerminalInterests(count int, id linuxcompat.TerminalID, events uint16) int {
	local := thisCPU()
	snapshot, found := linuxcompat.Terminal(id)
	if !found {
		return count
	}
	var entries [3]kernelwait.Entry
	for index, total := 0, terminalInterests(snapshot, id, entries[:]); index < total; index++ {
		if count >= len(local.linuxPollEntries) {
			return count
		}
		count = addPollInterest(count, entries[index])
	}
	if events&linuxcompat.PollOut != 0 {
		if _, outputID, live := terminalStream(snapshot.Endpoints.Output); live && count < len(local.linuxPollEntries) {
			count = addPollInterest(count, kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindStream, Object: outputID, Side: 1},
				Events: kernelwait.Writable | kernelwait.PeerClosed})
		}
	}
	return count
}
