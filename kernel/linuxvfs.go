//go:build uefi

package main

// The Linux VFS bridge. A Linux file operation becomes one or more requests
// on the channel PID 1 granted the process at launch, each followed by a
// blocked wait for the reply with the same correlation. The bridge holds no
// filesystem authority of its own: a process without that channel handle, or
// without the rights on it, reaches no file. linuxfiles.go holds the calls
// built on it.

import (
	"goos/internal/capability"
	"goos/internal/limits"
	"goos/kernel/arch/amd64"
	"goos/kernel/clock"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/sched"
	"goos/kernel/shmem"
	"goos/kernel/vfs"
)

// linuxVFSReplyTicks bounds one wait for the VFS, matching what a native
// client allows its service before it gives up.
const linuxVFSReplyTicks = 8 * clock.TicksPerSecond

// vfsEndpoint resolves the session channel through the process's own handle
// table, so the rights PID 1 granted are checked on every exchange.
func (handler *linuxCallHandler) vfsEndpoint(state *linuxcompat.ProcessState) (*process.Process, *ipc.Channel, uint64, uint8, bool) {
	owner, found := handler.self()
	context, contextFound := linuxcompat.VFSContext(state.FamilyID)
	if !found || !contextFound || context.Session.Failed() {
		return nil, nil, 0, 0, false
	}
	authority, allowed := owner.Handles().Lookup(state.VFSHandle, handle.TypeChannel, capability.Read|capability.Write|capability.Wait)
	channelID, side, valid := ipc.DecodeEndpointObject(authority.Object())
	channel, exists := channelRegistry.Lookup(channelID)
	if !allowed || !valid || !exists {
		return nil, nil, 0, 0, false
	}
	return owner, channel, channelID, side, true
}

// vfsStage reports where the current call stands: inactive on a first entry,
// or the continuation of a call that already began. A retry must carry the
// registers the call began with.
func (handler *linuxCallHandler) vfsStage(request linuxcompat.Request) (linuxcompat.Pending, bool, linuxabi.Errno) {
	pending, active := linuxcompat.SyscallContinuations.Pending(handler.task)
	if !active {
		return pending, false, 0
	}
	if pending.Number != request.Number || pending.Arguments != request.Arguments || pending.Phase != linuxcompat.ContinuationReady {
		handler.vfsAbandon()
		return pending, false, linuxabi.EINVAL
	}
	return pending, true, 0
}

// vfsBegin starts the continuation of a call that uses the bridge. It keeps
// copies of the resolved paths and one note, so later stages read neither
// user memory nor scratch buffers another call may have reused.
func (handler *linuxCallHandler) vfsBegin(request linuxcompat.Request, path []byte, target []byte, note uint64) linuxabi.Errno {
	if !linuxcompat.SyscallContinuations.Begin(handler.task, request, path) ||
		(len(target) != 0 || note != 0) && !linuxcompat.SyscallContinuations.Annotate(handler.task, target, note) {
		handler.vfsAbandon()
		return linuxabi.EIO
	}
	return 0
}

func (handler *linuxCallHandler) vfsAbandon() {
	next := linuxcompat.CancelVFSTask(handler.task)
	linuxcompat.SyscallContinuations.Cancel(handler.task)
	ipcWaitSet.Cancel(handler.task)
	wakeVFSContextWaiter(next)
}

// vfsNext commits a finished stage and names the one that follows.
func (handler *linuxCallHandler) vfsNext(stage uint8, next uint8, partial uint64) linuxabi.Errno {
	if !linuxcompat.SyscallContinuations.Advance(handler.task, stage, next, partial) {
		return handler.vfsFailed()
	}
	return 0
}

func (handler *linuxCallHandler) vfsFinish() {
	state, found := linuxcompat.FindProcess(handler.root)
	if !linuxcompat.SyscallContinuations.Finish(handler.task) || !found {
		return
	}
	next, released := linuxcompat.ReleaseVFS(state.FamilyID, handler.task)
	if released {
		wakeVFSContextWaiter(next)
	}
}

// vfsAttachment says which memory object travels with a request.
type vfsAttachment uint8

