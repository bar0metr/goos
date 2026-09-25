//go:build uefi

package main

// Native virtual memory (ADR 0104): demand-zero fault resolution and the
// calls that reserve, release, reprotect, discard, and describe regions.

import (
	"goos/internal/capability"
	"goos/internal/logfmt"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/mm/region"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/shmem"
)

// faultTables bounds the page tables one fault can need: a page directory
// pointer table, a page directory, and a page table.
const faultTables = 3

var (
	vmDescriptor [kcall.VMDescriptorSize]byte
	vmRecord     [kcall.VMRecordSize]byte
	// userFaultsResolved counts demand-zero pages supplied on a fault.
	userFaultsResolved uint64
)

// Kinds of user page fault that retire the process, for the log.
const (
	faultInvalid    = uint64(1)
	faultGuard      = uint64(2)
	faultPermission = uint64(3)
	faultQuota      = uint64(4)
)

// resolveUserFaultEntry is called by the exception path for a page fault in
// user mode, with interrupts disabled, on the task's kernel stack. It returns
// one when a page was supplied and the instruction can be retried, zero when
// the process has to be retired.
//
//export goos_resolve_user_fault
func resolveUserFaultEntry(root uint64, address uint64, errorCode uint64) uint64 {
	const (
		faultPresent = 1 << 0
		faultWrite   = 1 << 1
		faultFetch   = 1 << 4
	)
	page := address &^ (vm.PageSize - 1)
	kind := faultPermission
	if errorCode&faultPresent == 0 {
		var resolved bool
		resolved, kind = demandFault(root, page, errorCode&faultWrite != 0, errorCode&faultFetch != 0)
		if resolved {
			userFaultsResolved++
			return 1
		}
	}
	kernelLogUint(logfmt.Warn, userFaultReason(kind), "address", address)
	return 0
}

func userFaultReason(kind uint64) string {
	switch kind {
	case faultGuard:
		return "user fault: guard page"
	case faultPermission:
		return "user fault: access not permitted"
	case faultQuota:
		return "user fault: page quota exhausted"
	}
	return "user fault: no mapping"
}

// resolveCopyFault is the resolver the kernel's own copies into and out of
// user memory use (vm.SetFaultResolver).
func resolveCopyFault(root uint64, page uint64, write bool) bool {
	resolved, _ := demandFault(root, page, write, false)
	return resolved
}

// demandFault supplies a zeroed page for an absent page of an anonymous or
// image region whose protection allows the access, charging it and any page
// tables it needs to the process. It reports why when it cannot.
func demandFault(root uint64, page uint64, write bool, fetch bool) (bool, uint64) {
	owner, found := processRegistry.ByRoot(root)
	if !found {
		return false, faultInvalid
	}
	entry, ok := owner.Regions().Find(page)
	if !ok {
		return false, faultInvalid
	}
	if entry.Backing == region.Guard {
		return false, faultGuard
	}
	need := region.Read
	if write {
		need |= region.Write
	}
	if fetch {
		need |= region.Execute
	}
	if !entry.Backing.DemandZero() || entry.Protection&need != need || vm.UserPageMapped(root, page) {
		return false, faultPermission
	}
	if owner.Available(process.ResourcePages) < 1+faultTables {
		return false, faultQuota
	}
	physical, allocated := pageAllocator.Allocate()
	if !allocated {
		return false, faultQuota
	}
	if !vm.InitializePhysical(physical, nil) {
		pageAllocator.Free(physical)
		return false, faultQuota
	}
	tables, mapped := vm.MapUserPageAt(&pageAllocator, root, page, physical, regionPermissions(entry.Protection), true)
	if tables != 0 {
		owner.Charge(process.ResourcePages, uint32(tables))
	}
	if !mapped {
		pageAllocator.Free(physical)
		return false, faultInvalid
	}
	owner.Charge(process.ResourcePages, 1)
	return true, 0
}

// regionPermissions is the leaf permission of a region protection. A region
// with no access at all maps nothing a user can reach.
func regionPermissions(protection region.Protection) vm.Permissions {
	return vm.Permissions{
		User:       protection&region.Read != 0,
		Writable:   protection&region.Write != 0,
		Executable: protection&region.Execute != 0,
	}
}

