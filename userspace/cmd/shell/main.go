//go:build goos_userspace

package main

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/tty"
	shellcmd "goos/userspace/shell"
	"runtime"
	"unsafe"
)

const consoleHandle = handle.ID(0x0000000100000001)

var (
	editor      tty.LineEditor
	inputByte   byte
	clockValue  uint64
	heapPointer uintptr
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

//export userspaceMain
func userspaceMain() {
	runtimeInitHeap()
	write("GoOS shell\nSHELL_PROCESS_OK\ngoos> ")
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
		write("commands: help echo clear uptime\nSHELL_HELP_OK\n")
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
	case shellcmd.CommandEmpty:
		return
	default:
		write("unknown command\n")
	}
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
