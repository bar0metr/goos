//go:build uefi

package main

// The Linux VFS bridge. A Linux file operation becomes one request on the
// channel PID 1 granted the process at launch, then a blocked wait for the
// reply with the same correlation. The bridge holds no filesystem authority
// of its own: a process without that channel handle, or without the rights on
// it, reaches no file.

import (
	"goos/internal/capability"
	"goos/kernel/arch/amd64"
	"goos/kernel/clock"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/mm/vm"
	"goos/kernel/sched"
	"goos/kernel/vfs"
)

// linuxVFSReplyTicks bounds one wait for the VFS, matching what a native
// client allows its service before it gives up.
const linuxVFSReplyTicks = 8 * clock.TicksPerSecond

var (
	// linuxVFSMessage holds the request being sent, then the reply being read.
	// A decoded reply aliases it until the handler returns.
	linuxVFSMessage ipc.Message
	linuxPath       [linuxcompat.VFSPathInline + 1]byte
)

// vfsEndpoint resolves the session channel through the process's own handle
// table, so the rights PID 1 granted are checked on every exchange.
func (handler *linuxCallHandler) vfsEndpoint(state *linuxcompat.ProcessState) (*ipc.Channel, uint64, uint8, bool) {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found || state.VFS.Failed() {
		return nil, 0, 0, false
	}
	authority, allowed := owner.Handles().Lookup(state.VFS.Handle, handle.TypeChannel, capability.Read|capability.Write|capability.Wait)
	channelID, side, valid := ipc.DecodeEndpointObject(authority.Object())
	channel, exists := channelRegistry.Lookup(channelID)
	if !allowed || !valid || !exists {
		return nil, 0, 0, false
	}
	return channel, channelID, side, true
}

// vfsResumed reports whether this entry retries a call that already sent its
// request. A retry must carry the registers the request was issued with.
func (handler *linuxCallHandler) vfsResumed(request linuxcompat.Request) (bool, linuxabi.Errno) {
	pending, active := linuxcompat.SyscallContinuations.Pending(handler.task)
	if !active {
		return false, 0
	}
	if pending.Number != request.Number || pending.Arguments != request.Arguments || pending.Phase != linuxcompat.ContinuationReady || !pending.Issued {
		handler.vfsAbandon()
		return false, linuxabi.EINVAL
	}
	return true, 0
}

func (handler *linuxCallHandler) vfsAbandon() {
	linuxcompat.SyscallContinuations.Cancel(handler.task)
	ipcWaitSet.Cancel(handler.task)
}

// vfsSend issues one request and records the continuation its reply must
// match. The continuation is marked issued before the message is queued, so a
// retried entry cannot send the request twice.
func (handler *linuxCallHandler) vfsSend(request linuxcompat.Request, operation vfs.Request) linuxabi.Errno {
	state, found := linuxcompat.FindProcess(handler.root)
	if !found {
		return linuxabi.EIO
	}
	channel, channelID, side, reachable := handler.vfsEndpoint(state)
	correlation, fresh := state.VFS.Next()
	if !reachable || !fresh {
		linuxcompat.NoteVFSFailure()
		return linuxabi.EIO
	}
	var encoded bool
	linuxVFSMessage, encoded = vfs.EncodeRequest(operation, correlation)
	if !encoded {
		return linuxabi.EINVAL
	}
	if !linuxcompat.SyscallContinuations.Begin(handler.task, request, nil) ||
		!linuxcompat.SyscallContinuations.Issue(handler.task, 1, correlation) {
		handler.vfsAbandon()
		return linuxabi.EIO
	}
	if status := channel.Send(side, linuxVFSMessage, nil, nil, nil); status != ipc.StatusOK {
		if status == ipc.StatusPeerDied {
			state.VFS.Fail()
		}
		handler.vfsAbandon()
		linuxcompat.NoteVFSFailure()
		return linuxabi.EIO
	}
	linuxcompat.NoteVFSRequest()
	wakeIPCWaiters(channelID, side^1, ipc.WaitMessage)
	return 0
}

