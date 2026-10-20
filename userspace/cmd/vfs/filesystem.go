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
		syslog.Warn("filesystem cache quarantined; online recovery unavailable")
	} else {
		syslog.Warn("filesystem service gone")
	}
}

// attachFilesystem is an owner-side barrier. An uncertain old cache writer
// is not fenced merely by a new endpoint or correlation: refuse reattachment
// for this instance instead of sharing its cache with a new filesystem.
func attachFilesystem(client handle.ID) vfs.Status {
	if filesystemState.availability.quarantined.Load() {
		sys.HandleClose(client)
		return vfs.StatusBackendError
	}
	if filesystem != 0 {
		sys.HandleClose(filesystem)
	}
	filesystem = client
	filesystemState.endpoint = client
	filesystemState.correlation = 0
	filesystemState.availability.reason.Store(0)
	filesystemState.availability.quarantined.Store(false)
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
		return vfs.StatusOK
	}
	if first {
		syslog.Error("persistent volume not mounted", syslog.Str("reason", "image rejected or device unavailable"))
		write("[WARN] no persistent storage\n")
	}
	return vfs.StatusBackendError
}
