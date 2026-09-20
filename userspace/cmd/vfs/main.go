//go:build goos_userspace

package main

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	blockproto "goos/userspace/block"
	"goos/userspace/startup"
	vfsservice "goos/userspace/vfs"
	userwait "goos/userspace/wait"
	"runtime"
	"unsafe"
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
	descriptor           = [3]uint64{uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000}
	heapPointer          uintptr
	arenaBase            uintptr
	sessions             [maxSessions]uint64
	correlation          uint64
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

//go:extern _heap_start
var heapStartSymbol [0]byte

//go:extern _heap_end
var heapEndSymbol [0]byte

//go:linkname consoleWrite goos_console_write
func consoleWrite(device uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname ipcSend goos_ipc_send
func ipcSend(channel uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname ipcWaitReceive goos_ipc_wait_receive
func ipcWaitReceive(channel uint64, descriptor unsafe.Pointer, size uintptr) uint64

//go:linkname ipcReceive goos_ipc_receive
func ipcReceive(channel uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname handleClose goos_handle_close
func handleClose(id uint64) uint64

//go:linkname startupRead goos_startup_read
func startupRead(address unsafe.Pointer, size uintptr) uint64

//export userspaceMain
func userspaceMain() {
	runtimeInitHeap()
	if startupRead(unsafe.Pointer(&startupWire[0]), startup.Size) != 0 {
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
	sessions[0] = uint64(initialSessionHandle)
	if !service.Init() {
		fatal("VFS_INIT_FAILED\n")
	}
	if !service.MountPersistent(blockDevice{}) {
		fatal("VFS_FAT32_MOUNT_FAILED\n")
	}
	arenaBase = heapPointer
	write("VFS_SERVICE_OK\n")
	write("FAT32_MOUNT_OK\n")
	for {
		heapPointer = arenaBase
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
				handleClose(session)
				sessions[index] = 0
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
	if !ok || ipcSend(uint64(blockHandle), unsafe.Pointer(&blockWire[0]), uintptr(size)) != 0 {
		return blockproto.Reply{}, false
	}
	blockDescriptor[0], blockDescriptor[1], blockDescriptor[2] = uint64(uintptr(unsafe.Pointer(&blockWire[0]))), ipc.WireMaxSize, blockproto.RequestDeadline
	if ipcWaitReceive(uint64(blockHandle), unsafe.Pointer(&blockDescriptor[0]), uintptr(len(blockDescriptor)*8)) != 0 {
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
	server, ok := vfs.DecodeAddSession(message)
	result := vfs.StatusResourceExhausted
	if ok {
		for index := range sessions {
			if sessions[index] == 0 {
				sessions[index], result = uint64(server), vfs.StatusOK
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
	if !ok || ipcSend(channel, unsafe.Pointer(&wire[0]), uintptr(size)) != uint64(kcall.StatusOK) {
		fatal("VFS_SEND_FAILED\n")
	}
}

func receiveNow(channel uint64) (ipc.Message, ipc.Status) {
	status := ipc.Status(ipcReceive(channel, unsafe.Pointer(&wire[0]), ipc.WireMaxSize))
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

func write(message string) {
	consoleWrite(uint64(consoleHandle), unsafe.Pointer(unsafe.StringData(message)), uintptr(len(message)))
}
func fatal(message string) {
	write(message)
	for {
	}
}
func main() {}

//go:linkname runtimeInitHeap runtime.initHeap
func runtimeInitHeap() { heapPointer = uintptr(unsafe.Pointer(&heapStartSymbol)) }

//go:linkname runtimeAlloc runtime.alloc
func runtimeAlloc(size uintptr, _ unsafe.Pointer) unsafe.Pointer {
	size = (size + 15) &^ 15
	start, end := heapPointer, heapPointer+size
	if end < start || end > uintptr(unsafe.Pointer(&heapEndSymbol)) {
		fatal("VFS_OUT_OF_MEMORY\n")
	}
	heapPointer = end
	// Go guarantees zeroed allocations, and the compiler relies on it when it
	// skips clearing large composite literals. The arena is reused after every
	// reset, so stale bytes would otherwise leak into fresh values.
	cleared := unsafe.Slice((*byte)(unsafe.Pointer(start)), size)
	for index := range cleared {
		cleared[index] = 0
	}
	return unsafe.Pointer(start)
}

//go:linkname runtimeFree runtime.free
func runtimeFree(unsafe.Pointer) {}

//go:linkname runtimeMarkRoots runtime.markRoots
func runtimeMarkRoots(uintptr, uintptr) {}

//go:linkname runtimeGC runtime.GC
func runtimeGC() {}

//go:linkname runtimeSetFinalizer runtime.SetFinalizer
func runtimeSetFinalizer(interface{}, interface{}) {}

//go:linkname runtimeReadMemStats runtime.ReadMemStats
func runtimeReadMemStats(*runtime.MemStats) {}
