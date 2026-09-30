//go:build uefi

package main

// Linux file calls on the VFS bridge: open, close, read, write, seek, the
// working directory, and the calls that create, remove, and rename names.
// Each handler is entered again after every wake, so it reads its progress
// from the continuation and keeps nothing in locals across an exchange.

import (
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/mm/vm"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
)

// Notes a call keeps for all its stages.
const (
	noteTrailing        = 1 << 0 // the path demanded a directory
	noteRemoveDirectory = 1 << 1 // rmdir, or unlinkat with AT_REMOVEDIR
	noteTargetTrailing  = 1 << 2 // the rename target demanded a directory
)

var linuxBlocked = linuxcompat.Result{Action: linuxabi.ActionBlock}

func linuxErrno(errno linuxabi.Errno) linuxcompat.Result {
	return linuxcompat.Result{Errno: errno}
}

// Open is the legacy form musl's open() uses on x86_64: openat from the
// working directory.
func (handler *linuxCallHandler) Open(request linuxcompat.Request) linuxcompat.Result {
	return handler.openPath(request, linuxabi.AtFDCWD, request.Arguments[0], request.Arguments[1])
}

func (handler *linuxCallHandler) Openat(request linuxcompat.Request) linuxcompat.Result {
	return handler.openPath(request, int32(request.Arguments[0]), request.Arguments[1], request.Arguments[2])
}

// openPath stats the resolved path, creates or empties the file when the
// flags ask for it, and opens it. A directory gets a descriptor that keeps
// its path; the VFS has no open file for one.
func (handler *linuxCallHandler) openPath(request linuxcompat.Request, dirfd int32, address uint64, flags uint64) linuxcompat.Result {
	_, active, errno := handler.vfsStage(request)
	if errno != 0 {
		return linuxErrno(errno)
	}
	intent, errno := linuxcompat.DecodeOpen(flags)
	state, found := linuxcompat.FindProcess(handler.root)
	if errno == 0 && !found {
		errno = linuxabi.EIO
	}
	if errno != 0 {
		return linuxErrno(errno)
	}
	if !active {
		path, trailing, errno := handler.resolvePath(state, dirfd, address, &linuxResolved)
		if errno == 0 && !state.FDs.HasFree() {
			errno = linuxabi.EMFILE
		}
		note := uint64(0)
		if trailing {
			note = noteTrailing
		}
		if errno == 0 {
			errno = handler.vfsBegin(request, path, nil, note)
		}
		if errno != 0 {
			return linuxErrno(errno)
		}
	}
	path, _ := linuxcompat.SyscallContinuations.Paths(handler.task)
	for {
		pending, _ := linuxcompat.SyscallContinuations.Pending(handler.task)
		switch pending.Stage {
		case linuxcompat.OpenStageStat:
			reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationStat, Path: path}, attachPaths)
			if blocked {
				return linuxBlocked
			}
			if errno != 0 {
				return linuxErrno(errno)
			}
			step, errno := linuxcompat.PlanOpen(intent, pending.Note&noteTrailing != 0, reply.Status, reply.Kind)
			next := linuxcompat.OpenStageOpen
			switch step {
			case linuxcompat.OpenFail:
				handler.vfsFinish()
				return linuxErrno(errno)
			case linuxcompat.OpenExistingDirectory:
				// The descriptor copies the path before the continuation
				// that holds it is released.
				result := handler.installDirectory(path, intent)
				handler.vfsFinish()
				return result
			case linuxcompat.OpenCreateFile:
				next = linuxcompat.OpenStageCreate
			case linuxcompat.OpenTruncateFile:
				next = linuxcompat.OpenStageTruncate
			}
			if errno := handler.vfsNext(pending.Stage, next, 0); errno != 0 {
				return linuxErrno(errno)
			}
		case linuxcompat.OpenStageCreate:
			reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationCreate, Path: path}, attachPaths)
			if blocked {
				return linuxBlocked
			}
			if errno != 0 {
				return linuxErrno(errno)
			}
			// Another process may have created the name since the stat. That
			// is what O_EXCL exists to refuse.
			raced := reply.Status == vfs.StatusAlreadyExists
			if reply.Status != vfs.StatusOK && !raced || raced && intent.Exclusive {
				handler.vfsFinish()
				return linuxErrno(linuxcompat.MutationErrno(reply.Status))
			}
			if errno := handler.vfsNext(pending.Stage, linuxcompat.OpenStageOpen, 0); errno != 0 {
				return linuxErrno(errno)
			}
		case linuxcompat.OpenStageTruncate:
			reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationTruncate, Path: path}, attachPaths)
			if blocked {
				return linuxBlocked
			}
			if errno != 0 {
				return linuxErrno(errno)
			}
			if reply.Status != vfs.StatusOK {
				handler.vfsFinish()
				return linuxErrno(linuxcompat.MutationErrno(reply.Status))
			}
			if errno := handler.vfsNext(pending.Stage, linuxcompat.OpenStageOpen, 0); errno != 0 {
				return linuxErrno(errno)
			}
		case linuxcompat.OpenStageOpen:
			reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationOpen, Rights: intent.Rights, Path: path}, attachPaths)
			if blocked {
				return linuxBlocked
			}
			if errno != 0 {
				return linuxErrno(errno)
			}
			handler.vfsFinish()
			if reply.Status != vfs.StatusOK {
				// GoOS has no per-file permissions, so a refused write open
				// is a read-only mount. An exhausted open is the VFS
				// open-file table.
				permission := linuxcompat.VFSPermissionAccess
				if intent.Rights&vfs.Write != 0 {
					permission = linuxcompat.VFSPermissionReadOnly
				}
				return linuxErrno(linuxcompat.VFSErrno(reply.Status, permission, linuxabi.ENFILE))
			}
			description := linuxcompat.FileDescription{Kind: linuxcompat.FDVFSFile, Flags: intent.StatusFlags(), Object: uint64(reply.Handle)}
			fd, errno := linuxcompat.AllocateFD(handler.root, description, intent.CloseOnExec)
			if errno != 0 {
				return linuxErrno(linuxabi.EIO)
			}
			return linuxcompat.Result{Value: uint64(fd)}
		default:
			return linuxErrno(handler.vfsFailed())
		}
	}
}

