//go:build uefi

package main

// Linux terminals (ADR 0206). An interactive Linux launch gets one terminal
// built from the native streams PID 1 granted: the keyboard input the native
// shell feeds in raw mode, the session's output, and a control stream that
// carries geometry. The line discipline, termios, the foreground process
// group, and the job-control signals live here and in the compatibility
// package; the native terminal service only moves bytes.
//
// Input is processed when it arrives, not when a program reads: a reader or
// poller of the terminal processes it first, and the kernel loop processes
// whatever no Linux task took within a tick, so Ctrl-C reaches a foreground
// group that never reads the terminal.

import (
	"goos/internal/terminalwire"
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/ipc"
	"goos/kernel/mm/vm"
	"goos/kernel/stream"
	kernelwait "goos/kernel/wait"
)

// linuxTerminalCorrelation marks the one wait a task reading a terminal has.
const linuxTerminalCorrelation = 1

// linuxTerminalBatches bounds the input one processing pass takes, so a
// writer that never stops cannot keep a reader or the kernel loop in it.
const linuxTerminalBatches = 16

// linuxTerminalScratch is the per-processor room a terminal pass uses.
type linuxTerminalScratch struct {
	raw     [linuxcompat.TerminalFeedSize]byte
	echo    linuxcompat.Echo
	output  [stream.AtomicWrite]byte
	control [terminalwire.RecordSize]byte
	members [linuxcompat.LinuxFamilyMaxProcesses]int32
	wire    [linuxabi.TermiosSize]byte
	// settings keeps a snapshot off the heap: Encode takes its address.
	settings linuxcompat.Termios
}

func terminalKey(id linuxcompat.TerminalID) kernelwait.Key {
	return kernelwait.Key{Kind: kernelwait.KindLinuxTerminal, Object: uint64(id)}
}

// terminalStream returns a live stream endpoint a terminal holds.
func terminalStream(object uint64) (*stream.Stream, uint64, bool) {
	streamID, _, valid := stream.DecodeEndpointObject(object)
	endpoint, live := streamRegistry.Lookup(streamID)
	return endpoint, streamID, valid && live
}

// pumpTerminal processes what is waiting for a terminal: geometry records
// on its control stream and typed bytes on its input. It echoes, sends the
// signals the input asks for to the foreground group, and wakes the tasks
// that wait for the terminal. The caller holds the stream lock and the lock
// of the terminal's family. caller is the process whose call processes the
// input, or nil in the kernel loop: a signal the input sends to caller
// itself becomes pending, and its call returns to take it.
func pumpTerminal(id linuxcompat.TerminalID, caller *linuxcompat.ProcessState) {
	scratch := &thisCPU().linuxTerminal
	snapshot, found := linuxcompat.Terminal(id)
	if !found {
		return
	}
	changed := pumpTerminalControl(id, snapshot, caller)
	input, inputID, live := terminalStream(snapshot.Endpoints.Input)
	for batch := 0; live && batch < linuxTerminalBatches; batch++ {
		room := linuxcompat.TerminalInputCapacity - linuxcompat.TerminalPending(id)
		if room <= 0 {
			break
		}
		count, status := input.Read(scratch.raw[:min(room, len(scratch.raw))])
		if status == stream.StatusPeerClosed {
			if linuxcompat.HangUpTerminal(id) {
				changed = true
				if snapshot.Foreground != 0 {
					signalTerminalGroup(snapshot, linuxabi.SignalHangup, caller)
				}
			}
			break
		}
		if status != stream.StatusOK {
			break
		}
		wakeStreamWaiters(inputID, 1, ipc.WaitMessage)
		scratch.echo.Reset()
		signals, after, ok := linuxcompat.FeedTerminal(id, scratch.raw[:count], &scratch.echo)
		if !ok {
			return
		}
		writeTerminalEcho(after)
		for _, signal := range [...]uint64{linuxabi.SignalInterrupt, linuxabi.SignalQuit, linuxabi.SignalTermStop} {
			if signals.Has(signal) && after.Foreground != 0 {
				signalTerminalGroup(after, signal, caller)
			}
		}
		changed = true
	}
	if changed {
		publishWaitAny(terminalKey(id), kernelwait.Readable)
	}
}

