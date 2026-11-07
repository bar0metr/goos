//go:build uefi

package main

// The kernel half of a family's GLNXSOCK3 session (ADR 0210): sending a
// request and waiting for its reply, draining the session channel and the
// complete ring, reading readiness from the pool, and sleeping on it.
//
// Every socket call of a family runs under the family's lock, so the calls
// of one family take turns. A call that waits leaves its continuation
// behind and is entered again when it wakes:
//
//   - A reply wait follows a request. It registers on the session channel
//     with an eight-second deadline, drains the channel, and sleeps only if
//     its reply is not in. Any message on the channel wakes it; it then
//     drains again and looks for its reply. A signal does not end it, since
//     the service answers every request at once. At the deadline the call
//     fails with ETIMEDOUT and leaves the request abandoned; the session
//     stays.
//   - A readiness sleep waits for a socket's readiness to change. It
//     registers on the socket's key and on the session channel's close,
//     with a deadline for SO_RCVTIMEO or SO_SNDTIMEO, drains the channel,
//     reads the readiness page again, and sleeps only if the socket is
//     still not ready. A drain that finds the socket changed wakes it, so
//     does the session's end, and a handled signal interrupts it.
//
// Draining the channel files each reply with the request it answers. A
// reply whose caller still waits for it stays for that caller; one whose
// caller is gone is applied: a socket opened or accepted for nobody is
// closed. Anything that does not fit the protocol fails the session.
//
// The service's send drains the channel too, right after its message lands
// (dispatchLinuxNet), so a reply or a readiness change wakes only the call
// it concerns. One drain of a session runs at a time; one asked for while
// another runs is left to that one, and a call that finds its reply only
// after it blocked takes it back itself.

import (
	"sync/atomic"

	"goos/internal/capability"
	"goos/internal/logfmt"
	"goos/kernel/arch/amd64"
	"goos/kernel/clock"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/sched"
	kernelwait "goos/kernel/wait"
	"goos/userspace/linuxsockapi"
)

// linuxSocketReadTries bounds how often a readiness entry is read again
// while the service rewrites it.
const linuxSocketReadTries = 64

// socketCall is what one socket call found: the socket a descriptor names
// and the session it belongs to.
type socketCall struct {
	familyID    uint64
	endpoint    linuxcompat.NetEndpoint
	object      uint64
	socket      linuxcompat.NetSocket
	description *linuxcompat.FileDescription
	// live reports that the socket belongs to the family's session and the
	// session has not failed.
	live bool
}

// linuxSocketScratch is each processor's socket scratch. The pump has its
// own message and reply, so it can run while a call holds a decoded reply.
type linuxSocketScratch struct {
	call        socketCall
	message     ipc.Message
	request     linuxsockapi.Request
	reply       linuxsockapi.Reply
	pumpMessage ipc.Message
	pumpReply   linuxsockapi.Reply
	bell        ipc.Message
	pieces      [linuxsockapi.RXSlots]linuxcompat.RXPiece
	waiters     [16]sched.TaskID
	wakes       [32]uint64
	changed     [32]linuxcompat.SocketWake
	// pollEpoch is the wake epoch of the socket pollSocket last looked at.
	pollEpoch uint32
	address   [linuxcompat.SockaddrInSize]byte
	word      [4]byte
	header    [linuxcompat.MsghdrSize]byte
	option    [16]byte
}

// socketDescriptor resolves a descriptor of a socket call: EBADF when it is
// not open, ENOTSOCK when it is not a socket.
func (handler *linuxCallHandler) socketDescriptor(fd uint64) (*socketCall, linuxabi.Errno) {
	call := &thisCPU().linuxSocket.call
	description, found := linuxcompat.DescriptionOf(handler.root, fd)
	if !found {
		return nil, linuxabi.EBADF
	}
	if description.Kind != linuxcompat.FDSocket {
		return nil, linuxabi.ENOTSOCK
	}
	return call, handler.socketOf(call, description)
}

// socketOf fills call for an FDSocket description.
func (handler *linuxCallHandler) socketOf(call *socketCall, description *linuxcompat.FileDescription) linuxabi.Errno {
	socket, familyID, live, found := linuxcompat.SocketOf(description.Object)
	if !found {
		return linuxabi.EBADF
	}
	endpoint, sessionLive, _ := linuxcompat.NetSession(familyID)
	call.familyID, call.endpoint, call.object, call.socket = familyID, endpoint, description.Object, socket
	call.description, call.live = description, live && sessionLive && endpoint.Session == socket.Session
	return 0
}

