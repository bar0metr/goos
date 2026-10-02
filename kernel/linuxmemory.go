//go:build uefi

package main

import (
	"encoding/binary"

	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/kcall"
	"goos/kernel/mm/region"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
)

var linuxFSBaseResult [8]byte

func (handler *linuxCallHandler) Brk(request linuxcompat.Request) linuxcompat.Result {
	state, found := linuxcompat.FindProcess(handler.root)
	owner, owned := processRegistry.ByRoot(handler.root)
	if !found || !owned {
		return linuxcompat.Result{Errno: linuxabi.ENOMEM}
	}
	requested := request.Arguments[0]
	if requested == 0 {
		return linuxcompat.Result{Value: state.BrkCurrent}
	}
	if requested < state.BrkBase || requested > state.BrkLimit {
		return linuxcompat.Result{Value: state.BrkCurrent}
	}
	oldPage := pageCeiling(state.BrkCurrent)
	newPage := pageCeiling(requested)
	if newPage > oldPage {
		// As for mmap: growth the page quota cannot back is refused, which
		// brk reports by leaving the break where it was.
		if (newPage-oldPage)/vm.PageSize+faultTables > uint64(owner.Available(process.ResourcePages)) {
			return linuxcompat.Result{Value: state.BrkCurrent}
		}
		entry := region.Region{Base: oldPage, Length: newPage - oldPage, Protection: region.Read | region.Write, Backing: region.Anonymous}
		if owner.Regions().Insert(entry) != region.OK {
			return linuxcompat.Result{Value: state.BrkCurrent}
		}
	} else if newPage < oldPage {
		if owner.Regions().Remove(newPage, oldPage-newPage, false) != region.OK {
			return linuxcompat.Result{Value: state.BrkCurrent}
		}
		releasePages(owner, vm.UnmapUserRange(&pageAllocator, handler.root, newPage, oldPage-newPage))
	}
	state.BrkCurrent = requested
	return linuxcompat.Result{Value: requested}
}

func (handler *linuxCallHandler) Mmap(request linuxcompat.Request) linuxcompat.Result {
	length, protection, errno := linuxcompat.AnonymousMapping(request.Arguments, vm.PageSize)
	if errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return linuxcompat.Result{Errno: linuxabi.ENOMEM}
	}
	// A mapping is lazy, and a page the quota cannot pay for would kill the
	// process when it is touched. A mapping larger than what the quota has
	// left is refused here instead, where the program can handle it.
	if length/vm.PageSize+faultTables > uint64(owner.Available(process.ResourcePages)) {
		return linuxcompat.Result{Errno: linuxabi.ENOMEM}
	}
	address, free := owner.Regions().FindFree(length, vm.UserMapBase, vm.UserMapLimit)
	if !free {
		return linuxcompat.Result{Errno: linuxabi.ENOMEM}
	}
	status := owner.Regions().Insert(region.Region{Base: address, Length: length, Protection: region.Protection(protection), Backing: region.Anonymous})
	if status != region.OK {
		return linuxcompat.Result{Errno: linuxRegionErrno(status)}
	}
	return linuxcompat.Result{Value: address}
}

func (handler *linuxCallHandler) Munmap(request linuxcompat.Request) linuxcompat.Result {
	address := request.Arguments[0]
	length, errno := linuxcompat.PageRange(address, request.Arguments[1], vm.PageSize)
	if errno != 0 || !vm.ValidateUserRange(address, length) {
		return linuxcompat.Result{Errno: linuxabi.EINVAL}
	}
	status := nativeCallHandler{root: handler.root}.VMUnmap(address, length)
	if status != kcall.StatusOK {
		return linuxcompat.Result{Errno: linuxVMErrno(status)}
	}
	return linuxcompat.Result{}
}