func (handler *linuxCallHandler) installDirectory(path []byte, intent linuxcompat.OpenIntent) linuxcompat.Result {
	reference, retained := linuxcompat.RetainDirectory(handler.root, path)
	if !retained {
		return linuxErrno(linuxabi.ENFILE)
	}
	description := linuxcompat.FileDescription{Kind: linuxcompat.FDVFSDirectory, Flags: intent.StatusFlags(), Object: reference}
	fd, errno := linuxcompat.AllocateFD(handler.root, description, intent.CloseOnExec)
	if errno != 0 {
		linuxcompat.ReleaseDirectory(handler.root, reference)
		return linuxErrno(errno)
	}
	return linuxcompat.Result{Value: uint64(fd)}
}

// Close releases the descriptor before anything can fail, as Linux does: the
// integer is free even when the release behind it reports an error.
func (handler *linuxCallHandler) Close(request linuxcompat.Request) linuxcompat.Result {
	pending, active, errno := handler.vfsStage(request)
	if errno != 0 {
		return linuxErrno(errno)
	}
	if !active {
		if request.Arguments[0] >= linuxcompat.LinuxMaxFD {
			return linuxErrno(linuxabi.EBADF)
		}
		released, last, errno := linuxcompat.CloseFD(handler.root, int(request.Arguments[0]))
		if errno != 0 || !last {
			return linuxErrno(errno)
		}
		switch released.Kind {
		case linuxcompat.FDStreamReader, linuxcompat.FDStreamWriter:
			// The native handle goes with the last descriptor, so the peer
			// sees end of input or a closed reader.
			nativeCallHandler{root: handler.root}.HandleClose(released.Object)
			return linuxcompat.Result{}
		case linuxcompat.FDVFSDirectory:
			linuxcompat.ReleaseDirectory(handler.root, released.Object)
			return linuxcompat.Result{}
		case linuxcompat.FDVFSFile:
			// The description is gone; the note carries its VFS handle to
			// the retry.
			if errno := handler.vfsBegin(request, nil, nil, released.Object); errno != 0 {
				return linuxErrno(errno)
			}
			pending.Note = released.Object
		default:
			return linuxcompat.Result{}
		}
	}
	reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationClose, Handle: vfs.Handle(pending.Note)}, attachPaths)
	if blocked {
		return linuxBlocked
	}
	if errno != 0 {
		return linuxErrno(errno)
	}
	handler.vfsFinish()
	if reply.Status != vfs.StatusOK {
		return linuxErrno(linuxabi.EIO)
	}
	return linuxcompat.Result{}
}

