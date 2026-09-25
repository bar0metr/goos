//go:build goos_userspace

package main

import (
	"goos/internal/validation"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/vfs"
	blockproto "goos/userspace/block"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
)

// The FAT32 filesystem service runs in its own process between the VFS and the
// block service (ADR 0096). It reaches each over a channel PID 1 creates. The
// VFS end is handed over through the VFS control endpoint once the service is
// running, at the first start and after every restart; the block end is the
// boot channel at first and a fresh one, handed to the block service over its
// control endpoint, after a restart.
const filesystemName = "fat32"

// attachDeadline bounds the wait for the VFS to mount a new filesystem
// service. The VFS gives each exchange with that service its own deadline, and
// mounting takes a few exchanges.
const attachDeadline = 20 * clock.TicksPerSecond

var (
	// filesystemClient is the VFS end of the channel to the filesystem service
	// about to be launched, handed over once it runs.
	filesystemClient handle.ID
	// filesystemStarted marks the first launch, which uses the block service's
	// boot channel; every later launch renews both channels.
	filesystemStarted bool
	// filesystemProcessHandle is kept only by the validation build, to inject
	// the fault that proves a crashed filesystem service is replaced.
	filesystemProcessHandle handle.ID
)

// renewFilesystemChannels gives a restarted filesystem service fresh channels
// to the VFS and to the block service. The endpoints of the instance that died
// went with its process; the first launch uses the ones created at boot.
func renewFilesystemChannels(spec *supervisor.LaunchSpec) bool {
	var toVFS [2]uint64
	if !createChannel(&toVFS) {
		syslog.Error("filesystem channel unavailable")
		return false
	}
	if !renewGrant(spec, filesystemName, "vfs", handle.ID(toVFS[1])) {
		closePair(toVFS)
		return false
	}
	if filesystemClient != 0 {
		sys.HandleClose(filesystemClient)
	}
	filesystemClient = handle.ID(toVFS[0])
	var toBlock [2]uint64
	if !createChannel(&toBlock) {
		syslog.Error("filesystem block channel unavailable")
		return false
	}
	correlation++
	request, ok := blockproto.SetFilesystemRequest(handle.ID(toBlock[1]), correlation)
	if !ok || !sendMessage(uint64(blockControl[0]), request) {
		closePair(toBlock)
		return false
	}
	reply, status := receive(uint64(blockControl[0]), vfsSessionDeadline)
	blockStatus, valid := blockproto.DecodeSetDriverReply(reply)
	if status != ipc.StatusOK || !valid || blockStatus != blockproto.StatusOK {
		sys.HandleClose(handle.ID(toBlock[0]))
		syslog.Error("block service refused the filesystem service")
		return false
	}
	return renewGrant(spec, filesystemName, "block", handle.ID(toBlock[0]))
}

// attachFilesystem hands the VFS its end of the channel to the filesystem
// service that has just started, and waits for the VFS to mount its volume, so
// the services started next find /state in place.
func attachFilesystem() {
	client := filesystemClient
	filesystemClient = 0
	correlation++
	request, ok := vfs.SetFilesystemRequest(client, correlation)
	if !ok || !sendMessage(vfsControlHandle, request) {
		sys.HandleClose(client)
		syslog.Error("filesystem service not handed to the vfs")
		return
	}
	reply, status := receive(vfsControlHandle, attachDeadline)
	vfsStatus, valid := vfs.DecodeAddSessionReply(reply)
	if status != ipc.StatusOK || !valid || vfsStatus != vfs.StatusOK {
		syslog.Warn("persistent volume unavailable")
		return
	}
	if filesystemStarted {
		syslog.Info("filesystem service reattached")
		mark("FAT32_RESTARTED_OK\n")
	}
}

// injectFilesystemFault kills the filesystem service, in the validation build
// only, so the boot test sees it restarted under a running system.
func injectFilesystemFault() bool {
	if !validation.Enabled || filesystemProcessHandle == 0 || sys.ProcessKill(filesystemProcessHandle) != 0 {
		return false
	}
	sys.HandleClose(filesystemProcessHandle)
	filesystemProcessHandle = 0
	syslog.Warn("filesystem fault injected")
	mark("FAT32_FAULT_INJECTED\n")
	return true
}
