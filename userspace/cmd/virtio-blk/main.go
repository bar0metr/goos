//go:build goos_userspace

package main

import (
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	blockproto "goos/userspace/block"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
)

const (
	dmaVirtualAddress = uint64(0x401c0000)
	dmaSize           = uint64(16384)
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
	wire           [ipc.WireMaxSize]byte
	requestMessage ipc.Message
	replyMessage   ipc.Message
	// dma is the queue, request header, status byte, and bounce buffer the
	// device reads and writes.
	dma            sys.DMARegion
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

func specification(offset uint16, width uint8) uint64 { return uint64(offset) | uint64(width)<<16 }

func portIn(offset uint16, width uint8) (uint32, bool) {
	value, status := sys.PortRead(portHandle, specification(offset, width))
	return uint32(value), status == kcall.StatusOK
}

func portOut(offset uint16, width uint8, value uint32) bool {
	return sys.PortWrite(portHandle, specification(offset, width), uint64(value)) == kcall.StatusOK
}

//export userspaceMain
func userspaceMain() {
	arena.Init()
	arena.OnExhausted(func() { fatal("VIRTIO_BLK_OUT_OF_MEMORY\n") })
	if sys.StartupRead(&startupWire) != 0 {
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
	if statusOffset >= dmaSize {
		write("VIRTIO_BLK_DMA_INIT_FAILED\n")
		return false
	}
	region, status := sys.DMAAllocate(dmaHandle, dmaVirtualAddress, dmaSize)
	if status != kcall.StatusOK {
		write("VIRTIO_BLK_DMA_INIT_FAILED\n")
		return false
	}
	dma = region
	if !portOut(8, 4, uint32(dma.Physical>>12)) || !portOut(18, 1, 7) {
		write("VIRTIO_BLK_QUEUE_ACTIVATE_FAILED\n")
		return false
	}
	return true
}

func serve() {
	for {
		status := sys.IPCWaitReceive(serviceHandle, wire[:], 100000)
		if status == kcall.StatusTimeout {
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
		if !encoded || !wireEncoded || sys.IPCSend(serviceHandle, wire[:size]) != kcall.StatusOK {
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
		sys.IPCSend(serviceHandle, wire[:size])
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
	putDescriptor(0, dma.Physical+headerOffset, 16, 1, 1)
	if operation == virtioFlush {
		putDescriptor(0, dma.Physical+headerOffset, 16, 1, 2)
	} else {
		flags := uint16(1)
		if operation == virtioRead {
			flags |= 2
		}
		putDescriptor(1, dma.Physical+dataOffset, dataLength, flags, 2)
	}
	putDescriptor(2, dma.Physical+statusOffset, 1, 2, 0)
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
		if sys.IRQWait(irqHandle) != kcall.StatusOK {
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
	return dma.Memory[offset : offset+size]
}
func put8(offset uint64, value byte) {
	dma.Memory[offset] = value
}
func get8(offset uint64) byte { return dma.Memory[offset] }
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
	sys.ConsoleWriteString(consoleHandle, message)
}
func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		write(message)
	}
	for {
	}
}
func main() {}