// pumpTerminalControl applies the geometry records PID 1 wrote. Only the
// newest matters; a change sends SIGWINCH to the foreground group.
func pumpTerminalControl(id linuxcompat.TerminalID, snapshot linuxcompat.TerminalSnapshot, caller *linuxcompat.ProcessState) bool {
	scratch := &thisCPU().linuxTerminal
	control, controlID, live := terminalStream(snapshot.Endpoints.Control)
	if !live {
		return false
	}
	newest, seen := terminalwire.Geometry{}, false
	for record := 0; record < linuxTerminalBatches; record++ {
		count, status := control.Read(scratch.control[:])
		if status != stream.StatusOK {
			break
		}
		if geometry, valid := terminalwire.Decode(scratch.control[:count]); valid && (!seen || geometry.Sequence >= newest.Sequence) {
			newest, seen = geometry, true
		}
	}
	if !seen {
		return false
	}
	wakeStreamWaiters(controlID, 1, ipc.WaitMessage)
	changed, after := linuxcompat.ResizeTerminal(id, newest.Rows, newest.Columns)
	if changed && after.Foreground != 0 {
		signalTerminalGroup(after, linuxabi.SignalWinch, caller)
	}
	return changed
}

// writeTerminalEcho writes what the discipline echoed. Echo never waits: a
// terminal whose output is full loses it, as a Linux terminal drops echo
// when its output queue is full.
func writeTerminalEcho(snapshot linuxcompat.TerminalSnapshot) {
	scratch := &thisCPU().linuxTerminal
	echo := scratch.echo.Bytes()
	output, outputID, live := terminalStream(snapshot.Endpoints.Output)
	for live && len(echo) != 0 {
		consumed, produced := snapshot.Settings.OutputSize(echo, len(scratch.output))
		snapshot.Settings.TransformOutput(echo[:consumed], scratch.output[:produced])
		if output.WriteStatusFor(produced) != stream.StatusOK {
			return
		}
		if _, status := output.Write(scratch.output[:produced]); status != stream.StatusOK {
			return
		}
		wakeStreamWaiters(outputID, 0, ipc.WaitMessage)
		echo = echo[consumed:]
	}
}

// pumpLinuxTerminals is the kernel loop's pass over the terminals: input or
// geometry no Linux task has taken yet is processed under the locks a Linux
// call would hold.
func pumpLinuxTerminals() {
	for index := 0; index < linuxcompat.MaxTerminals; index++ {
		id, snapshot, live := linuxcompat.TerminalAt(index)
		if !live {
			continue
		}
		held := heldLocks{set: registryRead | linuxState | streams, family: snapshot.FamilyID}
		held.take(nil)
		if terminalHasInput(snapshot) {
			pumpTerminal(id, nil)
		}
		held.drop()
	}
}

// terminalHasInput reports whether a terminal's input or control stream has
// something to process. The stream lock is held.
func terminalHasInput(snapshot linuxcompat.TerminalSnapshot) bool {
	for _, object := range [...]uint64{snapshot.Endpoints.Input, snapshot.Endpoints.Control} {
		if endpoint, _, live := terminalStream(object); live {
			if status := endpoint.ReadStatus(); status == stream.StatusOK || status == stream.StatusPeerClosed && object == snapshot.Endpoints.Input && !snapshot.Hangup {
				return true
			}
		}
	}
	return false
}