const (
	// attachPaths sends the paths in an object when they do not fit the
	// message, and nothing otherwise.
	attachPaths vfsAttachment = iota
	// attachSource lends the I/O buffer for the VFS to read: a file write.
	attachSource
	// attachSink lends the I/O buffer for the VFS to fill: a file read.
	attachSink
	// attachName lends the I/O buffer for a listed name too long for a
	// reply message. It also carries the request's path when that is too
	// long for the request message, as a native client's path page does.
	attachName
	// attachExecutable lends the executable snapshot being filled.
	attachExecutable
)

// vfsExchange runs the exchange of the current stage. It sends operation
// unless this stage already sent it, then returns the reply or blocks the
// task. blocked means the caller returns the block action and the syscall is
// retried after the wake. A returned errno has already ended the call.
func (handler *linuxCallHandler) vfsExchange(operation vfs.Request, attachment vfsAttachment) (vfs.Reply, linuxabi.Errno, bool) {
	pending, active := linuxcompat.SyscallContinuations.Pending(handler.task)
	if !active {
		return vfs.Reply{}, linuxabi.EIO, false
	}
	state, found := linuxcompat.FindProcess(handler.root)
	if !found {
		return vfs.Reply{}, handler.vfsFailed(), false
	}
	switch linuxcompat.AcquireVFS(state.FamilyID, handler.task) {
	case linuxcompat.VFSAccessQueued:
		if !linuxcompat.SyscallContinuations.BlockContext(handler.task, pending.Stage) {
			return vfs.Reply{}, handler.vfsFailed(), false
		}
		return vfs.Reply{}, 0, true
	case linuxcompat.VFSAccessAcquired:
	default:
		return vfs.Reply{}, handler.vfsFailed(), false
	}
	if !pending.Issued {
		if handler.vfsWantsPeer(state, operation, attachment) {
			// Nothing is sent and the continuation stays ready at this
			// stage; the dispatcher enters the call again holding the VFS
			// service's handle table, and the call resumes here.
			return vfs.Reply{}, 0, true
		}
		if errno := handler.vfsIssue(pending.Stage, operation, attachment); errno != 0 {
			return vfs.Reply{}, errno, false
		}
	}
	return handler.vfsReply(operation.Operation)
}

// vfsWantsPeer reports whether a request that moves a handle to the VFS
// service needs that service's handle table, which the call does not hold,
// and asks the dispatcher for it.
func (handler *linuxCallHandler) vfsWantsPeer(state *linuxcompat.ProcessState, operation vfs.Request, attachment vfsAttachment) bool {
	if attachment == attachPaths && vfs.FitsInline(operation) {
		return false
	}
	_, _, channelID, side, reachable := handler.vfsEndpoint(state)
	if !reachable {
		return false
	}
	peerObject, _ := ipc.EndpointObject(channelID, side^1)
	peer, found := processRegistry.EndpointHolder(peerObject)
	local := thisCPU()
	if !found || local.linuxCall.holdsPeer(peer) {
		return false
	}
	local.peerWanted = peer.ID()
	return true
}