// socketResume finds the continuation of a socket call that waited. done
// reports that result ends the call: a continuation that does not match, or
// a call still queued for a request slot.
func (handler *linuxCallHandler) socketResume(request linuxcompat.Request) (pending linuxcompat.Pending, resumed bool, result linuxcompat.Result, done bool) {
	pending, active := linuxcompat.SyscallContinuations.Pending(handler.task)
	if !active {
		return pending, false, linuxcompat.Result{}, false
	}
	if pending.Number != request.Number || pending.Arguments != request.Arguments {
		handler.socketEnd()
		return pending, false, linuxErrno(linuxabi.EINVAL), true
	}
	if pending.Phase == linuxcompat.ContinuationWaiting {
		if pending.Correlation == 0 {
			// Queued for a request slot; only a freed slot ends that wait.
			return pending, true, linuxBlocked, true
		}
		// A readiness sleep ends with a published event, which wakes the
		// task without resuming its continuation.
		linuxcompat.SyscallContinuations.Resume(handler.task, pending.Correlation, linuxcompat.WakeMessage)
		linuxcompat.NoteNet(linuxcompat.NetPollWakeups, 1)
	}
	waitAnyTable.Cancel(handler.task)
	ipcWaitSet.Cancel(handler.task)
	pending, _ = linuxcompat.SyscallContinuations.Pending(handler.task)
	return pending, true, linuxcompat.Result{}, false
}

// socketStart begins the continuation of a socket call. note is kept for
// every wait of the call: the absolute end of its timeout, or zero.
func (handler *linuxCallHandler) socketStart(request linuxcompat.Request, note uint64) linuxabi.Errno {
	if !linuxcompat.SyscallContinuations.Begin(handler.task, request, nil) ||
		note != 0 && !linuxcompat.SyscallContinuations.Annotate(handler.task, nil, note) {
		handler.socketEnd()
		return linuxabi.ENOMEM
	}
	return 0
}

// socketEnd ends a socket call's continuation. A request still in flight
// is abandoned, and its reply applied when it comes.
func (handler *linuxCallHandler) socketEnd() {
	pending, active := linuxcompat.SyscallContinuations.Pending(handler.task)
	if active && pending.Issued && linuxcompat.SocketRequestCorrelation(pending.Correlation) && handler.state != nil {
		local := thisCPU()
		cleanup, woken := linuxcompat.AbandonRequest(handler.state.FamilyID, pending.Correlation, local.linuxSocket.waiters[:])
		if endpoint, live, _ := linuxcompat.NetSession(handler.state.FamilyID); live {
			applySocketCleanup(handler.state.FamilyID, endpoint, cleanup)
		}
		wakeSocketWaiters(local.linuxSocket.waiters[:woken])
	}
	linuxcompat.RemoveSocketWaiter(handler.task)
	waitAnyTable.Cancel(handler.task)
	ipcWaitSet.Cancel(handler.task)
	linuxcompat.SyscallContinuations.Cancel(handler.task)
}

// socketProgress records a call's progress between its waits.
func (handler *linuxCallHandler) socketProgress(partial uint64) {
	linuxcompat.SyscallContinuations.Rearm(handler.task, partial)
}

// socketPending returns the call's continuation as it stands.
func (handler *linuxCallHandler) socketPending() linuxcompat.Pending {
	pending, _ := linuxcompat.SyscallContinuations.Pending(handler.task)
	return pending
}

