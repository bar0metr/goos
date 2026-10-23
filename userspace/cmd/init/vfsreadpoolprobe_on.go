//go:build goos_userspace && goos_vfs_probe

package main

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/runtime/sys"
	"goos/userspace/utility"
)

const (
	vfsReadPoolLocalPath      = "/tmp/.validation-readpool-local"
	vfsReadPoolWriterPath     = "/tmp/.validation-readpool-writer"
	vfsReadPoolPersistentPath = "/persist/.validation-readpool-slow"
	vfsReadPoolDeadPath       = "/persist/.validation-readpool-dead"
	vfsReadPoolImagePath      = "/bootstrap/vfs"
	vfsReadPoolRounds         = 4096
)

var vfsReadPoolData = []byte("vfs goroutine acceptance data")

type vfsReadPoolPending struct {
	session     handle.ID
	correlation uint64
	operation   vfs.Operation
}

// probeVFSReadPool is an explicit image-build acceptance fixture. PID 1 is the
// only process allowed to create the independent client sessions it needs; no
// command, grant, control endpoint, or production protocol is added.
func probeVFSReadPool() bool {
	mark("VFS_READPOOL_BEGIN\n")
	var clients [6]handle.ID
	for index := range clients {
		var reason string
		clients[index], reason = openVFSSession()
		if reason != "" {
			closeVFSReadPoolClients(&clients)
			return vfsReadPoolFailed("session")
		}
	}
	defer closeVFSReadPoolClients(&clients)

	const (
		ramfsClient = iota
		imageClient
		imageCheckClient
		persistentClient
		writerClient
		deadClient
	)
	if !writeVFSReadPoolFile(clients[ramfsClient], vfsReadPoolLocalPath, vfsReadPoolData[:]) ||
		!writeVFSReadPoolFile(clients[persistentClient], vfsReadPoolPersistentPath, vfsReadPoolData[:]) ||
		!writeVFSReadPoolFile(clients[persistentClient], vfsReadPoolDeadPath, vfsReadPoolData[:]) ||
		!writeVFSReadPoolFile(clients[writerClient], vfsReadPoolWriterPath, nil) {
		return vfsReadPoolFailed("prepare-files")
	}

	ramfs, ok := openVFSReadPoolFile(clients[ramfsClient], vfsReadPoolLocalPath, vfs.Read)
	if !ok {
		return vfsReadPoolFailed("open-ramfs")
	}
	image, ok := openVFSReadPoolFile(clients[imageClient], vfsReadPoolImagePath, vfs.Read)
	if !ok {
		return vfsReadPoolFailed("open-image")
	}
	imageCheck, ok := openVFSReadPoolFile(clients[imageCheckClient], vfsReadPoolImagePath, vfs.Read)
	if !ok {
		return vfsReadPoolFailed("open-image-check")
	}
	persistent, ok := openVFSReadPoolFile(clients[persistentClient], vfsReadPoolPersistentPath, vfs.Read)
	if !ok {
		return vfsReadPoolFailed("open-persistent")
	}
	writer, ok := openVFSReadPoolFile(clients[writerClient], vfsReadPoolWriterPath, vfs.Write)
	if !ok {
		return vfsReadPoolFailed("open-writer")
	}
	dead, ok := openVFSReadPoolFile(clients[deadClient], vfsReadPoolDeadPath, vfs.Read)
	if !ok {
		return vfsReadPoolFailed("open-dead")
	}

	// Two independent local clients must occupy the two workers together.
	ramfsRead, ok := sendVFSReadPoolRead(clients[ramfsClient], ramfs, uint32(len(vfsReadPoolData)))
	if !ok {
		return vfsReadPoolFailed("send-ramfs")
	}
	imageRead, ok := sendVFSReadPoolRead(clients[imageClient], image, uint32(len(vfsReadPoolData)))
	if !ok {
		return vfsReadPoolFailed("send-image")
	}
	ramfsReply, ok := receiveVFSReadPoolRead(ramfsRead)
	if !ok {
		return vfsReadPoolFailed("reply-ramfs")
	}
	if !sameBytes(ramfsReply.Data, vfsReadPoolData[:]) {
		vfsReadPoolReplyFailed("ramfs-bytes", uint64(len(ramfsReply.Data)))
		return vfsReadPoolFailed("reply-ramfs")
	}
	imageReply, ok := receiveVFSReadPoolRead(imageRead)
	if !ok {
		return vfsReadPoolFailed("reply-image")
	}
	imageHash := hashVFSReadPool(imageReply.Data)
	imageCheckRead, ok := sendVFSReadPoolRead(clients[imageCheckClient], imageCheck, uint32(len(vfsReadPoolData)))
	if !ok {
		return vfsReadPoolFailed("send-image-check")
	}
	imageCheckReply, ok := receiveVFSReadPoolRead(imageCheckRead)
	if !ok || imageHash != hashVFSReadPool(imageCheckReply.Data) || len(imageReply.Data) != len(imageCheckReply.Data) {
		return vfsReadPoolFailed("reply-image-check")
	}
	writeVFSReadPoolHashes(hashVFSReadPool(ramfsReply.Data), imageHash)

	mark("VFS_READPOOL_STEP local\n")
	// The selected persistent plan reports entry from the VFS owner. Only then
	// do we enqueue local reads plus write and control barriers behind it.
	persistentRead, ok := sendVFSReadPoolRead(clients[persistentClient], persistent, uint32(len(vfsReadPoolData)))
	if !ok || !awaitVFSReadPoolEntry() {
		return vfsReadPoolFailed("persistent-entry")
	}
	localDuringPersistent, ok := sendVFSReadPoolRead(clients[ramfsClient], ramfs, uint32(len(vfsReadPoolData)))
	if !ok {
		return vfsReadPoolFailed("send-local-during")
	}
	imageDuringPersistent, ok := sendVFSReadPoolRead(clients[imageClient], image, uint32(len(vfsReadPoolData)))
	if !ok || !awaitVFSReadPoolLocal() {
		return vfsReadPoolFailed("send-image-during-or-local-ready")
	}
	writePending, ok := sendVFSReadPoolWrite(clients[writerClient], writer, []byte("gate"))
	if !ok {
		return vfsReadPoolFailed("send-write")
	}
	correlation++
	syncRequest, encoded := vfs.SyncRequest(correlation)
	if !encoded || !sendMessage(vfsControlHandle, syncRequest) {
		return vfsReadPoolFailed("send-sync")
	}
	syncCorrelation := correlation
	if _, ok = receiveVFSReadPoolRead(localDuringPersistent); !ok {
		return vfsReadPoolFailed("reply-local-during")
	}
	if _, ok = receiveVFSReadPoolRead(imageDuringPersistent); !ok {
		return vfsReadPoolFailed("reply-image-during")
	}
	persistentReply, ok := receiveVFSReadPoolRead(persistentRead)
	if !ok || !sameBytes(persistentReply.Data, vfsReadPoolData[:]) {
		return vfsReadPoolFailed("reply-persistent")
	}
	if _, ok = receiveVFSReadPoolRead(writePending); !ok || !receiveVFSReadPoolSync(syncCorrelation) {
		return vfsReadPoolFailed("reply-write-sync")
	}

	mark("VFS_READPOOL_STEP barriers\n")
	// Close the only client endpoint after transferring a writable page to a
	// ReadInto request. The VFS must unmap/close it before its peer-death close.
	if !sendVFSReadPoolDeadRead(clients[deadClient], dead) {
		return vfsReadPoolFailed("send-dead")
	}
	sys.HandleClose(clients[deadClient])
	clients[deadClient] = 0

	mark("VFS_READPOOL_STEP peer-death\n")
	// This long portion is selected only by VFS_PROBE=1. It produces four VFS
	// memory samples through the real concurrent-driver read/retire path.
	for round := 0; round < vfsReadPoolRounds; round++ {
		reply, received := callSession(clients[ramfsClient], vfs.Request{Operation: vfs.OperationRead, Handle: ramfs, Size: uint32(len(vfsReadPoolData))})
		if !received || reply.Status != vfs.StatusOK {
			return vfsReadPoolFailed("plateau-read")
		}
	}
	mark("VFS_READPOOL_OK\n")
	return true
}

