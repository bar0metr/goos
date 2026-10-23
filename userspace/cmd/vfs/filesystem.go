//go:build goos_userspace

package main

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/fsproto"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
)

var (
	// filesystem is owner-only. The transport uses its captured endpoint,
	// so a worker never reads a control operation's mutable handle variable.
	filesystem      handle.ID
	filesystemState filesystemTransport
	fileCache       []byte
)

// filesystemTransport belongs to the whole-Remote execution lane. Its
// scratch is private to this connection. Only availability is cross-thread;
// controls replace the endpoint only after the read pool drains.
type filesystemTransport struct {
	endpoint     handle.ID
	correlation  uint64
	wire         [ipc.WireMaxSize]byte
	request      ipc.Message
	reply        ipc.Message
	name         [ramfs.MaxName]byte
	availability filesystemAvailability
	// applied is owner-only: the offline notification has been handled.
	applied bool
	// generation is owner-only: the attachment PID 1 named for this client.
	generation uint64
}

func (transport *filesystemTransport) Exchange(request fsproto.Request) (fsproto.Reply, ramfs.Status) {
	if failure := transport.availability.failure(); failure != ramfs.StatusOK {
		return fsproto.Reply{}, failure
	}
	if transport.endpoint == 0 {
		return fsproto.Reply{}, ramfs.StatusPeerDied
	}
	if transport.correlation == ^uint64(0) {
		transport.availability.fail(ramfs.StatusIOError, false)
		return fsproto.Reply{}, ramfs.StatusIOError
	}
	transport.correlation++
	if !fsproto.EncodeRequest(request, transport.correlation, &transport.request, fileCache) {
		return fsproto.Reply{}, ramfs.StatusIOError
	}
	size, ok := ipc.EncodeFrom(&transport.request, transport.wire[:])
	if !ok {
		return fsproto.Reply{}, ramfs.StatusIOError
	}
	start, clockStatus := sys.ClockRead()
	if clockStatus != kcall.StatusOK {
		return fsproto.Reply{}, ramfs.StatusIOError
	}
	status := sys.IPCSend(transport.endpoint, transport.wire[:size])
	accepted := status == kcall.StatusOK
	for status == kcall.StatusOK {
		now, clockStatus := sys.ClockRead()
		remaining, valid := remainingExchange(start, now, fsproto.RequestDeadline)
		if clockStatus != kcall.StatusOK || !valid {
			status = kcall.StatusTimeout
			break
		}
		status = sys.IPCWaitReceive(transport.endpoint, transport.wire[:], remaining)
		if status != kcall.StatusOK {
			break
		}
		if !ipc.DecodeInto(transport.wire[:wireSize(transport.wire[:])], &transport.reply) {
			transport.availability.fail(ramfs.StatusIOError, true)
			return fsproto.Reply{}, ramfs.StatusIOError
		}
		if transport.reply.Correlation < transport.correlation {
			continue
		}
		reply, valid := fsproto.DecodeReply(&transport.reply, fileCache, &transport.name)
		if !valid || transport.reply.Correlation != transport.correlation || reply.Operation != request.Operation {
			transport.availability.fail(ramfs.StatusIOError, true)
			return fsproto.Reply{}, ramfs.StatusIOError
		}
		return reply, ramfs.StatusOK
	}
	failure := ramfs.StatusIOError
	switch status {
	case kcall.StatusPeerDied:
		failure = ramfs.StatusPeerDied
	case kcall.StatusTimeout:
		failure = ramfs.StatusTimeout
	}
	transport.availability.fail(failure, accepted)
	return fsproto.Reply{}, failure
}

// filesystemOfflinePending is safe to observe while a worker runs. Applying
// it is not: the owner does that only at a drained serial barrier.
func filesystemOfflinePending() bool {
	return !filesystemState.applied && filesystemState.availability.failure() != ramfs.StatusOK
}

func applyFilesystemOffline() {
	if !filesystemOfflinePending() {
		return
	}
	filesystemState.applied = true
	if filesystem != 0 {
		sys.HandleClose(filesystem)
		filesystem = 0
	}
	service.DetachFilesystem()
	if filesystemState.availability.quarantined.Load() {
		syslog.Warn("filesystem cache quarantined; replacement requested")
		notifyFilesystemLost(filesystemState.generation)
	} else {
		syslog.Warn("filesystem service gone")
	}
}