// socketExchange sends one request of the call and returns its reply, or
// blocks until it comes. blocked means the caller returns linuxBlocked; the
// call resumes in socketAwait.
//
//go:noinline
func (handler *linuxCallHandler) socketExchange(call *socketCall, request *linuxsockapi.Request) (linuxsockapi.Reply, linuxabi.Errno, bool) {
	local := thisCPU()
	if !call.live {
		return linuxsockapi.Reply{}, linuxabi.ENETRESET, false
	}
	var correlation uint64
	for attempt := 0; ; attempt++ {
		var status linuxcompat.RequestStatus
		_, correlation, status = linuxcompat.BeginRequest(call.familyID, call.endpoint.Session, handler.task, request,
			amd64.TimerTicks()+linuxVFSReplyTicks, true)
		if status == linuxcompat.RequestStarted {
			break
		}
		if status == linuxcompat.RequestFailed {
			return linuxsockapi.Reply{}, linuxabi.ENETRESET, false
		}
		if !linuxcompat.SyscallContinuations.BlockContext(handler.task, 1) {
			linuxcompat.RemoveSocketWaiter(handler.task)
			return linuxsockapi.Reply{}, linuxabi.EIO, false
		}
		// A slot freed by a call of another processor between the two
		// steps woke nobody; take it now.
		if attempt != 0 || !linuxcompat.RequestSlotFree(call.familyID) {
			return linuxsockapi.Reply{}, 0, true
		}
		linuxcompat.SyscallContinuations.ResumeContext(handler.task)
		linuxcompat.RemoveSocketWaiter(handler.task)
	}
	if !linuxcompat.SyscallContinuations.Issue(handler.task, 1, correlation) ||
		!linuxsockapi.EncodeRequest(request, correlation, &local.linuxSocket.message) {
		woken := linuxcompat.CancelRequest(call.familyID, correlation, local.linuxSocket.waiters[:])
		wakeSocketWaiters(local.linuxSocket.waiters[:woken])
		handler.socketProgress(handler.socketPending().Partial)
		return linuxsockapi.Reply{}, linuxabi.EIO, false
	}
	status := channelSend(call.endpoint.Channel, call.endpoint.Side, &local.linuxSocket.message, nil, nil, nil)
	if status != ipc.StatusOK {
		woken := linuxcompat.CancelRequest(call.familyID, correlation, local.linuxSocket.waiters[:])
		wakeSocketWaiters(local.linuxSocket.waiters[:woken])
		handler.socketProgress(handler.socketPending().Partial)
		if status == ipc.StatusResourceExhausted {
			return linuxsockapi.Reply{}, linuxabi.ENOBUFS, false
		}
		failSocketSession(call.familyID, call.endpoint, socketFailedSend)
		return linuxsockapi.Reply{}, linuxabi.ENETRESET, false
	}
	linuxcompat.NoteNet(linuxcompat.NetRequests, 1)
	wakeIPCWaiters(call.endpoint.Channel, call.endpoint.Side^1, ipc.WaitMessage)
	return handler.socketAwait(call)
}

// socketAwait returns the reply to the call's request in flight, or blocks
// for it.
//
//go:noinline
func (handler *linuxCallHandler) socketAwait(call *socketCall) (linuxsockapi.Reply, linuxabi.Errno, bool) {
	local := thisCPU()
	pending := handler.socketPending()
	if !pending.Issued || !linuxcompat.SocketRequestCorrelation(pending.Correlation) {
		return linuxsockapi.Reply{}, linuxabi.EIO, false
	}
	endpoint, live, _ := linuxcompat.NetSession(call.familyID)
	if !live || endpoint.Session != call.endpoint.Session {
		return linuxsockapi.Reply{}, linuxabi.ENETRESET, false
	}
	// Register before draining: a reply queued after the drain wakes the
	// registration.
	deadline := linuxcompat.RequestDeadline(call.familyID, endpoint.Session, pending.Correlation)
	registered := deadline != 0 && pending.Wake != linuxcompat.WakeTimeout &&
		ipcWaitSet.Add(handler.task, endpoint.Channel, endpoint.Side, deadline)
	pumpSocketSession(call.familyID, endpoint, handler.task)
	wait, _, woken := linuxcompat.TakeReply(call.familyID, endpoint.Session, pending.Correlation, &local.linuxSocket.reply, local.linuxSocket.waiters[:])
	wakeSocketWaiters(local.linuxSocket.waiters[:woken])
	switch wait {
	case linuxcompat.ReplyTaken:
		ipcWaitSet.Cancel(handler.task)
		handler.socketProgress(pending.Partial)
		return local.linuxSocket.reply, 0, false
	case linuxcompat.ReplyGone:
		ipcWaitSet.Cancel(handler.task)
		return linuxsockapi.Reply{}, linuxabi.ENETRESET, false
	}
	if !registered {
		ipcWaitSet.Cancel(handler.task)
		linuxcompat.NoteNet(linuxcompat.NetReplyTimeouts, 1)
		return linuxsockapi.Reply{}, linuxabi.ETIMEDOUT, false
	}
	if !linuxcompat.SyscallContinuations.Block(handler.task, 1) {
		ipcWaitSet.Cancel(handler.task)
		return linuxsockapi.Reply{}, linuxabi.EIO, false
	}
	// A drain on another processor may have filed the reply after the look
	// above and found the call not blocked yet. Whoever moves the call out
	// of its wait first carries on: this call when it does, otherwise the
	// wake already sent.
	if linuxcompat.ReplyReady(call.familyID, pending.Correlation) &&
		linuxcompat.SyscallContinuations.Resume(handler.task, pending.Correlation, linuxcompat.WakeMessage) {
		ipcWaitSet.Cancel(handler.task)
		wait, _, woken = linuxcompat.TakeReply(call.familyID, endpoint.Session, pending.Correlation, &local.linuxSocket.reply, local.linuxSocket.waiters[:])
		wakeSocketWaiters(local.linuxSocket.waiters[:woken])
		if wait != linuxcompat.ReplyTaken {
			return linuxsockapi.Reply{}, linuxabi.ENETRESET, false
		}
		handler.socketProgress(pending.Partial)
		return local.linuxSocket.reply, 0, false
	}
	return linuxsockapi.Reply{}, 0, true
}

