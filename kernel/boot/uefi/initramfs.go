//go:build uefi && amd64

package uefi

import (
	deviceuefi "device/uefi"
	"goos/kernel/boot"
	"unsafe"
)

// InitramfsTooLarge is what LoadInitramfs returns for an archive over
// boot.MaxInitramfs.
const InitramfsTooLarge = deviceuefi.EFI_BAD_BUFFER_SIZE

// initramfsPages is the read buffer, the archive's bound in pages.
const initramfsPages = uintptr(boot.MaxInitramfs / boot.PageSize)

var (
	loadedImageGUID      = deviceuefi.EFI_GUID{Data1: 0x5b1b31a1, Data2: 0x9562, Data3: 0x11d2, Data4: [8]byte{0x8e, 0x3f, 0x00, 0xa0, 0xc9, 0x69, 0x72, 0x3b}}
	simpleFileSystemGUID = deviceuefi.EFI_GUID{Data1: 0x964e5b22, Data2: 0x6459, Data3: 0x11d2, Data4: [8]byte{0x8e, 0x39, 0x00, 0xa0, 0xc9, 0x69, 0x72, 0x3b}}
	initramfsPath        = [...]deviceuefi.CHAR16{'\\', 'i', 'n', 'i', 't', 'r', 'a', 'm', 'f', 's', '.', 't', 'a', 'r', 0}
)

type loadedImageProtocol struct {
	revision     uint32
	_            uint32
	parentHandle deviceuefi.EFI_HANDLE
	systemTable  uintptr
	deviceHandle deviceuefi.EFI_HANDLE
}

type simpleFileSystemProtocol struct {
	revision   uint64
	openVolume uintptr
}

type fileProtocol struct {
	revision uint64
	open     uintptr
	close    uintptr
	delete   uintptr
	read     uintptr
}

func LoadInitramfs(info *boot.Info) deviceuefi.EFI_STATUS {
	if info == nil {
		return deviceuefi.EFI_INVALID_PARAMETER
	}
	var loaded *loadedImageProtocol
	status := deviceuefi.BS().HandleProtocol(deviceuefi.GetImageHandle(), &loadedImageGUID, unsafe.Pointer(&loaded))
	if status != deviceuefi.EFI_SUCCESS {
		return status
	}
	if loaded == nil || loaded.deviceHandle == 0 {
		return deviceuefi.EFI_COMPROMISED_DATA
	}
	var filesystem *simpleFileSystemProtocol
	status = deviceuefi.BS().HandleProtocol(loaded.deviceHandle, &simpleFileSystemGUID, unsafe.Pointer(&filesystem))
	if status != deviceuefi.EFI_SUCCESS {
		return status
	}
	if filesystem == nil || filesystem.openVolume == 0 {
		return deviceuefi.EFI_COMPROMISED_DATA
	}
	var root *fileProtocol
	status = deviceuefi.UefiCall2(filesystem.openVolume, uintptr(unsafe.Pointer(filesystem)), uintptr(unsafe.Pointer(&root)))
	if status != deviceuefi.EFI_SUCCESS {
		return status
	}
	if root == nil || root.open == 0 || root.close == 0 {
		return deviceuefi.EFI_COMPROMISED_DATA
	}
	defer deviceuefi.UefiCall1(root.close, uintptr(unsafe.Pointer(root)))
	var file *fileProtocol
	status = deviceuefi.UefiCall5(root.open, uintptr(unsafe.Pointer(root)), uintptr(unsafe.Pointer(&file)), uintptr(unsafe.Pointer(&initramfsPath[0])), 1, 0)
	if status != deviceuefi.EFI_SUCCESS {
		return status
	}
	if file == nil || file.read == 0 || file.close == 0 {
		return deviceuefi.EFI_COMPROMISED_DATA
	}
	defer deviceuefi.UefiCall1(file.close, uintptr(unsafe.Pointer(file)))
	address := deviceuefi.EFI_PHYSICAL_ADDRESS(1<<30 - 1)
	status = deviceuefi.BS().AllocatePages(deviceuefi.AllocateMaxAddress, deviceuefi.EfiLoaderData, deviceuefi.UINTN(initramfsPages), &address)
	if status != deviceuefi.EFI_SUCCESS {
		return status
	}
	size := uintptr(initramfsPages) * uintptr(boot.PageSize)
	status = deviceuefi.UefiCall3(file.read, uintptr(unsafe.Pointer(file)), uintptr(unsafe.Pointer(&size)), uintptr(address))
	if status != deviceuefi.EFI_SUCCESS {
		deviceuefi.BS().FreePages(address, deviceuefi.UINTN(initramfsPages))
		return status
	}
	if size == 0 || size > uintptr(initramfsPages)*uintptr(boot.PageSize) {
		deviceuefi.BS().FreePages(address, deviceuefi.UINTN(initramfsPages))
		return deviceuefi.EFI_COMPROMISED_DATA
	}
	// A read that fills the buffer may have stopped short of the file. One
	// more byte tells: an archive over the bound is refused rather than used
	// cut short, which would lose whatever the tail held.
	if size == uintptr(initramfsPages)*uintptr(boot.PageSize) {
		var probe byte
		extra := uintptr(1)
		status = deviceuefi.UefiCall3(file.read, uintptr(unsafe.Pointer(file)), uintptr(unsafe.Pointer(&extra)), uintptr(unsafe.Pointer(&probe)))
		if status != deviceuefi.EFI_SUCCESS || extra != 0 {
			deviceuefi.BS().FreePages(address, deviceuefi.UINTN(initramfsPages))
			return InitramfsTooLarge
		}
	}
	// The pages past the archive go back to the firmware, so the bound costs
	// nothing when the archive is smaller.
	used := (size + uintptr(boot.PageSize) - 1) / uintptr(boot.PageSize)
	if used < initramfsPages {
		deviceuefi.BS().FreePages(address+deviceuefi.EFI_PHYSICAL_ADDRESS(used*uintptr(boot.PageSize)), deviceuefi.UINTN(initramfsPages-used))
	}
	info.InitramfsAddress = uint64(address)
	info.InitramfsSize = uint64(size)
	return deviceuefi.EFI_SUCCESS
}

func Initramfs(info *boot.Info) []byte {
	if info == nil || info.InitramfsAddress == 0 || info.InitramfsSize == 0 || info.InitramfsAddress+info.InitramfsSize < info.InitramfsAddress {
		return nil
	}
	// SAFETY: LoadInitramfs allocated and populated these reserved pages before
	// ExitBootServices. Their identity mapping remains present after handoff.
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(info.InitramfsAddress))), int(info.InitramfsSize))
}
