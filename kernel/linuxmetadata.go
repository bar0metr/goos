//go:build uefi

package main

// chmod, fchmod, fchmodat, and utimensat (ADR 0186). GoOS filesystems keep
// one permission, whether a file may be written, and one time, the last
// modification. chmod sets the read-only attribute when the mode has no
// write bit and clears it otherwise; the other bits are accepted and not
// kept, as on a FAT volume Linux mounts with the quiet option. utimensat
// sets the modification time; the access time is accepted and not kept.

import (
	"encoding/binary"

	"goos/internal/timekeeping"
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/mm/vm"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
)

var linuxTimes [32]byte

// metadataChange decodes what the call asks to change. A change with nothing
// left in it, an access time alone or two omitted times, still checks that
// the file exists.
func (handler *linuxCallHandler) metadataChange(request linuxcompat.Request) (ramfs.MetadataChange, linuxabi.Errno) {
	arguments := &request.Arguments
	var change ramfs.MetadataChange
	switch request.Number {
	case linuxabi.SysChmod, linuxabi.SysFchmod:
		change.SetReadOnly, change.ReadOnly = true, arguments[1]&0o222 == 0
		return change, 0
	case linuxabi.SysFchmodat:
		change.SetReadOnly, change.ReadOnly = true, arguments[2]&0o222 == 0
		return change, 0
	}
	if arguments[2] == 0 {
		return handler.nowChange()
	}
	if !vm.CopyIn(handler.root, arguments[2], linuxTimes[:]) {
		return change, linuxabi.EFAULT
	}
	access := binary.LittleEndian.Uint64(linuxTimes[8:16])
	seconds, nanos := int64(binary.LittleEndian.Uint64(linuxTimes[16:24])), binary.LittleEndian.Uint64(linuxTimes[24:32])
	for _, value := range [...]uint64{access, nanos} {
		if value != linuxabi.UtimeNow && value != linuxabi.UtimeOmit && value >= uint64(timekeeping.NanosPerSecond) {
			return change, linuxabi.EINVAL
		}
	}
	switch nanos {
	case linuxabi.UtimeOmit:
		return change, 0
	case linuxabi.UtimeNow:
		return handler.nowChange()
	}
	change.SetModified = true
	change.Modified = ramfs.Timestamp{Seconds: seconds, Nanoseconds: uint32(nanos), Available: true}
	return change, 0
}

func (handler *linuxCallHandler) nowChange() (ramfs.MetadataChange, linuxabi.Errno) {
	now, set := realtimeClock.Now(&monotonicClock, amd64.ReadTSC())
	if !set {
		// Without a wall clock there is no "now" to record.
		return ramfs.MetadataChange{}, 0
	}
	return ramfs.MetadataChange{SetModified: true, Modified: ramfs.Timestamp{Seconds: now.Seconds, Nanoseconds: now.Nanoseconds, Available: true}}, 0
}

// changeMetadata serves the four calls through the VFS, as status serves
// stat: by path, or by the descriptor's open file when descriptor is set.
func (handler *linuxCallHandler) changeMetadata(request linuxcompat.Request, dirfd int32, pathAddress uint64, descriptor bool) linuxcompat.Result {
	_, active, errno := handler.vfsStage(request)
	state, found := linuxcompat.FindProcess(handler.root)
	if errno == 0 && !found {
		errno = linuxabi.EIO
	}
	if errno != 0 {
		return linuxErrno(errno)
	}
	change, errno := handler.metadataChange(request)
	if errno != 0 {
		return linuxErrno(errno)
	}
	if !active {
		if descriptor {
			if result, done := handler.beginDescriptorMetadata(request, state, dirfd); done {
				return result
			}
		} else {
			path, trailing, errno := handler.resolvePath(state, dirfd, pathAddress, &linuxResolved)
			device := linuxcompat.DeviceNone
			if errno == 0 {
				device, errno = linuxcompat.LookupDevice(path)
			}
			if errno == 0 && device != linuxcompat.DeviceNone {
				// A device node keeps no mode or time of its own.
				return linuxcompat.Result{}
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
	pending, _ := linuxcompat.SyscallContinuations.Pending(handler.task)
	path, _ := linuxcompat.SyscallContinuations.Paths(handler.task)
	handle := vfs.Handle(0)
	if len(path) == 0 {
		handle = vfs.Handle(pending.Note)
	}
	operation := vfs.MetadataRequest(path, handle, change)
	if !change.SetModified && !change.SetReadOnly {
		operation = vfs.Request{Operation: vfs.OperationStat, Path: path, Handle: handle}
	}
	reply, errno, blocked := handler.vfsExchange(operation, attachPaths)
	if blocked {
		return linuxBlocked
	}
	if errno != 0 {
		return linuxErrno(errno)
	}
	handler.vfsFinish()
	switch {
	case reply.Status == vfs.StatusOK:
	case reply.Status == vfs.StatusIsDirectory && change.SetReadOnly && !change.SetModified:
		// A directory has no read-only attribute; its mode is presentation.
	default:
		return linuxErrno(linuxcompat.MutationErrno(reply.Status))
	}
	if len(path) != 0 && pending.Note&noteTrailing != 0 && operation.Operation == vfs.OperationStat && reply.Kind != ramfs.KindDirectory {
		return linuxErrno(linuxabi.ENOTDIR)
	}
	return linuxcompat.Result{}
}

// beginDescriptorMetadata starts a change of what descriptor fd holds. A
// pipe or a device keeps nothing to change, and the call succeeds there.
func (handler *linuxCallHandler) beginDescriptorMetadata(request linuxcompat.Request, state *linuxcompat.ProcessState, fd int32) (linuxcompat.Result, bool) {
	description, open := state.FDs.Lookup(int(fd))
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
	default:
		return linuxcompat.Result{}, true
	}
	return linuxErrno(errno), errno != 0
}
