//go:build goos_userspace

package utility

import (
	"goos/kernel/ipc"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/shell"
	"goos/userspace/startup"
	"runtime"
	"unsafe"
)

type Kind uint8

const (
	Pwd Kind = iota + 1
	Ls
	Cat
)
const consoleHandle = uint64(0x0000000100000001)
const vfsHandle = uint64(0x0000000100000002)

var wire [ipc.WireMaxSize]byte
var descriptor = [3]uint64{uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000}
var startupWire [startup.Size]byte
var pathBuffer [shell.MaxPath]byte
var correlation uint64
var heapPointer uintptr

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

//go:linkname handleClose goos_handle_close
func handleClose(id uint64) uint64

//go:linkname processExit goos_exit
func processExit(code uint64)

func Run() {
	runtimeInitHeap()
	if startupRead(unsafe.Pointer(&startupWire[0]), startup.Size) != 0 {
		finish(1, "startup unavailable\n")
	}
	data, ok := startup.Decode(startupWire[:])
	if !ok {
		finish(1, "invalid startup data\n")
	}
	switch Kind(data.Command) {
	case Pwd:
		writeBytes(data.CWD)
		write("\nPWD_PROCESS_OK\n")
	case Ls:
		runLs(data)
	case Cat:
		runCat(data)
	default:
		finish(1, "invalid utility\n")
	}
	finish(0, "")
}

func runLs(data startup.Data) {
	argument := data.Arguments
	if len(argument) == 0 {
		argument = []byte(".")
	}
	path, ok := shell.ResolvePath(data.CWD, argument, &pathBuffer)
	if !ok {
		finish(2, "ls: invalid path\n")
	}
	stat, ok := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
	if !ok || stat.Status != vfs.StatusOK {
		finish(2, "ls: not found\n")
	}
	if stat.Kind == ramfs.KindFile {
		writeBytes(baseName(path))
		write("\nLS_PROCESS_OK\n")
		return
	}
	var cursor uint32
	for {
		reply, received := callVFS(vfs.Request{Operation: vfs.OperationReadDir, Path: path, Offset: uint64(cursor)})
		if !received {
			finish(2, "ls: service unavailable\n")
		}
		if reply.Status == vfs.StatusNotFound {
			break
		}
		if reply.Status != vfs.StatusOK {
			finish(2, "ls: VFS error\n")
		}
		writeBytes(reply.Name)
		if reply.Kind == ramfs.KindDirectory {
			write("/")
		}
		write("\n")
		cursor = reply.Cursor
	}
	write("LS_PROCESS_OK\n")
}

func runCat(data startup.Data) {
	if len(data.Arguments) == 0 {
		finish(2, "cat: path required\n")
	}
	path, ok := shell.ResolvePath(data.CWD, data.Arguments, &pathBuffer)
	if !ok {
		finish(2, "cat: invalid path\n")
	}
	opened, received := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	if !received || opened.Status != vfs.StatusOK {
		finish(2, "cat: not found\n")
	}
	for {
		reply, received := callVFS(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: vfs.MaxReplyData})
		if !received || reply.Status != vfs.StatusOK {
			finish(2, "cat: VFS error\n")
		}
		if len(reply.Data) == 0 {
			break
		}
		writeBytes(reply.Data)
	}
	callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	write("CAT_PROCESS_OK\n")
}

func callVFS(request vfs.Request) (vfs.Reply, bool) {
	correlation++
	message, ok := vfs.EncodeRequest(request, correlation)
	if !ok {
		return vfs.Reply{}, false
	}
	size, ok := ipc.Encode(message, wire[:])
	if !ok || ipcSend(vfsHandle, unsafe.Pointer(&wire[0]), uintptr(size)) != 0 {
		return vfs.Reply{}, false
	}
	for {
		descriptor[0], descriptor[1], descriptor[2] = uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000
		status := ipcWaitReceive(vfsHandle, unsafe.Pointer(&descriptor[0]), uintptr(len(descriptor)*8))
		if status == uint64(ipc.StatusTimeout) {
			continue
		}
		if status != 0 {
			return vfs.Reply{}, false
		}
		payloadSize := int(wire[24]) | int(wire[25])<<8
		size := ipc.WireHeaderSize + payloadSize + int(wire[26])*8
		received, valid := ipc.Decode(wire[:size])
		if !valid || received.Correlation != correlation {
			return vfs.Reply{}, false
		}
		reply, valid := vfs.DecodeReply(received)
		return reply, valid && reply.Operation == request.Operation
	}
}

func finish(code uint64, message string) {
	if message != "" {
		write(message)
	}
	handleClose(vfsHandle)
	processExit(code)
	for {
	}
}
func write(message string) {
	consoleWrite(consoleHandle, unsafe.Pointer(unsafe.StringData(message)), uintptr(len(message)))
}
func writeBytes(value []byte) {
	if len(value) != 0 {
		consoleWrite(consoleHandle, unsafe.Pointer(&value[0]), uintptr(len(value)))
	}
}
func baseName(path []byte) []byte {
	for index := len(path) - 1; index >= 0; index-- {
		if path[index] == '/' {
			return path[index+1:]
		}
	}
	return path
}

//go:linkname runtimeInitHeap runtime.initHeap
func runtimeInitHeap() { heapPointer = uintptr(unsafe.Pointer(&heapStartSymbol)) }

//go:linkname runtimeAlloc runtime.alloc
func runtimeAlloc(size uintptr, _ unsafe.Pointer) unsafe.Pointer {
	size = (size + 15) &^ 15
	start, end := heapPointer, heapPointer+size
	if end < start || end > uintptr(unsafe.Pointer(&heapEndSymbol)) {
		finish(1, "UTILITY_OUT_OF_MEMORY\n")
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