// vfsReadPoolFailed names the step that failed and reports failure.
func vfsReadPoolFailed(step string) bool {
	mark("VFS_READPOOL_STEP_FAILED " + step + "\n")
	return false
}

func closeVFSReadPoolClients(clients *[6]handle.ID) {
	for index, client := range clients {
		if client != 0 {
			sys.HandleClose(client)
			clients[index] = 0
		}
	}
}

func writeVFSReadPoolFile(session handle.ID, path string, data []byte) bool {
	created, ok := callSession(session, vfs.Request{Operation: vfs.OperationCreate, Path: []byte(path)})
	if !ok || created.Status != vfs.StatusOK && created.Status != vfs.StatusAlreadyExists {
		return false
	}
	truncated, ok := callSession(session, vfs.Request{Operation: vfs.OperationTruncate, Path: []byte(path)})
	if !ok || truncated.Status != vfs.StatusOK {
		return false
	}
	if len(data) == 0 {
		return true
	}
	file, ok := openVFSReadPoolFile(session, path, vfs.Write)
	if !ok {
		return false
	}
	written, ok := callSession(session, vfs.Request{Operation: vfs.OperationWrite, Handle: file, Data: data})
	closed, closedOK := callSession(session, vfs.Request{Operation: vfs.OperationClose, Handle: file})
	return ok && written.Status == vfs.StatusOK && written.Size == uint64(len(data)) && closedOK && closed.Status == vfs.StatusOK
}