// vfsReply returns the reply to the request in flight, or blocks the task
// until one arrives. blocked means the caller returns the block action and
// the syscall is retried after the wake.
func (handler *linuxCallHandler) vfsReply(operation vfs.Operation) (reply vfs.Reply, errno linuxabi.Errno, blocked bool) {
	state, found := linuxcompat.FindProcess(handler.root)
	pending, active := linuxcompat.SyscallContinuations.Pending(handler.task)
	if !found || !active {
		handler.vfsAbandon()
		return vfs.Reply{}, linuxabi.EIO, false
	}
	channel, channelID, side, reachable := handler.vfsEndpoint(state)
	if !reachable {
		return vfs.Reply{}, handler.vfsFailed(), false
	}
	// At most a full queue of replies to abandoned requests precedes this one.
	for drained := 0; drained <= ipc.QueueCapacity; drained++ {
		var status ipc.Status
		linuxVFSMessage, status = channel.Receive(side)
		if status == ipc.StatusNotFound {
			break
		}
		if status != ipc.StatusOK {
			// The peer died with nothing queued. The session stays dead.
			state.VFS.Fail()
			return vfs.Reply{}, handler.vfsFailed(), false
		}
		handler.dropAttachedHandles()
		decoded, verdict := linuxcompat.ClassifyVFSReply(&linuxVFSMessage, pending.Correlation, operation)
		switch verdict {
		case linuxcompat.VFSReplyAccepted:
			linuxcompat.SyscallContinuations.Finish(handler.task)
			return decoded, 0, false
		case linuxcompat.VFSReplyStale:
			linuxcompat.NoteVFSStale()
		default:
			return vfs.Reply{}, handler.vfsFailed(), false
		}
	}
	if pending.Wake == linuxcompat.WakeTimeout {
		handler.vfsAbandon()
		linuxcompat.NoteVFSFailure()
		return vfs.Reply{}, linuxabi.ETIMEDOUT, false
	}
	now := amd64.TimerTicks()
	if !ipcWaitSet.Add(handler.task, channelID, side, now+linuxVFSReplyTicks) ||
		!linuxcompat.SyscallContinuations.Block(handler.task, 1) {
		return vfs.Reply{}, handler.vfsFailed(), false
	}
	return vfs.Reply{}, 0, true
}

func (handler *linuxCallHandler) vfsFailed() linuxabi.Errno {
	handler.vfsAbandon()
	linuxcompat.NoteVFSFailure()
	return linuxabi.EIO
}

// dropAttachedHandles closes handles a message carried into the process. No
// reply the bridge accepts attaches any, and a Linux process must not keep
// native authority PID 1 never granted it.
func (handler *linuxCallHandler) dropAttachedHandles() {
	for index := 0; index < int(linuxVFSMessage.HandleCount) && index < len(linuxVFSMessage.Handles); index++ {
		nativeCallHandler{root: handler.root}.HandleClose(uint64(linuxVFSMessage.Handles[index]))
	}
}

// resumeLinuxIPCWaiter routes a channel wake to the continuation of a Linux
// task. It reports false for a native task, which the caller wakes itself.
func resumeLinuxIPCWaiter(task sched.TaskID, reason linuxcompat.WakeReason) bool {
	pending, found := linuxcompat.SyscallContinuations.Pending(task)
	if !found {
		return false
	}
	if !resumeLinuxSyscall(task, pending.Correlation, reason) {
		cancelTaskWaits(task)
		amd64.TerminateTask(uint64(task), uint64(1)<<63|5)
	}
	return true
}

// copyPath copies a NUL-terminated user path of at most VFSPathInline bytes.
// It reads page by page, so a short string at the end of a mapping is not
// refused for the unmapped page after it.
func (handler *linuxCallHandler) copyPath(address uint64) ([]byte, linuxabi.Errno) {
	length := 0
	for length < len(linuxPath) {
		chunk := int(vm.PageSize - (address+uint64(length))%vm.PageSize)
		if chunk > len(linuxPath)-length {
			chunk = len(linuxPath) - length
		}
		if !vm.CopyIn(handler.root, address+uint64(length), linuxPath[length:length+chunk]) {
			return nil, linuxabi.EFAULT
		}
		for index := length; index < length+chunk; index++ {
			if linuxPath[index] == 0 {
				return linuxPath[:index], 0
			}
		}
		length += chunk
	}
	return nil, linuxabi.ENAMETOOLONG
}

