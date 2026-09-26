//go:build goos_userspace

package main

import (
	"goos/internal/limits"
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/fsproto"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/pages"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	vfsservice "goos/userspace/vfs"
	userwait "goos/userspace/wait"
)

const maxSessions = 12

var (
	service              vfsservice.Service
	consoleHandle        handle.ID
	initialSessionHandle handle.ID
	controlHandle        handle.ID
	wire                 [ipc.WireMaxSize]byte
	arenaBase            arena.Mark
	sessions             [maxSessions]uint64
	correlation          uint64
	waitInterests        *[userwait.MaxObjects]userwait.Interest
	waitCount            int
	liveSessions         int
)

// filesystem is the channel to the persistent filesystem service, or zero
// while there is none (ADR 0096). PID 1 hands it over when that service starts
// and again after every restart.
var (
	filesystem        handle.ID
	filesystemWire    [ipc.WireMaxSize]byte
	filesystemRequest ipc.Message
	filesystemReply   ipc.Message
	// filesystemName holds a name a reply carried in the file cache.
	filesystemName [ramfs.MaxName]byte
	// fileCache is the file cache mapping, once PID 1 has handed it over.
	fileCache []byte
)

// filesystemTransport carries backend calls to the filesystem service. Each
// exchange is bounded by the protocol deadline; a service that has died is
// forgotten at once, so later calls fail without waiting.
type filesystemTransport struct{}

func (filesystemTransport) Exchange(request fsproto.Request) (fsproto.Reply, bool) {
	if filesystem == 0 {
		return fsproto.Reply{}, false
	}
	correlation++
	if !fsproto.EncodeRequest(request, correlation, &filesystemRequest, fileCache) {
		return fsproto.Reply{}, false
	}
	size, ok := ipc.EncodeFrom(&filesystemRequest, filesystemWire[:])
	if !ok {
		return fsproto.Reply{}, false
	}
	status := sys.IPCSend(filesystem, filesystemWire[:size])
	// A reply to an earlier request that timed out may still be queued; it is
	// discarded rather than taken for this one's.
	for status == kcall.StatusOK {
		status = sys.IPCWaitReceive(filesystem, filesystemWire[:], fsproto.RequestDeadline)
		if status != kcall.StatusOK {
			break
		}
		if !ipc.DecodeInto(filesystemWire[:wireSize(filesystemWire[:])], &filesystemReply) {
			return fsproto.Reply{}, false
		}
		if filesystemReply.Correlation < correlation {
			continue
		}
		reply, valid := fsproto.DecodeReply(&filesystemReply, fileCache, &filesystemName)
		return reply, valid && filesystemReply.Correlation == correlation
	}
	if status == kcall.StatusPeerDied {
		dropFilesystem()
	}
	return fsproto.Reply{}, false
}

// dropFilesystem forgets a filesystem service that has gone. Its mounts answer
// with I/O errors until PID 1 hands over a restarted one.
func dropFilesystem() {
	sys.HandleClose(filesystem)
	filesystem = 0
	service.DetachFilesystem()
	syslog.Warn("filesystem service gone")
}

// attachFilesystem installs the channel to a filesystem service that has just
// started. The first attach mounts its volume; a later one checks that the
// restarted service serves the volume the mounts already name.
func attachFilesystem(client handle.ID) vfs.Status {
	if filesystem != 0 {
		sys.HandleClose(filesystem)
	}
	filesystem = client
	first := !service.PersistentMounted()
	if service.AttachFilesystem(filesystemTransport{}) {
		if first {
			mark("FAT32_MOUNT_OK\n")
			syslog.Info("mounted", syslog.Str("fs", "fat32"), syslog.Str("path", "/persist"))
		} else {
			mark("FAT32_REATTACHED_OK\n")
			syslog.Info("filesystem service reattached", syslog.Str("path", "/persist"))
		}
		return vfs.StatusOK
	}
	// A volume that cannot be mounted is not a reason for the system to stop.
	// The root filesystem and the system image are up, so the machine comes to
	// a prompt with no persistent storage rather than to nothing at all, which
	// is the state from which the volume can be inspected (ADR 0077).
	if first {
		syslog.Error("persistent volume not mounted", syslog.Str("reason", "image rejected or device unavailable"))
		write("[WARN] no persistent storage\n")
	}
	return vfs.StatusBackendError
}

