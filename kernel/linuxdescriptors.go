//go:build uefi

package main

// Linux descriptor calls: dup, dup2, dup3, fcntl, and ioctl.

import (
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/stream"
	"goos/kernel/vfs"
)

func (handler *linuxCallHandler) Descriptor(request linuxcompat.Request) linuxcompat.Result {
	state, found := linuxcompat.FindProcess(handler.root)
	if !found {
		return linuxErrno(linuxabi.EBADF)
	}
	arguments := &request.Arguments
	fd := descriptorIndex(arguments[0])
	switch request.Number {
	case linuxabi.SysIoctl:
		// A terminal takes the terminal requests. Pipes, files, and devices
		// take none: ENOTTY is what Linux answers any ioctl a file does not
		// take, and what makes isatty false for them.
		description, open := state.FDs.Lookup(fd)
		if !open {
			return linuxErrno(linuxabi.EBADF)
		}
		if description.Kind == linuxcompat.FDTerminal {
			return handler.terminalIoctl(state, description, arguments[1], arguments[2])
		}
		return linuxErrno(linuxabi.ENOTTY)
	case linuxabi.SysDup:
		return duplicated(state.FDs.Duplicate(fd, 0, false))
	case linuxabi.SysDup2:
		if arguments[0] == arguments[1] {
			// Duplicating a descriptor onto itself does nothing, provided
			// it is open.
			if _, open := state.FDs.Lookup(fd); !open {
				return linuxErrno(linuxabi.EBADF)
			}
			return linuxcompat.Result{Value: arguments[1]}
		}
		return handler.duplicateOnto(request, state, fd, arguments[1], false)
	case linuxabi.SysDup3:
		if arguments[0] == arguments[1] || arguments[2]&^uint64(linuxabi.OpenCloseExec) != 0 {
			return linuxErrno(linuxabi.EINVAL)
		}
		return handler.duplicateOnto(request, state, fd, arguments[1], arguments[2] != 0)
	case linuxabi.SysFcntl:
		return handler.fcntl(state, fd, arguments[1], arguments[2])
	}
	return linuxErrno(linuxabi.ENOSYS)
}

// descriptorIndex narrows a descriptor argument. Anything out of the table
// becomes an index no lookup accepts.
func descriptorIndex(value uint64) int {
	if value >= linuxcompat.LinuxMaxFD {
		return -1
	}
	return int(value)
}

func duplicated(fd int, ok bool) linuxcompat.Result {
	if !ok {
		if fd == -2 {
			return linuxErrno(linuxabi.EBADF)
		}
		return linuxErrno(linuxabi.EMFILE)
	}
	return linuxcompat.Result{Value: uint64(fd)}
}

// duplicateOnto makes target name what fd names. An open target is closed
// first, and when it was the last descriptor of a VFS file that close is an
// exchange the call waits for before it installs the duplicate.
func (handler *linuxCallHandler) duplicateOnto(request linuxcompat.Request, state *linuxcompat.ProcessState, fd int, target uint64, closeOnExec bool) linuxcompat.Result {
	pending, active, errno := handler.vfsStage(request)
	if errno != 0 {
		return linuxErrno(errno)
	}
	if !active {
		if _, open := state.FDs.Lookup(fd); !open || target >= linuxcompat.LinuxMaxFD {
			return linuxErrno(linuxabi.EBADF)
		}
		if released, last, errno := linuxcompat.CloseFD(handler.root, int(target)); errno == 0 && last {
			if handler.releaseDescription(released) {
				if errno := handler.vfsBegin(request, nil, nil, released.Object); errno != 0 {
					return linuxErrno(errno)
				}
				pending.Note, active = released.Object, true
			}
		}
	}
	if active {
		_, errno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationClose, Handle: vfs.Handle(pending.Note)}, attachPaths)
		if blocked {
			return linuxBlocked
		}
		// The target is free either way. A failed release of what it held
		// is no reason to refuse the duplicate; Linux reports none either.
		if errno == 0 {
			handler.vfsFinish()
		}
	}
	if !state.FDs.DuplicateAt(fd, int(target), closeOnExec) {
		return linuxErrno(linuxabi.EBADF)
	}
	return linuxcompat.Result{Value: target}
}

// releaseDescription gives up what the last descriptor of a description
// held. It reports true when that is a VFS file, whose close is an exchange
// the caller must run.
func (handler *linuxCallHandler) releaseDescription(released linuxcompat.FileDescription) bool {
	switch released.Kind {
	case linuxcompat.FDStreamReader, linuxcompat.FDStreamWriter, linuxcompat.FDTerminal:
		releaseLinuxDescriptionReference(released)
	case linuxcompat.FDVFSDirectory:
		if state, found := linuxcompat.FindProcess(handler.root); found {
			linuxcompat.ReleaseDirectory(state.FamilyID, released.Object)
		}
	case linuxcompat.FDVFSFile:
		return true
	}
	return false
}

// releaseLinuxDescriptionReference releases kernel objects owned by a shared
// open-file description after its last descriptor closes.
func releaseLinuxDescriptionReference(released linuxcompat.FileDescription) {
	if released.Kind == linuxcompat.FDTerminal {
		releaseTerminal(linuxcompat.TerminalID(released.Object))
		return
	}
	if released.Kind != linuxcompat.FDStreamReader && released.Kind != linuxcompat.FDStreamWriter {
		return
	}
	streamID, writer, closed, ok := streamRegistry.Drop(released.Object)
	if ok {
		finishStreamClosure(stream.Closure{Stream: streamID, Writer: writer, Closed: closed})
	}
}

func (handler *linuxCallHandler) fcntl(state *linuxcompat.ProcessState, fd int, command uint64, argument uint64) linuxcompat.Result {
	description, open := state.FDs.Description(fd)
	if !open {
		return linuxErrno(linuxabi.EBADF)
	}
	switch command {
	case linuxabi.FcntlDupFD, linuxabi.FcntlDupFDCloExec:
		if argument >= linuxcompat.LinuxMaxFD {
			return linuxErrno(linuxabi.EINVAL)
		}
		return duplicated(state.FDs.Duplicate(fd, int(argument), command == linuxabi.FcntlDupFDCloExec))
	case linuxabi.FcntlGetFD:
		if closeOnExec, _ := state.FDs.CloseOnExec(fd); closeOnExec {
			return linuxcompat.Result{Value: linuxabi.DescriptorCloExec}
		}
		return linuxcompat.Result{}
	case linuxabi.FcntlSetFD:
		state.FDs.SetCloseOnExec(fd, argument&linuxabi.DescriptorCloExec != 0)
		return linuxcompat.Result{}
	case linuxabi.FcntlGetFL:
		return linuxcompat.Result{Value: uint64(linuxcompat.StatusFlagsOf(*description))}
	case linuxabi.FcntlSetFL:
		return linuxErrno(linuxcompat.SetStatusFlags(description, argument))
	}
	// Locks, owners, leases, and seals have nothing behind them.
	return linuxErrno(linuxabi.EINVAL)
}