// socketPoolPage returns one page of a session's pool, from linuxPools: a
// socket call holds no shared memory lock.
func socketPoolPage(endpoint linuxcompat.NetEndpoint, index uint32) ([]byte, bool) {
	if endpoint.PoolObject == 0 || index >= linuxsockapi.PoolPages {
		return nil, false
	}
	for slot := range linuxPools {
		pool := &linuxPools[slot]
		if pool.object.Load() == endpoint.PoolObject {
			page := pool.pages[index]
			return page, len(page) == int(vm.PageSize)
		}
	}
	return nil, false
}

// linuxPool holds the pages of one session pool. The kernel fills it, and
// retains the pool object, before the session goes live, and lets both go
// when the session's holder exits: that exit holds the family's lock and
// the shared memory lock, so no socket call of the family and no drain in
// a service's send can be using the pages then.
type linuxPool struct {
	object atomic.Uint64
	family uint64
	pages  [linuxsockapi.PoolPages][]byte
}

// linuxPools caches the pools of the sessions whose holder still runs: a
// live session's, and those of sessions replaced since. Attaches and exits
// write it under the shared memory lock; calls read it without a lock.
var linuxPools [2 * linuxcompat.MaxNetFamilies]linuxPool

// cacheLinuxPool puts family's session pool in linuxPools before the
// session goes live. The caller holds the shared memory lock. With no entry
// free it takes the oldest one no live session uses.
func cacheLinuxPool(objectID uint64, family uint64) bool {
	for slot := range linuxPools {
		if linuxPools[slot].object.Load() == objectID {
			return true
		}
	}
	object, found := sharedObjects.Find(objectID)
	if !found || object.PageCount() != linuxsockapi.PoolPages {
		return false
	}
	pool := freeLinuxPool()
	if pool == nil {
		return false
	}
	for index := range pool.pages {
		frame, present := object.Frame(vm.PhysicalPage, uint32(index))
		page, mapped := vm.PhysicalPage(frame)
		if !present || !mapped || len(page) < int(vm.PageSize) {
			return false
		}
		pool.pages[index] = page[:vm.PageSize]
	}
	object.Retain()
	pool.family = family
	pool.object.Store(objectID)
	return true
}

// freeLinuxPool returns an empty entry of linuxPools, emptying one whose
// pool no live session uses when none is.
func freeLinuxPool() *linuxPool {
	for slot := range linuxPools {
		if linuxPools[slot].object.Load() == 0 {
			return &linuxPools[slot]
		}
	}
	var families [linuxcompat.MaxNetFamilies]uint64
	var endpoints [linuxcompat.MaxNetFamilies]linuxcompat.NetEndpoint
	live := linuxcompat.LiveSessions(&families, &endpoints)
	for slot := range linuxPools {
		pool := &linuxPools[slot]
		old, inUse := pool.object.Load(), false
		for index := 0; index < live; index++ {
			inUse = inUse || endpoints[index].PoolObject == old
		}
		if !inUse {
			pool.object.Store(0)
			dropShared(old)
			return pool
		}
	}
	return nil
}

// releaseLinuxPools lets go of every pool family's sessions used, when the
// holder of its session exits.
func releaseLinuxPools(family uint64) {
	for slot := range linuxPools {
		pool := &linuxPools[slot]
		if old := pool.object.Load(); old != 0 && pool.family == family {
			pool.object.Store(0)
			pool.family = 0
			dropShared(old)
		}
	}
}

// socketReadiness reads a socket's entry in the readiness pages. live false
// means the socket is gone: its session failed, or the service publishes
// another socket in its entry.
func socketReadiness(call *socketCall) (linuxsockapi.Readiness, bool) {
	if !call.live {
		return linuxsockapi.Readiness{}, false
	}
	index, valid := linuxsockapi.SocketIndex(call.socket.Identity)
	header, found := socketPoolPage(call.endpoint, linuxsockapi.ReadinessPage)
	if !valid || !found || linuxsockapi.Magic(header) != linuxsockapi.ReadinessMagic {
		return linuxsockapi.Readiness{}, false
	}
	page, found := socketPoolPage(call.endpoint, linuxsockapi.EntryPage(index))
	if !found {
		return linuxsockapi.Readiness{}, false
	}
	state, status := linuxsockapi.ReadEntry(page, index, linuxSocketReadTries)
	switch status {
	case linuxsockapi.EntryBusy:
		// The service is rewriting the entry, and may have been preempted
		// doing it. Not ready yet: its notification follows the write.
		state = linuxsockapi.Readiness{Socket: call.socket.Identity}
	case linuxsockapi.EntryInvalid:
		failSocketSession(call.familyID, call.endpoint, socketFailedReadiness)
		return linuxsockapi.Readiness{}, false
	}
	if state.Socket != call.socket.Identity {
		return linuxsockapi.Readiness{}, false
	}
	// The entry first, then the ring: the service publishes a slot before
	// the entry that counts it gone from netd, so whatever the entry no
	// longer counts is on the ring by now.
	if !drainSocketData(call.familyID, call.endpoint) {
		return linuxsockapi.Readiness{}, false
	}
	return linuxcompat.MergeReadiness(call.familyID, call.endpoint.Session, call.socket.Datagram, state), true
}

