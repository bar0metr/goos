//go:build uefi

package main

// Device native calls: the console, the clock, system counters, I/O ports,
// and DMA regions, each authorized by a device capability.

import (
	"goos/internal/capability"
	"goos/internal/validation"
	"goos/kernel/arch/amd64"
	"goos/kernel/device"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
)

func (handler nativeCallHandler) ConsoleWrite(id uint64, address uint64, length uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	cap, ok := owner.Handles().Lookup(handle.ID(id), handle.TypeDevice, capability.Write)
	if !ok || cap.Object() != device.ConsoleObject || length == 0 || length > kcall.MaxConsoleWrite {
		return kcall.StatusPermissionDenied
	}
	data := kernelCallData[:int(length)]
	if !vm.CopyIn(handler.root, address, data) {
		if validation.Enabled {
			mark("USER_COPYIN_OK")
		}
		return kcall.StatusInvalidArgument
	}
	console := amd64.NewCOM1()
	for _, value := range data {
		console.WriteByte(value)
	}
	// Validation bookkeeping, not authority: the boot sequence waits until
	// PID 1 has printed and its supervisor has reported. The write itself was
	// authorized by the capability above like any other process's.
	if validation.Enabled && owner == bootInitProcess {
		initAnnounced = 1
		if equalBytes(data, "USERSPACE_SUPERVISOR_OK\n") {
			userspaceSupervisorOK = 1
		}
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) ConsoleRead(device uint64, address uint64, length uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found || length != 1 {
		return kcall.StatusPermissionDenied
	}
	cap, ok := owner.Handles().Lookup(handle.ID(device), handle.TypeDevice, capability.Read)
	if !ok || cap.Object() != 1 {
		return kcall.StatusPermissionDenied
	}
	// The byte comes from the ring the serial interrupt fills, not from the
	// UART: polling the device here is what made a reader spin.
	taken := amd64.ConsoleTake()
	if taken == 0 {
		return kcall.StatusNotFound
	}
	consoleByteWire[0] = byte(taken)
	if !vm.CopyOut(handler.root, address, consoleByteWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// ConsoleInterrupt reports whether Ctrl-C was typed and consumes it. It exists
// so a shell can watch for an interrupt while a command owns the console: the
// alternative, reading bytes and discarding the ones that are not Ctrl-C, takes
// input away from the process the user is typing to.
func (handler nativeCallHandler) ConsoleInterrupt(device uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	cap, ok := owner.Handles().Lookup(handle.ID(device), handle.TypeDevice, capability.Read)
	if !ok || cap.Object() != deviceConsoleObject {
		return kcall.StatusPermissionDenied
	}
	if !amd64.ConsoleTakeInterrupt() {
		return kcall.StatusNotFound
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) ClockRead(address uint64, length uint64) kcall.Status {
	if length != 8 {
		return kcall.StatusInvalidArgument
	}
	putUint64(clockWire[:], amd64.TimerTicks())
	if !vm.CopyOut(handler.root, address, clockWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) SystemInfo(address uint64, length uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found || length != kcall.SystemInfoSize {
		return kcall.StatusInvalidArgument
	}
	statistics := pageAllocator.Statistics()
	putUint64(systemInfoWire[0:8], uint64(owner.ID()))
	putUint64(systemInfoWire[8:16], uint64(processRegistry.Count()))
	putUint64(systemInfoWire[16:24], statistics.ManagedPages)
	putUint64(systemInfoWire[24:32], statistics.FreePages)
	putUint64(systemInfoWire[32:40], amd64.TimerTicks())
	if !vm.CopyOut(handler.root, address, systemInfoWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func portSpecification(specification uint64) (uint16, uint8, bool) {
	if specification>>24 != 0 {
		return 0, 0, false
	}
	offset, width := uint16(specification), uint8(specification>>16)
	return offset, width, width == 1 || width == 2 || width == 4
}

func (handler nativeCallHandler) PortRead(id uint64, specification uint64, address uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	cap, ok := owner.Handles().Lookup(handle.ID(id), handle.TypeDevice, capability.Read)
	base, size, valid := device.DecodePortObject(cap.Object())
	offset, width, specValid := portSpecification(specification)
	if !ok || !valid || !specValid || uint32(offset)+uint32(width) > uint32(size) {
		return kcall.StatusPermissionDenied
	}
	value, read := amd64.PortRead(base+offset, width)
	if !read {
		return kcall.StatusInvalidArgument
	}
	putUint32(portWire[:], value)
	if !vm.CopyOut(handler.root, address, portWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) PortWrite(id uint64, specification uint64, value uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	cap, ok := owner.Handles().Lookup(handle.ID(id), handle.TypeDevice, capability.Write)
	base, size, valid := device.DecodePortObject(cap.Object())
	offset, width, specValid := portSpecification(specification)
	valueValid := width == 1 && value <= 0xff || width == 2 && value <= 0xffff || width == 4 && value <= 0xffffffff
	if !ok || !valid || !specValid || uint32(offset)+uint32(width) > uint32(size) || !valueValid {
		return kcall.StatusPermissionDenied
	}
	if !amd64.PortWrite(base+offset, width, uint32(value)) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) DMAAllocate(id uint64, virtual uint64, address uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found || virtual != vm.UserDMAAddress {
		return kcall.StatusInvalidArgument
	}
	for page := uint64(0); page < 4; page++ {
		if !vm.CanMapUserPage(handler.root, virtual+page*vm.PageSize) {
			return kcall.StatusInvalidArgument
		}
	}
	cap, ok := owner.Handles().Lookup(handle.ID(id), handle.TypeDevice, capability.Write)
	if _, valid := device.DecodeDMAObject(cap.Object()); !ok || !valid {
		return kcall.StatusPermissionDenied
	}
	if !vm.CopyOut(handler.root, address, dmaWire[:]) {
		return kcall.StatusInvalidArgument
	}
	// The DMA region is kernel memory allocated at the driver's request, so it
	// is charged to the driver like any other page it causes to be allocated.
	if !owner.Charge(process.ResourcePages, 4) {
		return kcall.StatusResourceExhausted
	}
	physical, allocated := pageAllocator.AllocateContiguous(4)
	if !allocated {
		owner.Release(process.ResourcePages, 4)
		return kcall.StatusResourceExhausted
	}
	for index := range kernelCallData {
		kernelCallData[index] = 0
	}
	for page := uint64(0); page < 4; page++ {
		if !vm.InitializePhysical(physical+page*vm.PageSize, kernelCallData[:]) {
			for release := uint64(0); release < 4; release++ {
				pageAllocator.Free(physical + release*vm.PageSize)
			}
			owner.Release(process.ResourcePages, 4)
			return kcall.StatusResourceExhausted
		}
	}
	for page := uint64(0); page < 4; page++ {
		if !vm.MapOwnedUserPage(handler.root, virtual+page*vm.PageSize, physical+page*vm.PageSize, vm.Permissions{User: true, Writable: true}) {
			// Mapped pages are owned and return with the address space, so only
			// the unmapped remainder is freed here; the charge stays with the
			// process until its teardown releases the whole account.
			for release := page; release < 4; release++ {
				pageAllocator.Free(physical + release*vm.PageSize)
			}
			return kcall.StatusResourceExhausted
		}
	}
	putUint64(dmaWire[0:8], physical)
	putUint64(dmaWire[8:16], virtual)
	putUint64(dmaWire[16:24], 4*vm.PageSize)
	if !vm.CopyOut(handler.root, address, dmaWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}
