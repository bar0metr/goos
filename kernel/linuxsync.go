//go:build uefi

package main

// fsync and fdatasync. The VFS asks the filesystem that holds the file or
// directory, and only that one, to make it durable (ADR 0223). FAT32 flushes
// its device. RAMFS and the boot archive have nothing to write and succeed.
// A read-only ext4 volume has no pending writes and succeeds. A filesystem
// with no durability operation is EINVAL, as Linux answers for one that
// cannot sync; a failed flush is EIO. Each sync covers the data and the
// metadata alike, so fdatasync is the same call.

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
		var path []byte
		note := description.Object
		switch description.Kind {
		case linuxcompat.FDVFSFile:
		case linuxcompat.FDVFSDirectory:
			// A directory has no VFS handle; its descriptor keeps the path.
			named := false
			path, named = linuxcompat.DirectoryPath(state.FamilyID, description.Object)
			if !named {
				return linuxErrno(linuxabi.EBADF)
			}
			note = 0
		default:
			// Linux refuses fsync on a pipe, a terminal, and a character
			// device with EINVAL.
			return linuxErrno(linuxabi.EINVAL)
		}
		if errno := handler.vfsBegin(request, path, nil, note); errno != 0 {
			return linuxErrno(errno)
		}
		pending, _ = linuxcompat.SyscallContinuations.Pending(handler.task)
	}
	operation := vfs.Request{Operation: vfs.OperationSync, Handle: vfs.Handle(pending.Note)}
	if path, _ := linuxcompat.SyscallContinuations.Paths(handler.task); len(path) != 0 {
		operation = vfs.Request{Operation: vfs.OperationSync, Path: path}
	}
	reply, errno, blocked := handler.vfsExchange(operation, attachPaths)
	if blocked {
		return linuxBlocked
	}
	if errno != 0 {
		return linuxErrno(errno)
	}
	handler.vfsFinish()
	return linuxErrno(linuxcompat.SyncErrno(reply.Status))
}