// VMMap reserves anonymous demand-zero memory.
func (handler nativeCallHandler) VMMap(descriptor uint64, protection uint64, flags uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	if protection&kcall.VMExecute != 0 {
		// Executable anonymous memory needs a write-then-seal interface.
		return kcall.StatusPermissionDenied
	}
	if !vm.CopyIn(handler.root, descriptor, vmDescriptor[:]) {
		return kcall.StatusInvalidArgument
	}
	address, length := decodeUint64(vmDescriptor[0:8]), decodeUint64(vmDescriptor[8:16])
	if length == 0 || length%vm.PageSize != 0 || length > vm.UserMapLimit-vm.UserMapBase {
		return kcall.StatusInvalidArgument
	}
	regions := owner.Regions()
	if flags&kcall.VMMapFixed == 0 {
		base, ok := regions.FindFree(length, vm.UserMapBase, vm.UserMapLimit)
		if !ok {
			return kcall.StatusResourceExhausted
		}
		address = base
	}
	switch regions.Insert(region.Region{Base: address, Length: length, Protection: region.Protection(protection), Backing: region.Anonymous}) {
	case region.OK:
	case region.Overlap:
		return kcall.StatusAlreadyExists
	case region.Exhausted:
		return kcall.StatusResourceExhausted
	default:
		return kcall.StatusInvalidArgument
	}
	putUint64(vmDescriptor[0:8], address)
	if !vm.CopyOut(handler.root, descriptor, vmDescriptor[:8]) {
		regions.Remove(address, length, false)
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// VMUnmap removes a range: every region piece in it and every page mapped
// there. Owned pages are freed and uncharged; shared and image pages are only
// unmapped. Pinned regions cannot be removed.
func (handler nativeCallHandler) VMUnmap(address uint64, length uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	if status := regionStatus(owner.Regions().Remove(address, length, false)); status != kcall.StatusOK {
		return status
	}
	releasePages(owner, vm.UnmapUserRange(&pageAllocator, handler.root, address, length))
	return kcall.StatusOK
}

// VMDiscard frees the pages of an anonymous range but keeps the reservation,
// so the range reads as zeros again and costs nothing until touched.
func (handler nativeCallHandler) VMDiscard(address uint64, length uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	regions := owner.Regions()
	if !regions.Covered(address, length, false) {
		if regions.Covered(address, length, true) {
			return kcall.StatusPermissionDenied
		}
		return kcall.StatusInvalidArgument
	}
	count := regions.Overlapping(address, length, &vmPieces)
	for index := 0; index < count; index++ {
		if vmPieces[index].Region.Backing != region.Anonymous {
			return kcall.StatusPermissionDenied
		}
	}
	releasePages(owner, vm.UnmapUserRange(&pageAllocator, handler.root, address, length))
	return kcall.StatusOK
}

// vmPieces is the scratch the protection and discard checks inspect.
var vmPieces region.Pieces

// VMProtect changes the protection of a range covered by unpinned regions.
// Anonymous memory may become inaccessible, read-only, or read-write, never
// executable; shared and device mappings keep the protection they were mapped
// with.
func (handler nativeCallHandler) VMProtect(address uint64, length uint64, protection uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	requested := region.Protection(protection)
	if !requested.Valid() {
		return kcall.StatusInvalidArgument
	}
	regions := owner.Regions()
	count := regions.Overlapping(address, length, &vmPieces)
	for index := 0; index < count; index++ {
		backing := vmPieces[index].Region.Backing
		if backing != region.Anonymous || requested&region.Execute != 0 {
			return kcall.StatusPermissionDenied
		}
	}
	if status := regionStatus(regions.Protect(address, length, requested)); status != kcall.StatusOK {
		return status
	}
	if !vm.ProtectUserRange(handler.root, address, length, regionPermissions(requested)) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// VMQuery describes the region containing address.
func (handler nativeCallHandler) VMQuery(address uint64, record uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	entry, ok := owner.Regions().Find(address)
	if !ok {
		return kcall.StatusNotFound
	}
	clear(vmRecord[:])
	putUint64(vmRecord[0:8], entry.Base)
	putUint64(vmRecord[8:16], entry.Length)
	vmRecord[16], vmRecord[17] = byte(entry.Protection), byte(entry.Backing)
	if entry.Flags&region.Pinned != 0 {
		vmRecord[18] = 1
	}
	if !vm.CopyOut(handler.root, record, vmRecord[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// MemoryMap maps a whole kernel-created shared-memory object at a free
// address the caller names, or, with MemoryMapAnywhere, at one the kernel
// chooses and writes to the eight bytes address names. The mapping is never
// owned, so the object outlives it.
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
	regions := owner.Regions()
	target := address
	if flags&kcall.MemoryMapAnywhere != 0 {
		base, ok := regions.FindFree(size, vm.UserMapBase, vm.UserMapLimit)
		if !ok {
			return kcall.StatusResourceExhausted
		}
		target = base
		putUint64(vmDescriptor[0:8], target)
		if !vm.CopyOut(handler.root, address, vmDescriptor[:8]) {
			return kcall.StatusInvalidArgument
		}
	}
	protection := region.Read
	if writable {
		protection |= region.Write
	}
	if status := regionStatus(regions.Insert(region.Region{Base: target, Length: size, Protection: protection, Backing: region.Shared, Object: authority.Object()})); status != kcall.StatusOK {
		if status == kcall.StatusAlreadyExists {
			return kcall.StatusInvalidArgument
		}
		return status
	}
	if owner.Available(process.ResourcePages) < uint32(faultTables*(object.PageCount()/512+1)) {
		regions.Remove(target, size, false)
		return kcall.StatusResourceExhausted
	}
	tables, mapped := object.Map(&pageAllocator, owner.Handles(), handle.ID(memoryHandle), handler.root, target, 0, object.PageCount(), writable)
	if tables != 0 {
		owner.Charge(process.ResourcePages, uint32(tables))
	}
	if !mapped {
		vm.UnmapUserRange(&pageAllocator, handler.root, target, size)
		regions.Remove(target, size, false)
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
		caller.MemoryMap(uint64(memory), vm.UserAddressLimit, kcall.MemoryMapWritable) == kcall.StatusInvalidArgument &&
		caller.MemoryMap(uint64(memory), vm.PageSize, 0) == kcall.StatusInvalidArgument
}

// mapPinnedShared maps a shared-memory object into a process at a fixed
// address as a pinned region, for the pages the kernel shares with PID 1 at
// boot. The page tables it creates are counted by the caller's allocator
// difference, not charged here.
func mapPinnedShared(owner *process.Process, object *shmem.Object, memory handle.ID, address uint64) bool {
	size := uint64(object.PageCount()) * vm.PageSize
	if owner.Regions().Insert(region.Region{Base: address, Length: size, Protection: region.Read | region.Write, Backing: region.Shared, Flags: region.Pinned, Object: object.ID()}) != region.OK {
		return false
	}
	_, mapped := object.Map(&pageAllocator, owner.Handles(), memory, owner.AddressSpace().Root, address, 0, object.PageCount(), true)
	return mapped
}

// recordKernelRegion adds a region for a mapping the kernel makes on a
// process's behalf, or reports why it cannot.
func recordKernelRegion(owner *process.Process, entry region.Region) kcall.Status {
	status := regionStatus(owner.Regions().Insert(entry))
	if status == kcall.StatusAlreadyExists {
		return kcall.StatusInvalidArgument
	}
	return status
}

func releasePages(owner *process.Process, pages uint64) {
	if pages != 0 {
		owner.Release(process.ResourcePages, uint32(pages))
	}
}

func regionStatus(status region.Status) kcall.Status {
	switch status {
	case region.OK:
		return kcall.StatusOK
	case region.Overlap:
		return kcall.StatusAlreadyExists
	case region.NotMapped:
		return kcall.StatusNotFound
	case region.PermissionDenied:
		return kcall.StatusPermissionDenied
	case region.Exhausted:
		return kcall.StatusResourceExhausted
	}
	return kcall.StatusInvalidArgument
}