// readFile reads at the file position through the I/O buffer. The VFS fills
// the buffer and advances the position; the bytes reach user memory after
// the reply.
func (handler *linuxCallHandler) readFile(request linuxcompat.Request, description *linuxcompat.FileDescription) linuxcompat.Result {
	_, active, errno := handler.vfsStage(request)
	if errno != 0 {
		return linuxErrno(errno)
	}
	address, limit := request.Arguments[1], request.Arguments[2]
	if limit > linuxIOBytes {
		limit = linuxIOBytes
	}
	if !active {
		switch {
		case description.Flags&linuxabi.OpenAccess == linuxabi.OpenWriteOnly:
			return linuxErrno(linuxabi.EBADF)
		case limit == 0:
			return linuxcompat.Result{}
		case !vm.CanCopyOut(handler.root, address, limit):
			return linuxErrno(linuxabi.EFAULT)
		}
		if errno := handler.vfsBegin(request, nil, nil, 0); errno != 0 {
			return linuxErrno(errno)
		}
	}
	reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationReadInto, Handle: vfs.Handle(description.Object), Size: uint32(limit)}, attachSink)
	if blocked {
		return linuxBlocked
	}
	if errno != 0 {
		return linuxErrno(errno)
	}
	handler.vfsFinish()
	if reply.Status != vfs.StatusOK {
		return linuxErrno(linuxcompat.VFSErrno(reply.Status, linuxcompat.VFSPermissionAccess, linuxabi.ENOMEM))
	}
	state, _ := linuxcompat.FindProcess(handler.root)
	owner, owned := processRegistry.ByRoot(handler.root)
	if reply.Size > limit || state == nil || !owned {
		return linuxErrno(linuxabi.EIO)
	}
	object, found := ioBuffer(state, owner)
	if !found {
		return linuxErrno(linuxabi.EIO)
	}
	// The position moved in the VFS whether or not the copy succeeds.
	description.Offset += reply.Size
	if !handler.ioTransfer(object, 0, address, reply.Size, true) {
		return linuxErrno(linuxabi.EFAULT)
	}
	return linuxcompat.Result{Value: reply.Size}
}

// A write is one exchange, or three for a descriptor opened with O_APPEND:
// the file length, a seek to it, and the write.
const (
	writeStageLength = uint8(1)
	writeStageSeek   = uint8(2)
	writeStageData   = uint8(3)
)

// writeFile gathers the caller's bytes into the I/O buffer once, then has
// the VFS write them at the file position. vector selects writev's argument
// layout. A request longer than the buffer is a short write.
func (handler *linuxCallHandler) writeFile(request linuxcompat.Request, description *linuxcompat.FileDescription, vector bool) linuxcompat.Result {
	_, active, errno := handler.vfsStage(request)
	if errno != 0 {
		return linuxErrno(errno)
	}
	if !active {
		if description.Flags&linuxabi.OpenAccess == linuxabi.OpenReadOnly {
			return linuxErrno(linuxabi.EBADF)
		}
		total, errno := handler.gatherWrite(request, vector)
		if errno != 0 || total == 0 {
			return linuxErrno(errno)
		}
		if errno := handler.vfsBegin(request, nil, nil, total); errno != 0 {
			return linuxErrno(errno)
		}
	}
	file := vfs.Handle(description.Object)
	appending := description.Flags&linuxabi.OpenAppend != 0
	for {
		pending, _ := linuxcompat.SyscallContinuations.Pending(handler.task)
		switch {
		case appending && pending.Stage == writeStageLength:
			reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationStat, Handle: file}, attachPaths)
			if blocked {
				return linuxBlocked
			}
			if errno != 0 {
				return linuxErrno(errno)
			}
			if reply.Status != vfs.StatusOK {
				handler.vfsFinish()
				return linuxErrno(linuxcompat.VFSErrno(reply.Status, linuxcompat.VFSPermissionAccess, linuxabi.ENOSPC))
			}
			if errno := handler.vfsNext(writeStageLength, writeStageSeek, reply.Size); errno != 0 {
				return linuxErrno(errno)
			}
		case appending && pending.Stage == writeStageSeek:
			reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationSeek, Handle: file, Offset: pending.Partial}, attachPaths)
			if blocked {
				return linuxBlocked
			}
			if errno != 0 {
				return linuxErrno(errno)
			}
			if reply.Status != vfs.StatusOK {
				handler.vfsFinish()
				return linuxErrno(linuxabi.EIO)
			}
			if errno := handler.vfsNext(writeStageSeek, writeStageData, pending.Partial); errno != 0 {
				return linuxErrno(errno)
			}
		case appending && pending.Stage == writeStageData || !appending && pending.Stage == 1:
			reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationWriteFrom, Handle: file, Size: uint32(pending.Note)}, attachSource)
			if blocked {
				return linuxBlocked
			}
			if errno != 0 {
				return linuxErrno(errno)
			}
			handler.vfsFinish()
			if reply.Status != vfs.StatusOK {
				return linuxErrno(linuxcompat.MutationErrno(reply.Status))
			}
			if reply.Size > pending.Note {
				return linuxErrno(linuxabi.EIO)
			}
			if appending {
				description.Offset = pending.Partial
			}
			description.Offset += reply.Size
			return linuxcompat.Result{Value: reply.Size}
		default:
			return linuxErrno(handler.vfsFailed())
		}
	}
}