func openVFSReadPoolFile(session handle.ID, path string, rights vfs.Rights) (vfs.Handle, bool) {
	reply, ok := callSession(session, vfs.Request{Operation: vfs.OperationOpen, Rights: rights, Path: []byte(path)})
	return reply.Handle, ok && reply.Status == vfs.StatusOK && reply.Handle != 0
}

func sendVFSReadPoolRead(session handle.ID, file vfs.Handle, size uint32) (vfsReadPoolPending, bool) {
	return sendVFSReadPoolRequest(session, vfs.Request{Operation: vfs.OperationRead, Handle: file, Size: size}, 0)
}

func sendVFSReadPoolWrite(session handle.ID, file vfs.Handle, data []byte) (vfsReadPoolPending, bool) {
	return sendVFSReadPoolRequest(session, vfs.Request{Operation: vfs.OperationWrite, Handle: file, Data: data}, 0)
}

func sendVFSReadPoolRequest(session handle.ID, request vfs.Request, attached handle.ID) (vfsReadPoolPending, bool) {
	correlation++
	message, encoded := vfs.EncodeRequest(request, correlation)
	if !encoded {
		if attached != 0 {
			sys.HandleClose(attached)
		}
		return vfsReadPoolPending{}, false
	}
	if attached != 0 {
		message.HandleCount, message.Handles[0] = 1, attached
	}
	if !sendMessage(uint64(session), message) {
		if attached != 0 {
			sys.HandleClose(attached)
		}
		return vfsReadPoolPending{}, false
	}
	return vfsReadPoolPending{session: session, correlation: correlation, operation: request.Operation}, true
}

