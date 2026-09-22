//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
)

// MemoryMap maps a whole kernel-created shared-memory object into the caller's
// shared window. The mapping is never owned, so the object outlives it.
func (handler nativeCallHandler) MemoryMap(memoryHandle uint64, address uint64, flags uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	writable := flags&kcall.MemoryMapWritable != 0
	required := capability.Read
	if writable {
		required |= capability.Write
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(memoryHandle), handle.TypeMemory, required)
	if !allowed {
		return kcall.StatusPermissionDenied
	}
	object, known := sharedObjects.Find(authority.Object())
	if !known {
		return kcall.StatusPermissionDenied
	}
	size := uint64(object.PageCount()) * vm.PageSize
	if address%vm.PageSize != 0 || address < vm.UserSharedAddress || address >= vm.UserSharedLimit || size > vm.UserSharedLimit-address {
		return kcall.StatusInvalidArgument
	}
	if !object.Map(owner.Handles(), handle.ID(memoryHandle), handler.root, address, 0, object.PageCount(), writable) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// memoryMapRejectsInvalid checks the refusal paths against PID 1 without
// mapping anything, so the exec page stays free for PID 1 to place.
func memoryMapRejectsInvalid(root uint64, device handle.ID, memory handle.ID) bool {
	caller := nativeCallHandler{root: root}
	return caller.MemoryMap(uint64(device), vm.UserSharedAddress, 0) == kcall.StatusPermissionDenied &&
		caller.MemoryMap(uint64(memory), vm.UserSharedAddress+1, 0) == kcall.StatusInvalidArgument &&
		caller.MemoryMap(uint64(memory), vm.UserSharedLimit, kcall.MemoryMapWritable) == kcall.StatusInvalidArgument &&
		caller.MemoryMap(uint64(memory), vm.UserSharedAddress-vm.PageSize, 0) == kcall.StatusInvalidArgument
}
