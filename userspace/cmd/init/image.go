//go:build goos_userspace

package main

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/mm/vm"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	"unsafe"
)

//go:linkname imageMap goos_image_map
func imageMap(device uint64, virtual uint64, address unsafe.Pointer) uint64

// systemImage is the boot system image, mapped read-only into PID 1. It is the
// executable source for every launch: PID 1 finds a program here under its own
// search policy and names it to the kernel by extent, so the kernel resolves
// no path after the bootstrap (ADR 0093).
var systemImage []byte

// mapSystemImage maps the image before anything is launched. The filesystem
// service is itself launched from it, so PID 1 cannot wait for /system to be
// served before it can start a process.
func mapSystemImage() bool {
	var descriptor [2]uint64
	if imageMap(uint64(systemImageHandle), vm.UserImageAddress, unsafe.Pointer(&descriptor[0])) != 0 {
		return false
	}
	if descriptor[0] != vm.UserImageAddress || descriptor[1] == 0 || descriptor[1] > vm.UserImageLimit-vm.UserImageAddress {
		return false
	}
	// SAFETY: the kernel mapped exactly descriptor[1] bytes of the boot image
	// read-only at descriptor[0] before returning success, and the mapping
	// lives for as long as PID 1's address space, which is the whole boot.
	systemImage = unsafe.Slice((*byte)(unsafe.Pointer(uintptr(descriptor[0]))), int(descriptor[1]))
	return true
}

// locateExecutable turns an executable path into what the kernel launches: an
// extent of the system image, the handle that authorizes executing from it,
// and the display name ps reports. A path the image does not hold never
// reaches the kernel.
func locateExecutable(path []byte) (supervisor.Executable, string, supervisor.LaunchStatus) {
	offset, size, status := execproto.LocateImage(systemImage, path)
	switch status {
	case execproto.ImageFound:
	case execproto.ImageNotFound:
		return supervisor.Executable{}, "", supervisor.LaunchNotFound
	case execproto.ImageOutsideSystem:
		// The system image is the only executable source so far. A path
		// outside it is refused rather than looked up anywhere else.
		return supervisor.Executable{}, "", supervisor.LaunchCapabilityDenied
	default:
		return supervisor.Executable{}, "", supervisor.LaunchInvalidImage
	}
	name := path
	for index := len(path) - 1; index >= 0; index-- {
		if path[index] == '/' {
			name = path[index+1:]
			break
		}
	}
	if len(name) > supervisor.MaxProcessName {
		name = name[:supervisor.MaxProcessName]
	}
	return supervisor.Executable{Image: systemImageHandle, Offset: offset, Size: size}, bytesString(name), supervisor.LaunchOK
}

// requestLaunch sends one launch request and waits for its reply. The caller
// has written the startup template into staging slot slot.
func requestLaunch(executable supervisor.Executable, name string, grants []supervisor.Capability, slot uint8, startupSize int) (supervisor.LifecycleEvent, supervisor.LaunchStatus) {
	correlation++
	request, ok := supervisor.LaunchRequestWithStartup(1, executable, name, grants, slot, uint16(startupSize), correlation)
	if !ok {
		return supervisor.LifecycleEvent{}, supervisor.LaunchInvalidImage
	}
	if !sendMessage(lifecycleHandle, request) {
		return supervisor.LifecycleEvent{}, supervisor.LaunchProcessFailed
	}
	event, status := receiveLaunchResult(correlation)
	if status != ipc.StatusOK {
		return supervisor.LifecycleEvent{}, supervisor.LaunchProcessFailed
	}
	return event, supervisor.LaunchStatus(event.Code)
}

// probeLaunchAuthority proves, in the validation build, that the kernel
// launches only what an image capability with Execute authorizes, and only
// bytes that lie inside the image and validate as a native image.
func probeLaunchAuthority() bool {
	valid, _, status := locateExecutable([]byte("/system/fault-probe"))
	motd, _, motdStatus := locateExecutable([]byte("/system/motd"))
	if status != supervisor.LaunchOK || motdStatus != supervisor.LaunchOK {
		return false
	}
	cases := []struct {
		executable supervisor.Executable
		want       supervisor.LaunchStatus
	}{
		// The console device is a real handle, but not an image object.
		{supervisor.Executable{Image: serviceDeviceHandle, Offset: valid.Offset, Size: valid.Size}, supervisor.LaunchCapabilityDenied},
		// A handle PID 1 does not hold.
		{supervisor.Executable{Image: handle.ID(0x00000001000000ff), Offset: valid.Offset, Size: valid.Size}, supervisor.LaunchCapabilityDenied},
		// An extent that runs past the end of the image.
		{supervisor.Executable{Image: systemImageHandle, Offset: uint32(len(systemImage) - 1), Size: valid.Size}, supervisor.LaunchInvalidImage},
		// An extent that wraps.
		{supervisor.Executable{Image: systemImageHandle, Offset: ^uint32(0), Size: ^uint32(0)}, supervisor.LaunchInvalidImage},
		// Bytes inside the image that are not a native image.
		{motd, supervisor.LaunchInvalidImage},
		// Half an executable.
		{supervisor.Executable{Image: systemImageHandle, Offset: valid.Offset, Size: valid.Size / 2}, supervisor.LaunchInvalidImage},
	}
	for _, probe := range cases {
		event, status := requestLaunch(probe.executable, "probe", nil, 0, 0)
		if status != probe.want || event.Process != 0 {
			return false
		}
	}
	_, _, outside := locateExecutable([]byte("/persist/fault-probe"))
	_, _, missing := locateExecutable([]byte("/system/bin/missing"))
	return outside == supervisor.LaunchCapabilityDenied && missing == supervisor.LaunchNotFound
}