// terminalInterests are the waits that end a terminal read: typed input,
// geometry, the end of input, and input the kernel loop processed.
func terminalInterests(snapshot linuxcompat.TerminalSnapshot, id linuxcompat.TerminalID, target []kernelwait.Entry) int {
	count := 0
	for _, object := range [...]uint64{snapshot.Endpoints.Input, snapshot.Endpoints.Control} {
		if _, streamID, live := terminalStream(object); live && count < len(target) {
			target[count] = kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindStream, Object: streamID, Side: 0},
				Events: kernelwait.Readable | kernelwait.PeerClosed}
			count++
		}
	}
	if count < len(target) {
		target[count] = kernelwait.Entry{Key: terminalKey(id), Events: kernelwait.Readable}
		count++
	}
	return count
}

// terminalRead serves read and readv on a terminal: processed input, a line
// at a time in canonical mode.
func (handler *linuxCallHandler) terminalRead(request linuxcompat.Request, description *linuxcompat.FileDescription, vector bool) linuxcompat.Result {
	local := thisCPU()
	if pending, active := linuxcompat.SyscallContinuations.Pending(handler.task); active {
		if pending.Number != request.Number || pending.Arguments != request.Arguments {
			handler.vfsAbandon()
			return linuxErrno(linuxabi.EINVAL)
		}
		waitAnyTable.Cancel(handler.task)
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		if pending.Wake == linuxcompat.WakeInterrupted {
			return handler.interrupted(true)
		}
	}
	if description.Flags&linuxabi.OpenAccess == linuxabi.OpenWriteOnly {
		return linuxErrno(linuxabi.EBADF)
	}
	id := linuxcompat.TerminalID(description.Object)
	if result, background := handler.backgroundRead(id); background {
		return result
	}
	limit, wire, errno := handler.readLimit(request, vector, uint64(len(local.linuxIOBuffer)))
	if errno != 0 || limit == 0 {
		return linuxcompat.Result{Errno: errno}
	}
	pumpTerminal(id, handler.state)
	// Input that signalled the reader's own group ends the read so the
	// signal is taken as the call returns.
	if handler.state != nil && handler.state.Signals.Acting() {
		return handler.interrupted(true)
	}
	count, ready, found := linuxcompat.ReadTerminal(id, local.linuxIOBuffer[:limit])
	if !found {
		return linuxErrno(linuxabi.EIO)
	}
	if ready {
		if !handler.deliver(request, wire, 0, local.linuxIOBuffer[:count], nil, uint64(count)) {
			return linuxErrno(linuxabi.EFAULT)
		}
		return linuxcompat.Result{Value: uint64(count)}
	}
	if description.Flags&linuxabi.OpenNonBlock != 0 {
		return linuxErrno(linuxabi.EAGAIN)
	}
	state := handler.state
	if state != nil && state.Signals.Interrupting() {
		return handler.interrupted(true)
	}
	snapshot, _ := linuxcompat.Terminal(id)
	interests := terminalInterests(snapshot, id, local.linuxPollEntries[:])
	if !linuxcompat.SyscallContinuations.Begin(handler.task, request, nil) ||
		!linuxcompat.SyscallContinuations.Issue(handler.task, 1, linuxTerminalCorrelation) ||
		!waitAnyTable.Begin(handler.task, local.linuxPollEntries[:interests], 0) ||
		!linuxcompat.SyscallContinuations.Block(handler.task, 1) {
		waitAnyTable.Cancel(handler.task)
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		return linuxErrno(linuxabi.ENOMEM)
	}
	return linuxBlocked
}