// notifyFilesystemLost asks PID 1 to replace the filesystem service of the
// attachment generation. Closing the endpoint does not stop a service that
// still holds the cache mapping; PID 1 ends it and attaches a new one behind
// a fence. A notice that cannot be sent leaves the cache quarantined.
func notifyFilesystemLost(generation uint64) {
	notice, ok := vfs.FilesystemLostNotice(generation)
	var size int
	if ok {
		size, ok = ipc.Encode(notice, wire[:])
	}
	if !ok || sys.IPCSend(recoveryHandle, wire[:size]) != kcall.StatusOK {
		syslog.Error("filesystem replacement not requested")
	}
}

// attachFilesystem is an owner-side barrier. An uncertain old cache writer
// is not fenced merely by a new endpoint or correlation: while the cache is
// quarantined, only an attach PID 1 sends behind its fence is accepted.
func attachFilesystem(attach vfs.FilesystemAttach) vfs.Status {
	allowed, recovered := filesystemState.availability.admit(attach.Fenced)
	if !allowed {
		sys.HandleClose(attach.Client)
		syslog.Warn("filesystem reattach refused", syslog.Str("reason", "cache quarantined; fence missing or recovery limit reached"))
		return vfs.StatusBackendError
	}
	if filesystem != 0 {
		sys.HandleClose(filesystem)
	}
	filesystem = attach.Client
	filesystemState.endpoint = attach.Client
	filesystemState.correlation = 0
	filesystemState.generation = attach.Generation
	filesystemState.applied = false
	first := !service.PersistentMounted()
	if service.AttachFilesystem(&filesystemState) {
		if service.SystemProtected() {
			mark("SYSTEM_PROTECTED_OK\n")
		}
		if first {
			mark("FAT32_MOUNT_OK\n")
			syslog.Info("mounted", syslog.Str("fs", "fat32"), syslog.Str("path", "/persist"))
		} else {
			mark("FAT32_REATTACHED_OK\n")
			syslog.Info("filesystem service reattached", syslog.Str("path", "/persist"))
		}
		if recovered {
			mark("FILE_CACHE_RECOVERED_OK\n")
			syslog.Info("filesystem cache recovered behind a fence")
		}
		return vfs.StatusOK
	}
	if first {
		syslog.Error("persistent volume not mounted", syslog.Str("reason", "image rejected or device unavailable"))
		write("[WARN] no persistent storage\n")
	}
	return vfs.StatusBackendError
}

// pollRecovery runs at the same drained owner boundary as ordinary controls.
// Its endpoint is independent of a control exchange PID 1 may have abandoned
// while a persistent read occupied the execution lane.
func pollRecovery() bool {
	message, status := receiveNow(uint64(recoveryHandle))
	if status == ipc.StatusNotFound {
		return false
	}
	if status != ipc.StatusOK {
		fatal("VFS_RECOVERY_FAILED\n")
	}
	return processRecovery(message)
}

func processRecovery(message ipc.Message) bool {
	server, valid := vfs.DecodeReplaceControl(message)
	result := vfs.StatusInvalidArgument
	if valid {
		// Older admitted controls have retired before this barrier. Do not
		// execute unread old controls, even if their requests transferred
		// handles: PID 1 cannot know whether they completed. Release their
		// attachments and close the endpoint holding any stale replies.
		controlHandle = retireControl(controlHandle, server, func(endpoint handle.ID) (ipc.Message, ipc.Status) {
			return receiveNow(uint64(endpoint))
		}, func(id handle.ID) { sys.HandleClose(id) })
		result = vfs.StatusOK
	} else {
		closeAttached(message.Handles[:message.HandleCount])
	}
	reply, ok := vfs.AddSessionReply(result, message.Correlation)
	if !ok {
		fatal("VFS_RECOVERY_REPLY_FAILED\n")
	}
	send(uint64(recoveryHandle), reply)
	return true
}
