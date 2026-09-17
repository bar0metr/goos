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

type driverBackend struct {
	capacity  uint64
	blockSize uint32
}

func (backend *driverBackend) Capacity() uint64  { return backend.capacity }
func (backend *driverBackend) BlockSize() uint32 { return backend.blockSize }
func (backend *driverBackend) ReadAt(offset uint64, target []byte) blockproto.Status {
	reply, ok := exchange(blockproto.Request{Operation: blockproto.OperationRead, Offset: offset, Length: uint32(len(target))})
	if !ok || reply.Status != blockproto.StatusOK || len(reply.Data) != len(target) {
		return blockproto.StatusIOError
	}
	copy(target, reply.Data)
	return blockproto.StatusOK
}
func (backend *driverBackend) WriteAt(offset uint64, data []byte) blockproto.Status {
	reply, ok := exchange(blockproto.Request{Operation: blockproto.OperationWrite, Offset: offset, Length: uint32(len(data)), Data: data})
	if !ok {
		return blockproto.StatusIOError
	}
	return reply.Status
}
func (backend *driverBackend) Flush() blockproto.Status {
	reply, ok := exchange(blockproto.Request{Operation: blockproto.OperationFlush})
	if !ok {
		return blockproto.StatusIOError
	}
	return reply.Status
}

var (
	backend       driverBackend
	service       blockproto.Service
	consoleHandle handle.ID
	serviceHandle handle.ID
	driverHandle  handle.ID
	startupWire   [startup.Size]byte
	wire          [ipc.WireMaxSize]byte
	descriptor    [3]uint64
	heapPointer   uintptr
	correlation   uint64
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
	driverHandle, ok = data.Handle("driver")
	if !ok {
		fatal("BLOCK_DRIVER_MISSING\n")
	}
	info, ok := exchange(blockproto.Request{Operation: blockproto.OperationInfo})
	if !ok || info.Status != blockproto.StatusOK || info.Capacity == 0 || info.BlockSize != 512 {
		fatal("BLOCK_DRIVER_INFO_FAILED\n")
	}
	backend.capacity, backend.blockSize = info.Capacity, info.BlockSize
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

func exchange(request blockproto.Request) (blockproto.Reply, bool) {
	correlation++
	message, ok := blockproto.EncodeRequest(request, correlation)
	if !ok {
		return blockproto.Reply{}, false
	}
	size, ok := ipc.Encode(message, wire[:])
	if !ok || ipcSend(uint64(driverHandle), unsafe.Pointer(&wire[0]), uintptr(size)) != 0 {
		return blockproto.Reply{}, false
	}
	descriptor[0], descriptor[1], descriptor[2] = uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000
	if ipcWaitReceive(uint64(driverHandle), unsafe.Pointer(&descriptor[0]), uintptr(len(descriptor)*8)) != 0 {
		return blockproto.Reply{}, false
	}
	payloadSize := int(wire[24]) | int(wire[25])<<8
	received, valid := ipc.Decode(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8])
	reply, validReply := blockproto.DecodeReply(received)
	return reply, valid && validReply && received.Correlation == correlation
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