// terminalWrite serves write and writev on a terminal: the bytes go to the
// session's output with output processing applied. Under backpressure the
// call waits as a stream write does, and returns the bytes it wrote once it
// has written some.
func (handler *linuxCallHandler) terminalWrite(request linuxcompat.Request, description *linuxcompat.FileDescription, vector bool) linuxcompat.Result {
	local := thisCPU()
	scratch := &local.linuxTerminal
	if errno := handler.consumeWake(request); errno == linuxabi.EINTR {
		return handler.interrupted(true)
	} else if errno != 0 {
		return linuxErrno(errno)
	}
	if description.Flags&linuxabi.OpenAccess == linuxabi.OpenReadOnly {
		return linuxErrno(linuxabi.EBADF)
	}
	snapshot, found := linuxcompat.Terminal(linuxcompat.TerminalID(description.Object))
	if !found {
		return linuxErrno(linuxabi.EIO)
	}
	output, outputID, live := terminalStream(snapshot.Endpoints.Output)
	if !live {
		return linuxErrno(linuxabi.EIO)
	}
	var wire []byte
	total := request.Arguments[2]
	if vector {
		var errno linuxabi.Errno
		if wire, errno = handler.copyIOVec(request); errno != 0 {
			return linuxErrno(errno)
		}
		total = 0
		for index := 0; index < len(wire)/linuxabi.IOVecSize; index++ {
			_, length := linuxcompat.IOVecEntry(wire, index)
			total += length
		}
	} else if !userRange(request.Arguments[1], total) {
		return linuxErrno(linuxabi.EFAULT)
	}
	if total > linuxcompat.MaxVectorBytes {
		total = linuxcompat.MaxVectorBytes
	}
	written := uint64(0)
	for written < total {
		// Half the output room, so every newline can become two bytes.
		chunk := min(total-written, uint64(len(scratch.output)/2))
		address := request.Arguments[1] + written
		if wire != nil {
			piece, remaining, ok := linuxcompat.VectorPiece(wire, written)
			if !ok {
				break
			}
			address, chunk = piece, min(chunk, remaining)
		}
		if chunk == 0 {
			break
		}
		source := local.linuxIOBuffer[:chunk]
		if handler.accessible(address, chunk, false) != chunk || !vm.CopyIn(handler.root, address, source) {
			if written == 0 {
				return linuxErrno(linuxabi.EFAULT)
			}
			break
		}
		consumed, produced := snapshot.Settings.OutputSize(source, len(scratch.output))
		snapshot.Settings.TransformOutput(source[:consumed], scratch.output[:produced])
		status := output.WriteStatusFor(produced)
		if status == stream.StatusOK {
			_, status = output.Write(scratch.output[:produced])
		}
		if status == stream.StatusOK {
			wakeStreamWaiters(outputID, 0, ipc.WaitMessage)
			written += uint64(consumed)
			continue
		}
		if written != 0 {
			break
		}
		if status == stream.StatusWouldBlock {
			blocked, retry := handler.blockStream(request, output, outputID, 1, produced)
			if blocked.Action == linuxabi.ActionBlock || !retry {
				return blocked
			}
			continue
		}
		return linuxErrno(linuxcompat.StreamWriteErrno(status))
	}
	return linuxcompat.Result{Value: written}
}