// gatherWrite copies a write's bytes, or a writev's elements in order, into
// the I/O buffer and returns how many it holds. A fault after the first byte
// shortens the write; a fault before it is EFAULT.
func (handler *linuxCallHandler) gatherWrite(request linuxcompat.Request, vector bool) (uint64, linuxabi.Errno) {
	state, found := linuxcompat.FindProcess(handler.root)
	owner, owned := processRegistry.ByRoot(handler.root)
	if !found || !owned {
		return 0, linuxabi.EIO
	}
	if !vector {
		address, length := request.Arguments[1], request.Arguments[2]
		if length > linuxIOBytes {
			length = linuxIOBytes
		}
		if length == 0 {
			return 0, 0
		}
		object, ready := ioBuffer(state, owner)
		if !ready {
			return 0, linuxabi.ENOMEM
		}
		if !vm.ValidateUserRange(address, length) || !handler.ioTransfer(object, 0, address, length, false) {
			return 0, linuxabi.EFAULT
		}
		return length, 0
	}
	count := request.Arguments[2]
	if count > linuxcompat.MaxIOVec {
		return 0, linuxabi.EINVAL
	}
	if count == 0 {
		return 0, 0
	}
	wire := linuxIOVecWire[:count*linuxabi.IOVecSize]
	if !vm.CopyIn(handler.root, request.Arguments[1], wire) {
		return 0, linuxabi.EFAULT
	}
	if errno := linuxcompat.ValidateIOVec(wire, vm.UserAddressLimit); errno != 0 {
		return 0, errno
	}
	object, ready := ioBuffer(state, owner)
	if !ready {
		return 0, linuxabi.ENOMEM
	}
	total := uint64(0)
	for index := 0; index < int(count) && total < linuxIOBytes; index++ {
		base, length := linuxcompat.IOVecEntry(wire, index)
		if length > linuxIOBytes-total {
			length = linuxIOBytes - total
		}
		if length == 0 {
			continue
		}
		if !handler.ioTransfer(object, total, base, length, false) {
			if total == 0 {
				return 0, linuxabi.EFAULT
			}
			break
		}
		total += length
	}
	return total, 0
}

