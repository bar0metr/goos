//go:build goos_userspace

package main

import (
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	blockproto "goos/userspace/block"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/arena"
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
	blockHandle          handle.ID
	startupWire          [startup.Size]byte
	wire                 [ipc.WireMaxSize]byte
	blockWire            [ipc.WireMaxSize]byte
	blockDescriptor      [3]uint64
	arenaBase            arena.Mark
	sessions             [maxSessions]uint64
	correlation          uint64
	waitInterests        *[userwait.MaxObjects]userwait.Interest
	waitCount            int
	liveSessions         int
	persistentMounted    bool
)

type blockDevice struct{}

func (blockDevice) ReadAt(offset uint64, target []byte) bool {
	for len(target) != 0 {
		chunk := len(target)
		if chunk > blockproto.MaxData {
			chunk = blockproto.MaxData
		}
		reply, ok := blockExchange(blockproto.Request{Operation: blockproto.OperationRead, Offset: offset, Length: uint32(chunk)})
		if !ok || reply.Status != blockproto.StatusOK || len(reply.Data) != chunk {
			return false
		}
		copy(target[:chunk], reply.Data)
		target = target[chunk:]
		offset += uint64(chunk)
	}
	return true
}
func (blockDevice) WriteAt(offset uint64, data []byte) bool {
	for len(data) != 0 {
		chunk := len(data)
		if chunk > blockproto.MaxData {
			chunk = blockproto.MaxData
		}
		reply, ok := blockExchange(blockproto.Request{Operation: blockproto.OperationWrite, Offset: offset, Length: uint32(chunk), Data: data[:chunk]})
		if !ok || reply.Status != blockproto.StatusOK {
			return false
		}
		data = data[chunk:]
		offset += uint64(chunk)
	}
	return true
}
func (blockDevice) Flush() bool {
	reply, ok := blockExchange(blockproto.Request{Operation: blockproto.OperationFlush})
	return ok && reply.Status == blockproto.StatusOK
}

// Capacity asks the block service how large the device really is, so the
// filesystem can check the geometry its image declares against it. The answer
// is cached: it does not change while the device is attached, and the
// filesystem asks once per mount.
func (blockDevice) Capacity() uint64 {
	if blockCapacity != 0 {
		return blockCapacity
	}
	reply, ok := blockExchange(blockproto.Request{Operation: blockproto.OperationInfo})
	if !ok || reply.Status != blockproto.StatusOK {
		return 0
	}
	blockCapacity = reply.Capacity
	return blockCapacity
}

var blockCapacity uint64

// mountSystemImage maps the read-only boot image and mounts it at /system, so
// the executables the launch path resolves are the ones a listing shows. The
// capability is the only authority the service has over the image, and the
// mapping it produces is not writable.
func mountSystemImage(imageHandle handle.ID) bool {
	image, status := sys.ImageMap(imageHandle)
	return status == kcall.StatusOK && service.MountSystemImage(image)
}

//export userspaceMain
func userspaceMain() {
	arena.Init()
	arena.OnExhausted(func() { fatal("VFS_OUT_OF_MEMORY\n") })
	if sys.StartupRead(&startupWire) != 0 {
		fatal("VFS_STARTUP_UNAVAILABLE\n")
	}
	startupData, validStartup := startup.Decode(startupWire[:])
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
	blockHandle, found = startupData.Handle("block")
	if !found {
		fatal("VFS_BLOCK_MISSING\n")
	}
	imageHandle, found := startupData.Handle("image")
	if !found {
		fatal("VFS_IMAGE_MISSING\n")
	}
	sessions[0] = uint64(initialSessionHandle)
	liveSessions = 1
	if !service.Init() {
		fatal("VFS_INIT_FAILED\n")
	}
	if !mountSystemImage(imageHandle) {
		fatal("VFS_IMAGE_MOUNT_FAILED\n")
	}
	// A volume that cannot be mounted is not a reason for the system to stop.
	// The root filesystem and the system image are already up, so the machine
	// boots to a prompt with no persistent storage rather than to nothing at
	// all, which is the state from which the volume can be inspected.
	persistentMounted = service.MountPersistent(blockDevice{})
	if !persistentMounted {
		syslog.Error("persistent volume not mounted", syslog.Str("reason", "image rejected or device unavailable"))
		write("[WARN] no persistent storage\n")
	}
	arenaBase = arena.Current()
	mark("VFS_SERVICE_OK\n")
	mark("SYSTEM_IMAGE_MOUNT_OK\n")
	syslog.Info("mounted", syslog.Str("fs", "image"), syslog.Str("path", "/system"))
	if persistentMounted {
		mark("FAT32_MOUNT_OK\n")
		syslog.Info("mounted", syslog.Str("fs", "fat32"), syslog.Str("path", "/persist"))
	}
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
			for {
			}
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
				sys.HandleClose(handle.ID(session))
				sessions[index] = 0
				liveSessions--
				continue
			}
			if receiveStatus != ipc.StatusOK {
				fatal("VFS_RECEIVE_FAILED\n")
			}
			serve(session, message)
		}
	}
}

func blockExchange(request blockproto.Request) (blockproto.Reply, bool) {
	correlation++
	message, ok := blockproto.EncodeRequest(request, correlation)
	if !ok {
		return blockproto.Reply{}, false
	}
	size, ok := ipc.Encode(message, blockWire[:])
	if !ok || sys.IPCSend(blockHandle, blockWire[:size]) != 0 {
		return blockproto.Reply{}, false
	}
	if sys.IPCWaitReceive(blockHandle, blockWire[:], blockproto.RequestDeadline) != 0 {
		return blockproto.Reply{}, false
	}
	payloadSize := int(blockWire[24]) | int(blockWire[25])<<8
	received, valid := ipc.Decode(blockWire[:ipc.WireHeaderSize+payloadSize+int(blockWire[26])*8])
	reply, validReply := blockproto.DecodeReply(received)
	return reply, valid && validReply && received.Correlation == correlation
}

func pollControl() bool {
	message, status := receiveNow(uint64(controlHandle))
	if status == ipc.StatusNotFound {
		return false
	}
	if status != ipc.StatusOK {
		fatal("VFS_CONTROL_FAILED\n")
	}
	if vfs.IsSync(message) {
		result := vfs.StatusBackendError
		if (blockDevice{}).Flush() {
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

func serve(session uint64, message ipc.Message) {
	request, ok := vfs.DecodeRequest(message)
	if !ok {
		// Answer unusable requests: a silent drop strands the caller, which is
		// waiting for exactly this correlation.
		operation := vfs.Operation(message.Payload[0])
		if message.Correlation != 0 && operation >= vfs.OperationOpen && operation <= vfs.OperationTruncate {
			if reply, encoded := vfs.EncodeReply(vfs.Reply{Operation: operation, Status: vfs.StatusInvalidArgument}, message.Correlation); encoded {
				send(session, reply)
			}
		}
		return
	}
	reply, ok := vfs.EncodeReply(service.Dispatch(request), message.Correlation)
	if !ok {
		fatal("VFS_REPLY_FAILED\n")
	}
	send(session, reply)
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
func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		write(message)
	}
	if sys.Diagnostics(consoleHandle) == 0 {
		writeSessions()
	}
	for {
	}
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
