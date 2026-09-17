//go:build goos_userspace

package main

import (
	"goos/kernel/handle"
	"goos/userspace/startup"
	"runtime"
	"unsafe"
)

const dmaVirtualAddress = uint64(0x401f0000)

var (
	consoleHandle handle.ID
	portHandle    handle.ID
	dmaHandle     handle.ID
	startupWire   [startup.Size]byte
	dmaDescriptor [3]uint64
	heapPointer   uintptr
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

//go:linkname processExit goos_exit
func processExit(code uint64)

func specification(offset uint16, width uint8) uint64 { return uint64(offset) | uint64(width)<<16 }

func read(offset uint16, width uint8) (uint32, bool) {
	var value uint32
	ok := portRead(uint64(portHandle), specification(offset, width), unsafe.Pointer(&value)) == 0
	return value, ok
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
	if !ok {
		fatal("VIRTIO_BLK_CONSOLE_MISSING\n")
	}
	portHandle, ok = data.Handle("ports")
	if !ok {
		fatal("VIRTIO_BLK_PORTS_MISSING\n")
	}
	if _, ok = data.Handle("irq"); !ok {
		fatal("VIRTIO_BLK_IRQ_MISSING\n")
	}
	dmaHandle, ok = data.Handle("dma")
	if !ok {
		fatal("VIRTIO_BLK_DMA_MISSING\n")
	}
	if portWrite(uint64(portHandle), specification(18, 1), 0) != 0 ||
		portWrite(uint64(portHandle), specification(14, 2), 0) != 0 {
		fatal("VIRTIO_BLK_RESET_FAILED\n")
	}
	features, featuresOK := read(0, 4)
	queueSize, queueOK := read(12, 2)
	capacityLow, lowOK := read(20, 4)
	capacityHigh, highOK := read(24, 4)
	if !featuresOK || !queueOK || !lowOK || !highOK || features == 0 || queueSize == 0 || uint64(capacityHigh)<<32|uint64(capacityLow) == 0 {
		fatal("VIRTIO_BLK_PROBE_FAILED\n")
	}
	if dmaAllocate(uint64(dmaHandle), dmaVirtualAddress, unsafe.Pointer(&dmaDescriptor[0])) != 0 || dmaDescriptor[0] == 0 || dmaDescriptor[1] != dmaVirtualAddress || dmaDescriptor[2] != 4096 {
		fatal("VIRTIO_BLK_DMA_FAILED\n")
	}
	write("VIRTIO_BLK_CAPABILITIES_OK\n")
	write("VIRTIO_BLK_DRIVER_OK\n")
	processExit(0)
}

func write(message string) {
	consoleWrite(uint64(consoleHandle), unsafe.Pointer(unsafe.StringData(message)), uintptr(len(message)))
}
func fatal(message string) {
	write(message)
	processExit(1)
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
