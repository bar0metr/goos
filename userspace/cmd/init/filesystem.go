//go:build goos_userspace

package main

import (
	"goos/internal/validation"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	blockproto "goos/userspace/block"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/partition"
	"goos/userspace/runtime/sys"
	"goos/userspace/utility"
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

// filesystemNoticeInterval bounds how long a replacement notice waits while
// every wait slot of the main loop is taken by other work.
const filesystemNoticeInterval = clock.TicksPerSecond

var (
	// filesystemClient is the VFS end of the channel to the filesystem service
	// about to be launched, handed over once it runs.
	filesystemClient handle.ID
	// filesystemStarted marks the first launch, which uses the block service's
	// boot channel; every later launch renews both channels.
	filesystemStarted bool
	// filesystemProcessHandle names the running filesystem service. PID 1
	// ends it when the VFS reports it stalled with the file cache mapped;
	// the validation build also kills it to prove a crash is recovered from.
	filesystemProcessHandle handle.ID
	// cacheFence decides whether an attach may state that no earlier
	// filesystem service can still write the file cache.
	cacheFence filesystemFence
	// vfsRecovery is independent of ordinary VFS controls. A timed-out
	// AddSession or Sync must not hide a filesystem replacement notice.
	vfsRecovery         [2]uint64
	vfsRecoveryRequests utility.VFSRequests
	// filesystemChecker is the one application temporarily holding the
	// exclusive filesystem block slice.
	filesystemChecker uint64
	// filesystemCheckComplete keeps the checker exit across either ordering
	// of its lifecycle event and the old filesystem service's exit event.
	filesystemCheckComplete bool
	// filesystemVolume says which volume of the disk the filesystem service
	// may reach: automatically chosen, as at boot (ADR 0115). Choosing
	// another belongs to mounting (GoOS 0.1.4 specification).
	filesystemVolume partition.Selector
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
	request, ok := blockproto.SetFilesystemRequest(handle.ID(toBlock[1]), filesystemVolume, correlation)
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
// the services started next find /state in place. fenced states that every
// earlier instance has exited; the VFS needs it to end a cache quarantine.
func attachFilesystem(fenced bool) {
	// The old service can exit before its lost notice reaches PID 1. A
	// quarantined ordinary control still needs a fresh endpoint before any
	// attachment is sent, even when its notice has not been consumed yet.
	if !filesystemRequests.PollControl(handle.ID(vfsControlHandle)) && !replaceVFSControl() {
		sys.HandleClose(filesystemClient)
		filesystemClient = 0
		filesystemAttached = false
		return
	}
	client := filesystemClient
	filesystemClient = 0
	filesystemAttached = false
	correlation++
	request, ok := vfs.SetFilesystemRequest(vfs.FilesystemAttach{Client: client, Generation: cacheFence.attaching(), Fenced: fenced}, correlation)
	if !ok {
		sys.HandleClose(client)
		syslog.Error("filesystem service not handed to the vfs")
		return
	}
	// Sent distinguishes an untransferred client handle from one VFS owns,
	// including a mount whose completion reply never reached PID 1.
	vfsStatus, status, sent := filesystemRequests.Control(handle.ID(vfsControlHandle), &request, attachDeadline)
	if !sent {
		sys.HandleClose(client)
	}
	if status != ipc.StatusOK || vfsStatus != vfs.StatusOK {
		syslog.Warn("persistent volume unavailable")
		return
	}
	filesystemAttached = true
	if filesystemStarted {
		syslog.Info("filesystem service reattached")
		mark("FAT32_RESTARTED_OK\n")
	}
}

// pollFilesystemNotices takes the replacement notices the VFS sent on its
// control endpoint, and reports whether that endpoint may be waited on.
func pollFilesystemNotices() bool {
	if vfsRecovery[0] == 0 {
		return false
	}
	usable := vfsRecoveryRequests.PollControl(handle.ID(vfsRecovery[0]))
	if generation, lost := vfsRecoveryRequests.FilesystemLost(); lost {
		replaceFilesystem(generation)
	}
	return usable
}

// replaceFilesystem acts on the VFS's notice that the filesystem service of
// an attachment left an exchange unanswered and may still write the file
// cache. The service is ended rather than asked to stop: it may be stalled.
// The main loop sees its exit before the supervisor starts it again, so the
// replacement's attach can state the fence.
func replaceFilesystem(generation uint64) {
	switch cacheFence.lost(generation) {
	case filesystemLostStop:
		if !replaceVFSControl() {
			return
		}
		syslog.Warn("filesystem service stalled; replacing it")
		mark("FAT32_STALL_REPLACING\n")
		if filesystemProcessHandle == 0 || sys.ProcessKill(filesystemProcessHandle) != kcall.StatusOK {
			// Its exit may already be on its way. If it is not, no fenced
			// attach follows and the cache stays quarantined.
			syslog.Warn("filesystem service not stopped")
		}
	case filesystemLostAwait:
		if !replaceVFSControl() {
			return
		}
		syslog.Warn("filesystem service gone with the file cache quarantined")
	case filesystemLostExhausted:
		syslog.Error("filesystem cache stays quarantined", syslog.Str("reason", "recovery limit reached"))
		mark("FILE_CACHE_RECOVERY_LIMIT\n")
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

// replaceVFSControl retires any unresolved ordinary control exchange without
// replaying it. The independent recovery endpoint orders the handoff after
// admitted work drains. Only its acknowledgement permits PID 1 to use the
// new client; a lost acknowledgement leaves both paths fail-closed.
func replaceVFSControl() bool {
	var pair [2]uint64
	if !createChannel(&pair) {
		return false
	}
	correlation++
	request, ok := vfs.ReplaceControlRequest(handle.ID(pair[1]), correlation)
	if !ok {
		closePair(pair)
		return false
	}
	result, status, sent := vfsRecoveryRequests.Control(handle.ID(vfsRecovery[0]), &request, attachDeadline)
	if status != kcall.StatusOK || result != vfs.StatusOK {
		if !sent {
			sys.HandleClose(handle.ID(pair[1]))
		}
		sys.HandleClose(handle.ID(pair[0]))
		syslog.Error("vfs control replacement failed")
		return false
	}
	sys.HandleClose(handle.ID(vfsControlHandle))
	vfsControlHandle = pair[0]
	mark("VFS_CONTROL_REPLACED_OK\n")
	return true
}