// terminalIoctl serves the terminal requests: termios, geometry, the
// foreground group, the session, and the input queue. Any other request is
// ENOTTY, as Linux answers a request no terminal driver knows, and none of
// them reaches the console, the keyboard, or the native terminal service.
func (handler *linuxCallHandler) terminalIoctl(state *linuxcompat.ProcessState, description linuxcompat.FileDescription, command uint64, argument uint64) linuxcompat.Result {
	scratch := &thisCPU().linuxTerminal
	id := linuxcompat.TerminalID(description.Object)
	pumpTerminal(id, state)
	snapshot, found := linuxcompat.Terminal(id)
	if !found {
		return linuxErrno(linuxabi.EIO)
	}
	caller, _ := linuxcompat.FindProcessRecord(state.PID)
	switch command {
	case linuxabi.IoctlTCGETS:
		scratch.settings = snapshot.Settings
		scratch.settings.Encode(&scratch.wire)
		if !vm.CopyOut(handler.root, argument, scratch.wire[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		return linuxcompat.Result{}
	case linuxabi.IoctlTCSETS, linuxabi.IoctlTCSETSW, linuxabi.IoctlTCSETSF:
		if !vm.CopyIn(handler.root, argument, scratch.wire[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		// The new state applies at once. GoOS sees when bytes leave for
		// the terminal service, not when the terminal shows them, so the
		// wait of TCSETSW and TCSETSF is for no queue at all.
		linuxcompat.SetTerminalSettings(id, linuxcompat.DecodeTermios(&scratch.wire), command == linuxabi.IoctlTCSETSF)
		return linuxcompat.Result{}
	case linuxabi.IoctlTIOCGWINSZ:
		wire := scratch.wire[:linuxabi.WinsizeSize]
		putUint16(wire[0:2], snapshot.Rows)
		putUint16(wire[2:4], snapshot.Columns)
		if !vm.CopyOut(handler.root, argument, wire[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		return linuxcompat.Result{}
	case linuxabi.IoctlTIOCSWINSZ:
		wire := scratch.wire[:linuxabi.WinsizeSize]
		if !vm.CopyIn(handler.root, argument, wire[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		if changed, after := linuxcompat.ResizeTerminal(id, uint16(wire[0])|uint16(wire[1])<<8, uint16(wire[2])|uint16(wire[3])<<8); changed && after.Foreground != 0 {
			signalTerminalGroup(after, linuxabi.SignalWinch, state)
		}
		return linuxcompat.Result{}
	case linuxabi.IoctlTIOCGPGRP, linuxabi.IoctlTIOCGSID:
		// Only the session a terminal controls may ask.
		if snapshot.Session == 0 || caller.SID != snapshot.Session {
			return linuxErrno(linuxabi.ENOTTY)
		}
		value := snapshot.Foreground
		if command == linuxabi.IoctlTIOCGSID {
			value = snapshot.Session
		}
		wire := scratch.wire[:4]
		putUint32(wire[:], uint32(value))
		if !vm.CopyOut(handler.root, argument, wire[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		return linuxcompat.Result{}
	case linuxabi.IoctlTIOCSPGRP:
		wire := scratch.wire[:4]
		if !vm.CopyIn(handler.root, argument, wire[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		group := int32(uint32(wire[0]) | uint32(wire[1])<<8 | uint32(wire[2])<<16 | uint32(wire[3])<<24)
		return linuxErrno(linuxcompat.SetTerminalForeground(id, state.PID, group))
	case linuxabi.IoctlFIONREAD:
		wire := scratch.wire[:4]
		putUint32(wire[:], uint32(linuxcompat.TerminalPending(id)))
		if !vm.CopyOut(handler.root, argument, wire[:]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		return linuxcompat.Result{}
	case linuxabi.IoctlTCFLSH:
		switch argument {
		case linuxabi.TCIFLUSH, linuxabi.TCIOFLUSH:
			linuxcompat.FlushTerminalInput(id)
		case linuxabi.TCOFLUSH:
			// Output already handed to the terminal service cannot be
			// taken back.
		default:
			return linuxErrno(linuxabi.EINVAL)
		}
		return linuxcompat.Result{}
	case linuxabi.IoctlTCSBRK:
		// tcdrain: output is with the terminal service once written.
		return linuxcompat.Result{}
	}
	linuxcompat.NoteTerminalIoctl(id, command)
	return linuxErrno(linuxabi.ENOTTY)
}

// signalTerminalGroup sends signal from the terminal to every member of its
// foreground group. The group lives in the terminal's family, whose lock
// the caller holds; no signal leaves the family.
func signalTerminalGroup(snapshot linuxcompat.TerminalSnapshot, signal uint64, caller *linuxcompat.ProcessState) {
	scratch := &thisCPU().linuxTerminal
	count := linuxcompat.GroupMembers(snapshot.FamilyID, snapshot.Foreground, scratch.members[:])
	info := linuxcompat.SignalInfo{Code: linuxabi.SignalCodeKernel}
	for index := 0; index < count; index++ {
		if receiver, found := linuxcompat.FindProcessByPID(scratch.members[index]); found && receiver.FamilyID == snapshot.FamilyID {
			sendLinuxSignal(receiver, signal, info, caller)
		}
	}
}

// sendLinuxSignal generates signal for receiver, which sender, when not nil,
// sends from its own call. SIGCONT continues a stopped receiver before
// anything else. A stop signal its receiver does not catch stops it now. A
// signal the receiver would die of ends it now, unless it is the sender,
// whose call returns first. Anything else becomes pending and interrupts a
// wait if a handler will run.
func sendLinuxSignal(receiver *linuxcompat.ProcessState, signal uint64, info linuxcompat.SignalInfo, sender *linuxcompat.ProcessState) {
	if signal == linuxabi.SignalContinue {
		continueLinuxProcess(receiver)
	}
	if receiver.Signals.StopsNow(signal) {
		if stopRefused(receiver, signal) {
			continueRefusedRoot(receiver)
			return
		}
		// A pending SIGCONT goes with the stop.
		receiver.Signals.CancelContinue()
		stopLinuxProcess(receiver, signal)
		return
	}
	if receiver != sender && receiver.Signals.Fatal(signal) {
		if owner, alive := processRegistry.ByRoot(receiver.Root); alive {
			for _, task := range owner.Tasks() {
				cancelTaskWaits(task)
				amd64.TerminateTask(uint64(task), linuxcompat.SignalExitCode(signal))
			}
		}
		return
	}
	receiver.Signals.Raise(signal, info)
	if receiver != sender {
		interruptLinuxSleeper(receiver)
	}
}

// stopRefused reports whether a stop signal is discarded instead of
// stopping receiver. Linux discards SIGTSTP, SIGTTIN, and SIGTTOU sent to an
// orphaned group. GoOS also discards any stop of a launch root: its parent
// is PID 1, which never sends SIGCONT, so a stopped root would hold its
// terminal session for good.
func stopRefused(receiver *linuxcompat.ProcessState, signal uint64) bool {
	record, found := linuxcompat.FindProcessRecord(receiver.PID)
	if !found || record.ParentPID == 1 {
		return true
	}
	return signal != linuxabi.SignalStop && linuxcompat.GroupOrphaned(receiver.FamilyID, record.PGID)
}

// continueRefusedRoot makes a stop a launch root refused look like a stop
// and an immediate continue: SIGCONT reaches the root, so a program that
// suspended itself, as nano does, restores its terminal in its handler.
func continueRefusedRoot(receiver *linuxcompat.ProcessState) {
	if record, found := linuxcompat.FindProcessRecord(receiver.PID); found && record.ParentPID == 1 &&
		receiver.Signals.Raise(linuxabi.SignalContinue, linuxcompat.SignalInfo{Code: linuxabi.SignalCodeKernel}) {
		interruptLinuxSleeper(receiver)
	}
}

// stopLinuxProcess stops every task of a process. Each keeps its wait, its
// continuation, and its pending signals; a wait that completes meanwhile
// is kept for when it continues. The parent hears of the stop through
// wait4 and, unless it asked otherwise, SIGCHLD.
func stopLinuxProcess(state *linuxcompat.ProcessState, signal uint64) {
	parentPID, changed := linuxcompat.MarkStopped(state.PID, signal)
	if !changed {
		return
	}
	if owner, alive := processRegistry.ByRoot(state.Root); alive {
		for _, task := range owner.Tasks() {
			amd64.StopTask(task)
		}
	}
	notifyJobChange(parentPID, state.PID, linuxabi.SignalCodeChildStopped, int32(signal))
}

// continueLinuxProcess ends the stop of a process.
func continueLinuxProcess(state *linuxcompat.ProcessState) {
	parentPID, changed := linuxcompat.MarkContinued(state.PID)
	if !changed {
		return
	}
	if owner, alive := processRegistry.ByRoot(state.Root); alive {
		for _, task := range owner.Tasks() {
			amd64.ContinueTask(task)
		}
	}
	notifyJobChange(parentPID, state.PID, linuxabi.SignalCodeChildContinued, int32(linuxabi.SignalContinue))
}

// notifyJobChange tells the parent a child stopped or continued: a wait4
// asleep for it runs again, and SIGCHLD is raised unless SA_NOCLDSTOP.
func notifyJobChange(parentPID int32, childPID int32, code int32, status int32) {
	wakeChildWaiter(parentPID)
	parent, found := linuxcompat.FindProcessByPID(parentPID)
	if !found || !parent.Signals.NotifiesStops() {
		return
	}
	parent.Signals.Raise(linuxabi.SignalChild, linuxcompat.SignalInfo{Code: code, PID: childPID, UID: linuxcompat.StatUID, Status: status})
	interruptLinuxSleeper(parent)
}

// releaseTerminal gives up one reference to a terminal and, with the last,
// the native stream ends it held.
func releaseTerminal(id linuxcompat.TerminalID) {
	endpoints, last := linuxcompat.ReleaseTerminal(id)
	if !last {
		return
	}
	for _, object := range [...]uint64{endpoints.Input, endpoints.Output, endpoints.Error, endpoints.Control} {
		if object == 0 {
			continue
		}
		if streamID, writer, closed, ok := streamRegistry.Drop(object); ok {
			finishStreamClosure(stream.Closure{Stream: streamID, Writer: writer, Closed: closed})
		}
	}
}

// openTerminal serves an open of /dev/tty: one more description of the
// caller's controlling terminal, or ENXIO without one.
func (handler *linuxCallHandler) openTerminal(state *linuxcompat.ProcessState, intent linuxcompat.OpenIntent, trailing bool) linuxcompat.Result {
	if trailing || intent.Directory {
		return linuxErrno(linuxabi.ENOTDIR)
	}
	if intent.Create && intent.Exclusive {
		return linuxErrno(linuxabi.EEXIST)
	}
	record, found := linuxcompat.FindProcessRecord(state.PID)
	if !found {
		return linuxErrno(linuxabi.ENXIO)
	}
	id, controlled := linuxcompat.ControllingTerminal(record.SID)
	if !controlled || !linuxcompat.RetainTerminal(id) {
		return linuxErrno(linuxabi.ENXIO)
	}
	description := linuxcompat.FileDescription{Kind: linuxcompat.FDTerminal, Flags: intent.StatusFlags(), Object: uint64(id)}
	fd, errno := linuxcompat.AllocateFD(handler.root, description, intent.CloseOnExec)
	if errno != 0 {
		releaseTerminal(id)
		return linuxErrno(errno)
	}
	return linuxcompat.Result{Value: uint64(fd)}
}

// backgroundRead applies job control to a read of the controlling terminal
// by a process outside its foreground group: SIGTTIN goes to the reader's
// group, which stops it by default, and the read runs again once it
// continues. A reader that ignores or blocks SIGTTIN, or whose group is
// orphaned, gets EIO, as on Linux.
func (handler *linuxCallHandler) backgroundRead(id linuxcompat.TerminalID) (linuxcompat.Result, bool) {
	state := handler.state
	snapshot, found := linuxcompat.Terminal(id)
	if state == nil || !found || snapshot.Session == 0 {
		return linuxcompat.Result{}, false
	}
	caller, known := linuxcompat.FindProcessRecord(state.PID)
	if !known || caller.SID != snapshot.Session || caller.PGID == snapshot.Foreground {
		return linuxcompat.Result{}, false
	}
	if state.Signals.Blocked(linuxabi.SignalTTYIn) || state.Signals.Outcome(linuxabi.SignalTTYIn) == linuxcompat.SignalDiscard ||
		linuxcompat.GroupOrphaned(state.FamilyID, caller.PGID) {
		return linuxErrno(linuxabi.EIO), true
	}
	handler.killGroup(state, caller.PGID, linuxabi.SignalTTYIn, linuxcompat.SignalInfo{Code: linuxabi.SignalCodeKernel})
	return linuxcompat.Result{Restart: true}, true
}