// sleepOutcome is how a readiness sleep went.
type sleepOutcome uint8

const (
	// sleepBlocked: the call returns linuxBlocked.
	sleepBlocked sleepOutcome = iota + 1
	// sleepReady: readiness changed meanwhile; the call goes on.
	sleepReady
	// sleepSignalled: a handled signal is pending; the call is interrupted.
	sleepSignalled
	// sleepFailed: the wait could not be registered.
	sleepFailed
)

// socketSleep waits until the socket is ready for one of want, or in error
// or hung up, or end passes, end being an absolute time in nanoseconds or
// zero.
//
//go:noinline
func (handler *linuxCallHandler) socketSleep(call *socketCall, want linuxsockapi.Ready, end uint64) sleepOutcome {
	local := thisCPU()
	if handler.state != nil && (handler.state.Signals.Interrupting() || handler.state.Signals.Acting()) {
		return sleepSignalled
	}
	deadline := uint64(0)
	if end != 0 {
		now := nowNanos()
		if now >= end {
			deadline = amd64.TimerTicks() + 1
		} else {
			deadline = amd64.TimerTicks() + linuxcompat.RemainingTicks(end-now, clock.TicksPerSecond, linuxSleepTicks)
		}
	}
	interest := local.linuxPollEntries[:2]
	interest[0] = kernelwait.Entry{Key: socketWaitKey(call.object), Events: socketWaitEvents(want)}
	interest[1] = sessionCloseEntry(call.endpoint)
	if !linuxcompat.SyscallContinuations.Issue(handler.task, 1, linuxcompat.SocketSleepCorrelation) ||
		!waitAnyTable.Begin(handler.task, interest, deadline) {
		waitAnyTable.Cancel(handler.task)
		handler.socketProgress(handler.socketPending().Partial)
		return sleepFailed
	}
	pumpSocketSession(call.familyID, call.endpoint, handler.task)
	refreshSocket(call)
	if state, live := socketReadiness(call); !live || state.Ready&(want|linuxsockapi.ReadyError|linuxsockapi.ReadyHangup) != 0 {
		waitAnyTable.Cancel(handler.task)
		handler.socketProgress(handler.socketPending().Partial)
		return sleepReady
	}
	if !linuxcompat.SyscallContinuations.Block(handler.task, 1) {
		waitAnyTable.Cancel(handler.task)
		handler.socketProgress(handler.socketPending().Partial)
		return sleepFailed
	}
	return sleepBlocked
}

// refreshSocket reads the call's socket and session again after a drain,
// which may have failed the session.
func refreshSocket(call *socketCall) {
	socket, familyID, live, found := linuxcompat.SocketOf(call.object)
	endpoint, sessionLive, _ := linuxcompat.NetSession(familyID)
	if !found {
		call.live = false
		return
	}
	call.socket = socket
	call.live = live && sessionLive && endpoint.Session == socket.Session
}

// pumpSocketSession drains a session's channel. self is the draining task,
// which takes its own reply without a wake. It reports false once the
// session failed.
//
//go:noinline
func pumpSocketSession(familyID uint64, endpoint linuxcompat.NetEndpoint, self sched.TaskID) bool {
	if !linuxcompat.BeginPump(familyID) {
		return true
	}
	for {
		live := drainSocketSession(familyID, endpoint, self)
		if live {
			wakeChangedSockets(familyID, endpoint)
		}
		if !linuxcompat.EndPump(familyID) {
			return live
		}
	}
}

