//go:build uefi

package main

import (
	"goos/internal/capability"
	bootuefi "goos/kernel/boot/uefi"
	"goos/kernel/device"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/mm/region"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
)

// ImageMap maps the boot system image into the caller and writes back the
// address and length it landed at. The filesystem service uses it to serve
// /system from the same bytes the launch path resolves, so what a listing shows
// cannot drift from what actually runs.
//
// The mapping is read-only and never executable, which keeps the system image
// immutable to the one process allowed to read it. The pages belong to the
// firmware reservation rather than to the caller, so they are mapped unowned
// and the caller's teardown leaves them intact for the next mount.
func (handler nativeCallHandler) ImageMap(id uint64, virtual uint64, address uint64) kcall.Status {
	local := thisCPU()
	owner, found := processRegistry.ByRoot(handler.root)
	if !found || virtual != vm.UserImageAddress {
		return kcall.StatusInvalidArgument
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(id), handle.TypeDevice, capability.Read)
	if _, valid := device.DecodeImageObject(authority.Object()); !allowed || !valid {
		return kcall.StatusPermissionDenied
	}
	image := bootuefi.Initramfs(&bootInfo)
	physical, size := bootInfo.InitramfsAddress, uint64(len(image))
	if size == 0 || physical%vm.PageSize != 0 {
		return kcall.StatusNotFound
	}
	pages := (size + vm.PageSize - 1) / vm.PageSize
	if virtual+pages*vm.PageSize > vm.UserImageLimit {
		return kcall.StatusResourceExhausted
	}
	// The descriptor pointer is validated before anything is mapped, so a bad
	// one cannot leave a partially mapped image behind.
	for index := range local.imageWire {
		local.imageWire[index] = 0
	}
	if !vm.CopyOut(handler.root, address, local.imageWire[:]) {
		return kcall.StatusInvalidArgument
	}
	// The window is a pinned read-only region; its page tables are created
	// as the pages are mapped and charged to the caller. A second call finds
	// the window taken and fails here.
	if recordKernelRegion(owner, region.Region{Base: virtual, Length: pages * vm.PageSize, Protection: region.Read, Backing: region.Device, Flags: region.Pinned}) != kcall.StatusOK {
		return kcall.StatusInvalidArgument
	}
	for page := uint64(0); page < pages; page++ {
		if owner.Available(process.ResourcePages) < faultTables {
			return kcall.StatusResourceExhausted
		}
		tables, mapped := vm.MapUserPageAt(&pageAllocator, handler.root, virtual+page*vm.PageSize, physical+page*vm.PageSize, vm.Permissions{User: true}, false)
		if tables != 0 {
			owner.Charge(process.ResourcePages, uint32(tables))
		}
		if !mapped {
			return kcall.StatusResourceExhausted
		}
	}
	putUint64(local.imageWire[0:8], virtual)
	putUint64(local.imageWire[8:16], size)
	if !vm.CopyOut(handler.root, address, local.imageWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}
