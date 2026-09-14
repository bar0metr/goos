//go:build uefi && amd64

package uefi

import (
	deviceuefi "device/uefi"
	"goos/kernel/boot"
	"unsafe"
)

const (
	memoryMapPages = uintptr(64)
	memoryMapBytes = memoryMapPages * uintptr(boot.PageSize)
	maxExitRetries = 3
)

type memoryDescriptor struct {
	Type          uint32
	Padding       uint32
	PhysicalStart uint64
	VirtualStart  uint64
	NumberOfPages uint64
	Attribute     uint64
}

// bootServicesLayout mirrors the stable prefix of EFI_BOOT_SERVICES through
// ExitBootServices. UEFI specifies this binary layout. Keeping the mirror in
// the boot boundary is required because TinyGo 0.42 does not export wrappers
// for GetMemoryMap or ExitBootServices.
type bootServicesLayout struct {
	header                     deviceuefi.EFI_TABLE_HEADER
	raiseTPL                   uintptr
	restoreTPL                 uintptr
	allocatePages              uintptr
	freePages                  uintptr
	getMemoryMap               uintptr
	allocatePool               uintptr
	freePool                   uintptr
	createEvent                uintptr
	setTimer                   uintptr
	waitForEvent               uintptr
	signalEvent                uintptr
	closeEvent                 uintptr
	checkEvent                 uintptr
	installProtocolInterface   uintptr
	reinstallProtocolInterface uintptr
	uninstallProtocolInterface uintptr
	handleProtocol             uintptr
	reserved                   uintptr
	registerProtocolNotify     uintptr
	locateHandle               uintptr
	locateDevicePath           uintptr
	installConfigurationTable  uintptr
	loadImage                  uintptr
	startImage                 uintptr
	exit                       uintptr
	unloadImage                uintptr
	exitBootServices           uintptr
}

type memoryMap struct {
	address           deviceuefi.EFI_PHYSICAL_ADDRESS
	size              uintptr
	key               uintptr
	descriptorSize    uintptr
	descriptorVersion uint32
}

// CaptureAndExit snapshots the firmware map and permanently transfers machine
// ownership to GoOS. It must be called after every operation that can allocate
// through UEFI. No TinyGo heap growth is permitted after a successful return.
func CaptureAndExit(info *boot.Info) deviceuefi.EFI_STATUS {
	var raw memoryMap
	status := deviceuefi.BS().AllocatePages(
		deviceuefi.AllocateAnyPages,
		deviceuefi.EfiLoaderData,
		deviceuefi.UINTN(memoryMapPages),
		&raw.address,
	)
	if status != deviceuefi.EFI_SUCCESS {
		return status
	}

	services := servicesLayout()
	for attempt := 0; attempt < maxExitRetries; attempt++ {
		raw.size = memoryMapBytes
		status = getMemoryMap(services, &raw)
		if status != deviceuefi.EFI_SUCCESS {
			return status
		}
		if !parseMemoryMap(info, &raw) {
			return deviceuefi.EFI_COMPROMISED_DATA
		}
		status = deviceuefi.UefiCall2(
			services.exitBootServices,
			uintptr(deviceuefi.GetImageHandle()),
			raw.key,
		)
		if status == deviceuefi.EFI_SUCCESS {
			return status
		}
	}
	return status
}

func servicesLayout() *bootServicesLayout {
	// SAFETY: EFI_BOOT_SERVICES begins with the standardized table layout
	// mirrored above. The pointer originates from TinyGo's initialized UEFI
	// system table and remains valid until ExitBootServices succeeds.
	return (*bootServicesLayout)(unsafe.Pointer(deviceuefi.BS()))
}

func getMemoryMap(services *bootServicesLayout, raw *memoryMap) deviceuefi.EFI_STATUS {
	// SAFETY: address names memoryMapPages of firmware-allocated EfiLoaderData;
	// all scalar output pointers refer to fields that live through the call.
	return deviceuefi.UefiCall5(
		services.getMemoryMap,
		uintptr(unsafe.Pointer(&raw.size)),
		uintptr(raw.address),
		uintptr(unsafe.Pointer(&raw.key)),
		uintptr(unsafe.Pointer(&raw.descriptorSize)),
		uintptr(unsafe.Pointer(&raw.descriptorVersion)),
	)
}

func parseMemoryMap(info *boot.Info, raw *memoryMap) bool {
	// Sizeof verifies the standardized descriptor prefix without dereferencing
	// firmware memory.
	if raw.descriptorSize < unsafe.Sizeof(memoryDescriptor{}) || raw.descriptorSize == 0 || raw.size > memoryMapBytes {
		return false
	}
	info.RegionCount = 0
	info.UsableBytes = 0
	info.FirmwareName = "UEFI"
	for offset := uintptr(0); offset+raw.descriptorSize <= raw.size; offset += raw.descriptorSize {
		// SAFETY: getMemoryMap initialized raw.size bytes at raw.address and the
		// descriptor-size checks above keep every read within that allocation.
		descriptor := (*memoryDescriptor)(unsafe.Pointer(uintptr(raw.address) + offset))
		if !info.AddRegion(boot.MemoryRegion{
			Start: descriptor.PhysicalStart,
			Pages: descriptor.NumberOfPages,
			Kind:  memoryKind(deviceuefi.EFI_MEMORY_TYPE(descriptor.Type)),
		}) {
			return false
		}
	}
	return info.RegionCount != 0
}

func memoryKind(kind deviceuefi.EFI_MEMORY_TYPE) boot.MemoryKind {
	switch kind {
	case deviceuefi.EfiConventionalMemory:
		return boot.MemoryUsable
	case deviceuefi.EfiBootServicesCode, deviceuefi.EfiBootServicesData:
		return boot.MemoryReclaimable
	case deviceuefi.EfiRuntimeServicesCode, deviceuefi.EfiRuntimeServicesData:
		return boot.MemoryRuntime
	default:
		return boot.MemoryReserved
	}
}