func receiveVFSReadPoolRead(pending vfsReadPoolPending) (vfs.Reply, bool) {
	message, status := receive(uint64(pending.session), vfsSessionDeadline)
	if status != ipc.StatusOK {
		return vfs.Reply{}, vfsReadPoolReplyFailed("receive", uint64(status))
	}
	if message.Correlation != pending.correlation {
		return vfs.Reply{}, vfsReadPoolReplyFailed("correlation", message.Correlation)
	}
	reply, decoded := vfs.DecodeReply(message)
	if !decoded || reply.Operation != pending.operation {
		return vfs.Reply{}, vfsReadPoolReplyFailed("decode", uint64(reply.Operation))
	}
	if reply.Status != vfs.StatusOK {
		return vfs.Reply{}, vfsReadPoolReplyFailed("status", uint64(reply.Status))
	}
	return reply, true
}

// vfsReadPoolReplyFailed records why a reply was refused.
func vfsReadPoolReplyFailed(reason string, value uint64) bool {
	var line [96]byte
	text := append(line[:0], "VFS_READPOOL_REPLY_FAILED "...)
	text = append(text, reason...)
	text = append(text, ' ')
	text = utility.AppendUint(text, value)
	mark(string(append(text, '\n')))
	return false
}

// The VFS reports the fixture's events as control notifications whose one
// payload byte names the event.
const (
	vfsReadPoolPersistentEntered byte = 1
	vfsReadPoolLocalCompleted    byte = 2
)

func awaitVFSReadPoolEntry() bool { return awaitVFSReadPoolEvent(vfsReadPoolPersistentEntered) }

func awaitVFSReadPoolLocal() bool { return awaitVFSReadPoolEvent(vfsReadPoolLocalCompleted) }

func awaitVFSReadPoolEvent(event byte) bool {
	message, status := receive(vfsControlHandle, vfsSessionDeadline)
	if status != ipc.StatusOK {
		return vfsReadPoolReplyFailed("event-receive", uint64(status))
	}
	if message.ProtocolID != vfs.ControlProtocolID || message.Kind != ipc.KindNotification || message.PayloadSize != 1 || message.Payload[0] != event {
		return vfsReadPoolReplyFailed("event", uint64(message.Payload[0]))
	}
	return true
}

func receiveVFSReadPoolSync(correlation uint64) bool {
	message, status := receive(vfsControlHandle, vfsSessionDeadline)
	if status != ipc.StatusOK || message.Correlation != correlation {
		return false
	}
	result, decoded := vfs.DecodeAddSessionReply(message)
	return decoded && result == vfs.StatusOK
}

func sendVFSReadPoolDeadRead(session handle.ID, file vfs.Handle) bool {
	memory, status := sys.MemoryCreate(1)
	if status != kcall.StatusOK {
		return false
	}
	attached, status := sys.HandleDuplicate(memory, capability.Read|capability.Write|capability.Transfer)
	if status != kcall.StatusOK {
		sys.HandleClose(memory)
		return false
	}
	_, sent := sendVFSReadPoolRequest(session, vfs.Request{Operation: vfs.OperationReadInto, Handle: file, Size: uint32(len(vfsReadPoolData))}, attached)
	sys.HandleClose(memory)
	return sent
}

func hashVFSReadPool(data []byte) uint64 {
	result := uint64(1469598103934665603)
	for _, value := range data {
		result ^= uint64(value)
		result *= 1099511628211
	}
	return result
}

func writeVFSReadPoolHashes(ramfs, image uint64) {
	var line [96]byte
	prefix := "VFS_READPOOL_HASH ramfs=0x"
	size := copy(line[:], prefix)
	for shift := uint(60); ; shift -= 4 {
		line[size] = "0123456789abcdef"[(ramfs>>shift)&0xf]
		size++
		if shift == 0 {
			break
		}
	}
	size += copy(line[size:], " image=0x")
	for shift := uint(60); ; shift -= 4 {
		line[size] = "0123456789abcdef"[(image>>shift)&0xf]
		size++
		if shift == 0 {
			break
		}
	}
	line[size] = '\n'
	sys.ConsoleWrite(serviceDeviceHandle, line[:size+1])
}

func sameBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