// Filesystem routes the path and position calls, which share their decoding
// more than they differ.
func (handler *linuxCallHandler) Filesystem(request linuxcompat.Request) linuxcompat.Result {
	arguments := &request.Arguments
	switch request.Number {
	case linuxabi.SysLseek:
		return handler.seek(request)
	case linuxabi.SysGetcwd:
		return handler.getcwd(request)
	case linuxabi.SysChdir:
		return handler.chdir(request)
	case linuxabi.SysMkdir:
		return handler.mkdir(request, linuxabi.AtFDCWD, arguments[0])
	case linuxabi.SysMkdirat:
		return handler.mkdir(request, int32(arguments[0]), arguments[1])
	case linuxabi.SysUnlink:
		return handler.remove(request, linuxabi.AtFDCWD, arguments[0], false)
	case linuxabi.SysRmdir:
		return handler.remove(request, linuxabi.AtFDCWD, arguments[0], true)
	case linuxabi.SysUnlinkat:
		if arguments[2]&^linuxabi.AtRemoveDir != 0 {
			return linuxErrno(linuxabi.EINVAL)
		}
		return handler.remove(request, int32(arguments[0]), arguments[1], arguments[2] == linuxabi.AtRemoveDir)
	case linuxabi.SysRename:
		return handler.rename(request, linuxabi.AtFDCWD, arguments[0], linuxabi.AtFDCWD, arguments[1])
	case linuxabi.SysRenameat2:
		// RENAME_NOREPLACE, RENAME_EXCHANGE, and RENAME_WHITEOUT have no
		// VFS operation behind them.
		if arguments[4] != 0 {
			return linuxErrno(linuxabi.EINVAL)
		}
		fallthrough
	case linuxabi.SysRenameat:
		return handler.rename(request, int32(arguments[0]), arguments[1], int32(arguments[2]), arguments[3])
	}
	return linuxErrno(linuxabi.ENOSYS)
}

// A seek from the end is two exchanges: the file length, then the seek.
const (
	seekStageLength = uint8(1)
	seekStageSet    = uint8(2)
)

func (handler *linuxCallHandler) seek(request linuxcompat.Request) linuxcompat.Result {
	_, active, errno := handler.vfsStage(request)
	if errno != 0 {
		return linuxErrno(errno)
	}
	description, found := linuxcompat.DescriptionOf(handler.root, request.Arguments[0])
	if !found {
		return linuxErrno(linuxabi.EBADF)
	}
	offset, whence := int64(request.Arguments[1]), request.Arguments[2]
	switch description.Kind {
	case linuxcompat.FDVFSFile:
	case linuxcompat.FDVFSDirectory:
		// A directory position is a listing cursor the kernel keeps. It has
		// no length to seek from.
		if whence == linuxabi.SeekEnd {
			return linuxErrno(linuxabi.EINVAL)
		}
		target, errno := linuxcompat.SeekTarget(whence, offset, description.Offset, 0)
		if errno != 0 {
			return linuxErrno(errno)
		}
		description.Offset = target
		return linuxcompat.Result{Value: target}
	default:
		return linuxErrno(linuxabi.ESPIPE)
	}
	fromEnd := whence == linuxabi.SeekEnd
	if !active {
		if !fromEnd {
			if _, errno := linuxcompat.SeekTarget(whence, offset, description.Offset, 0); errno != 0 {
				return linuxErrno(errno)
			}
		}
		if errno := handler.vfsBegin(request, nil, nil, 0); errno != 0 {
			return linuxErrno(errno)
		}
	}
	file := vfs.Handle(description.Object)
	for {
		pending, _ := linuxcompat.SyscallContinuations.Pending(handler.task)
		if fromEnd && pending.Stage == seekStageLength {
			reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationStat, Handle: file}, attachPaths)
			if blocked {
				return linuxBlocked
			}
			if errno != 0 {
				return linuxErrno(errno)
			}
			target, errno := linuxcompat.SeekTarget(whence, offset, 0, reply.Size)
			if reply.Status != vfs.StatusOK {
				errno = linuxabi.EIO
			}
			if errno != 0 {
				handler.vfsFinish()
				return linuxErrno(errno)
			}
			if errno := handler.vfsNext(seekStageLength, seekStageSet, target); errno != 0 {
				return linuxErrno(errno)
			}
			continue
		}
		target := pending.Partial
		if !fromEnd {
			target, _ = linuxcompat.SeekTarget(whence, offset, description.Offset, 0)
		}
		reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationSeek, Handle: file, Offset: target}, attachPaths)
		if blocked {
			return linuxBlocked
		}
		if errno != 0 {
			return linuxErrno(errno)
		}
		handler.vfsFinish()
		if reply.Status != vfs.StatusOK {
			return linuxErrno(linuxabi.EIO)
		}
		description.Offset = target
		return linuxcompat.Result{Value: target}
	}
}