// wireSize is the length of the message at the start of data, from its header.
func wireSize(data []byte) int {
	size := ipc.WireHeaderSize + (int(data[24]) | int(data[25])<<8) + int(data[26])*8
	if size > len(data) {
		return len(data)
	}
	return size
}

// mountSystemImage maps the read-only boot image and mounts it at /system, so
// the executables the launch path resolves are the ones a listing shows. The
// capability is the only authority the service has over the image, and the
// mapping it produces is not writable.
func mountSystemImage(imageHandle handle.ID) bool {
	image, status := sys.ImageMap(imageHandle)
	return status == kcall.StatusOK && service.MountSystemImage(image)
}

// The volatile root's bounds. Its pages count against this process's page
// quota, which is what bounds /tmp in the end; the page limit keeps a full
// /tmp from taking the memory the VFS itself needs to serve requests.
const (
	ramfsReserve   = uint64(1 << 30)
	ramfsNodes     = 8192
	ramfsPageLimit = 4096
)

// ramfsPages is the memory of the volatile root.
var ramfsPages pages.Pool

// ramfsStore gives the volatile root its pages from ramfsPages.
type ramfsStore struct{}

func (ramfsStore) Allocate() (ramfs.PageRef, bool) {
	ref, ok := ramfsPages.Allocate()
	return ramfs.PageRef(ref), ok
}
func (ramfsStore) Page(ref ramfs.PageRef) *[ramfs.PageSize]byte { return ramfsPages.Page(uint32(ref)) }
func (ramfsStore) Free(ref ramfs.PageRef)                       { ramfsPages.Free(uint32(ref)) }
func (ramfsStore) Chunk() *ramfs.NodeChunk                      { return pages.Permanent[ramfs.NodeChunk](&ramfsPages) }

