//go:build goos_userspace

package main

import (
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	blockproto "goos/userspace/block"
	syslog "goos/userspace/log"
	"goos/userspace/startup"
	"runtime"
	"unsafe"
)

const (
	dmaVirtualAddress = uint64(0x401c0000)
	sectorSize        = uint64(512)
	virtioRead        = uint32(0)
	virtioWrite       = uint32(1)
	virtioFlush       = uint32(4)
)

var (
	consoleHandle  handle.ID
	portHandle     handle.ID
	irqHandle      handle.ID
	dmaHandle      handle.ID
	serviceHandle  handle.ID
	startupWire    [startup.Size]byte
	dmaDescriptor  [3]uint64
	wire           [ipc.WireMaxSize]byte
	waitDescriptor [3]uint64
	requestMessage ipc.Message
	replyMessage   ipc.Message
	portValue      uint32
	heapPointer    uintptr
	queueSize      uint16
	capacity       uint64
	availableIndex uint16
	usedIndex      uint16
	irqConfirmed   bool
	usedOffset     uint64
	dataOffset     uint64
	headerOffset   uint64
	statusOffset   uint64
)

//go:extern _heap_start
var heapStartSymbol [0]byte

//go:extern _heap_end
var heapEndSymbol [0]byte

//go:linkname consoleWrite goos_console_write
func consoleWrite(device uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname startupRead goos_startup_read
func startupRead(address unsafe.Pointer, size uintptr) uint64

//go:linkname portRead goos_port_read
func portRead(device uint64, specification uint64, address unsafe.Pointer) uint64

//go:linkname portWrite goos_port_write
func portWrite(device uint64, specification uint64, value uint64) uint64

//go:linkname dmaAllocate goos_dma_allocate
func dmaAllocate(device uint64, virtual uint64, address unsafe.Pointer) uint64

//go:linkname ipcSend goos_ipc_send
func ipcSend(channel uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname ipcWaitReceive goos_ipc_wait_receive
func ipcWaitReceive(channel uint64, descriptor unsafe.Pointer, size uintptr) uint64

//go:linkname irqWait goos_irq_wait
func irqWait(irq uint64) uint64

func specification(offset uint16, width uint8) uint64 { return uint64(offset) | uint64(width)<<16 }

func portIn(offset uint16, width uint8) (uint32, bool) {
	portValue = 0
	ok := portRead(uint64(portHandle), specification(offset, width), unsafe.Pointer(&portValue)) == 0
	return portValue, ok
}

func portOut(offset uint16, width uint8, value uint32) bool {
	return portWrite(uint64(portHandle), specification(offset, width), uint64(value)) == 0
}

//export userspaceMain
func userspaceMain() {
	runtimeInitHeap()
	if startupRead(unsafe.Pointer(&startupWire[0]), startup.Size) != 0 {
		fatal("VIRTIO_BLK_STARTUP_FAILED\n")
	}
	data, ok := startup.Decode(startupWire[:])
	if !ok {
		fatal("VIRTIO_BLK_STARTUP_INVALID\n")
	}
	consoleHandle, ok = data.Handle("console")
	syslog.Init(consoleHandle, "virtio-blk")
	if !ok {
		fatal("VIRTIO_BLK_CONSOLE_MISSING\n")
	}
	portHandle, ok = data.Handle("ports")
	if !ok {
		fatal("VIRTIO_BLK_PORTS_MISSING\n")
	}
	irqHandle, ok = data.Handle("irq")
	if !ok {
		fatal("VIRTIO_BLK_IRQ_MISSING\n")
	}
	dmaHandle, ok = data.Handle("dma")
	if !ok {
		fatal("VIRTIO_BLK_DMA_MISSING\n")
	}
	serviceHandle, ok = data.Handle("service")
	if !ok {
		fatal("VIRTIO_BLK_SERVICE_MISSING\n")
	}
	if !initializeDevice() {
		fatal("VIRTIO_BLK_INIT_FAILED\n")
	}
	mark("VIRTIO_BLK_CAPABILITIES_OK\n")
	mark("VIRTIO_BLK_DRIVER_OK\n")
	syslog.Info("device ready", syslog.Uint("capacity-mib", capacity>>20), syslog.Uint("queue", uint64(queueSize)))
	serve()
}

func initializeDevice() bool {
	if !portOut(18, 1, 0) || !portOut(18, 1, 1) || !portOut(18, 1, 3) {
		write("VIRTIO_BLK_STATUS_FAILED\n")
		return false
	}
	features, featuresOK := portIn(0, 4)
	if !featuresOK || features == 0 || !portOut(4, 4, features&(1<<9)) || !portOut(14, 2, 0) {
		write("VIRTIO_BLK_FEATURES_FAILED\n")
		return false
	}
	queue, queueOK := portIn(12, 2)
	capacityLow, lowOK := portIn(20, 4)
	capacityHigh, highOK := portIn(24, 4)
	queueSize, capacity = uint16(queue), (uint64(capacityHigh)<<32|uint64(capacityLow))*sectorSize
	if !queueOK || !lowOK || !highOK || queueSize < 3 || queueSize > 256 || capacity == 0 {
		write("VIRTIO_BLK_CONFIG_FAILED\n")
		return false
	}
	usedOffset = align(uint64(16)*uint64(queueSize)+4+uint64(queueSize)*2+2, 4096)
	dataOffset = align(usedOffset+4+uint64(queueSize)*8+2, 1024)
	headerOffset, statusOffset = dataOffset+1024, dataOffset+1040
	if statusOffset >= 16384 || dmaAllocate(uint64(dmaHandle), dmaVirtualAddress, unsafe.Pointer(&dmaDescriptor[0])) != 0 || dmaDescriptor[0] == 0 || dmaDescriptor[1] != dmaVirtualAddress || dmaDescriptor[2] != 16384 {
		write("VIRTIO_BLK_DMA_INIT_FAILED\n")
		return false
	}
	if !portOut(8, 4, uint32(dmaDescriptor[0]>>12)) || !portOut(18, 1, 7) {
		write("VIRTIO_BLK_QUEUE_ACTIVATE_FAILED\n")
		return false
	}
	return true
}

func serve() {
	for {
		waitDescriptor[0], waitDescriptor[1], waitDescriptor[2] = uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000
		status := ipcWaitReceive(uint64(serviceHandle), unsafe.Pointer(&waitDescriptor[0]), uintptr(len(waitDescriptor)*8))
		if status == uint64(ipc.StatusTimeout) {
			continue
		}
		if status != 0 {
			fatal("VIRTIO_BLK_RECEIVE_FAILED\n")
		}
		payloadSize := int(wire[24]) | int(wire[25])<<8
		if !ipc.DecodeInto(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8], &requestMessage) {
			continue
		}
		request, validRequest := blockproto.DecodeRequestFrom(&requestMessage)
		if !validRequest {
			reject()
			continue
		}
		encoded := blockproto.EncodeReplyInto(dispatch(request), requestMessage.Correlation, &replyMessage)
		size, wireEncoded := ipc.EncodeFrom(&replyMessage, wire[:])
		if !encoded || !wireEncoded || ipcSend(uint64(serviceHandle), unsafe.Pointer(&wire[0]), uintptr(size)) != uint64(kcall.StatusOK) {
			fatal("VIRTIO_BLK_REPLY_FAILED\n")
		}
	}
}

// reject answers a request the driver cannot parse instead of dropping it and
// stranding the block service until its deadline expires.
func reject() {
	operation := blockproto.Operation(requestMessage.Payload[0])
	if requestMessage.Correlation == 0 || operation < blockproto.OperationInfo || operation > blockproto.OperationFlush {
		return
	}
	if !blockproto.EncodeReplyInto(blockproto.Reply{Operation: operation, Status: blockproto.StatusInvalidArgument}, requestMessage.Correlation, &replyMessage) {
		return
	}
	if size, ok := ipc.EncodeFrom(&replyMessage, wire[:]); ok {
		ipcSend(uint64(serviceHandle), unsafe.Pointer(&wire[0]), uintptr(size))
	}
}

func dispatch(request blockproto.Request) blockproto.Reply {
	reply := blockproto.Reply{Operation: request.Operation}
	switch request.Operation {
	case blockproto.OperationInfo:
		reply.Capacity, reply.BlockSize = capacity, uint32(sectorSize)
	case blockproto.OperationRead:
		if !rangeValid(request.Offset, uint64(request.Length)) {
			reply.Status = blockproto.StatusOutOfRange
			break
		}
		first, count, start := sectorRange(request.Offset, uint64(request.Length))
		if !transfer(virtioRead, first, count) {
			write("VIRTIO_BLK_READ_FAILED\n")
			reply.Status = blockproto.StatusIOError
			break
		}
		reply.Data = memory(dataOffset+start, uint64(request.Length))
	case blockproto.OperationWrite:
		length := uint64(len(request.Data))
		if !rangeValid(request.Offset, length) {
			reply.Status = blockproto.StatusOutOfRange
			break
		}
		first, count, start := sectorRange(request.Offset, length)
		if (start != 0 || length != uint64(count)*sectorSize) && !transfer(virtioRead, first, count) {
			write("VIRTIO_BLK_RMW_READ_FAILED\n")
			reply.Status = blockproto.StatusIOError
			break
		}
		copy(memory(dataOffset+start, length), request.Data)
		if !transfer(virtioWrite, first, count) {
			write("VIRTIO_BLK_WRITE_FAILED\n")
			reply.Status = blockproto.StatusIOError
		}
	case blockproto.OperationFlush:
		if !transfer(virtioFlush, 0, 0) {
			write("VIRTIO_BLK_FLUSH_FAILED\n")
			reply.Status = blockproto.StatusIOError
		}
	default:
		reply.Status = blockproto.StatusInvalidArgument
	}
	return reply
}

func rangeValid(offset, length uint64) bool {
	return length != 0 && offset <= capacity && length <= capacity-offset
}

func sectorRange(offset, length uint64) (uint64, uint16, uint64) {
	first, start := offset/sectorSize, offset%sectorSize
	count := uint16((start + length + sectorSize - 1) / sectorSize)
	return first, count, start
}

func transfer(operation uint32, sector uint64, sectors uint16) bool {
	dataLength := uint32(sectors) * uint32(sectorSize)
	put32(headerOffset, operation)
	put32(headerOffset+4, 0)
	put64(headerOffset+8, sector)
	put8(statusOffset, 0xff)
	putDescriptor(0, dmaDescriptor[0]+headerOffset, 16, 1, 1)
	if operation == virtioFlush {
		putDescriptor(0, dmaDescriptor[0]+headerOffset, 16, 1, 2)
	} else {
		flags := uint16(1)
		if operation == virtioRead {
			flags |= 2
		}
		putDescriptor(1, dmaDescriptor[0]+dataOffset, dataLength, flags, 2)
	}
	putDescriptor(2, dmaDescriptor[0]+statusOffset, 1, 2, 0)
	avail := uint64(16) * uint64(queueSize)
	put16(avail+4+uint64(availableIndex%queueSize)*2, 0)
	availableIndex++
	put16(avail+2, availableIndex)
	if !portOut(16, 2, 0) {
		return false
	}
	current := get16(usedOffset + 2)
	acknowledged := false
	for current == usedIndex {
		if irqWait(uint64(irqHandle)) != 0 {
			return false
		}
		if _, ok := portIn(19, 1); !ok {
			return false
		}
		acknowledged = true
		current = get16(usedOffset + 2)
	}
	if !acknowledged {
		if _, ok := portIn(19, 1); !ok {
			return false
		}
	}
	usedIndex = current
	if !irqConfirmed {
		mark("VIRTIO_BLK_IRQ_OK\n")
		irqConfirmed = true
	}
	return get8(statusOffset) == 0
}

func putDescriptor(index uint64, address uint64, length uint32, flags, next uint16) {
	offset := index * 16
	put64(offset, address)
	put32(offset+8, length)
	put16(offset+12, flags)
	put16(offset+14, next)
}

func memory(offset, size uint64) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(dmaVirtualAddress+offset))), int(size))
}
func put8(offset uint64, value byte) {
	*(*byte)(unsafe.Pointer(uintptr(dmaVirtualAddress + offset))) = value
}
func get8(offset uint64) byte { return *(*byte)(unsafe.Pointer(uintptr(dmaVirtualAddress + offset))) }
func put16(offset uint64, value uint16) {
	data := memory(offset, 2)
	data[0], data[1] = byte(value), byte(value>>8)
}
func get16(offset uint64) uint16 {
	data := memory(offset, 2)
	return uint16(data[0]) | uint16(data[1])<<8
}
func put32(offset uint64, value uint32) {
	data := memory(offset, 4)
	for index := range data {
		data[index] = byte(value >> uint(index*8))
	}
}
func put64(offset uint64, value uint64) {
	data := memory(offset, 8)
	for index := range data {
		data[index] = byte(value >> uint(index*8))
	}
}
func align(value, alignment uint64) uint64 { return (value + alignment - 1) &^ (alignment - 1) }

func mark(message string) {
	if validation.Enabled {
		write(message)
	}
}

func write(message string) {
	consoleWrite(uint64(consoleHandle), unsafe.Pointer(unsafe.StringData(message)), uintptr(len(message)))
}
func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		write(message)
	}
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
		fatal("VIRTIO_BLK_OUT_OF_MEMORY\n")
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