// drainSocketSession is one pass of pumpSocketSession.
func drainSocketSession(familyID uint64, endpoint linuxcompat.NetEndpoint, self sched.TaskID) bool {
	local := thisCPU()
	scratch := &local.linuxSocket
	for drained := 0; drained <= ipc.QueueCapacity; drained++ {
		status := channelReceive(endpoint.Channel, endpoint.Side, &scratch.pumpMessage)
		if status == ipc.StatusNotFound {
			break
		}
		if status != ipc.StatusOK {
			failSocketSession(familyID, endpoint, socketFailedReceive)
			return false
		}
		verdict, owner, correlation := linuxcompat.ClassifySocketMessage(familyID, endpoint.Session, &scratch.pumpMessage, &scratch.pumpReply)
		switch verdict {
		case linuxcompat.MessageReadyChanged:
			if page, found := socketPoolPage(endpoint, linuxsockapi.ReadinessPage); found {
				linuxsockapi.TakeToken(page)
			}
			linuxcompat.NoteReadyChanged(familyID)
		case linuxcompat.MessageReply:
			if owner != 0 && socketOwnerWaits(owner, correlation) && linuxcompat.DeliverReply(familyID, correlation, &scratch.pumpReply) {
				if owner != self && ipcWaitSet.Cancel(owner) {
					resumeLinuxSyscall(owner, correlation, linuxcompat.WakeMessage)
				}
				continue
			}
			cleanup, woken := linuxcompat.AbandonReply(familyID, correlation, &scratch.pumpReply, scratch.waiters[:])
			applySocketCleanup(familyID, endpoint, cleanup)
			wakeSocketWaiters(scratch.waiters[:woken])
		default:
			failSocketSession(familyID, endpoint, socketFailedMessage)
			return false
		}
	}
	flushSocketSession(familyID, endpoint)
	if !releaseSocketOrphans(familyID, endpoint) {
		return false
	}
	return drainSocketData(familyID, endpoint)
}

// socketOwnerWaits reports whether task's call still waits for correlation.
func socketOwnerWaits(task sched.TaskID, correlation uint64) bool {
	pending, active := linuxcompat.SyscallContinuations.Pending(task)
	return active && pending.Issued && pending.Correlation == correlation
}

// applySocketCleanup carries out what an abandoned reply left: a Close for
// a socket nobody installs.
func applySocketCleanup(familyID uint64, endpoint linuxcompat.NetEndpoint, cleanup linuxcompat.SocketCleanup) {
	if cleanup.Close != 0 {
		sendSocketClose(familyID, endpoint, cleanup.Close)
	}
}

// flushSocketSession sends the Closes a full queue deferred.
func flushSocketSession(familyID uint64, endpoint linuxcompat.NetEndpoint) {
	for attempt := 0; attempt < linuxcompat.MaxSockets; attempt++ {
		identity, found := linuxcompat.TakeDeferredClose(familyID)
		if !found {
			break
		}
		if !sendSocketClose(familyID, endpoint, identity) {
			break
		}
	}
}

// sendSocketClose sends Close for identity with nobody waiting for the
// reply. One that cannot go now is deferred to the next drain.
func sendSocketClose(familyID uint64, endpoint linuxcompat.NetEndpoint, identity uint64) bool {
	local := thisCPU()
	// The RX slots it had queued go back before the Close.
	if rings, found := socketPoolPage(endpoint, linuxsockapi.RingPage); found && linuxcompat.RXDrop(familyID, endpoint.Session, rings, identity) {
		ringSocketDoorbell(endpoint, rings)
	}
	request := linuxsockapi.Request{Opcode: linuxsockapi.OpClose, Socket: identity}
	_, correlation, status := linuxcompat.BeginRequest(familyID, endpoint.Session, 0, &request, 0, false)
	if status == linuxcompat.RequestFailed {
		return false
	}
	if status == linuxcompat.RequestBusy || !linuxsockapi.EncodeRequest(&request, correlation, &local.linuxSocket.pumpMessage) {
		if status == linuxcompat.RequestStarted {
			linuxcompat.CancelRequest(familyID, correlation, nil)
		}
		linuxcompat.DeferClose(familyID, identity)
		return false
	}
	sent := channelSend(endpoint.Channel, endpoint.Side, &local.linuxSocket.pumpMessage, nil, nil, nil)
	if sent != ipc.StatusOK {
		linuxcompat.CancelRequest(familyID, correlation, nil)
		if sent == ipc.StatusResourceExhausted {
			linuxcompat.DeferClose(familyID, identity)
		}
		return false
	}
	wakeIPCWaiters(endpoint.Channel, endpoint.Side^1, ipc.WaitMessage)
	return true
}

// failSocketSession ends a session that broke: its channel side closes, so
// the service sees the family go, and every waiter of the family on the
// channel wakes to find its socket failed.
// socketFailure names why a family session failed, for the kernel log.
type socketFailure uint8

