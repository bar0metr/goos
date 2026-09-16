//go:build goos_userspace

package main

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/ramfs"
	"goos/kernel/tty"
	"goos/kernel/vfs"
	shellcmd "goos/userspace/shell"
	"runtime"
	"unsafe"
)

const consoleHandle = handle.ID(0x0000000100000001)
const vfsHandle = uint64(0x0000000100000002)

var (
	editor      tty.LineEditor
	inputByte   byte
	clockValue  uint64
	heapPointer uintptr
	wire        [ipc.WireMaxSize]byte
	descriptor  = [3]uint64{uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000}
	correlation uint64
	cwd         = [shellcmd.MaxPath]byte{'/'}
	cwdSize     = 1
	pathBuffer  [shellcmd.MaxPath]byte
)

//go:extern _heap_start
var heapStartSymbol [0]byte

//go:extern _heap_end
var heapEndSymbol [0]byte

//go:linkname consoleWrite goos_console_write
func consoleWrite(device uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname consoleRead goos_console_read
func consoleRead(device uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname clockRead goos_clock_read
func clockRead(address unsafe.Pointer, size uintptr) uint64

//go:linkname ipcSend goos_ipc_send
func ipcSend(channel uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname ipcWaitReceive goos_ipc_wait_receive
func ipcWaitReceive(channel uint64, descriptor unsafe.Pointer, size uintptr) uint64

//export userspaceMain
func userspaceMain() {
	runtimeInitHeap()
	root, ok := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: cwd[:cwdSize]})
	if !ok || root.Status != vfs.StatusOK || root.Kind != ramfs.KindDirectory {
		fatal("SHELL_VFS_CONNECT_FAILED\n")
	}
	write("GoOS shell\nSHELL_PROCESS_OK\nSHELL_VFS_CONNECTED_OK\ngoos> ")
	for {
		status := consoleRead(uint64(consoleHandle), unsafe.Pointer(&inputByte), 1)
		if status == uint64(kcall.StatusNotFound) {
			continue
		}
		if status != 0 {
			fatal("SHELL_INPUT_FAILED\n")
		}
		line, complete, accepted := editor.Feed(inputByte)
		if !accepted {
			continue
		}
		if !complete {
			echoInput(inputByte)
			continue
		}
		write("\n")
		execute(line)
		editor.Reset()
		write("goos> ")
	}
}

func echoInput(value byte) {
	if value == 8 || value == 127 {
		write("\b \b")
		return
	}
	consoleWrite(uint64(consoleHandle), unsafe.Pointer(&inputByte), 1)
}

func execute(line []byte) {
	command, arguments := shellcmd.Parse(line)
	switch command {
	case shellcmd.CommandHelp:
		write("commands: help echo clear uptime pwd ls cd cat\nSHELL_HELP_OK\n")
	case shellcmd.CommandEcho:
		writeBytes(arguments)
		write("\nSHELL_ECHO_OK\n")
	case shellcmd.CommandClear:
		write("\x1b[2J\x1b[H")
		write("SHELL_CLEAR_OK\n")
	case shellcmd.CommandUptime:
		if clockRead(unsafe.Pointer(&clockValue), 8) != 0 {
			write("uptime unavailable\n")
			return
		}
		write("uptime: ")
		writeUint(clockValue)
		write(" ticks\nSHELL_UPTIME_OK\n")
	case shellcmd.CommandPwd:
		writeBytes(cwd[:cwdSize])
		write("\nSHELL_PWD_OK\n")
	case shellcmd.CommandLs:
		executeLs(arguments)
	case shellcmd.CommandCd:
		executeCd(arguments)
	case shellcmd.CommandCat:
		executeCat(arguments)
	case shellcmd.CommandEmpty:
		return
	default:
		write("unknown command\n")
	}
}

func resolve(argument []byte, defaultCurrent bool) ([]byte, bool) {
	if len(argument) == 0 {
		if defaultCurrent {
			return cwd[:cwdSize], true
		}
		argument = []byte("/")
	}
	return shellcmd.ResolvePath(cwd[:cwdSize], argument, &pathBuffer)
}

func executeLs(argument []byte) {
	path, ok := resolve(argument, true)
	if !ok {
		write("ls: invalid path\n")
		return
	}
	stat, ok := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
	if !ok || stat.Status != vfs.StatusOK {
		writeVFSError("ls", stat.Status)
		return
	}
	if stat.Kind == ramfs.KindFile {
		writeBytes(baseName(path))
		write("\nSHELL_LS_OK\n")
		return
	}
	var cursor uint32
	for {
		reply, received := callVFS(vfs.Request{Operation: vfs.OperationReadDir, Path: path, Offset: uint64(cursor)})
		if !received {
			write("ls: service unavailable\n")
			return
		}
		if reply.Status == vfs.StatusNotFound {
			break
		}
		if reply.Status != vfs.StatusOK {
			writeVFSError("ls", reply.Status)
			return
		}
		writeBytes(reply.Name)
		if reply.Kind == ramfs.KindDirectory {
			write("/")
		}
		write("\n")
		cursor = reply.Cursor
	}
	write("SHELL_LS_OK\n")
}

func executeCd(argument []byte) {
	path, ok := resolve(argument, false)
	if !ok {
		write("cd: invalid path\n")
		return
	}
	reply, received := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
	if !received || reply.Status != vfs.StatusOK {
		writeVFSError("cd", reply.Status)
		return
	}
	if reply.Kind != ramfs.KindDirectory {
		write("cd: not a directory\n")
		return
	}
	copy(cwd[:], path)
	cwdSize = len(path)
	write("SHELL_CD_OK\n")
}

func executeCat(argument []byte) {
	path, ok := resolve(argument, false)
	if !ok || len(argument) == 0 {
		write("cat: path required\n")
		return
	}
	opened, received := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	if !received || opened.Status != vfs.StatusOK {
		writeVFSError("cat", opened.Status)
		return
	}
	for {
		reply, received := callVFS(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: vfs.MaxReplyData})
		if !received || reply.Status != vfs.StatusOK {
			writeVFSError("cat", reply.Status)
			break
		}
		if len(reply.Data) == 0 {
			break
		}
		writeBytes(reply.Data)
	}
	closed, received := callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	if !received || closed.Status != vfs.StatusOK {
		writeVFSError("cat", closed.Status)
		return
	}
	write("SHELL_CAT_OK\n")
}

