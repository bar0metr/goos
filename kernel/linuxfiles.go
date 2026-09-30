//go:build uefi

package main

// Linux file calls on the VFS bridge: open, close, read, write, seek, the
// working directory, and the calls that create, remove, and rename names.
// Each handler is entered again after every wake, so it reads its progress
// from the continuation and keeps nothing in locals across an exchange.

import (
	"goos/internal/limits"
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
		device := linuxcompat.DeviceNone
		if errno == 0 {
			device, errno = linuxcompat.LookupDevice(path)
		}
		if errno == 0 && device != linuxcompat.DeviceNone {
			return handler.openDevice(device, intent, trailing)
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
				result := handler.installDirectory(state, path, intent)
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

func (handler *linuxCallHandler) installDirectory(state *linuxcompat.ProcessState, path []byte, intent linuxcompat.OpenIntent) linuxcompat.Result {
	reference, retained := linuxcompat.RetainDirectory(state.FamilyID, path)
	if !retained {
		return linuxErrno(linuxabi.ENFILE)
	}
	description := linuxcompat.FileDescription{Kind: linuxcompat.FDVFSDirectory, Flags: intent.StatusFlags(), Object: reference}
	fd, errno := linuxcompat.AllocateFD(handler.root, description, intent.CloseOnExec)
	if errno != 0 {
		linuxcompat.ReleaseDirectory(state.FamilyID, reference)
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
		if !handler.releaseDescription(released) {
			return linuxcompat.Result{}
		}
		// The description is gone; the note carries its VFS handle to the
		// retry.
		if errno := handler.vfsBegin(request, nil, nil, released.Object); errno != 0 {
			return linuxErrno(errno)
		}
		pending.Note = released.Object
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
// the reply, at one address or across a readv vector.
func (handler *linuxCallHandler) readFile(request linuxcompat.Request, description *linuxcompat.FileDescription, vector bool) linuxcompat.Result {
	pending, active, errno := handler.vfsStage(request)
	if errno != 0 {
		return linuxErrno(errno)
	}
	if !active {
		if description.Flags&linuxabi.OpenAccess == linuxabi.OpenWriteOnly {
			return linuxErrno(linuxabi.EBADF)
		}
		limit, _, errno := handler.readLimit(request, vector, linuxIOBytes)
		if errno != 0 || limit == 0 {
			return linuxErrno(errno)
		}
		if errno := handler.vfsBegin(request, nil, nil, limit); errno != 0 {
			return linuxErrno(errno)
		}
		pending.Note = limit
	}
	reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationReadInto, Handle: vfs.Handle(description.Object), Size: uint32(pending.Note)}, attachSink)
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
	if reply.Size > pending.Note || state == nil || !owned {
		return linuxErrno(linuxabi.EIO)
	}
	object, found := ioBuffer(state, owner)
	if !found {
		return linuxErrno(linuxabi.EIO)
	}
	// The position moved in the VFS whether or not the copy succeeds.
	description.Offset += reply.Size
	var wire []byte
	if vector {
		// The vector is read again: the copy made before the wait shared
		// its scratch with nothing, but the wait ended this entry.
		if wire, errno = handler.copyIOVec(request); errno != 0 {
			return linuxErrno(errno)
		}
	}
	if !handler.deliver(request, wire, 0, nil, object, reply.Size) {
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

// positionFailed marks, in the value the transfer stage hands the restore
// stage, that the transfer failed with the errno in the low bits.
const positionFailed = uint64(1) << 63

// positioned serves pread64 and pwrite64. The VFS keeps one position per
// open file and reads and writes there, so the call moves the position to
// the offset, transfers, and moves it back to where the description has it.
// A process has one task and no other process shares its open files, so
// nothing can use the position in between. The description's offset never
// changes, as Linux promises.
//
// A descriptor opened with O_APPEND writes at the end of the file whatever
// the offset says. Linux does the same and documents it as a bug.
func (handler *linuxCallHandler) positioned(request linuxcompat.Request, write bool) linuxcompat.Result {
	_, active, errno := handler.vfsStage(request)
	if errno != 0 {
		return linuxErrno(errno)
	}
	// Linux refuses a negative offset before it looks at the descriptor.
	offset := request.Arguments[3]
	if int64(offset) < 0 {
		return linuxErrno(linuxabi.EINVAL)
	}
	description, found := linuxcompat.DescriptionOf(handler.root, request.Arguments[0])
	if !found {
		return linuxErrno(linuxabi.EBADF)
	}
	switch description.Kind {
	case linuxcompat.FDVFSFile:
	case linuxcompat.FDVFSDirectory:
		if write {
			return linuxErrno(linuxabi.EBADF)
		}
		return linuxErrno(linuxabi.EISDIR)
	case linuxcompat.FDSynthetic:
		if write {
			return handler.deviceWrite(request, *description, false)
		}
		return handler.deviceRead(request, *description, false)
	default:
		// A stream has no positions.
		return linuxErrno(linuxabi.ESPIPE)
	}
	appending := write && description.Flags&linuxabi.OpenAppend != 0
	if !active {
		access := description.Flags & linuxabi.OpenAccess
		if write && access == linuxabi.OpenReadOnly || !write && access == linuxabi.OpenWriteOnly {
			return linuxErrno(linuxabi.EBADF)
		}
		var count uint64
		if write {
			count, errno = handler.gatherWrite(request, false)
		} else {
			count, _, errno = handler.readLimit(request, false, linuxIOBytes)
		}
		if errno != 0 || count == 0 {
			return linuxErrno(errno)
		}
		if errno := handler.vfsBegin(request, nil, nil, count); errno != 0 {
			return linuxErrno(errno)
		}
	}
	// An appending write asks for the file length first, so its stages start
	// one later.
	seekStage := uint8(1)
	if appending {
		seekStage = 2
	}
	file := vfs.Handle(description.Object)
	for {
		pending, _ := linuxcompat.SyscallContinuations.Pending(handler.task)
		switch pending.Stage {
		case seekStage - 1:
			reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationStat, Handle: file}, attachPaths)
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
			if errno := handler.vfsNext(pending.Stage, seekStage, reply.Size); errno != 0 {
				return linuxErrno(errno)
			}
		case seekStage:
			target := offset
			if appending {
				target = pending.Partial
			}
			reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationSeek, Handle: file, Offset: target}, attachPaths)
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
			if errno := handler.vfsNext(seekStage, seekStage+1, 0); errno != 0 {
				return linuxErrno(errno)
			}
		case seekStage + 1:
			operation, attachment := vfs.OperationReadInto, attachSink
			if write {
				operation, attachment = vfs.OperationWriteFrom, attachSource
			}
			reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: operation, Handle: file, Size: uint32(pending.Note)}, attachment)
			if blocked {
				return linuxBlocked
			}
			if errno != 0 {
				return linuxErrno(errno)
			}
			// A failed transfer still puts the position back.
			outcome := reply.Size
			switch {
			case reply.Status != vfs.StatusOK && write:
				outcome = positionFailed | uint64(linuxcompat.MutationErrno(reply.Status))
			case reply.Status != vfs.StatusOK:
				outcome = positionFailed | uint64(linuxcompat.VFSErrno(reply.Status, linuxcompat.VFSPermissionAccess, linuxabi.ENOMEM))
			case reply.Size > pending.Note:
				outcome = positionFailed | uint64(linuxabi.EIO)
			}
			if errno := handler.vfsNext(seekStage+1, seekStage+2, outcome); errno != 0 {
				return linuxErrno(errno)
			}
		case seekStage + 2:
			reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationSeek, Handle: file, Offset: description.Offset}, attachPaths)
			if blocked {
				return linuxBlocked
			}
			if errno != 0 {
				return linuxErrno(errno)
			}
			handler.vfsFinish()
			switch {
			case pending.Partial&positionFailed != 0:
				return linuxErrno(linuxabi.Errno(pending.Partial &^ positionFailed))
			case reply.Status != vfs.StatusOK:
				return linuxErrno(linuxabi.EIO)
			case write:
				return linuxcompat.Result{Value: pending.Partial}
			}
			state, _ := linuxcompat.FindProcess(handler.root)
			owner, owned := processRegistry.ByRoot(handler.root)
			if state == nil || !owned {
				return linuxErrno(linuxabi.EIO)
			}
			object, ready := ioBuffer(state, owner)
			if !ready {
				return linuxErrno(linuxabi.EIO)
			}
			if !handler.deliver(request, nil, 0, nil, object, pending.Partial) {
				return linuxErrno(linuxabi.EFAULT)
			}
			return linuxcompat.Result{Value: pending.Partial}
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
		if !userRange(address, length) {
			return 0, linuxabi.EFAULT
		}
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
		if length = handler.accessible(address, length, false); length == 0 || !handler.ioTransfer(object, 0, address, length, false) {
			return 0, linuxabi.EFAULT
		}
		return length, 0
	}
	wire, errno := handler.copyIOVec(request)
	if errno != 0 || len(wire) == 0 {
		return 0, errno
	}
	object, ready := ioBuffer(state, owner)
	if !ready {
		return 0, linuxabi.ENOMEM
	}
	total := uint64(0)
	for index := 0; index < len(wire)/linuxabi.IOVecSize && total < linuxIOBytes; index++ {
		base, length := linuxcompat.IOVecEntry(wire, index)
		if length > linuxIOBytes-total {
			length = linuxIOBytes - total
		}
		if length == 0 {
			continue
		}
		readable := handler.accessible(base, length, false)
		if readable == 0 || !handler.ioTransfer(object, total, base, readable, false) {
			if total == 0 {
				return 0, linuxabi.EFAULT
			}
			break
		}
		total += readable
		if readable < length {
			break
		}
	}
	return total, 0
}