const (
	socketFailedSend      socketFailure = iota + 1 // a request could not be queued
	socketFailedReadiness                          // a readiness entry held values outside the protocol
	socketFailedReceive                            // the channel failed or the service died
	socketFailedMessage                            // a message the protocol does not allow
	socketFailedHolder                             // the process holding the session ended
	socketFailedRing                               // a ring entry broke the protocol
)

func failSocketSession(familyID uint64, endpoint linuxcompat.NetEndpoint, reason socketFailure) {
	local := thisCPU()
	_, woken, failed := linuxcompat.FailNet(familyID, endpoint.Session, local.linuxSocket.waiters[:])
	if !failed {
		return
	}
	kernelLogUints(logfmt.Warn, "linux socket session failed", "family", familyID, "reason", uint64(reason))
	wakeSocketWaiters(local.linuxSocket.waiters[:woken])
	for cursor := 0; cursor >= 0; {
		var count int
		count, cursor = linuxcompat.FamilySocketObjects(familyID, cursor, local.linuxSocket.wakes[:])
		publishSocketWakes(local.linuxSocket.wakes[:count])
	}
	if closeChannelSide(endpoint.Channel, endpoint.Side) {
		wakeIPCWaiters(endpoint.Channel, endpoint.Side^1, ipc.WaitPeerDied)
	}
	wakeIPCWaiters(endpoint.Channel, endpoint.Side, ipc.WaitPeerDied)
}

// wakeSocketWaiters wakes tasks queued for a request slot.
func wakeSocketWaiters(tasks []sched.TaskID) {
	for index := range tasks {
		if linuxcompat.SyscallContinuations.ResumeContext(tasks[index]) {
			amd64.WakeTaskID(uint64(tasks[index]))
		}
		tasks[index] = 0
	}
}

// releaseSocketDescription closes the socket of a description whose last
// reference went. It runs in a call and in process teardown alike, so it
// sends Close without waiting for the reply.
func releaseSocketDescription(object uint64) {
	identity, familyID, send := linuxcompat.CloseSocket(object)
	// A call another thread left asleep on the socket wakes to find it gone.
	publishWaitAny(socketWaitKey(object), socketWakeEvents)
	if !send {
		return
	}
	if endpoint, live, _ := linuxcompat.NetSession(familyID); live {
		sendSocketClose(familyID, endpoint, identity)
	}
}

// endLinuxNetHolder fails the session of a family whose holder, the
// process with the session's handles, exits. Its handles close with it, and
// the family's other members must not wait on a channel nobody serves.
func endLinuxNetHolder(exited *process.Process, familyID uint64) {
	grant, found := linuxcompat.NetHolder(familyID)
	if found && grant.Holder != exited.ID() {
		return
	}
	if endpoint, live, _ := linuxcompat.NetSession(familyID); found && live {
		failSocketSession(familyID, endpoint, socketFailedHolder)
	}
	// The family's last exit forgets its context before this runs; its
	// pools go then too.
	releaseLinuxPools(familyID)
}

// resolveLinuxNet checks the two session handles a process holds and
// returns the grant to install: a channel with Read, Write, and Wait whose
// other side is open, and a pool of linuxsockapi.PoolPages pages with Read
// and Write.
func resolveLinuxNet(holder *process.Process, channelHandle handle.ID, poolHandle handle.ID) (linuxcompat.NetGrant, bool) {
	channelCap, channelOK := holder.Handles().Lookup(channelHandle, handle.TypeChannel, capability.Read|capability.Write|capability.Wait)
	poolCap, poolOK := holder.Handles().Lookup(poolHandle, handle.TypeMemory, capability.Read|capability.Write)
	if !channelOK || !poolOK {
		return linuxcompat.NetGrant{}, false
	}
	channelID, side, valid := ipc.DecodeEndpointObject(channelCap.Object())
	if !valid || !channelSideOpen(channelID, side) || !channelSideOpen(channelID, side^1) {
		return linuxcompat.NetGrant{}, false
	}
	pool, found := sharedObjects.Find(poolCap.Object())
	if !found || pool.PageCount() != linuxsockapi.PoolPages {
		return linuxcompat.NetGrant{}, false
	}
	return linuxcompat.NetGrant{Holder: holder.ID(), ChannelHandle: channelHandle, PoolHandle: poolHandle,
		Channel: channelID, Side: side, PoolObject: pool.ID()}, true
}

// attachLaunchNet installs the session a Linux launch carries as its
// linux-net and linux-net-pool roles. A launch without them has none.
func attachLaunchNet(spawned *process.Process, root uint64, channelHandle handle.ID, poolHandle handle.ID) bool {
	if channelHandle == 0 && poolHandle == 0 {
		return true
	}
	state, found := linuxcompat.FindProcess(root)
	grant, valid := resolveLinuxNet(spawned, channelHandle, poolHandle)
	if !found || !valid {
		return false
	}
	if !cacheLinuxPool(grant.PoolObject, state.FamilyID) {
		return false
	}
	_, _, attached := linuxcompat.AttachNet(state.FamilyID, grant, false)
	return attached
}