func callVFS(request vfs.Request) (vfs.Reply, bool) {
	correlation++
	if correlation == 0 {
		correlation++
	}
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
		wireSize := ipc.WireHeaderSize + payloadSize + int(wire[26])*8
		if wireSize < ipc.WireHeaderSize || wireSize > len(wire) {
			return vfs.Reply{}, false
		}
		received, valid := ipc.Decode(wire[:wireSize])
		if !valid || received.Correlation != correlation {
			return vfs.Reply{}, false
		}
		reply, valid := vfs.DecodeReply(received)
		if !valid || reply.Operation != request.Operation {
			return vfs.Reply{}, false
		}
		return reply, true
	}
}

func writeVFSError(command string, status vfs.Status) {
	write(command)
	write(": ")
	switch status {
	case vfs.StatusNotFound:
		write("not found\n")
	case vfs.StatusPermissionDenied:
		write("permission denied\n")
	case vfs.StatusResourceExhausted:
		write("resource exhausted\n")
	default:
		write("VFS error\n")
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

func write(message string) {
	consoleWrite(uint64(consoleHandle), unsafe.Pointer(unsafe.StringData(message)), uintptr(len(message)))
}

func writeBytes(value []byte) {
	if len(value) != 0 {
		consoleWrite(uint64(consoleHandle), unsafe.Pointer(&value[0]), uintptr(len(value)))
	}
}

func writeUint(value uint64) {
	var digits [20]byte
	position := len(digits)
	if value == 0 {
		write("0")
		return
	}
	for value != 0 {
		position--
		digits[position] = byte(value%10) + '0'
		value /= 10
	}
	writeBytes(digits[position:])
}

func fatal(message string) {
	write(message)
	for {
	}
}

func main() {}

//go:linkname runtimeInitHeap runtime.initHeap
func runtimeInitHeap() {
	heapPointer = uintptr(unsafe.Pointer(&heapStartSymbol))
}

//go:linkname runtimeAlloc runtime.alloc
func runtimeAlloc(size uintptr, _ unsafe.Pointer) unsafe.Pointer {
	size = (size + 15) &^ 15
	start := heapPointer
	end := start + size
	if end < start || end > uintptr(unsafe.Pointer(&heapEndSymbol)) {
		fatal("SHELL_OUT_OF_MEMORY\n")
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
