//go:build uefi

package main

// fsync and fdatasync. GoOS has one durability operation, the VFS sync that
// flushes every completed write of the persistent volume to its device. A
// call on a regular file asks for it and succeeds only when the flush did.
// It covers more than the one file, and the data and metadata alike, so
// fdatasync is the same call. A file outside the persistent volume has
// nothing to flush and succeeds once the VFS confirms it is open.

import (
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/vfs"
)

func (handler *linuxCallHandler) sync(request linuxcompat.Request) linuxcompat.Result {
	pending, active, errno := handler.vfsStage(request)
	if errno != 0 {
		return linuxErrno(errno)
	}
	if !active {
		state, found := linuxcompat.FindProcess(handler.root)
		if !found {
			return linuxErrno(linuxabi.EBADF)
		}
		description, open := state.FDs.Lookup(descriptorIndex(request.Arguments[0]))
		if !open {
			return linuxErrno(linuxabi.EBADF)
		}
		switch description.Kind {
		case linuxcompat.FDVFSFile:
		case linuxcompat.FDVFSDirectory:
			// A directory's entries are written with the file operations
			// that change them; there is nothing further to flush.
			return linuxcompat.Result{}
		default:
			// Linux refuses fsync on a pipe, a terminal, and a character
			// device with EINVAL.
			return linuxErrno(linuxabi.EINVAL)
		}
		if errno := handler.vfsBegin(request, nil, nil, description.Object); errno != 0 {
			return linuxErrno(errno)
		}
		pending, _ = linuxcompat.SyscallContinuations.Pending(handler.task)
	}
	reply, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationSync, Handle: vfs.Handle(pending.Note)}, attachPaths)
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