func (handler *linuxCallHandler) Openat(request linuxcompat.Request) linuxcompat.Result {
	resumed, errno := handler.vfsResumed(request)
	if errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	rights, closeOnExec, errno := linuxcompat.OpenRights(request.Arguments[2])
	if errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	if !resumed {
		state, found := linuxcompat.FindProcess(handler.root)
		if !found {
			return linuxcompat.Result{Errno: linuxabi.EIO}
		}
		path, errno := handler.copyPath(request.Arguments[1])
		if errno == 0 {
			errno = linuxcompat.SessionPath(path)
		}
		if errno == 0 && !state.FDs.HasFree() {
			errno = linuxabi.EMFILE
		}
		if errno == 0 {
			errno = handler.vfsSend(request, vfs.Request{Operation: vfs.OperationOpen, Rights: rights, Path: path})
		}
		if errno != 0 {
			return linuxcompat.Result{Errno: errno}
		}
	}
	reply, errno, blocked := handler.vfsReply(vfs.OperationOpen)
	if blocked {
		return linuxcompat.Result{Action: linuxabi.ActionBlock}
	}
	if errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	if reply.Status != vfs.StatusOK {
		// GoOS has no per-file permissions, so a refused write is a read-only
		// mount. An exhausted open is the VFS open-file table.
		permission := linuxcompat.VFSPermissionAccess
		if rights&vfs.Write != 0 {
			permission = linuxcompat.VFSPermissionReadOnly
		}
		return linuxcompat.Result{Errno: linuxcompat.VFSErrno(reply.Status, permission, linuxabi.ENFILE)}
	}
	description := linuxcompat.FileDescription{Kind: linuxcompat.FDVFSFile, Flags: uint32(request.Arguments[2]) & linuxabi.OpenAccess, Object: uint64(reply.Handle)}
	fd, errno := linuxcompat.AllocateFD(handler.root, description, closeOnExec)
	if errno != 0 {
		return linuxcompat.Result{Errno: linuxabi.EIO}
	}
	return linuxcompat.Result{Value: uint64(fd)}
}

// Close releases the descriptor before anything can fail, as Linux does: the
// integer is free even when the release behind it reports an error.
func (handler *linuxCallHandler) Close(request linuxcompat.Request) linuxcompat.Result {
	resumed, errno := handler.vfsResumed(request)
	if errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	if !resumed {
		if request.Arguments[0] >= linuxcompat.LinuxMaxFD {
			return linuxcompat.Result{Errno: linuxabi.EBADF}
		}
		released, last, errno := linuxcompat.CloseFD(handler.root, int(request.Arguments[0]))
		if errno != 0 || !last {
			return linuxcompat.Result{Errno: errno}
		}
		switch released.Kind {
		case linuxcompat.FDStreamReader, linuxcompat.FDStreamWriter:
			// The native handle goes with the last descriptor, so the peer
			// sees end of input or a closed reader.
			nativeCallHandler{root: handler.root}.HandleClose(released.Object)
			return linuxcompat.Result{}
		case linuxcompat.FDVFSFile:
			if errno := handler.vfsSend(request, vfs.Request{Operation: vfs.OperationClose, Handle: vfs.Handle(released.Object)}); errno != 0 {
				return linuxcompat.Result{Errno: errno}
			}
		default:
			return linuxcompat.Result{}
		}
	}
	reply, errno, blocked := handler.vfsReply(vfs.OperationClose)
	if blocked {
		return linuxcompat.Result{Action: linuxabi.ActionBlock}
	}
	if errno == 0 && reply.Status != vfs.StatusOK {
		errno = linuxabi.EIO
	}
	return linuxcompat.Result{Errno: errno}
}