// getcwd copies the working directory and its terminator and returns the
// length it copied, as the Linux call does.
func (handler *linuxCallHandler) getcwd(request linuxcompat.Request) linuxcompat.Result {
	state, found := linuxcompat.FindProcess(handler.root)
	if !found {
		return linuxErrno(linuxabi.ENOENT)
	}
	size := copy(linuxResolved[:], state.CWD())
	if uint64(size)+1 > request.Arguments[1] {
		return linuxErrno(linuxabi.ERANGE)
	}
	if !vm.CopyOut(handler.root, request.Arguments[0], linuxResolved[:size]) ||
		!vm.CopyOut(handler.root, request.Arguments[0]+uint64(size), linuxTerminator[:]) {
		return linuxErrno(linuxabi.EFAULT)
	}
	return linuxcompat.Result{Value: uint64(size) + 1}
}

var linuxTerminator [1]byte

// chdir accepts a path only after the VFS confirms it names a directory.
func (handler *linuxCallHandler) chdir(request linuxcompat.Request) linuxcompat.Result {
	_, active, errno := handler.vfsStage(request)
	state, found := linuxcompat.FindProcess(handler.root)
	if errno == 0 && !found {
		errno = linuxabi.EIO
	}
	if errno != 0 {
		return linuxErrno(errno)
	}
	if !active {
		path, _, errno := handler.resolvePath(state, linuxabi.AtFDCWD, request.Arguments[0], &linuxResolved)
		if errno == 0 {
			errno = handler.vfsBegin(request, path, nil, 0)
		}
		if errno != 0 {
			return linuxErrno(errno)
		}
	}
	path, _ := linuxcompat.SyscallContinuations.Paths(handler.task)
	reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationStat, Path: path}, attachPaths)
	if blocked {
		return linuxBlocked
	}
	if errno != 0 {
		return linuxErrno(errno)
	}
	switch {
	case reply.Status != vfs.StatusOK:
		errno = linuxcompat.VFSErrno(reply.Status, linuxcompat.VFSPermissionAccess, linuxabi.ENOMEM)
	case reply.Kind != ramfs.KindDirectory:
		errno = linuxabi.ENOTDIR
	case !state.SetCWD(path):
		errno = linuxabi.EIO
	}
	handler.vfsFinish()
	return linuxErrno(errno)
}

// mkdir ignores the mode argument: GoOS has no permission bits to set.
func (handler *linuxCallHandler) mkdir(request linuxcompat.Request, dirfd int32, address uint64) linuxcompat.Result {
	_, active, errno := handler.vfsStage(request)
	state, found := linuxcompat.FindProcess(handler.root)
	if errno == 0 && !found {
		errno = linuxabi.EIO
	}
	if errno != 0 {
		return linuxErrno(errno)
	}
	if !active {
		path, _, errno := handler.resolvePath(state, dirfd, address, &linuxResolved)
		if errno == 0 {
			errno = handler.vfsBegin(request, path, nil, 0)
		}
		if errno != 0 {
			return linuxErrno(errno)
		}
	}
	path, _ := linuxcompat.SyscallContinuations.Paths(handler.task)
	reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationMkdir, Path: path}, attachPaths)
	if blocked {
		return linuxBlocked
	}
	if errno != 0 {
		return linuxErrno(errno)
	}
	handler.vfsFinish()
	return linuxErrno(linuxcompat.MutationErrno(reply.Status))
}

// Removing a name is a stat, so unlink can refuse a directory and rmdir a
// file, then the removal.
const (
	removeStageStat   = uint8(1)
	removeStageUnlink = uint8(2)
)

