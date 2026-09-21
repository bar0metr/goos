//go:build goos_userspace

package main

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	blockproto "goos/userspace/block"
	"goos/userspace/startup"
	userwait "goos/userspace/wait"
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
	backend          driverBackend
	service          blockproto.Service
	consoleHandle    handle.ID
	serviceHandle    handle.ID
	filesystemHandle handle.ID
	driverHandle     handle.ID
	startupWire      [startup.Size]byte
	wire             [ipc.WireMaxSize]byte
	driverWire       [ipc.WireMaxSize]byte
	requestMessage   ipc.Message
	replyMessage     ipc.Message
	driverRequest    ipc.Message
	driverReply      ipc.Message
	driverDescriptor [3]uint64
	heapPointer      uintptr
	arenaBase        uintptr
	correlation      uint64
)

//go:extern _heap_start
var heapStartSymbol [0]byte

//go:extern _heap_end
var heapEndSymbol [0]byte

//go:linkname diagnostics goos_diagnostics
func diagnostics(device uint64) uint64

//go:linkname consoleWrite goos_console_write
func consoleWrite(device uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname ipcSend goos_ipc_send
func ipcSend(channel uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname ipcWaitReceive goos_ipc_wait_receive
func ipcWaitReceive(channel uint64, descriptor unsafe.Pointer, size uintptr) uint64

//go:linkname ipcReceive goos_ipc_receive
func ipcReceive(channel uint64, address unsafe.Pointer, size uintptr) uint64

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
	filesystemHandle, ok = data.Handle("filesystem")
	if !ok {
		fatal("BLOCK_FILESYSTEM_MISSING\n")
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
	arenaBase = heapPointer
	write("BLOCK_SERVICE_OK\n")
	for {
		heapPointer = arenaBase
		endpoints := [...]handle.ID{serviceHandle, filesystemHandle}
		var interests [userwait.MaxObjects]userwait.Interest
		for index, endpoint := range endpoints {
			interests[index] = userwait.Interest{Handle: endpoint, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		}
		result, status := userwait.Any(&interests, len(endpoints), kcall.WaitAnyInfiniteTimeout)
		if status != kcall.StatusOK {
			fatal("BLOCK_WAIT_FAILED\n")
		}
		for index, endpoint := range endpoints {
			if result.ReadyMask&(1<<index) == 0 {
				continue
			}
			if ipcReceive(uint64(endpoint), unsafe.Pointer(&wire[0]), ipc.WireMaxSize) != uint64(ipc.StatusOK) {
				fatal("BLOCK_RECEIVE_FAILED\n")
			}
			serve(endpoint)
		}
	}
}

func serve(endpoint handle.ID) {
	payloadSize := int(wire[24]) | int(wire[25])<<8
	valid := ipc.DecodeInto(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8], &requestMessage)
	if !valid {
		return
	}
	request, validRequest := blockproto.DecodeRequestFrom(&requestMessage)
	if !validRequest {
		reject(endpoint)
		return
	}
	encoded := blockproto.EncodeReplyInto(service.Dispatch(request), requestMessage.Correlation, &replyMessage)
	size, wireEncoded := ipc.EncodeFrom(&replyMessage, wire[:])
	if !encoded || !wireEncoded || ipcSend(uint64(endpoint), unsafe.Pointer(&wire[0]), uintptr(size)) != uint64(kcall.StatusOK) {
		fatal("BLOCK_REPLY_FAILED\n")
	}
}

// reject answers a request this service cannot parse. Staying silent would
// strand the caller until its deadline expires instead of reporting the fault.
func reject(endpoint handle.ID) {
	operation := blockproto.Operation(requestMessage.Payload[0])
	if requestMessage.Correlation == 0 || operation < blockproto.OperationInfo || operation > blockproto.OperationFlush {
		return
	}
	if !blockproto.EncodeReplyInto(blockproto.Reply{Operation: operation, Status: blockproto.StatusInvalidArgument}, requestMessage.Correlation, &replyMessage) {
		return
	}
	if size, ok := ipc.EncodeFrom(&replyMessage, wire[:]); ok {
		ipcSend(uint64(endpoint), unsafe.Pointer(&wire[0]), uintptr(size))
	}
}

func exchange(request blockproto.Request) (blockproto.Reply, bool) {
	correlation++
	if !blockproto.EncodeRequestInto(request, correlation, &driverRequest) {
		return blockproto.Reply{}, false
	}
	size, ok := ipc.EncodeFrom(&driverRequest, driverWire[:])
	if !ok || ipcSend(uint64(driverHandle), unsafe.Pointer(&driverWire[0]), uintptr(size)) != 0 {
		return blockproto.Reply{}, false
	}
	driverDescriptor[0], driverDescriptor[1], driverDescriptor[2] = uint64(uintptr(unsafe.Pointer(&driverWire[0]))), ipc.WireMaxSize, blockproto.RequestDeadline
	if ipcWaitReceive(uint64(driverHandle), unsafe.Pointer(&driverDescriptor[0]), uintptr(len(driverDescriptor)*8)) != 0 {
		return blockproto.Reply{}, false
	}
	payloadSize := int(driverWire[24]) | int(driverWire[25])<<8
	valid := ipc.DecodeInto(driverWire[:ipc.WireHeaderSize+payloadSize+int(driverWire[26])*8], &driverReply)
	reply, validReply := blockproto.DecodeReplyFrom(&driverReply)
	return reply, valid && validReply && driverReply.Correlation == correlation
}

func write(message string) {
	consoleWrite(uint64(consoleHandle), unsafe.Pointer(unsafe.StringData(message)), uintptr(len(message)))
}
func fatal(message string) {
	write(message)
	diagnostics(uint64(consoleHandle))
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