// Filesystem routes the path and position calls, which share their decoding
// more than they differ.
func (handler *linuxCallHandler) Filesystem(request linuxcompat.Request) linuxcompat.Result {
	arguments := &request.Arguments
	switch request.Number {
	case linuxabi.SysAccess:
		return handler.access(request, linuxabi.AtFDCWD, arguments[0], arguments[1])
	case linuxabi.SysFaccessat:
		return handler.access(request, int32(arguments[0]), arguments[1], arguments[2])
	case linuxabi.SysStat, linuxabi.SysLstat:
		return handler.status(request, linuxabi.AtFDCWD, arguments[0], arguments[1], 0, false)
	case linuxabi.SysFstat:
		return handler.status(request, int32(arguments[0]), 0, arguments[1], 0, true)
	case linuxabi.SysNewfstatat:
		return handler.status(request, int32(arguments[0]), arguments[1], arguments[2], arguments[3], false)
	case linuxabi.SysStatx:
		// The flags are a C int; the mask stays in the request for status.
		return handler.status(request, int32(arguments[0]), arguments[1], arguments[4], uint64(uint32(arguments[2])), false)
	case linuxabi.SysGetdents64:
		return handler.getdents(request)
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

var (
	linuxStat  [linuxcompat.StatSize]byte
	linuxStatx [linuxcompat.StatxSize]byte
)

// status serves stat, lstat, fstat, newfstatat, and statx. lstat is stat:
// GoOS has no symbolic links to stop at. A path is stat-ed by name. A
// descriptor is stat-ed by what it holds: a file by its VFS handle, which
// follows the file through a rename; a directory by the path its description
// keeps; a stream by a synthetic FIFO record and a device by its fixed
// record, both with no VFS exchange. statx differs from the others only in
// its flags and in the structure the answer is written as.
func (handler *linuxCallHandler) status(request linuxcompat.Request, dirfd int32, pathAddress uint64, buffer uint64, flags uint64, descriptor bool) linuxcompat.Result {
	_, active, errno := handler.vfsStage(request)
	state, found := linuxcompat.FindProcess(handler.root)
	if errno == 0 && !found {
		errno = linuxabi.EIO
	}
	if errno != 0 {
		return linuxErrno(errno)
	}
	extended := request.Number == linuxabi.SysStatx
	if !active {
		var emptyPath bool
		var errno linuxabi.Errno
		if extended {
			emptyPath, errno = linuxcompat.StatxFlags(flags, request.Arguments[3])
		} else {
			emptyPath, errno = linuxcompat.StatFlags(flags)
		}
		switch {
		case errno != 0 || descriptor || !emptyPath:
		case pathAddress == 0 && (extended || flags == linuxabi.AtEmptyPath):
			// Linux 6.11 and later take a null path with AT_EMPTY_PATH as
			// the empty one.
			descriptor = true
		default:
			// AT_EMPTY_PATH with an empty path names dirfd itself.
			var raw []byte
			raw, errno = handler.copyPath(pathAddress)
			descriptor = errno == 0 && len(raw) == 0
		}
		if errno != 0 {
			return linuxErrno(errno)
		}
		if descriptor {
			// Only the calls with a flags word may name the working directory
			// this way; fstat of AT_FDCWD is a bad descriptor.
			if result, done := handler.beginDescriptorStatus(request, state, dirfd, buffer, emptyPath); done {
				return result
			}
		} else {
			path, trailing, errno := handler.resolvePath(state, dirfd, pathAddress, &linuxResolved)
			device := linuxcompat.DeviceNone
			if errno == 0 {
				device, errno = linuxcompat.LookupDevice(path)
			}
			if errno == 0 && device != linuxcompat.DeviceNone {
				if trailing {
					return linuxErrno(linuxabi.ENOTDIR)
				}
				return handler.deliverStatus(request, buffer, linuxcompat.DeviceStatus(device))
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
	}
	// A call with a path stats it and keeps flags in its note. A call
	// without one stats the open file whose VFS handle the note holds.
	pending, _ := linuxcompat.SyscallContinuations.Pending(handler.task)
	path, _ := linuxcompat.SyscallContinuations.Paths(handler.task)
	operation := vfs.Request{Operation: vfs.OperationStat, Path: path}
	trailing := pending.Note&noteTrailing != 0
	if len(path) == 0 {
		operation.Handle, trailing = vfs.Handle(pending.Note), false
	}
	reply, errno, blocked := handler.vfsExchange(operation, attachPaths)
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
	if trailing && reply.Kind != ramfs.KindDirectory {
		return linuxErrno(linuxabi.ENOTDIR)
	}
	described, errno := linuxcompat.StatusOfReply(&reply)
	if errno != 0 {
		return linuxErrno(errno)
	}
	return handler.deliverStatus(request, buffer, described)
}

// access answers from the mode a stat of the path would report, so the two
// calls cannot disagree.
func (handler *linuxCallHandler) access(request linuxcompat.Request, dirfd int32, pathAddress uint64, requested uint64) linuxcompat.Result {
	_, active, errno := handler.vfsStage(request)
	state, found := linuxcompat.FindProcess(handler.root)
	if errno == 0 && !found {
		errno = linuxabi.EIO
	}
	if errno == 0 {
		errno = linuxcompat.AccessErrno(requested, 0o777)
	}
	if errno != 0 {
		return linuxErrno(errno)
	}
	if !active {
		path, trailing, errno := handler.resolvePath(state, dirfd, pathAddress, &linuxResolved)
		device := linuxcompat.DeviceNone
		if errno == 0 {
			device, errno = linuxcompat.LookupDevice(path)
		}
		if errno == 0 && device != linuxcompat.DeviceNone {
			if trailing {
				return linuxErrno(linuxabi.ENOTDIR)
			}
			return linuxErrno(linuxcompat.AccessErrno(requested, linuxcompat.DeviceStatus(device).Mode))
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
	pending, _ := linuxcompat.SyscallContinuations.Pending(handler.task)
	path, _ := linuxcompat.SyscallContinuations.Paths(handler.task)
	reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationStat, Path: path}, attachPaths)
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
	if pending.Note&noteTrailing != 0 && reply.Kind != ramfs.KindDirectory {
		return linuxErrno(linuxabi.ENOTDIR)
	}
	described, errno := linuxcompat.StatusOfReply(&reply)
	if errno != 0 {
		return linuxErrno(errno)
	}
	return linuxErrno(linuxcompat.AccessErrno(requested, described.Mode))
}

// beginDescriptorStatus starts the stat of what a descriptor holds. done
// reports that result is final: an error, or a stream or a device, which
// need no VFS.
func (handler *linuxCallHandler) beginDescriptorStatus(request linuxcompat.Request, state *linuxcompat.ProcessState, fd int32, buffer uint64, workingDirectory bool) (linuxcompat.Result, bool) {
	if fd == linuxabi.AtFDCWD && workingDirectory {
		errno := handler.vfsBegin(request, state.CWD(), nil, 0)
		return linuxErrno(errno), errno != 0
	}
	description, open := state.FDs.Lookup(int(fd))
	index, _ := state.FDs.DescriptionIndex(int(fd))
	if !open {
		return linuxErrno(linuxabi.EBADF), true
	}
	errno := linuxabi.Errno(0)
	switch description.Kind {
	case linuxcompat.FDVFSFile:
		errno = handler.vfsBegin(request, nil, nil, description.Object)
	case linuxcompat.FDVFSDirectory:
		path, named := linuxcompat.DirectoryPath(state.FamilyID, description.Object)
		if !named {
			return linuxErrno(linuxabi.EBADF), true
		}
		errno = handler.vfsBegin(request, path, nil, 0)
	case linuxcompat.FDSynthetic:
		return handler.deliverStatus(request, buffer, linuxcompat.DeviceStatus(linuxcompat.DeviceOf(description))), true
	default:
		return handler.deliverStatus(request, buffer, linuxcompat.StreamStatus(index)), true
	}
	return linuxErrno(errno), errno != 0
}

// deliverStatus writes the answer in the structure the call asked for:
// struct statx for statx and struct stat for the rest.
func (handler *linuxCallHandler) deliverStatus(request linuxcompat.Request, buffer uint64, described linuxcompat.FileStatus) linuxcompat.Result {
	encoded := linuxStat[:]
	if request.Number == linuxabi.SysStatx {
		linuxcompat.EncodeStatx(&linuxStatx, described)
		encoded = linuxStatx[:]
	} else {
		linuxcompat.EncodeStat(&linuxStat, described)
	}
	if !vm.CopyOut(handler.root, buffer, encoded) {
		return linuxErrno(linuxabi.EFAULT)
	}
	return linuxcompat.Result{}
}

var linuxDirent [linuxcompat.MaxDirentSize]byte

// direntRetry marks, in the value one stage hands the next, that the entry
// at the current position has a name too long for a reply message and must
// be asked for again with a memory object to return it in. The rest of the
// value is the bytes already written to the caller's buffer.
const direntRetry = uint64(1) << 63

// getdents fills the caller's buffer with whole linux_dirent64 records, one
// VFS exchange per entry. The listing position is the VFS cursor, kept in
// the directory's description and returned as each record's d_off, so lseek
// can rewind or resume it. GoOS directories hold no "." or ".." entries and
// none are made up.
//
// A record that does not fit ends the call: EINVAL when it is the first,
// the bytes so far otherwise. Its entry stays unread for the next call.
func (handler *linuxCallHandler) getdents(request linuxcompat.Request) linuxcompat.Result {
	_, active, errno := handler.vfsStage(request)
	if errno != 0 {
		return linuxErrno(errno)
	}
	state, processFound := linuxcompat.FindProcess(handler.root)
	description, found := linuxcompat.DescriptionOf(handler.root, request.Arguments[0])
	if !processFound || !found {
		return linuxErrno(linuxabi.EBADF)
	}
	if description.Kind != linuxcompat.FDVFSDirectory {
		return linuxErrno(linuxabi.ENOTDIR)
	}
	buffer, capacity := request.Arguments[1], request.Arguments[2]
	if !active {
		path, named := linuxcompat.DirectoryPath(state.FamilyID, description.Object)
		if !named {
			return linuxErrno(linuxabi.EBADF)
		}
		if errno := handler.vfsBegin(request, path, nil, 0); errno != 0 {
			return linuxErrno(errno)
		}
	}
	path, _ := linuxcompat.SyscallContinuations.Paths(handler.task)
	for {
		pending, _ := linuxcompat.SyscallContinuations.Pending(handler.task)
		written, retry := pending.Partial&^direntRetry, pending.Partial&direntRetry != 0
		attachment := attachPaths
		if retry {
			attachment = attachName
		}
		reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationReadDir, Path: path, Offset: description.Offset, NameReturn: retry}, attachment)
		if blocked {
			return linuxBlocked
		}
		if errno != 0 {
			return direntResult(written, errno)
		}
		name := reply.Name
		switch {
		case reply.Status == vfs.StatusNotFound:
			// The end of the directory.
			handler.vfsFinish()
			return linuxcompat.Result{Value: written}
		case reply.Status == vfs.StatusNameTooLong && !retry:
			if errno := handler.vfsNext(pending.Stage, pending.Stage+1, written|direntRetry); errno != 0 {
				return direntResult(written, errno)
			}
			continue
		case reply.Status != vfs.StatusOK:
			handler.vfsFinish()
			return direntResult(written, linuxcompat.VFSErrno(reply.Status, linuxcompat.VFSPermissionAccess, linuxabi.ENOMEM))
		case reply.AttachedName != 0:
			name = handler.returnedName(int(reply.AttachedName))
		}
		size, encoded := linuxcompat.EncodeDirent(linuxDirent[:], reply.Node, uint64(reply.Cursor), reply.Kind, name)
		switch {
		case !encoded:
			errno = linuxabi.EIO
		case uint64(size) > capacity || written > capacity-uint64(size):
			errno = linuxabi.EINVAL
		case !vm.CopyOut(handler.root, buffer+written, linuxDirent[:size]):
			errno = linuxabi.EFAULT
		}
		if errno != 0 {
			handler.vfsFinish()
			return direntResult(written, errno)
		}
		description.Offset, written = uint64(reply.Cursor), written+uint64(size)
		if pending.Stage >= linuxcompat.MaxDirentExchanges {
			handler.vfsFinish()
			return linuxcompat.Result{Value: written}
		}
		if errno := handler.vfsNext(pending.Stage, pending.Stage+1, written); errno != 0 {
			return direntResult(written, errno)
		}
	}
}

// direntResult reports the records already delivered when a later entry
// fails. The failure reaches the caller on its next call, as on Linux.
func direntResult(written uint64, errno linuxabi.Errno) linuxcompat.Result {
	if written != 0 {
		return linuxcompat.Result{Value: written}
	}
	return linuxErrno(errno)
}

// returnedName reads a listed name the VFS wrote to the start of the I/O
// buffer.
func (handler *linuxCallHandler) returnedName(size int) []byte {
	state, found := linuxcompat.FindProcess(handler.root)
	owner, owned := processRegistry.ByRoot(handler.root)
	if !found || !owned || size > limits.NameMax {
		return nil
	}
	object, ready := ioBuffer(state, owner)
	if !ready {
		return nil
	}
	frame, located := object.Frame(vm.PhysicalPage, 0)
	page, mapped := vm.PhysicalPage(frame)
	if !located || !mapped {
		return nil
	}
	return page[:size]
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
	case linuxcompat.FDSynthetic:
		// /dev/null and /dev/zero accept any seek and stay at zero.
		return linuxcompat.Result{}
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
			errno = linuxcompat.DevicePathErrno(path, linuxabi.ENOTDIR)
		}
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
			errno = linuxcompat.DevicePathErrno(path, linuxabi.EEXIST)
		}
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
			// The device names are fixed: rmdir finds no directory there,
			// and unlink is refused as it is for any file the caller does
			// not own the directory of.
			refusal := linuxabi.EACCES
			if directory || trailing {
				refusal = linuxabi.ENOTDIR
			}
			errno = linuxcompat.DevicePathErrno(path, refusal)
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
			errno = linuxcompat.DevicePathErrno(source, linuxabi.EACCES)
		}
		if errno == 0 {
			errno = linuxcompat.DevicePathErrno(target, linuxabi.EACCES)
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