// vfsIssue sends one request. The continuation is marked issued before the
// message is queued, so a retried entry cannot send the request twice.
//
// It and vfsReply stay out of line: each builds message-sized values, and
// merged into one caller their frames would stack on the task's kernel stack
// instead of following one another.
//
//go:noinline
func (handler *linuxCallHandler) vfsIssue(stage uint8, operation vfs.Request, attachment vfsAttachment) linuxabi.Errno {
	local := thisCPU()
	state, found := linuxcompat.FindProcess(handler.root)
	if !found {
		return handler.vfsFailed()
	}
	owner, channel, channelID, side, reachable := handler.vfsEndpoint(state)
	context, contextFound := linuxcompat.VFSContext(state.FamilyID)
	if !contextFound {
		return handler.vfsFailed()
	}
	needsPeer := attachment != attachPaths || !vfs.FitsInline(operation)
	correlation, fresh := context.Session.Next()
	if !reachable || !fresh {
		return handler.vfsFailed()
	}
	var transfers [1]ipc.Transfer
	var peerHandles *handle.Table
	attached := 0
	if needsPeer {
		peerObject, _ := ipc.EndpointObject(channelID, side^1)
		peer, _ := processRegistry.EndpointHolder(peerObject)
		if peer == nil {
			context.Session.Fail()
			return handler.vfsFailed()
		}

		rights := capability.Read
		var object handle.ID
		var granted bool
		switch attachment {
		case attachPaths:
			object, granted = attachPath(owner, operation.Path, operation.Target)
			operation.PathsAttached = true
		case attachSink:
			rights |= capability.Write
			object, granted = lendIOBuffer(handler.task, state, owner, rights)
		case attachName:
			rights |= capability.Write
			object, granted = lendIOBuffer(handler.task, state, owner, rights)
			if buffer, found := ioBuffer(handler.task, state, owner); granted && found && !vfs.FitsInline(operation) {
				copyToObject(buffer, 0, operation.Path)
				operation.PathsAttached = true
			}
		case attachExecutable:
			rights = capability.Write
			object, granted = lendLinuxExecObject(handler.task, owner, rights)
		default:
			object, granted = lendIOBuffer(handler.task, state, owner, rights)
		}
		if !granted {
			handler.vfsAbandon()
			return linuxabi.ENOMEM
		}
		transfers[0] = ipc.Transfer{Handle: object, Type: handle.TypeMemory, Rights: rights}
		peerHandles, attached = peer.Handles(), 1
	}
	var encoded bool
	local.linuxVFSMessage, encoded = vfs.EncodeRequest(operation, correlation)
	if !encoded || !linuxcompat.SyscallContinuations.Issue(handler.task, stage, correlation) {
		handler.vfsUnsent(transfers[:attached])
		return linuxabi.EIO
	}
	if status := channel.Send(side, local.linuxVFSMessage, owner.Handles(), peerHandles, transfers[:attached]); status != ipc.StatusOK {
		if status == ipc.StatusPeerDied {
			context.Session.Fail()
		}
		handler.vfsUnsent(transfers[:attached])
		linuxcompat.NoteVFSFailure()
		return linuxabi.EIO
	}
	// The VFS now holds the attached handle; this side's copy goes without
	// dropping the reference that moved.
	for index := 0; index < attached; index++ {
		closeMovedHandle(owner.Handles(), transfers[index].Handle)
	}
	linuxcompat.NoteVFSRequest()
	wakeIPCWaiters(channelID, side^1, ipc.WaitMessage)
	return 0
}

// vfsUnsent undoes a request that never reached the queue: the continuation
// goes, and a handle nobody received gives up its reference.
func (handler *linuxCallHandler) vfsUnsent(transfers []ipc.Transfer) {
	handler.vfsAbandon()
	for index := range transfers {
		nativeCallHandler{root: handler.root}.HandleClose(uint64(transfers[index].Handle))
	}
}

// attachPath puts a request's paths, one after the other, in a memory object
// charged to owner and returns a handle the request can move to the VFS. The
// object lives until the VFS closes that handle, and its charge returns to
// owner then.
func attachPath(owner *process.Process, path []byte, target []byte) (handle.ID, bool) {
	pages := uint32((uint64(len(path)+len(target)) + vm.PageSize - 1) / vm.PageSize)
	charge := pages + 1 // the pages and their frame list
	if pages == 0 || pages > 2 || owner.Handles().Available() == 0 || !owner.Charge(process.ResourcePages, charge) {
		return 0, false
	}
	object, created := sharedObjects.Create(&pageAllocator, vm.PhysicalPage, zeroPhysicalPage, pages, uint64(owner.ID()))
	if !created {
		owner.Release(process.ResourcePages, charge)
		return 0, false
	}
	if copyToObject(object, 0, path) && copyToObject(object, uint64(len(path)), target) {
		if granted, ok := object.Grant(owner.Handles(), capability.Read|capability.Transfer); ok {
			return granted, true
		}
	}
	sharedObjects.Drop(object, &pageAllocator, vm.PhysicalPage)
	owner.Release(process.ResourcePages, charge)
	return 0, false
}

// copyToObject writes kernel bytes into a memory object at offset.
func copyToObject(object *shmem.Object, offset uint64, data []byte) bool {
	for len(data) != 0 {
		frame, found := object.Frame(vm.PhysicalPage, uint32(offset/vm.PageSize))
		page, mapped := vm.PhysicalPage(frame)
		if !found || !mapped {
			return false
		}
		copied := copy(page[offset%vm.PageSize:], data)
		data, offset = data[copied:], offset+uint64(copied)
	}
	return true
}

