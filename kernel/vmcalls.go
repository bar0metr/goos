//go:build uefi

package main

// Native virtual memory (ADR 0104): demand-zero fault resolution and the
// calls that reserve, release, reprotect, discard, and describe regions.

import (
	"sync/atomic"

	"goos/internal/capability"
	"goos/internal/logfmt"
	bootuefi "goos/kernel/boot/uefi"
	"goos/kernel/executable"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/loader"
	"goos/kernel/mm/region"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/shmem"
)

// faultTables bounds the page tables one fault can need: a page directory
// pointer table, a page directory, and a page table.
const faultTables = 3

var (

	// userFaultsResolved counts demand-zero pages supplied on a fault.
	userFaultsResolved atomic.Uint64
	// userFaultsRetried counts faults on pages that already allowed the
	// access when the kernel looked.
	userFaultsRetried atomic.Uint64
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
// the process has to be retired. rip is the faulting instruction, for the
// log.
//
//export goos_resolve_user_fault
func resolveUserFaultEntry(root uint64, address uint64, errorCode uint64, rip uint64) uint64 {
	const (
		faultPresent  = 1 << 0
		faultWrite    = 1 << 1
		faultReserved = 1 << 3
		faultFetch    = 1 << 4
	)
	page := address &^ (vm.PageSize - 1)
	// Another task of the process may have mapped the page, or widened its
	// protection, on another processor since this one translated the
	// address. The access is retried.
	if errorCode&faultReserved == 0 && vm.UserPageAllows(root, page, errorCode&faultWrite != 0, errorCode&faultFetch != 0) {
		userFaultsRetried.Add(1)
		return 1
	}
	kind := faultPermission
	if errorCode&faultPresent == 0 {
		var resolved bool
		resolved, kind = demandFault(root, page, errorCode&faultWrite != 0, errorCode&faultFetch != 0)
		if resolved {
			userFaultsResolved.Add(1)
			return 1
		}
	}
	kernelLogUints(logfmt.Warn, userFaultReason(kind), "address", address, "rip", rip)
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
	if entry.Backing == region.Image && entry.Flags&region.FromObject != 0 {
		if object, found := executables.Find(entry.Object); found && object.FileBacked() {
			pending, failed := requestExecutablePage(owner, entry, page)
			if pending {
				return true, 0
			}
			if failed {
				return false, faultInvalid
			}
		}
	}
	physical, allocated := pageAllocator.Allocate()
	if !allocated {
		return false, faultQuota
	}
	if !vm.InitializePhysical(physical, nil) {
		pageAllocator.Free(physical)
		return false, faultQuota
	}
	if entry.Backing == region.Image && !fillFromOrigin(entry, page, physical) {
		pageAllocator.Free(physical)
		return false, faultInvalid
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

// pagingObject is the executable object a fault is reading, for objectPage.
var pagingObject *executable.Object

// objectPage is the page source of pagingObject; see executablePage.
func objectPage(index uint64) ([]byte, bool) {
	if pagingObject == nil {
		return nil, false
	}
	frame, ok := pagingObject.Frame(vm.PhysicalPage, index)
	if !ok {
		return nil, false
	}
	return vm.PhysicalPage(frame)
}

// fillFromOrigin copies into the zeroed page at physical the bytes an image
// region's page holds in its origin: an extent of the system image or a sealed
// snapshot object (ADR 0106). File-backed sources are requested asynchronously
// before this path. A page past the segment's file bytes stays zero.
func fillFromOrigin(entry region.Region, page uint64, physical uint64) bool {
	within := page - entry.Base
	if within >= entry.Data {
		return true
	}
	count := min(vm.PageSize, entry.Data-within)
	target, ok := vm.PhysicalPage(physical)
	if !ok {
		return false
	}
	offset := entry.Offset + within
	if entry.Flags&region.FromObject != 0 {
		object, found := executables.Find(entry.Object)
		if !found || !object.Sealed() {
			return false
		}
		pagingObject = object
		source, valid := loader.Paged(objectPage, vm.PageSize, object.Size())
		copied := valid && source.CopyOut(offset, target[:count])
		pagingObject = nil
		return copied
	}
	image, _, inside := loader.ImageExtent(bootuefi.Initramfs(&bootInfo), entry.Object>>32, entry.Object&0xffffffff)
	return inside && loader.Contiguous(image).CopyOut(offset, target[:count])
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
	local := thisCPU()
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	if protection&kcall.VMExecute != 0 {
		// Executable anonymous memory needs a write-then-seal interface.
		return kcall.StatusPermissionDenied
	}
	if !vm.CopyIn(handler.root, descriptor, local.vmDescriptor[:]) {
		return kcall.StatusInvalidArgument
	}
	address, length := decodeUint64(local.vmDescriptor[0:8]), decodeUint64(local.vmDescriptor[8:16])
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
	putUint64(local.vmDescriptor[0:8], address)
	if !vm.CopyOut(handler.root, descriptor, local.vmDescriptor[:8]) {
		regions.Remove(address, length, false)
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// VMUnmap removes a range: every region piece in it and every page mapped
// there. Owned pages are freed and uncharged; shared and image pages are only
// unmapped. Pinned regions cannot be removed.
func (handler nativeCallHandler) VMUnmap(address uint64, length uint64) kcall.Status {
	local := thisCPU()
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	// A shared mapping is one reference to its object, so it goes whole or
	// not at all.
	regions := owner.Regions()
	count := regions.Overlapping(address, length, &local.vmPieces)
	for index := 0; index < count; index++ {
		piece := local.vmPieces[index]
		if piece.Region.Backing == region.Shared && (piece.Base != piece.Region.Base || piece.Length != piece.Region.Length) {
			return kcall.StatusInvalidArgument
		}
	}
	if status := regionStatus(regions.Remove(address, length, false)); status != kcall.StatusOK {
		return status
	}
	releasePages(owner, vm.UnmapUserRange(&pageAllocator, handler.root, address, length))
	for index := 0; index < count; index++ {
		if local.vmPieces[index].Region.Backing == region.Shared {
			dropShared(local.vmPieces[index].Region.Object)
		}
	}
	return kcall.StatusOK
}

// VMDiscard frees the pages of an anonymous range but keeps the reservation,
// so the range reads as zeros again and costs nothing until touched.
func (handler nativeCallHandler) VMDiscard(address uint64, length uint64) kcall.Status {
	local := thisCPU()
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
	count := regions.Overlapping(address, length, &local.vmPieces)
	for index := 0; index < count; index++ {
		if local.vmPieces[index].Region.Backing != region.Anonymous {
			return kcall.StatusPermissionDenied
		}
	}
	releasePages(owner, vm.UnmapUserRange(&pageAllocator, handler.root, address, length))
	return kcall.StatusOK
}

// VMProtect changes the protection of a range covered by unpinned regions.
// Anonymous memory may become inaccessible, read-only, or read-write, never
// executable; shared and device mappings keep the protection they were mapped
// with.
func (handler nativeCallHandler) VMProtect(address uint64, length uint64, protection uint64) kcall.Status {
	local := thisCPU()
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	requested := region.Protection(protection)
	if !requested.Valid() {
		return kcall.StatusInvalidArgument
	}
	regions := owner.Regions()
	count := regions.Overlapping(address, length, &local.vmPieces)
	for index := 0; index < count; index++ {
		backing := local.vmPieces[index].Region.Backing
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
	local := thisCPU()
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	entry, ok := owner.Regions().Find(address)
	if !ok {
		return kcall.StatusNotFound
	}
	clear(local.vmRecord[:])
	putUint64(local.vmRecord[0:8], entry.Base)
	putUint64(local.vmRecord[8:16], entry.Length)
	local.vmRecord[16], local.vmRecord[17] = byte(entry.Protection), byte(entry.Backing)
	if entry.Flags&region.Pinned != 0 {
		local.vmRecord[18] = 1
	}
	if !vm.CopyOut(handler.root, record, local.vmRecord[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// MemoryMap maps a whole kernel-created shared-memory object at a free
// address the caller names, or, with MemoryMapAnywhere, at one the kernel
// chooses and writes to the eight bytes address names. The mapping is never
// owned, so the object outlives it.
func (handler nativeCallHandler) MemoryMap(memoryHandle uint64, address uint64, flags uint64) kcall.Status {
	local := thisCPU()
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
		putUint64(local.vmDescriptor[0:8], target)
		if !vm.CopyOut(handler.root, address, local.vmDescriptor[:8]) {
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
	if owner.Available(process.ResourcePages) < uint32(faultTables*(uint64(object.PageCount())/512+1)) {
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
	// The mapping keeps the object alive after every handle has closed.
	object.Retain()
	return kcall.StatusOK
}

// memoryMapRejectsInvalid checks the refusal paths against PID 1 without
// mapping anything, so the exec page stays free for PID 1 to place.
func memoryMapRejectsInvalid(root uint64, device handle.ID, memory handle.ID) bool {
	caller := nativeCallHandler{root: root}
	return caller.MemoryMap(uint64(device), vm.UserSharedAddress, 0) == kcall.StatusPermissionDenied &&
		caller.MemoryMap(uint64(memory), vm.UserSharedAddress+1, 0) == kcall.StatusInvalidArgument &&
		caller.MemoryMap(uint64(memory), vm.UserAddressLimit, kcall.MemoryMapWritable) == kcall.StatusInvalidArgument &&
		caller.MemoryMap(uint64(memory), 0, 0) == kcall.StatusInvalidArgument
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
	if mapped {
		object.Retain()
	}
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
