//go:build uefi

package main

// The character devices a Linux process can open: /dev/null and /dev/zero.
// The kernel serves them from the descriptor alone, with no VFS exchange and
// nothing to wait for.

import (
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
)

// deviceRead serves read, readv, and pread64. /dev/null is at its end from
// the first byte. /dev/zero fills the destination with zeros, up to a bound
// and up to the first page the process cannot write.
func (handler *linuxCallHandler) deviceRead(request linuxcompat.Request, description linuxcompat.FileDescription, vector bool) linuxcompat.Result {
	if description.Flags&linuxabi.OpenAccess == linuxabi.OpenWriteOnly {
		return linuxErrno(linuxabi.EBADF)
	}
	if linuxcompat.DeviceOf(description) != linuxcompat.DeviceZero {
		// The vector of a readv is still read and checked, as on Linux.
		if vector {
			_, errno := handler.copyIOVec(request)
			return linuxErrno(errno)
		}
		// A device that never touches the memory accepts any range in the
		// user half, mapped or not.
		if !userRange(request.Arguments[1], request.Arguments[2]) {
			return linuxErrno(linuxabi.EFAULT)
		}
		return linuxcompat.Result{}
	}
	limit, wire, errno := handler.readLimit(request, vector, linuxcompat.MaxDeviceTransfer)
	if errno != 0 || limit == 0 {
		return linuxErrno(errno)
	}
	clear(linuxIOBuffer[:])
	for delivered := uint64(0); delivered < limit; {
		chunk := uint64(min64(limit-delivered, len(linuxIOBuffer)))
		if !handler.deliver(request, wire, delivered, linuxIOBuffer[:chunk], nil, chunk) {
			if delivered != 0 {
				return linuxcompat.Result{Value: delivered}
			}
			return linuxErrno(linuxabi.EFAULT)
		}
		delivered += chunk
	}
	return linuxcompat.Result{Value: limit}
}

// deviceWrite serves write, writev, and pwrite64. Both devices discard what
// they are given and report all of it written. They read none of it, so the
// bytes need not be mapped.
func (handler *linuxCallHandler) deviceWrite(request linuxcompat.Request, description linuxcompat.FileDescription, vector bool) linuxcompat.Result {
	if description.Flags&linuxabi.OpenAccess == linuxabi.OpenReadOnly {
		return linuxErrno(linuxabi.EBADF)
	}
	total := request.Arguments[2]
	if vector {
		wire, errno := handler.copyIOVec(request)
		if errno != 0 {
			return linuxErrno(errno)
		}
		total = 0
		for index := 0; index < len(wire)/linuxabi.IOVecSize; index++ {
			_, length := linuxcompat.IOVecEntry(wire, index)
			if length > linuxcompat.MaxTransfer-total {
				length = linuxcompat.MaxTransfer - total
			}
			total += length
		}
	} else if !userRange(request.Arguments[1], total) {
		return linuxErrno(linuxabi.EFAULT)
	}
	if total > linuxcompat.MaxTransfer {
		total = linuxcompat.MaxTransfer
	}
	return linuxcompat.Result{Value: total}
}

// openDevice gives the process a descriptor for a device. The description
// holds the device and the access mode; there is no position to keep.
func (handler *linuxCallHandler) openDevice(device linuxcompat.Device, intent linuxcompat.OpenIntent, trailing bool) linuxcompat.Result {
	if errno := linuxcompat.DeviceOpenErrno(device, intent, trailing); errno != 0 {
		return linuxErrno(errno)
	}
	description := linuxcompat.FileDescription{Kind: linuxcompat.FDSynthetic, Flags: intent.StatusFlags(), Object: uint64(device)}
	fd, errno := linuxcompat.AllocateFD(handler.root, description, intent.CloseOnExec)
	if errno != 0 {
		return linuxErrno(errno)
	}
	return linuxcompat.Result{Value: uint64(fd)}
}