// linuxIOPages sizes the memory object a Linux process moves file bytes
// through. One read or write transfers at most this much; a longer request
// is a short transfer, which the program continues.
const (
	linuxIOPages = 16
	linuxIOBytes = linuxIOPages * vm.PageSize
)

// ioBuffer returns the I/O object of the calling thread, creating it on first
// use. The process pays for it and holds its handle in its own table, where a
// Linux program cannot name it. Each thread has its own: one thread's file
// read must not see the bytes another thread's write gathered. The first
// thread's goes with the process's other handles at exit; another thread's
// goes when the thread ends (releaseThreadIOBuffer).
func ioBuffer(task sched.TaskID, state *linuxcompat.ProcessState, owner *process.Process) (*shmem.Object, bool) {
	slot := &state.IOBuffer
	if thread, isThread := linuxcompat.FindThread(task); isThread {
		slot = &thread.IOBuffer
	}
	if *slot == 0 {
		const charge = linuxIOPages + 1
		if owner.Handles().Available() == 0 || !owner.Charge(process.ResourcePages, charge) {
			return nil, false
		}
		object, created := sharedObjects.Create(&pageAllocator, vm.PhysicalPage, zeroPhysicalPage, linuxIOPages, uint64(owner.ID()))
		if !created {
			owner.Release(process.ResourcePages, charge)
			return nil, false
		}
		granted, ok := object.Grant(owner.Handles(), capability.Read|capability.Write|capability.Transfer)
		if !ok {
			sharedObjects.Drop(object, &pageAllocator, vm.PhysicalPage)
			owner.Release(process.ResourcePages, charge)
			return nil, false
		}
		*slot = granted
	}
	authority, allowed := owner.Handles().Lookup(*slot, handle.TypeMemory, capability.Read|capability.Write|capability.Transfer)
	if !allowed {
		return nil, false
	}
	return sharedObjects.Find(authority.Object())
}

// lendIOBuffer makes a second handle to the I/O object with only the rights
// one request needs. The request moves it to the VFS, which maps the object
// for that request and closes the handle.
func lendIOBuffer(task sched.TaskID, state *linuxcompat.ProcessState, owner *process.Process, rights capability.Rights) (handle.ID, bool) {
	object, found := ioBuffer(task, state, owner)
	if !found {
		return 0, false
	}
	return object.Grant(owner.Handles(), rights|capability.Transfer)
}

// releaseThreadIOBuffer closes the I/O object of a thread that ended and
// returns its pages to the process quota.
func releaseThreadIOBuffer(owner *process.Process, id handle.ID) {
	if id == 0 {
		return
	}
	_, authority, found := owner.Handles().Describe(id)
	if !found || !owner.Handles().Close(id) {
		return
	}
	releaseMemory(authority)
	owner.Release(process.ResourcePages, linuxIOPages+1)
}

// ioTransfer moves length bytes between the I/O object at offset and user
// memory at address, a page of the object at a time.
func (handler *linuxCallHandler) ioTransfer(object *shmem.Object, offset uint64, address uint64, length uint64, toUser bool) bool {
	for moved := uint64(0); moved < length; {
		position := offset + moved
		frame, found := object.Frame(vm.PhysicalPage, uint32(position/vm.PageSize))
		page, mapped := vm.PhysicalPage(frame)
		if !found || !mapped {
			return false
		}
		chunk := page[position%vm.PageSize:]
		if uint64(len(chunk)) > length-moved {
			chunk = chunk[:length-moved]
		}
		if toUser && !vm.CopyOut(handler.root, address+moved, chunk) || !toUser && !vm.CopyIn(handler.root, address+moved, chunk) {
			return false
		}
		moved += uint64(len(chunk))
	}
	return true
}

