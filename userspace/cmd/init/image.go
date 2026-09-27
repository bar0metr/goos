//go:build goos_userspace

package main

import (
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
)

// systemImage is the boot system image, mapped read-only into PID 1. It is the
// executable source for every service and every command under /system: PID 1
// finds a program here under its own search policy and names it to the kernel
// by extent, so the kernel resolves no path after the bootstrap (ADR 0093).
// Programs outside the image use file-backed executable sources (ADR 0121).
var systemImage []byte

// mapSystemImage maps the image before anything is launched. The filesystem
// service is itself launched from it, so PID 1 cannot wait for /system to be
// served before it can start a process.
func mapSystemImage() bool {
	image, status := sys.ImageMap(systemImageHandle)
	systemImage = image
	return status == kcall.StatusOK
}

// locateExecutable turns a path in the system image into what the kernel
// launches: an extent of the image, the handle that authorizes executing from
// it, and the display name ps reports. A path the image does not hold never
// reaches the kernel. Services are launched only from here, so every service
// is a program of the immutable image.
func locateExecutable(path []byte) (supervisor.Executable, string, supervisor.LaunchStatus) {
	offset, size, status := execproto.LocateImage(systemImage, path)
	switch status {
	case execproto.ImageFound:
	case execproto.ImageNotFound:
		return supervisor.Executable{}, "", supervisor.LaunchNotFound
	case execproto.ImageOutsideSystem, execproto.ImageInvalidPath:
		return supervisor.Executable{}, "", supervisor.LaunchCapabilityDenied
	default:
		return supervisor.Executable{}, "", supervisor.LaunchInvalidImage
	}
	return supervisor.Executable{Image: systemImageHandle, Offset: offset, Size: size}, processName(path), supervisor.LaunchOK
}

// openExecutable resolves a command: in the system image when the path is
// under /system, and otherwise from the file-backed source prepared before
// the job started (ADR 0121). The caller closes an object with
// closeExecutable once the launch is answered.
func openExecutable(path []byte) (supervisor.Executable, string, supervisor.LaunchStatus) {
	if _, _, status := execproto.LocateImage(systemImage, path); status != execproto.ImageOutsideSystem {
		return locateExecutable(path)
	}
	// A job's program sources were prepared before its pipeline started
	// (ADR 0108).
	if executable, status, found := takePrepared(path); found {
		return executable, processName(path), status
	}
	syslog.Warn("executable source missing", syslog.Str("path", bytesString(path)))
	return supervisor.Executable{}, processName(path), supervisor.LaunchProcessFailed
}

// closeExecutable releases the executable object a launch named, if any. The
// system image handle is PID 1's for the whole boot.
func closeExecutable(executable supervisor.Executable) {
	if executable.Image != 0 && executable.Image != systemImageHandle {
		sys.HandleClose(executable.Image)
	}
}

func processName(path []byte) string {
	name := execproto.ExecutableName(path)
	if len(name) > supervisor.MaxProcessName {
		name = name[:supervisor.MaxProcessName]
	}
	return bytesString(name)
}

// fillDeadline bounds the wait for a program of size bytes to be read: the
// ordinary session deadline plus a second for every MiB.
func fillDeadline(size uint64) uint64 {
	return vfsSessionDeadline + (size>>20)*clock.TicksPerSecond
}

// requestLaunch sends one launch request and waits for its reply. The caller
// has written the startup template into staging slot slot.
func requestLaunch(executable supervisor.Executable, name string, grants []supervisor.Capability, profile supervisor.ResourceProfile, slot uint8, startupSize uint32) (supervisor.LifecycleEvent, supervisor.LaunchStatus) {
	correlation++
	request, ok := supervisor.LaunchRequestWithStartup(1, executable, name, grants, profile, slot, startupSize, correlation)
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
		event, status := requestLaunch(probe.executable, "probe", nil, supervisor.ProfileTiny, 0, 0)
		if status != probe.want || event.Process != 0 {
			return false
		}
	}
	_, _, outside := locateExecutable([]byte("/persist/fault-probe"))
	_, _, missing := locateExecutable([]byte("/system/bin/missing"))
	return outside == supervisor.LaunchCapabilityDenied && missing == supervisor.LaunchNotFound && probeExecutableObjects(valid, motd)
}

// probeExecutableObjects proves, in the validation build, that the kernel
// launches an executable object only whole, only once sealed, and only while
// PID 1 still holds it, and validates its bytes as it does the image's
// (ADR 0097). valid and motd are extents of the image.
func probeExecutableObjects(valid, motd supervisor.Executable) bool {
	if _, status := sys.ExecutableCreate(systemImageHandle, 1); status != kcall.StatusPermissionDenied {
		return false
	}
	program := systemImage[valid.Offset : valid.Offset+valid.Size]
	object, status := sys.ExecutableCreate(loaderHandle, uint64(len(program)))
	if status != kcall.StatusOK || !fillObject(object, program[:len(program)/2]) {
		return false
	}
	whole := supervisor.Executable{Image: object, Size: valid.Size}
	cases := []struct {
		executable supervisor.Executable
		want       supervisor.LaunchStatus
	}{
		// Half written: not sealed.
		{whole, supervisor.LaunchInvalidImage},
	}
	if !launchesRefused(cases) || !fillObject(object, program[len(program)/2:]) {
		sys.HandleClose(object)
		return false
	}
	cases = []struct {
		executable supervisor.Executable
		want       supervisor.LaunchStatus
	}{
		// Sealed, but named in part.
		{supervisor.Executable{Image: object, Offset: 1, Size: valid.Size - 1}, supervisor.LaunchInvalidImage},
		{supervisor.Executable{Image: object, Size: valid.Size - 1}, supervisor.LaunchInvalidImage},
	}
	refused := launchesRefused(cases) && sys.ExecutableWrite(object, program[:1]) == kcall.StatusInvalidArgument
	if sys.HandleClose(object) != kcall.StatusOK || !refused {
		return false
	}
	text, status := sys.ExecutableCreate(loaderHandle, uint64(motd.Size))
	if status != kcall.StatusOK {
		return false
	}
	filled := fillObject(text, systemImage[motd.Offset:motd.Offset+motd.Size])
	cases = []struct {
		executable supervisor.Executable
		want       supervisor.LaunchStatus
	}{
		// Closed: PID 1 no longer holds it.
		{whole, supervisor.LaunchCapabilityDenied},
		// Sealed and whole, but not a program.
		{supervisor.Executable{Image: text, Size: motd.Size}, supervisor.LaunchInvalidImage},
	}
	refused = filled && launchesRefused(cases)
	return sys.HandleClose(text) == kcall.StatusOK && refused
}

// launchesRefused requests each launch and requires its refusal status and no
// process.
func launchesRefused(cases []struct {
	executable supervisor.Executable
	want       supervisor.LaunchStatus
}) bool {
	for _, probe := range cases {
		event, status := requestLaunch(probe.executable, "probe", nil, supervisor.ProfileTiny, 0, 0)
		if status != probe.want || event.Process != 0 {
			return false
		}
	}
	return true
}

// fillObject appends data to an executable object in the largest writes the
// kernel accepts.
func fillObject(object handle.ID, data []byte) bool {
	for len(data) != 0 {
		chunk := min(len(data), int(kcall.MaxExecutableWrite))
		if sys.ExecutableWrite(object, data[:chunk]) != kcall.StatusOK {
			return false
		}
		data = data[chunk:]
	}
	return true
}