// closeLinuxNetHandles closes a replaced session's handles in its holder's
// table: the channel side, which tells the old service the family left,
// and the pool's reference.
func closeLinuxNetHandles(holder *process.Process, old linuxcompat.NetGrant) {
	if typeID, authority, found := holder.Handles().Describe(old.ChannelHandle); found && typeID == handle.TypeChannel {
		if channel, side, valid := ipc.DecodeEndpointObject(authority.Object()); valid {
			if closeChannelSide(channel, side) {
				wakeIPCWaiters(channel, side^1, ipc.WaitPeerDied)
			}
			wakeIPCWaiters(channel, side, ipc.WaitPeerDied)
		}
		holder.Handles().Close(old.ChannelHandle)
	}
	if typeID, authority, found := holder.Handles().Describe(old.PoolHandle); found && typeID == handle.TypeMemory && holder.Handles().Close(old.PoolHandle) {
		releaseMemory(authority)
	}
}

// socketWakeEvents is what a drain publishes for a socket that changed:
// every event a socket wait names, since the drain does not tell them
// apart.
const socketWakeEvents = kernelwait.Readable | kernelwait.Writable | kernelwait.PeerClosed

// socketWakeEventsOf maps what a change made a socket ready for to the
// events a socket wait names.
func socketWakeEventsOf(events linuxcompat.WakeEvents) kernelwait.Event {
	var out kernelwait.Event
	if events&linuxcompat.WakeReadable != 0 {
		out |= kernelwait.Readable
	}
	if events&linuxcompat.WakeWritable != 0 {
		out |= kernelwait.Writable
	}
	if events&linuxcompat.WakeClosed != 0 {
		out |= kernelwait.PeerClosed
	}
	return out
}

// socketWaitKey is the wait key of the socket a description Object names.
func socketWaitKey(object uint64) kernelwait.Key {
	return kernelwait.Key{Kind: kernelwait.KindLinuxSocket, Object: object}
}

// socketWaitEvents is the wait events for a call that wants want: errors
// and hangups always end the wait.
func socketWaitEvents(want linuxsockapi.Ready) kernelwait.Event {
	events := kernelwait.PeerClosed
	if want&linuxsockapi.ReadyIn != 0 {
		events |= kernelwait.Readable
	}
	if want&linuxsockapi.ReadyOut != 0 {
		events |= kernelwait.Writable
	}
	return events
}

// sessionCloseEntry wakes a socket wait when the service's side of the
// session closes: the service died, and nobody sends a ReadyChanged again.
func sessionCloseEntry(endpoint linuxcompat.NetEndpoint) kernelwait.Entry {
	return kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindChannel, Object: endpoint.Channel, Side: endpoint.Side},
		Events: kernelwait.PeerClosed}
}

// publishSocketWakes wakes the calls that wait on the sockets objects name.
func publishSocketWakes(objects []uint64) {
	for index := range objects {
		publishWaitAny(socketWaitKey(objects[index]), socketWakeEvents)
		objects[index] = 0
	}
}

// wakeChangedSockets runs the wake scan after a drain that took a
// ReadyChanged or a ring entry.
func wakeChangedSockets(familyID uint64, endpoint linuxcompat.NetEndpoint) {
	if !linuxcompat.TakeRescan(familyID) {
		return
	}
	scratch := &thisCPU().linuxSocket
	var pages linuxcompat.ReadinessView
	for index := range pages {
		pages[index], _ = socketPoolPage(endpoint, linuxsockapi.ReadinessPage+uint32(index))
	}
	for cursor := 0; cursor >= 0; {
		var count int
		count, cursor = linuxcompat.ScanSocketWakes(familyID, endpoint.Session, &pages, cursor, scratch.changed[:])
		for index := range scratch.changed[:count] {
			wake := &scratch.changed[index]
			publishWaitAny(socketWaitKey(wake.Object), socketWakeEventsOf(wake.Events))
			*wake = linuxcompat.SocketWake{}
		}
	}
}

// dispatchLinuxNet drains the session whose kernel side is channel's side,
// in the send of the service that just queued a message on it. It reports
// false for a channel that carries no live session.
func dispatchLinuxNet(channel uint64, side uint8) bool {
	familyID, endpoint, found := linuxcompat.NetFamilyOfChannel(channel, side)
	if !found {
		return false
	}
	pumpSocketSession(familyID, endpoint, 0)
	return true
}