func (handler *linuxCallHandler) Mprotect(request linuxcompat.Request) linuxcompat.Result {
	address := request.Arguments[0]
	length, errno := linuxcompat.PageRange(address, request.Arguments[1], vm.PageSize)
	protection, protectionErrno := linuxcompat.MemoryProtection(request.Arguments[2])
	if errno != 0 || protectionErrno != 0 || !vm.ValidateUserRange(address, length) {
		return linuxcompat.Result{Errno: linuxabi.EINVAL}
	}
	status := nativeCallHandler{root: handler.root}.VMProtect(address, length, uint64(protection))
	if status == kcall.StatusPermissionDenied {
		status = handler.restrictImage(address, length, region.Protection(protection))
	}
	if status != kcall.StatusOK {
		return linuxcompat.Result{Errno: linuxVMErrno(status)}
	}
	return linuxcompat.Result{}
}

// restrictImage lets a Linux program take rights away from its own image
// segments, which the native call refuses because they are pinned. glibc
// makes its relocated data (PT_GNU_RELRO) read-only this way before main.
// Every page of a Linux image is a private copy (demandFault and
// installExecutablePage allocate and fill it), never a frame another
// process maps, so a narrower protection affects this process alone. Adding
// a right stays refused.
func (handler *linuxCallHandler) restrictImage(address, length uint64, requested region.Protection) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	regions := owner.Regions()
	count := regions.Overlapping(address, length, &vmPieces)
	for index := 0; index < count; index++ {
		if vmPieces[index].Region.Backing != region.Image {
			return kcall.StatusPermissionDenied
		}
	}
	if status := regionStatus(regions.Restrict(address, length, requested)); status != kcall.StatusOK {
		return status
	}
	if !vm.ProtectUserRange(handler.root, address, length, regionPermissions(requested)) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (handler *linuxCallHandler) Madvise(request linuxcompat.Request) linuxcompat.Result {
	address := request.Arguments[0]
	length, errno := linuxcompat.PageRange(address, request.Arguments[1], vm.PageSize)
	if errno != 0 || request.Arguments[2] != linuxabi.MadvDontNeed || !vm.ValidateUserRange(address, length) {
		return linuxcompat.Result{Errno: linuxabi.EINVAL}
	}
	status := nativeCallHandler{root: handler.root}.VMDiscard(address, length)
	if status != kcall.StatusOK {
		return linuxcompat.Result{Errno: linuxVMErrno(status)}
	}
	return linuxcompat.Result{}
}

func (handler *linuxCallHandler) ArchPrctl(request linuxcompat.Request) linuxcompat.Result {
	operation, argument := request.Arguments[0], request.Arguments[1]
	switch operation {
	case linuxabi.ArchSetFS:
		if !linuxcompat.UserFSBase(argument, vm.UserAddressLimit) || !amd64.SetTaskFSBase(handler.task, argument) {
			return linuxcompat.Result{Errno: linuxabi.EPERM}
		}
		return linuxcompat.Result{}
	case linuxabi.ArchGetFS:
		base, found := amd64.TaskFSBase(handler.task)
		if !found {
			return linuxcompat.Result{Errno: linuxabi.EINVAL}
		}
		binary.LittleEndian.PutUint64(linuxFSBaseResult[:], base)
		if !vm.CopyOut(handler.root, argument, linuxFSBaseResult[:]) {
			return linuxcompat.Result{Errno: linuxabi.EFAULT}
		}
		return linuxcompat.Result{}
	default:
		return linuxcompat.Result{Errno: linuxabi.EINVAL}
	}
}

func pageCeiling(address uint64) uint64 {
	return (address + vm.PageSize - 1) &^ (vm.PageSize - 1)
}

func linuxRegionErrno(status region.Status) linuxabi.Errno {
	switch status {
	case region.Exhausted:
		return linuxabi.ENOMEM
	case region.Overlap:
		return linuxabi.EEXIST
	default:
		return linuxabi.EINVAL
	}
}

func linuxVMErrno(status kcall.Status) linuxabi.Errno {
	switch status {
	case kcall.StatusResourceExhausted:
		return linuxabi.ENOMEM
	case kcall.StatusPermissionDenied:
		return linuxabi.EACCES
	case kcall.StatusNotFound:
		return linuxabi.ENOMEM
	default:
		return linuxabi.EINVAL
	}
}
