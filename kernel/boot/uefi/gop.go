//go:build uefi && amd64

package uefi

import (
	deviceuefi "device/uefi"
	"goos/kernel/boot"
	"unsafe"
)

const maxGOPModes = uint32(256)

var graphicsOutputProtocolGUID = deviceuefi.EFI_GUID{
	Data1: 0x9042a9de,
	Data2: 0x23dc,
	Data3: 0x4a38,
	Data4: [8]byte{0x96, 0xfb, 0x7a, 0xde, 0xd0, 0x80, 0x51, 0x6a},
}

// DiscoverFramebuffer selects and records one validated linear GOP mode. All
// protocol and firmware-owned pointers remain local to this call, which must
// finish before CaptureAndExit.
func DiscoverFramebuffer(info *boot.Info) deviceuefi.EFI_STATUS {
	if info == nil {
		return deviceuefi.EFI_INVALID_PARAMETER
	}
	info.Framebuffer = boot.FramebufferInfo{}

	var protocol *graphicsOutputProtocol
	status := deviceuefi.BS().LocateProtocol(&graphicsOutputProtocolGUID, nil, unsafe.Pointer(&protocol))
	if status != deviceuefi.EFI_SUCCESS {
		return status
	}
	if protocol == nil || protocol.queryMode == 0 || protocol.setMode == 0 || protocol.mode == nil ||
		protocol.mode.maxMode == 0 {
		return deviceuefi.EFI_COMPROMISED_DATA
	}

	choice, status := selectGOPMode(protocol)
	if status != deviceuefi.EFI_SUCCESS {
		return status
	}
	if !choice.valid {
		return deviceuefi.EFI_UNSUPPORTED
	}
	if choice.number != protocol.mode.mode {
		status = deviceuefi.UefiCall2(protocol.setMode, uintptr(unsafe.Pointer(protocol)), uintptr(choice.number))
		if status != deviceuefi.EFI_SUCCESS {
			return status
		}
	}

	mode := protocol.mode
	if mode == nil || mode.info == nil || mode.sizeOfInfo < unsafe.Sizeof(gopModeInformation{}) || mode.mode != choice.number {
		return deviceuefi.EFI_COMPROMISED_DATA
	}
	framebuffer, valid := framebufferFromGOP(*mode.info, mode.framebufferBase, uint64(mode.framebufferSize))
	if !valid {
		return deviceuefi.EFI_COMPROMISED_DATA
	}
	info.Framebuffer = framebuffer
	return deviceuefi.EFI_SUCCESS
}

func selectGOPMode(protocol *graphicsOutputProtocol) (gopModeChoice, deviceuefi.EFI_STATUS) {
	var best gopModeChoice
	modeCount := protocol.mode.maxMode
	if modeCount > maxGOPModes {
		modeCount = maxGOPModes
	}
	for number := uint32(0); number < modeCount; number++ {
		var size uintptr
		var raw *gopModeInformation
		status := deviceuefi.UefiCall4(
			protocol.queryMode,
			uintptr(unsafe.Pointer(protocol)),
			uintptr(number),
			uintptr(unsafe.Pointer(&size)),
			uintptr(unsafe.Pointer(&raw)),
		)
		if status != deviceuefi.EFI_SUCCESS {
			continue
		}
		if raw == nil {
			return gopModeChoice{}, deviceuefi.EFI_COMPROMISED_DATA
		}
		validSize := size >= unsafe.Sizeof(gopModeInformation{})
		if validSize {
			best = chooseGOPMode(best, number, protocol.mode.mode, *raw)
		}
		status = freePool(unsafe.Pointer(raw))
		if status != deviceuefi.EFI_SUCCESS {
			return gopModeChoice{}, status
		}
		if !validSize {
			continue
		}
	}
	return best, deviceuefi.EFI_SUCCESS
}

func freePool(buffer unsafe.Pointer) deviceuefi.EFI_STATUS {
	return deviceuefi.UefiCall1(servicesLayout().freePool, uintptr(buffer))
}
