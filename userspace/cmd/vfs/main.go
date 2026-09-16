//go:build goos_userspace

package main

import (
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	vfsservice "goos/userspace/vfs"
	"runtime"
	"unsafe"
)

const consoleHandle = uint64(0x0000000100000001)
const channelHandle = uint64(0x0000000100000002)

var (
	service     vfsservice.Service
	wire        [ipc.WireMaxSize]byte
	descriptor  = [3]uint64{uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000}
	heapPointer uintptr
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

//export userspaceMain
func userspaceMain() {
	runtimeInitHeap()
	if !service.Init() {
		fatal("VFS_INIT_FAILED\n")
	}
	write("VFS_SERVICE_OK\n")
	for {
		message, ok := receive()
		if !ok {
			fatal("VFS_RECEIVE_FAILED\n")
		}
		request, ok := vfs.DecodeRequest(message)
		if !ok {
			continue
		}
		reply, ok := vfs.EncodeReply(service.Dispatch(request), message.Correlation)
		if !ok {
			fatal("VFS_REPLY_FAILED\n")
		}
		size, ok := ipc.Encode(reply, wire[:])
		if !ok || ipcSend(channelHandle, unsafe.Pointer(&wire[0]), uintptr(size)) != uint64(kcall.StatusOK) {
			fatal("VFS_SEND_FAILED\n")
		}
	}
}

func receive() (ipc.Message, bool) {
	for {
		descriptor[0] = uint64(uintptr(unsafe.Pointer(&wire[0])))
		status := ipcWaitReceive(channelHandle, unsafe.Pointer(&descriptor[0]), uintptr(len(descriptor)*8))
		if status == uint64(ipc.StatusTimeout) {
			continue
		}
		if status != 0 {
			return ipc.Message{}, false
		}
		payloadSize := int(wire[24]) | int(wire[25])<<8
		size := ipc.WireHeaderSize + payloadSize + int(wire[26])*8
		if size < ipc.WireHeaderSize || size > len(wire) {
			return ipc.Message{}, false
		}
		return ipc.Decode(wire[:size])
	}
}

func write(message string) {
	consoleWrite(consoleHandle, unsafe.Pointer(unsafe.StringData(message)), uintptr(len(message)))
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
