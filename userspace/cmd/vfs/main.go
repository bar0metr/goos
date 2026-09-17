//go:build goos_userspace

package main

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/startup"
	vfsservice "goos/userspace/vfs"
	"runtime"
	"unsafe"
)

const maxSessions = 12

var (
	service              vfsservice.Service
	consoleHandle        handle.ID
	initialSessionHandle handle.ID
	controlHandle        handle.ID
	startupWire          [startup.Size]byte
	wire                 [ipc.WireMaxSize]byte
	descriptor           = [3]uint64{uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000}
	heapPointer          uintptr
	arenaBase            uintptr
	sessions             [maxSessions]uint64
)

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
	sessions[0] = uint64(initialSessionHandle)
	if !service.Init() {
		fatal("VFS_INIT_FAILED\n")
	}
	arenaBase = heapPointer
	write("VFS_SERVICE_OK\n")
	for {
		heapPointer = arenaBase
		worked := pollControl()
		for index, session := range sessions {
			if session == 0 {
				continue
			}
			message, status := receiveNow(session)
			if status == ipc.StatusNotFound {
				continue
			}
			if status == ipc.StatusPeerDied {
				handleClose(session)
				sessions[index] = 0
				worked = true
				continue
			}
			if status != ipc.StatusOK {
				fatal("VFS_RECEIVE_FAILED\n")
			}
			serve(session, message)
			worked = true
		}
		if !worked {
			descriptor[0], descriptor[1], descriptor[2] = uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 1
			ipcWaitReceive(uint64(controlHandle), unsafe.Pointer(&descriptor[0]), uintptr(len(descriptor)*8))
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
