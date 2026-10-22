//go:build goos_userspace

package main

import (
	"goos/internal/limits"
	"goos/internal/timekeeping"
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

var (
	service              vfsservice.Service
	consoleHandle        handle.ID
	initialSessionHandle handle.ID
	controlHandle        handle.ID
	recoveryHandle       handle.ID
	wire                 [ipc.WireMaxSize]byte
	sessions             [maxSessions]uint64
	sessionGenerations   [maxSessions]uint64
	waitInterests        *[userwait.MaxObjects]userwait.Interest
	waitCount            int
	liveSessions         int
)

// wireSize is the length of the message at the start of data, from its header.
func wireSize(data []byte) int {
	size := ipc.WireHeaderSize + (int(data[24]) | int(data[25])<<8) + int(data[26])*8
	if size > len(data) {
		return len(data)
	}
	return size
}

// mountBootstrap maps the read-only boot archive and mounts it at /bootstrap,
// so the programs PID 1 starts from it are the ones a listing shows. The
// capability is the only authority the service has over the archive, and the
// mapping it produces is not writable.
func mountBootstrap(imageHandle handle.ID) bool {
	image, status := sys.ImageMap(imageHandle)
	return status == kcall.StatusOK && service.MountBootstrap(image)
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

func ramfsRealtime() (ramfs.Timestamp, bool) {
	now, status := sys.ClockNow(timekeeping.Realtime)
	if status != kcall.StatusOK || !now.Valid() {
		return ramfs.Timestamp{}, false
	}
	return ramfs.Timestamp{Seconds: now.Seconds, Nanoseconds: now.Nanoseconds, Available: true}, true
}

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
	initRuntime()
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
	recoveryHandle, found = startupData.Handle("recovery")
	if !found {
		fatal("VFS_RECOVERY_MISSING\n")
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
	sessionGenerations[0] = 1
	liveSessions = 1
	// The volatile root keeps its files in whole pages outside the heap, which
	// go back to the kernel when a file shrinks or goes (ADR 0105).
	if !ramfsPages.Init(ramfsReserve) || !service.ConfigureRAMFS(ramfsStore{}, ramfs.Limits{Nodes: ramfsNodes, Pages: ramfsPageLimit}) {
		fatal("VFS_RAMFS_UNAVAILABLE\n")
	}
	service.SetRAMFSClock(ramfsRealtime)
	if !service.Init() {
		fatal("VFS_INIT_FAILED\n")
	}
	if !mountBootstrap(imageHandle) {
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
	mark("VFS_SERVICE_OK\n")
	mark("SYSTEM_IMAGE_MOUNT_OK\n")
	syslog.Info("mounted", syslog.Str("fs", "image"), syslog.Str("path", vfsservice.BootstrapPath))
	if validation.Enabled {
		service.SetCopyFault(reportCopyFault)
	}
	runService()
}

func pollControl() bool {
	message, status := receiveNow(uint64(controlHandle))
	if status == ipc.StatusNotFound {
		return false
	}
	if status != ipc.StatusOK {
		fatal("VFS_CONTROL_FAILED\n")
	}
	return processControl(message)
}

// processControl runs only on the namespace owner after earlier work drains.
func processControl(message ipc.Message) bool {
	if attach, ok := vfs.DecodeSetFilesystem(message); ok {
		reply, encoded := vfs.AddSessionReply(attachFilesystem(attach), message.Correlation)
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
	if vfs.IsPublish(message) || vfs.IsWithdraw(message) {
		var result vfs.Status
		if vfs.IsPublish(message) {
			if result = service.Publish(); result == vfs.StatusOK {
				mark("SYSTEM_PUBLISHED_OK\n")
				syslog.Info("mounted", syslog.Str("fs", "fat32"), syslog.Str("path", vfsservice.SystemPath))
			}
		} else if result = service.Withdraw(); result == vfs.StatusOK {
			syslog.Info("unmounted", syslog.Str("path", vfsservice.SystemPath))
		}
		reply, encoded := vfs.AddSessionReply(result, message.Correlation)
		if !encoded {
			fatal("VFS_CONTROL_REPLY_FAILED\n")
		}
		send(uint64(controlHandle), reply)
		return true
	}
	if vfs.IsUnmount(message) {
		result := service.UnmountFilesystem()
		if result == vfs.StatusOK {
			sys.HandleClose(filesystem)
			filesystem = 0
			filesystemState.endpoint = 0
			syslog.Info("unmounted", syslog.Str("path", "/persist"))
		}
		reply, encoded := vfs.AddSessionReply(result, message.Correlation)
		if !encoded {
			fatal("VFS_CONTROL_REPLY_FAILED\n")
		}
		send(uint64(controlHandle), reply)
		return true
	}
	if scope, ok := vfs.DecodeInstallScope(&message); ok {
		result := vfs.StatusInvalidArgument
		if !scope.Attached {
			result = service.InstallScope(scope.Slot, scope.Path, scope.Subtree, scope.Denied)
		} else {
			base, size, status := sys.MemoryMapAnywhere(message.Handles[0], false)
			if status == kcall.StatusOK {
				if size <= maxPathObject && scope.TakePath(sys.Window(base, size), &pathCopy) {
					result = service.InstallScope(scope.Slot, scope.Path, scope.Subtree, scope.Denied)
				}
				sys.VMUnmap(base, size)
			}
			closeAttached(message.Handles[:message.HandleCount])
		}
		reply, encoded := vfs.AddSessionReply(result, message.Correlation)
		if !encoded {
			fatal("VFS_CONTROL_REPLY_FAILED\n")
		}
		send(uint64(controlHandle), reply)
		return true
	}
	server, authority, ok := vfs.DecodeAddSessionAuthority(message)
	result := vfs.StatusInvalidArgument
	if ok {
		result = vfs.StatusResourceExhausted
		for index := range sessions {
			if sessions[index] == 0 {
				generation, valid := nextSessionGeneration(sessionGenerations[index])
				if !valid {
					continue
				}
				result = service.SetAuthority(sessionOwner(index), authority)
				if result == vfs.StatusOK {
					sessions[index] = uint64(server)
					sessionGenerations[index] = generation
					liveSessions++
				}
				break
			}
		}
	}
	if result != vfs.StatusOK && message.HandleCount <= ipc.MaxAttachedHandles {
		closeAttached(message.Handles[:message.HandleCount])
	}
	reply, encoded := vfs.AddSessionReply(result, message.Correlation)
	if !encoded {
		fatal("VFS_CONTROL_REPLY_FAILED\n")
	}
	send(uint64(controlHandle), reply)
	return true
}

// closeSession runs only after the owner's requests have retired. Kernel
// channel receive reports PeerDied after queued messages (and attachments)
// drain, so closing here does not abandon received handles in the IPC queue.
func closeSession(index int) {
	service.CloseSession(sessionOwner(index))
	readPoolProbeSessionClosed(sessionOwner(index))
	if validation.Enabled {
		releaseWithheld(sessionOwner(index), sessions[index], false)
		execCrashHandles[index] = 0
	}
	sys.HandleClose(handle.ID(sessions[index]))
	sessions[index] = 0
	liveSessions--
}

// sessionOwner names the session in slot index as the owner of the files it
// opens.
func sessionOwner(index int) vfs.Owner { return vfs.Owner(index + 1) }

func beginServing(owner vfs.Owner, session uint64) {
	if validation.Enabled {
		releaseWithheld(owner, session, true)
		reportMemory()
	}
}

func serve(owner vfs.Owner, session uint64, message ipc.Message) {
	if validation.Enabled {
		service.SetCopyContext(vfsservice.ReadSession{Owner: owner, Generation: sessionGenerations[int(owner)-1]}, 0)
	}
	beginServing(owner, session)
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
		if message.Correlation != 0 && operation >= vfs.OperationOpen && operation <= vfs.OperationReadMount {
			if reply, encoded := vfs.EncodeReply(vfs.Reply{Operation: operation, Status: vfs.StatusInvalidArgument}, message.Correlation); encoded {
				send(session, reply)
			}
		}
		return
	}
	if validation.Enabled && request.Operation == vfs.OperationPrepareExecutable && execCrashHandle(owner, request.Handle) {
		// The validation build dies here, with the caller's exec in its
		// preparation stage, as a crashed service would.
		sys.Exit(exitFailed)
	}
	var answer vfs.Reply
	if request.Operation.Bulk() {
		answer = serveBulk(owner, request, attached[0])
	} else if request.Operation == vfs.OperationReadExecutable {
		answer = serveExecutable(owner, request)
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
	if validation.Enabled {
		noteExecCrashOpen(owner, request, answer)
		readPoolProbeOpen(owner, request, answer)
		if withhold(owner, request, reply) {
			if request.Operation == vfs.OperationPrepareExecutable {
				mark("VFS_EXEC_SOURCE_WITHHELD\n")
			}
			return
		}
	}
	send(session, reply)
}

// stallPath is a name the validation build answers late. A request for it
// gets no reply until the same session sends its next request, which lets a
// test observe a client's deadline and then the late reply it must discard.
// The normal build has no such name.
const stallPath = "/tmp/.validation-vfs-stall"

// slowSourcePath is a name whose presence makes the validation build hold
// back every answer to a request to prepare an executable, as a stalled
// filesystem would. A test uses it to watch PID 1 keep serving while a
// service's program is being read (ADR 0175). The normal build has no such
// name.
const slowSourcePath = "/tmp/.validation-slow-source"

func slowSources() bool {
	return service.Dispatch(sessionOwner(0), vfs.Request{Operation: vfs.OperationStat, Path: []byte(slowSourcePath)}).Status == vfs.StatusOK
}

// execCrashPath is a name the validation build dies for. A session that
// opens it and then asks to prepare it as an executable ends the service
// before the reply, which lets a test observe a Linux execve losing its VFS
// in the middle of preparation. The normal build has no such name.
const execCrashPath = "/tmp/.validation-vfs-exec-crash"

// execCrashHandles holds, per session, the handle that opened execCrashPath.
var execCrashHandles [maxSessions]vfs.Handle

func noteExecCrashOpen(owner vfs.Owner, request vfs.Request, answer vfs.Reply) {
	index := int(owner) - 1
	if index >= 0 && index < maxSessions && request.Operation == vfs.OperationOpen &&
		answer.Status == vfs.StatusOK && string(request.Path) == execCrashPath {
		execCrashHandles[index] = answer.Handle
	}
}

func execCrashHandle(owner vfs.Owner, file vfs.Handle) bool {
	index := int(owner) - 1
	return index >= 0 && index < maxSessions && file != 0 && execCrashHandles[index] == file
}

var (
	withheld    [maxSessions]ipc.Message
	withholding [maxSessions]bool
)

// withhold keeps the reply to a request for stallPath, or to a request to
// prepare an executable while slowSourcePath exists, and reports that it
// did.
func withhold(owner vfs.Owner, request vfs.Request, reply ipc.Message) bool {
	index := int(owner) - 1
	if index < 0 || index >= maxSessions || withholding[index] ||
		string(request.Path) != stallPath && !(request.Operation == vfs.OperationPrepareExecutable && slowSources()) {
		return false
	}
	withheld[index], withholding[index] = reply, true
	return true
}

// releaseWithheld sends the reply a session is owed before its next request
// is served, or drops it when the session is gone.
func releaseWithheld(owner vfs.Owner, session uint64, deliver bool) {
	index := int(owner) - 1
	if index < 0 || index >= maxSessions || !withholding[index] {
		return
	}
	withholding[index] = false
	if deliver {
		send(session, withheld[index])
	}
}

// serveExecutable pins a source on its first request and reads one bounded
// range into the cache slot reserved from ordinary cached data. The kernel
// consumes the slot while handling this reply's send.
func serveExecutable(owner vfs.Owner, request vfs.Request) vfs.Reply {
	reply := vfs.Reply{Operation: request.Operation}
	if filesystemState.availability.quarantined.Load() {
		// The executable transport shares the old filesystem's cache object.
		// Do not overwrite a slot an unanswered request may still consume.
		reply.Status = vfs.StatusBackendError
		return reply
	}
	if request.Size == 0 || request.Size > fsproto.PageSize || request.Offset+uint64(request.Size) < request.Offset {
		reply.Status = vfs.StatusInvalidArgument
		return reply
	}
	start := vfsservice.ExecutableSlot * fsproto.PageSize
	target := fileCache[start : start+int(request.Size)]
	count, status := service.ReadFileAt(owner, request.Handle, request.Offset, target)
	reply.Status, reply.Size = status, uint64(count)
	return reply
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
	minimum := uint64(ramfs.MaxName)
	if request.Operation == vfs.OperationReadMount {
		minimum = limits.PathMax
	}
	if size > maxPathObject || request.NameReturn && size < minimum || request.PathsAttached && !request.TakePaths(memory, &pathCopy, &targetCopy) {
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
		answer.AttachedName = uint16(copy(object, answer.Name))
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

// fillBuffer is where a program's bytes pass on their way into the
// executable object a launcher attached.
var fillBuffer [128 << 10]byte

// serveBulk moves a file's bytes to or from the object the client attached
// (ADR 0107). The object is mapped for this request only.
func serveBulk(owner vfs.Owner, request vfs.Request, object handle.ID) vfs.Reply {
	reply := vfs.Reply{Operation: request.Operation}
	if request.Operation == vfs.OperationPrepareExecutable {
		if request.Offset != 0 || request.Size == 0 || request.Size > fsproto.PageSize {
			reply.Status = vfs.StatusInvalidArgument
			return reply
		}
		if reply.Status = service.PrepareExecutable(owner, request.Handle); reply.Status != vfs.StatusOK {
			return reply
		}
		count, status := service.ReadFileAt(owner, request.Handle, 0, fillBuffer[:request.Size])
		reply.Status, reply.Size = status, uint64(count)
		if status == vfs.StatusOK && (count != int(request.Size) || sys.ExecutableWrite(object, fillBuffer[:count]) != kcall.StatusOK) {
			reply.Status = vfs.StatusInvalidArgument
		}
		return reply
	}
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
	end, valid := bulkReadWindow(size, request.Offset, request.Size)
	if !valid {
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
	if !ok {
		fatal("VFS_SEND_FAILED\n")
	}
	status := sys.IPCSend(handle.ID(channel), wire[:size])
	// A client that went while its request was served, such as a process
	// that exited with a page of its program still being read, gets no
	// reply; its session is closed when the loop sees it gone.
	if status != kcall.StatusOK && status != kcall.StatusPeerDied {
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