// vfsReply returns the reply to the request in flight, or blocks the task
// until one arrives. An accepted reply leaves the continuation in place: the
// caller advances it to the next stage or finishes it.
//
//go:noinline
func (handler *linuxCallHandler) vfsReply(operation vfs.Operation) (reply vfs.Reply, errno linuxabi.Errno, blocked bool) {
	local := thisCPU()
	state, found := linuxcompat.FindProcess(handler.root)
	pending, active := linuxcompat.SyscallContinuations.Pending(handler.task)
	if !found || !active {
		handler.vfsAbandon()
		return vfs.Reply{}, linuxabi.EIO, false
	}
	_, channel, channelID, side, reachable := handler.vfsEndpoint(state)
	context, contextFound := linuxcompat.VFSContext(state.FamilyID)
	if !reachable || !contextFound {
		return vfs.Reply{}, handler.vfsFailed(), false
	}
	// At most a full queue of replies to abandoned requests precedes this one.
	for drained := 0; drained <= ipc.QueueCapacity; drained++ {
		var status ipc.Status
		local.linuxVFSMessage, status = channel.Receive(side)
		if status == ipc.StatusNotFound {
			break
		}
		if status != ipc.StatusOK {
			// The peer died with nothing queued. The session stays dead.
			context.Session.Fail()
			return vfs.Reply{}, handler.vfsFailed(), false
		}
		handler.dropAttachedHandles()
		decoded, verdict := linuxcompat.ClassifyVFSReply(&local.linuxVFSMessage, pending.Correlation, operation)
		switch verdict {
		case linuxcompat.VFSReplyAccepted:
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
		!linuxcompat.SyscallContinuations.Block(handler.task, pending.Stage) {
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
	local := thisCPU()
	for index := 0; index < int(local.linuxVFSMessage.HandleCount) && index < len(local.linuxVFSMessage.Handles); index++ {
		nativeCallHandler{root: handler.root}.HandleClose(uint64(local.linuxVFSMessage.Handles[index]))
	}
}

// resumeLinuxIPCWaiter routes a channel wake or an expired deadline to the
// continuation of a Linux task. It reports false for a native task, which
// the caller wakes itself.
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

func wakeVFSContextWaiter(task sched.TaskID) {
	for task != 0 {
		if linuxcompat.SyscallContinuations.ResumeContext(task) && amd64.WakeTaskID(uint64(task)) {
			return
		}
		linuxcompat.SyscallContinuations.Cancel(task)
		task = linuxcompat.CancelVFSTask(task)
	}
}

// copyPath copies a NUL-terminated user path. Linux bounds a path at PATH_MAX
// bytes with its terminator, so a string with no terminator in that span is
// ENAMETOOLONG. The copy goes page by page, so a short string at the end of a
// mapping is not refused for the unmapped page after it.
func (handler *linuxCallHandler) copyPath(address uint64) ([]byte, linuxabi.Errno) {
	local := thisCPU()
	length := 0
	for length < len(local.linuxPath) {
		chunk := int(vm.PageSize - (address+uint64(length))%vm.PageSize)
		if chunk > len(local.linuxPath)-length {
			chunk = len(local.linuxPath) - length
		}
		if !vm.CopyIn(handler.root, address+uint64(length), local.linuxPath[length:length+chunk]) {
			return nil, linuxabi.EFAULT
		}
		for index := length; index < length+chunk; index++ {
			if local.linuxPath[index] == 0 {
				return local.linuxPath[:index], 0
			}
		}
		length += chunk
	}
	return nil, linuxabi.ENAMETOOLONG
}

// resolvePath copies the path at address and resolves it into target in the
// GoOS VFS namespace. An absolute path ignores dirfd. A relative one starts
// at the working directory for AT_FDCWD and at the directory a descriptor
// names otherwise: ENOTDIR when the descriptor is not a directory, EBADF when
// it is not open.
func (handler *linuxCallHandler) resolvePath(state *linuxcompat.ProcessState, dirfd int32, address uint64, target *[limits.PathMax]byte) ([]byte, bool, linuxabi.Errno) {
	path, errno := handler.copyPath(address)
	if errno != 0 {
		return nil, false, errno
	}
	base := state.CWD()
	if len(path) != 0 && path[0] != '/' && dirfd != linuxabi.AtFDCWD {
		description, open := state.FDs.Lookup(int(dirfd))
		if !open {
			return nil, false, linuxabi.EBADF
		}
		var named bool
		if base, named = linuxcompat.DirectoryPath(state.FamilyID, description.Object); !named || description.Kind != linuxcompat.FDVFSDirectory {
			return nil, false, linuxabi.ENOTDIR
		}
	}
	size, directory, errno := linuxcompat.ResolvePath(target[:], base, path)
	if errno != 0 {
		return nil, false, errno
	}
	return target[:size], directory, 0
}