func (handler *linuxCallHandler) remove(request linuxcompat.Request, dirfd int32, address uint64, directory bool) linuxcompat.Result {
	_, active, errno := handler.vfsStage(request)
	state, found := linuxcompat.FindProcess(handler.root)
	if errno == 0 && !found {
		errno = linuxabi.EIO
	}
	if errno != 0 {
		return linuxErrno(errno)
	}
	if !active {
		path, trailing, errno := handler.resolvePath(state, dirfd, address, &linuxResolved)
		note := uint64(0)
		if trailing {
			note |= noteTrailing
		}
		if directory {
			note |= noteRemoveDirectory
		}
		if errno == 0 {
			errno = handler.vfsBegin(request, path, nil, note)
		}
		if errno != 0 {
			return linuxErrno(errno)
		}
	}
	path, _ := linuxcompat.SyscallContinuations.Paths(handler.task)
	for {
		pending, _ := linuxcompat.SyscallContinuations.Pending(handler.task)
		if pending.Stage == removeStageStat {
			reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationStat, Path: path}, attachPaths)
			if blocked {
				return linuxBlocked
			}
			if errno != 0 {
				return linuxErrno(errno)
			}
			if reply.Status != vfs.StatusOK {
				errno = linuxcompat.VFSErrno(reply.Status, linuxcompat.VFSPermissionAccess, linuxabi.ENOMEM)
			} else {
				errno = linuxcompat.RemovalErrno(pending.Note&noteRemoveDirectory != 0, pending.Note&noteTrailing != 0, reply.Kind)
			}
			if errno != 0 {
				handler.vfsFinish()
				return linuxErrno(errno)
			}
			if errno := handler.vfsNext(removeStageStat, removeStageUnlink, 0); errno != 0 {
				return linuxErrno(errno)
			}
			continue
		}
		reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationUnlink, Path: path}, attachPaths)
		if blocked {
			return linuxBlocked
		}
		if errno != 0 {
			return linuxErrno(errno)
		}
		handler.vfsFinish()
		return linuxErrno(linuxcompat.MutationErrno(reply.Status))
	}
}

// A rename stats the source to choose the VFS operation: a regular file
// replaces an existing target in one step, a directory moves only onto a
// free name.
const (
	renameStageStat    = uint8(1)
	renameStageReplace = uint8(2)
	renameStageMove    = uint8(3)
)

func (handler *linuxCallHandler) rename(request linuxcompat.Request, fromDirfd int32, fromAddress uint64, toDirfd int32, toAddress uint64) linuxcompat.Result {
	_, active, errno := handler.vfsStage(request)
	state, found := linuxcompat.FindProcess(handler.root)
	if errno == 0 && !found {
		errno = linuxabi.EIO
	}
	if errno != 0 {
		return linuxErrno(errno)
	}
	if !active {
		source, trailing, errno := handler.resolvePath(state, fromDirfd, fromAddress, &linuxResolved)
		if errno != 0 {
			return linuxErrno(errno)
		}
		target, targetTrailing, errno := handler.resolvePath(state, toDirfd, toAddress, &linuxTarget)
		note := uint64(0)
		if trailing {
			note |= noteTrailing
		}
		if targetTrailing {
			note |= noteTargetTrailing
		}
		if errno == 0 {
			errno = handler.vfsBegin(request, source, target, note)
		}
		if errno != 0 {
			return linuxErrno(errno)
		}
	}
	source, target := linuxcompat.SyscallContinuations.Paths(handler.task)
	for {
		pending, _ := linuxcompat.SyscallContinuations.Pending(handler.task)
		operation := vfs.OperationRename
		switch pending.Stage {
		case renameStageStat:
			reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationStat, Path: source}, attachPaths)
			if blocked {
				return linuxBlocked
			}
			if errno != 0 {
				return linuxErrno(errno)
			}
			next := renameStageMove
			switch {
			case reply.Status != vfs.StatusOK:
				errno = linuxcompat.VFSErrno(reply.Status, linuxcompat.VFSPermissionAccess, linuxabi.ENOMEM)
			case reply.Kind == ramfs.KindFile && pending.Note&(noteTrailing|noteTargetTrailing) != 0:
				errno = linuxabi.ENOTDIR
			case reply.Kind == ramfs.KindFile:
				next = renameStageReplace
			}
			// Renaming a name onto itself changes nothing and succeeds.
			if errno != 0 || samePath(source, target) {
				handler.vfsFinish()
				return linuxErrno(errno)
			}
			if errno := handler.vfsNext(renameStageStat, next, 0); errno != 0 {
				return linuxErrno(errno)
			}
			continue
		case renameStageReplace:
			operation = vfs.OperationReplace
		case renameStageMove:
		default:
			return linuxErrno(handler.vfsFailed())
		}
		reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: operation, Path: source, Target: target}, attachPaths)
		if blocked {
			return linuxBlocked
		}
		if errno != 0 {
			return linuxErrno(errno)
		}
		handler.vfsFinish()
		if reply.Status == vfs.StatusUnsupported {
			// The VFS moves a name within one filesystem only.
			return linuxErrno(linuxabi.EXDEV)
		}
		return linuxErrno(linuxcompat.MutationErrno(reply.Status))
	}
}

func samePath(first []byte, second []byte) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}
