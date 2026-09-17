//go:build goos_userspace

package main

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	blockproto "goos/userspace/block"
	"goos/userspace/startup"
	"runtime"
	"unsafe"
)

type memoryBackend struct{ data [4096]byte }

func (backend *memoryBackend) Capacity() uint64  { return uint64(len(backend.data)) }
func (backend *memoryBackend) BlockSize() uint32 { return 512 }
func (backend *memoryBackend) ReadAt(offset uint64, target []byte) blockproto.Status {
	copy(target, backend.data[offset:offset+uint64(len(target))])
	return blockproto.StatusOK
}
func (backend *memoryBackend) WriteAt(offset uint64, data []byte) blockproto.Status {
	copy(backend.data[offset:offset+uint64(len(data))], data)
	return blockproto.StatusOK
}
func (backend *memoryBackend) Flush() blockproto.Status { return blockproto.StatusOK }

var (
	backend       memoryBackend
	service       blockproto.Service
	consoleHandle handle.ID
	serviceHandle handle.ID
	startupWire   [startup.Size]byte
	wire          [ipc.WireMaxSize]byte
	descriptor    [3]uint64
	heapPointer   uintptr
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

//go:linkname startupRead goos_startup_read
func startupRead(address unsafe.Pointer, size uintptr) uint64

//export userspaceMain
func userspaceMain() {
	runtimeInitHeap()
	if startupRead(unsafe.Pointer(&startupWire[0]), startup.Size) != 0 {
		fatal("BLOCK_STARTUP_UNAVAILABLE\n")
	}
	data, ok := startup.Decode(startupWire[:])
	if !ok {
		fatal("BLOCK_STARTUP_INVALID\n")
	}
	consoleHandle, ok = data.Handle("console")
	if !ok {
		fatal("BLOCK_CONSOLE_MISSING\n")
	}
	serviceHandle, ok = data.Handle("block")
	if !ok {
		fatal("BLOCK_ENDPOINT_MISSING\n")
	}
	service, ok = blockproto.NewService(&backend)
	if !ok {
		fatal("BLOCK_SERVICE_INIT_FAILED\n")
	}
	write("BLOCK_SERVICE_OK\n")
	for {
		descriptor[0], descriptor[1], descriptor[2] = uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000
		status := ipcWaitReceive(uint64(serviceHandle), unsafe.Pointer(&descriptor[0]), uintptr(len(descriptor)*8))
		if status == uint64(ipc.StatusTimeout) {
			continue
		}
		if status != 0 {
			fatal("BLOCK_RECEIVE_FAILED\n")
		}
		payloadSize := int(wire[24]) | int(wire[25])<<8
		message, valid := ipc.Decode(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8])
		request, validRequest := blockproto.DecodeRequest(message)
		if !valid || !validRequest {
			continue
		}
		reply, encoded := blockproto.EncodeReply(service.Dispatch(request), message.Correlation)
		size, wireEncoded := ipc.Encode(reply, wire[:])
		if !encoded || !wireEncoded || ipcSend(uint64(serviceHandle), unsafe.Pointer(&wire[0]), uintptr(size)) != uint64(kcall.StatusOK) {
			fatal("BLOCK_REPLY_FAILED\n")
		}
	}
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
		fatal("BLOCK_OUT_OF_MEMORY\n")
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