//export userspaceMain
func userspaceMain() {
	arena.Init()
	arena.OnExhausted(func() { fatal("VFS_OUT_OF_MEMORY\n") })
	startupData, validStartup := startup.Read()
	var found bool
	if !validStartup {
		fatal("VFS_STARTUP_INVALID\n")
	}
	consoleHandle, found = startupData.Handle("console")
	if !found {
		fatal("VFS_CONSOLE_MISSING\n")
	}
	syslog.Init(consoleHandle, "vfs")
	initialSessionHandle, found = startupData.Handle("session")
	if !found {
		fatal("VFS_SESSION_MISSING\n")
	}
	controlHandle, found = startupData.Handle("control")
	if !found {
		fatal("VFS_CONTROL_MISSING\n")
	}
	imageHandle, found := startupData.Handle("image")
	if !found {
		fatal("VFS_IMAGE_MISSING\n")
	}
	cacheHandle, found := startupData.Handle("cache")
	if !found {
		fatal("VFS_CACHE_MISSING\n")
	}
	sessions[0] = uint64(initialSessionHandle)
	liveSessions = 1
	// The volatile root keeps its files in pages that outlive the per-request
	// arena and go back to the kernel when a file shrinks or goes (ADR 0105).
	if !ramfsPages.Init(ramfsReserve) || !service.ConfigureRAMFS(ramfsStore{}, ramfs.Limits{Nodes: ramfsNodes, Pages: ramfsPageLimit}) {
		fatal("VFS_RAMFS_UNAVAILABLE\n")
	}
	if !service.Init() {
		fatal("VFS_INIT_FAILED\n")
	}
	if !mountSystemImage(imageHandle) {
		fatal("VFS_IMAGE_MOUNT_FAILED\n")
	}
	// File data on the persistent volume moves through the cache this
	// process shares with the filesystem service (ADR 0098). Without it that
	// volume can be listed but not read, and the rest of the namespace works.
	if cache, status := sys.MemoryMap(cacheHandle, vm.UserSharedAddress, fsproto.CacheSize, true); status != kcall.StatusOK || !service.AttachCache(cache) {
		syslog.Error("file cache unavailable", syslog.Uint("status", uint64(status)))
	} else {
		fileCache = cache
		mark("FILE_CACHE_OK\n")
	}
	// The persistent volume is not mounted here: it is served by the FAT32
	// service, which PID 1 starts after this one and hands over through the
	// control endpoint (ADR 0096).
	arenaBase = arena.Current()
	mark("VFS_SERVICE_OK\n")
	mark("SYSTEM_IMAGE_MOUNT_OK\n")
	syslog.Info("mounted", syslog.Str("fs", "image"), syslog.Str("path", "/system"))
	for {
		arena.Reset(arenaBase)
		var interests [userwait.MaxObjects]userwait.Interest
		var sessionIndexes [maxSessions]int
		interests[0] = userwait.Interest{Handle: controlHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		count := 1
		for index, session := range sessions {
			if session != 0 {
				interests[count] = userwait.Interest{Handle: handle.ID(session), Events: kcall.WaitReadable | kcall.WaitPeerClosed}
				sessionIndexes[count-1] = index
				count++
			}
		}
		waitInterests, waitCount = &interests, count
		if count-1 != liveSessions {
			write("VFS_SESSIONS_CORRUPT\n")
			if sys.DiagnosticsTrace(consoleHandle, sessions[:]) == kcall.StatusOK {
				writeSessions()
			}
			sys.Exit(exitFailed)
		}
		result, status := userwait.Any(&interests, count, kcall.WaitAnyInfiniteTimeout)
		if status != kcall.StatusOK {
			switch status {
			case kcall.StatusPermissionDenied:
				fatal("VFS_WAIT_PERMISSION_DENIED\n")
			case kcall.StatusNotFound:
				fatal("VFS_WAIT_NOT_FOUND\n")
			case kcall.StatusResourceExhausted:
				fatal("VFS_WAIT_RESOURCE_EXHAUSTED\n")
			case kcall.StatusPeerDied:
				fatal("VFS_WAIT_PEER_DIED\n")
			default:
				fatal("VFS_WAIT_FAILED\n")
			}
		}
		if result.ReadyMask&1 != 0 {
			pollControl()
		}
		for interest := 1; interest < count; interest++ {
			if result.ReadyMask&(1<<interest) == 0 {
				continue
			}
			index := sessionIndexes[interest-1]
			session := sessions[index]
			message, receiveStatus := receiveNow(session)
			if receiveStatus == ipc.StatusPeerDied {
				// The files the session left open close with it, before its
				// slot, and with it its owner number, can be reused.
				service.CloseSession(sessionOwner(index))
				sys.HandleClose(handle.ID(session))
				sessions[index] = 0
				liveSessions--
				continue
			}
			if receiveStatus != ipc.StatusOK {
				fatal("VFS_RECEIVE_FAILED\n")
			}
			serve(sessionOwner(index), session, message)
		}
	}
}

func pollControl() bool {
	message, status := receiveNow(uint64(controlHandle))
	if status == ipc.StatusNotFound {
		return false
	}
	if status != ipc.StatusOK {
		fatal("VFS_CONTROL_FAILED\n")
	}
	if client, ok := vfs.DecodeSetFilesystem(message); ok {
		reply, encoded := vfs.AddSessionReply(attachFilesystem(client), message.Correlation)
		if !encoded {
			fatal("VFS_CONTROL_REPLY_FAILED\n")
		}
		send(uint64(controlHandle), reply)
		return true
	}
	if vfs.IsSync(message) {
		result := vfs.StatusBackendError
		if service.Sync() {
			result = vfs.StatusOK
			syslog.Info("synced")
		}
		reply, encoded := vfs.AddSessionReply(result, message.Correlation)
		if !encoded {
			fatal("VFS_CONTROL_REPLY_FAILED\n")
		}
		send(uint64(controlHandle), reply)
		return true
	}
	server, ok := vfs.DecodeAddSession(message)
	result := vfs.StatusResourceExhausted
	if ok {
		for index := range sessions {
			if sessions[index] == 0 {
				sessions[index], result = uint64(server), vfs.StatusOK
				liveSessions++
				break
			}
		}
	}
	reply, encoded := vfs.AddSessionReply(result, message.Correlation)
	if !encoded {
		fatal("VFS_CONTROL_REPLY_FAILED\n")
	}
	send(uint64(controlHandle), reply)
	return true
}

// sessionOwner names the session in slot index as the owner of the files it
// opens.
func sessionOwner(index int) vfs.Owner { return vfs.Owner(index + 1) }

func serve(owner vfs.Owner, session uint64, message ipc.Message) {
	request, ok := vfs.DecodeRequest(message)
	// A bulk operation, and a request whose paths or listed name travel in
	// memory, carry exactly one handle, and nothing else carries any.
	// Whatever arrived is this process's now and is closed once the request
	// is answered, so a client cannot fill the handle table.
	attached := message.Handles[:message.HandleCount]
	if ok && request.Attaches() != (len(attached) == 1) {
		ok = false
	}
	var object []byte
	var base, size uint64
	if ok && (request.PathsAttached || request.NameReturn) {
		object, base, size, ok = mapPathObject(&request, attached[0])
	}
	if !ok {
		closeAttached(attached)
		// Answer unusable requests: a silent drop strands the caller, which is
		// waiting for exactly this correlation.
		operation := vfs.Operation(message.Payload[0])
		if message.Correlation != 0 && operation >= vfs.OperationOpen && operation <= vfs.OperationFillExecutable {
			if reply, encoded := vfs.EncodeReply(vfs.Reply{Operation: operation, Status: vfs.StatusInvalidArgument}, message.Correlation); encoded {
				send(session, reply)
			}
		}
		return
	}
	var answer vfs.Reply
	if request.Operation.Bulk() {
		answer = serveBulk(owner, request, attached[0])
	} else {
		answer = service.Dispatch(owner, request)
		if len(answer.Name) > vfs.MaxInlineName {
			answer = returnName(answer, request, object)
		}
	}
	if object != nil {
		sys.VMUnmap(base, size)
	}
	closeAttached(attached)
	reply, ok := vfs.EncodeReply(answer, message.Correlation)
	if !ok {
		fatal("VFS_REPLY_FAILED\n")
	}
	send(session, reply)
}

// maxPathObject bounds the object a client may attach for paths or a name.
const maxPathObject = uint64(64 << 10)

// pathCopy and targetCopy hold the paths a request carried in memory, copied
// out of it before they are checked (ADR 0110).
var pathCopy, targetCopy [limits.PathMax]byte

// mapPathObject maps the object a request attached for its paths or a listed
// name, writable only for the name, and takes the paths out of it.
func mapPathObject(request *vfs.Request, object handle.ID) ([]byte, uint64, uint64, bool) {
	base, size, status := sys.MemoryMapAnywhere(object, request.NameReturn)
	if status != kcall.StatusOK {
		return nil, 0, 0, false
	}
	memory := sys.Window(base, size)
	if size > maxPathObject || request.NameReturn && size < ramfs.MaxName || request.PathsAttached && !request.TakePaths(memory, &pathCopy, &targetCopy) {
		sys.VMUnmap(base, size)
		return nil, 0, 0, false
	}
	return memory, base, size, true
}

// returnName delivers a listed name too long for the message: in the object
// the request attached for it, or, without one, as a refusal that tells the
// client how long it is so it can ask again with one.
func returnName(answer vfs.Reply, request vfs.Request, object []byte) vfs.Reply {
	if request.NameReturn && len(object) >= len(answer.Name) {
		answer.AttachedName = uint8(copy(object, answer.Name))
		answer.Name = nil
		return answer
	}
	return vfs.Reply{Operation: answer.Operation, Status: vfs.StatusNameTooLong, Size: uint64(len(answer.Name))}
}

func closeAttached(attached []handle.ID) {
	for _, id := range attached {
		sys.HandleClose(id)
	}
}

// maxBulkObject bounds the memory object a client may attach: the VFS maps
// the whole object, and its page tables are the VFS's to pay for.
const maxBulkObject = uint64(64 << 20)

// fillBuffer is where a program's bytes pass on their way into the
// executable object a launcher attached.
var fillBuffer [128 << 10]byte

// serveBulk moves a file's bytes to or from the object the client attached
// (ADR 0107). The object is mapped for this request only.
func serveBulk(owner vfs.Owner, request vfs.Request, object handle.ID) vfs.Reply {
	reply := vfs.Reply{Operation: request.Operation}
	if request.Operation == vfs.OperationFillExecutable {
		for reply.Size < uint64(request.Size) && reply.Status == vfs.StatusOK {
			chunk := min(uint64(len(fillBuffer)), uint64(request.Size)-reply.Size)
			count, status := service.ReadFile(owner, request.Handle, fillBuffer[:chunk])
			switch {
			case status != vfs.StatusOK:
				reply.Status = status
			case count == 0:
				// The file is shorter than the object: it changed size.
				reply.Status = vfs.StatusInvalidArgument
			case sys.ExecutableWrite(object, fillBuffer[:count]) != kcall.StatusOK:
				reply.Status = vfs.StatusPermissionDenied
			default:
				reply.Size += uint64(count)
			}
		}
		return reply
	}
	base, size, status := sys.MemoryMapAnywhere(object, request.Operation == vfs.OperationReadInto)
	if status != kcall.StatusOK {
		reply.Status = vfs.StatusPermissionDenied
		return reply
	}
	end := request.Offset + uint64(request.Size)
	if size > maxBulkObject || end < request.Offset || end > size {
		reply.Status = vfs.StatusInvalidArgument
		sys.VMUnmap(base, size)
		return reply
	}
	memory := sys.Window(base, size)[request.Offset:end]
	var count int
	if request.Operation == vfs.OperationReadInto {
		count, reply.Status = service.ReadFile(owner, request.Handle, memory)
	} else {
		count, reply.Status = service.WriteFile(owner, request.Handle, memory)
	}
	sys.VMUnmap(base, size)
	reply.Size = uint64(count)
	return reply
}

func send(channel uint64, message ipc.Message) {
	size, ok := ipc.Encode(message, wire[:])
	if !ok || sys.IPCSend(handle.ID(channel), wire[:size]) != kcall.StatusOK {
		fatal("VFS_SEND_FAILED\n")
	}
}

func receiveNow(channel uint64) (ipc.Message, ipc.Status) {
	status := ipc.Status(sys.IPCReceive(handle.ID(channel), wire[:]))
	if status != ipc.StatusOK {
		return ipc.Message{}, status
	}
	payloadSize := int(wire[24]) | int(wire[25])<<8
	size := ipc.WireHeaderSize + payloadSize + int(wire[26])*8
	if size < ipc.WireHeaderSize || size > len(wire) {
		return ipc.Message{}, ipc.StatusInvalidArgument
	}
	message, ok := ipc.Decode(wire[:size])
	if !ok {
		return ipc.Message{}, ipc.StatusInvalidArgument
	}
	return message, ipc.StatusOK
}

func mark(message string) {
	if validation.Enabled {
		write(message)
	}
}

func write(message string) {
	sys.ConsoleWriteString(consoleHandle, message)
}

// exitFailed is the code the service exits with when it cannot go on. It is
// not zero, so the supervisor restarts the service (ADR 0099).
const exitFailed = 1

// fatal reports why the service stops and exits. The VFS used to spin here,
// which left every client waiting on a service that would never answer; now
// PID 1 sees the exit and starts a new one.
func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		write(message)
	}
	if sys.Diagnostics(consoleHandle) == 0 {
		writeSessions()
	}
	sys.Exit(exitFailed)
}

func writeSessions() {
	write("VFS_DIAG control=")
	writeHex(uint64(controlHandle))
	for index, session := range sessions {
		if session != 0 {
			write(" session")
			writeHex(uint64(index))
			write("=")
			writeHex(session)
		}
	}
	if waitInterests != nil {
		write(" interests=")
		for index := 0; index < waitCount; index++ {
			writeHex(uint64(waitInterests[index].Handle))
			write(",")
		}
	}
	write("\n")
}

func writeHex(value uint64) {
	var digits [18]byte
	digits[0], digits[1] = '0', 'x'
	for index := 0; index < 16; index++ {
		digits[17-index] = "0123456789abcdef"[value>>(uint(index)*4)&0xf]
	}
	sys.ConsoleWrite(consoleHandle, digits[:])
}
func main() {}
