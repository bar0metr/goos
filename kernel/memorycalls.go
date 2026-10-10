//go:build uefi

package main

// Shared-memory objects created by processes, handle duplication, and the
// references that keep objects alive (ADR 0107).

import (
	"goos/internal/capability"
	"goos/kernel/executable"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/mm/region"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/shmem"
)

// zeroPhysicalPage clears a page before it becomes part of an object.
func zeroPhysicalPage(physical uint64) bool { return vm.InitializePhysical(physical, nil) }

// dropShared removes one reference to a shared-memory object and, with the
// last, frees it and returns its charge to the process that paid, unless that
// process is the one being torn down, whose account goes whole.
func dropShared(id uint64) {
	object, found := sharedObjects.Find(id)
	if !found {
		return
	}
	payer, charge, freed := sharedObjects.Drop(object, &pageAllocator, vm.PhysicalPage)
	if !freed || process.ID(payer) == releasingProcess {
		return
	}
	if holder, alive := processRegistry.ByID(process.ID(payer)); alive {
		holder.Release(process.ResourcePages, charge)
	}
}

// retainMemory counts a new handle to a memory object — a launch grant, a
// duplicate — as a reference, whichever kind of object it names.
func retainMemory(cap capability.Capability) {
	if object, found := sharedObjects.Find(cap.Object()); found {
		object.Retain()
	} else if object, found := executables.Find(cap.Object()); found {
		object.Retain()
	}
}

// releaseMemory drops the reference a closed memory handle held.
func releaseMemory(cap capability.Capability) {
	if executable.IsObject(cap.Object()) {
		dropExecutable(cap.Object())
		return
	}
	dropShared(cap.Object())
}

// releaseSharedMappings drops the reference each shared mapping of an exiting
// process held. Its region map still describes them; its page tables are about
// to go.
func releaseSharedMappings(exited *process.Process) {
	regions := exited.Regions()
	for index := 0; index < regions.Count(); index++ {
		if entry, ok := regions.At(index); ok && entry.Backing == region.Shared {
			dropShared(entry.Object)
		}
	}
}

// MemoryCreate creates a shared-memory object of pages zeroed pages, charged
// to the caller, and writes a handle with Read, Write, and Transfer to
// address. Any process may create one; what it can do with it is map it and
// pass copies of the handle on.
func (handler nativeCallHandler) MemoryCreate(pages uint64, address uint64) kcall.Status {
	local := thisCPU()
	owner, found := handler.self()
	if !found {
		return kcall.StatusPermissionDenied
	}
	if pages == 0 || pages > shmem.MaxPages {
		return kcall.StatusInvalidArgument
	}
	clear(local.memoryWire[:])
	if !vm.CopyOut(handler.root, address, local.memoryWire[:]) {
		return kcall.StatusInvalidArgument
	}
	if owner.Handles().Available() == 0 {
		return kcall.StatusResourceExhausted
	}
	// The charge is taken before anything is allocated, so a process past its
	// quota allocates nothing.
	charge := uint32(pages) + uint32((pages+511)/512)
	if !owner.Charge(process.ResourcePages, charge) {
		return kcall.StatusResourceExhausted
	}
	objectLimit := shmem.MaxObjectsPerProcess
	if owner == bootInitProcess {
		objectLimit = shmem.MaxObjectsPerSupervisor
	}
	object, created := sharedObjects.CreateWithLimit(&pageAllocator, vm.PhysicalPage, zeroPhysicalPage, uint32(pages), uint64(owner.ID()), objectLimit)
	if !created {
		owner.Release(process.ResourcePages, charge)
		return kcall.StatusResourceExhausted
	}
	granted, ok := object.Grant(owner.Handles(), capability.Read|capability.Write|capability.Transfer)
	if !ok {
		sharedObjects.Drop(object, &pageAllocator, vm.PhysicalPage)
		owner.Release(process.ResourcePages, charge)
		return kcall.StatusResourceExhausted
	}
	putUint64(local.memoryWire[:], uint64(granted))
	if !vm.CopyOut(handler.root, address, local.memoryWire[:]) {
		owner.Handles().Close(granted)
		dropShared(object.ID())
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// HandleDuplicate installs a second handle to the object a memory handle
// names, with rights that are a subset of the original's, and writes it to
// address. A process uses it to pass on narrower authority — write access to
// a buffer, for one request — and keep its own.
func (handler nativeCallHandler) HandleDuplicate(id uint64, rights uint64, address uint64) kcall.Status {
	local := thisCPU()
	owner, found := handler.self()
	if !found {
		return kcall.StatusPermissionDenied
	}
	typeID, original, described := owner.Handles().Describe(handle.ID(id))
	if !described {
		return kcall.StatusNotFound
	}
	if typeID != handle.TypeMemory {
		return kcall.StatusUnsupported
	}
	derived, allowed := original.Derive(capability.Rights(rights))
	if rights == 0 || !allowed {
		return kcall.StatusPermissionDenied
	}
	clear(local.memoryWire[:])
	if !vm.CopyOut(handler.root, address, local.memoryWire[:]) {
		return kcall.StatusInvalidArgument
	}
	duplicate, inserted := owner.Handles().Insert(handle.TypeMemory, derived)
	if !inserted {
		return kcall.StatusResourceExhausted
	}
	retainMemory(derived)
	putUint64(local.memoryWire[:], uint64(duplicate))
	if !vm.CopyOut(handler.root, address, local.memoryWire[:]) {
		owner.Handles().Close(duplicate)
		releaseMemory(derived)
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}
