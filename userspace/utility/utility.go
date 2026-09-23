//go:build goos_userspace

// Package utility is the runtime shared by the executables in /system/bin.
// Each one is its own program under userspace/bin and calls Run with its
// manifest; this package binds the startup handles, answers --help, checks the
// arguments, and provides the output, diagnostic and filesystem helpers the
// commands have in common.
package utility

import (
	"goos/internal/validation"
	"goos/kernel/clock"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/shell"
	"goos/userspace/startup"
	"runtime"
	"unsafe"
)

var wire [ipc.WireMaxSize]byte
var descriptor = [3]uint64{uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000}
var startupWire [startup.Size]byte
var pathBuffer [shell.MaxPath]byte
var targetBuffer [shell.MaxPath]byte
var correlation uint64
var heapPointer uintptr
var stdoutHandle uint64
var stderrHandle uint64
var stdinHandle uint64
var vfsHandle uint64
var clockValue uint64
var inputByte byte
var streamDescriptor [3]uint64

//go:extern _heap_start
var heapStartSymbol [0]byte

//go:extern _heap_end
var heapEndSymbol [0]byte

//go:linkname consoleWrite goos_console_write
func consoleWrite(device uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname consoleRead goos_console_read
func consoleRead(device uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname streamRead goos_stream_read
func streamRead(stream uint64, descriptor unsafe.Pointer, size uintptr) uint64

//go:linkname streamWrite goos_stream_write
func streamWrite(stream uint64, descriptor unsafe.Pointer, size uintptr) uint64

//go:linkname streamWait goos_stream_wait
func streamWait(stream uint64, timeout uint64) uint64

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

//go:linkname clockRead goos_clock_read
func clockRead(address unsafe.Pointer, size uintptr) uint64

//go:linkname clockReadNanos goos_clock_read_nanos
func clockReadNanos(address unsafe.Pointer, size uintptr) uint64

//go:linkname processInfo goos_process_info
func processInfo(index uint64, address unsafe.Pointer) uint64

//go:linkname systemInfoRead goos_system_info
func systemInfoRead(address unsafe.Pointer, size uintptr) uint64

// current is the running command's manifest, and commandName the name it was
// started under; usage errors and diagnostics are written from them.
var (
	current     commands.Manifest
	commandName []byte
	errorBuffer [512]byte
)

// Run starts the command described by manifest and calls main with its startup
// data. A command that returns from main exits with status 0.
//
// Utility startup is on every command's latency path, and under QEMU's TCG each
// launch translates the code it touches afresh (ADR 0079), so the checks here
// read the argument count byte and decode the vector only when there is a flag
// to test.
func Run(manifest commands.Manifest, main func(startup.Data)) {
	runtimeInitHeap()
	if startupRead(unsafe.Pointer(&startupWire[0]), startup.Size) != 0 {
		finish(1, "startup unavailable\n")
	}
	data, ok := startup.Decode(startupWire[:])
	if !ok {
		finish(1, "invalid startup data\n")
	}
	stdin, stdinFound := data.Handle("in")
	stdout, stdoutFound := data.Handle("stdout")
	stderr, stderrFound := data.Handle("stderr")
	vfsEndpoint, vfsFound := data.Handle("vfs")
	stdinHandle, stdoutHandle, stderrHandle, vfsHandle = uint64(stdin), uint64(stdout), uint64(stderr), uint64(vfsEndpoint)
	commandName, current = data.Command, manifest
	if !stdinFound || !stdoutFound || !stderrFound {
		finish(1, "startup handles unavailable\n")
	}
	count := data.ArgCount()
	if count == 1 {
		if first, _ := data.Arg(1); shell.IsHelpFlag(first) {
			PrintText("usage: ")
			PrintText(manifest.Usage)
			EndLine()
			PrintText("  ")
			PrintText(manifest.Summary)
			EndLine()
			Exit(0)
		}
	}
	if !manifest.Accepts(count) {
		Usage()
	}
	if manifest.Grants&commands.GrantVFS != 0 && !vfsFound {
		Fail(nil, "filesystem session not granted")
	}
	main(data)
	Exit(0)
}

// Exit ends the command with code. It does not return.
func Exit(code uint64) { finish(code, "") }

// Usage ends a malformed invocation with the command's synopsis.
func Usage() {
	size := copy(errorBuffer[:], "usage: ")
	size += copy(errorBuffer[size:len(errorBuffer)-1], current.Usage)
	errorBuffer[size] = '\n'
	writeErrorBytes(errorBuffer[:size+1])
	finish(2, "")
}

// Fail ends the command with "command: path: reason", or "command: reason"
// when path is nil. The line is written at once so it cannot interleave.
func Fail(path []byte, reason string) {
	// Every copy stops one byte short so the newline always fits.
	limit := len(errorBuffer) - 1
	size := copy(errorBuffer[:limit], commandName)
	size += copy(errorBuffer[size:limit], ": ")
	if path != nil {
		size += copy(errorBuffer[size:limit], path)
		size += copy(errorBuffer[size:limit], ": ")
	}
	size += copy(errorBuffer[size:limit], reason)
	errorBuffer[size] = '\n'
	writeErrorBytes(errorBuffer[:size+1])
	finish(2, "")
}

// Check ends the command when a VFS request about path failed. A refused
// mutation of a read-only mount is named as such.
func Check(reply vfs.Reply, received bool, path []byte, mutation bool) {
	if !received {
		Fail(path, "filesystem service unavailable")
	}
	if reply.Status != vfs.StatusOK {
		Fail(path, shell.VFSReason(reply.Status, mutation))
	}
}

// CheckWritten also refuses a short write, which would silently truncate.
func CheckWritten(reply vfs.Reply, received bool, path []byte, size int) {
	Check(reply, received, path, true)
	if reply.Size != uint64(size) {
		Fail(path, "short write")
	}
}

// Path resolves argument index against the working directory; a missing
// argument is a usage error.
func Path(data startup.Data, index int) []byte {
	argument, found := data.Arg(index)
	if !found {
		Usage()
	}
	return Resolve(data, argument)
}

// Target resolves argument index into a second buffer, so a command with two
// paths keeps the first.
func Target(data startup.Data, index int) []byte {
	argument, found := data.Arg(index)
	if !found {
		Usage()
	}
	path, ok := shell.ResolvePath(data.CWD, argument, &targetBuffer)
	if !ok {
		Fail(argument, "invalid path")
	}
	return path
}

// Resolve makes argument absolute against the working directory.
func Resolve(data startup.Data, argument []byte) []byte {
	path, ok := shell.ResolvePath(data.CWD, argument, &pathBuffer)
	if !ok {
		Fail(argument, "invalid path")
	}
	return path
}

// Arguments prints the arguments from start on, separated by spaces, into the
// current line.
func Arguments(data startup.Data, start int) {
	for index := start; ; index++ {
		argument, found := data.Arg(index)
		if !found {
			return
		}
		if index != start {
			PrintText(" ")
		}
		Print(argument)
	}
}

// Clock reads the monotonic timer in ticks.
func Clock() uint64 {
	if clockRead(unsafe.Pointer(&clockValue), 8) != 0 {
		Fail(nil, "clock unavailable")
	}
	return clockValue
}

// ClockNanos reads the calibrated nanosecond clock.
func ClockNanos() uint64 {
	if clockReadNanos(unsafe.Pointer(&clockValue), 8) != 0 {
		Fail(nil, "clock unavailable")
	}
	return clockValue
}

// SystemInfo fills record with the kernel's system summary.
func SystemInfo(record *[kcall.SystemInfoSize]byte) {
	if systemInfoRead(unsafe.Pointer(&record[0]), uintptr(len(record))) != 0 {
		Fail(nil, "system information unavailable")
	}
}

// ProcessInfo fills record with process index and reports whether it exists.
func ProcessInfo(index uint64, record *[kcall.ProcessInfoSize]byte) bool {
	return processInfo(index, unsafe.Pointer(&record[0])) == 0
}

// HeapMark and HeapReset let a command that repeats an operation discard what
// each repetition allocated; the arena never frees on its own.
func HeapMark() uintptr { return heapPointer }

func HeapReset(mark uintptr) { heapPointer = mark }

// Equal compares bytes with a string without converting either.
func Equal(value []byte, expected string) bool { return equal(value, expected) }

// ReadByte reads one byte of standard input; the status is as for ReadInput.
func ReadByte() (byte, uint64) {
	status, _ := ReadInput(unsafe.Slice(&inputByte, 1))
	return inputByte, status
}

// ReadInput reads what standard input has available into target.
// StatusNotFound and StatusResourceExhausted mean nothing arrived yet and the
// caller should retry; StatusPeerDied means end of input.
func ReadInput(target []byte) (uint64, int) {
	streamDescriptor[0] = uint64(uintptr(unsafe.Pointer(&target[0])))
	streamDescriptor[1], streamDescriptor[2] = uint64(len(target)), 0
	status := streamRead(stdinHandle, unsafe.Pointer(&streamDescriptor[0]), 24)
	if status == uint64(kcall.StatusResourceExhausted) {
		waitStatus := streamWait(stdinHandle, streamWaitDeadline)
		if waitStatus == uint64(kcall.StatusTimeout) {
			return uint64(kcall.StatusResourceExhausted), 0
		}
		if waitStatus != 0 {
			return waitStatus, 0
		}
		return uint64(kcall.StatusNotFound), 0
	}
	if status == uint64(kcall.StatusPermissionDenied) {
		status = consoleRead(stdinHandle, unsafe.Pointer(&target[0]), 1)
		if status == 0 {
			return 0, 1
		}
	}
	return status, int(streamDescriptor[2])
}

// Retry reports whether a ReadInput status means nothing has arrived yet.
func Retry(status uint64) bool {
	return status == uint64(kcall.StatusNotFound) || status == uint64(kcall.StatusResourceExhausted)
}

func equal(value []byte, expected string) bool {
	if len(value) != len(expected) {
		return false
	}
	for index := range value {
		if value[index] != expected[index] {
			return false
		}
	}
	return true
}

const (
	// streamWaitDeadline bounds one wait for stream readiness. Wakeups are
	// event-driven, so this only limits how long a lost one can stall a
	// utility before it retries.
	streamWaitDeadline = 5 * clock.TicksPerSecond
	// One VFS call may reach persistent storage, so allow a few bounded waits
	// before reporting failure.
	vfsReplyDeadline = 2 * clock.TicksPerSecond
	vfsReplyAttempts = 4
)

// CallVFS sends one request on the command's filesystem session and waits,
// bounded, for its reply.
func CallVFS(request vfs.Request) (vfs.Reply, bool) {
	correlation++
	message, ok := vfs.EncodeRequest(request, correlation)
	if !ok {
		return vfs.Reply{}, false
	}
	size, ok := ipc.Encode(message, wire[:])
	if !ok || ipcSend(vfsHandle, unsafe.Pointer(&wire[0]), uintptr(size)) != 0 {
		return vfs.Reply{}, false
	}
	// Bound the wait: a service that never answers must surface an error rather
	// than freeze the utility, and with it the shell, indefinitely.
	for attempt := 0; attempt < vfsReplyAttempts; attempt++ {
		descriptor[0], descriptor[1], descriptor[2] = uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, vfsReplyDeadline
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
	return vfs.Reply{}, false
}

func finish(code uint64, message string) {
	if message != "" {
		if code == 0 {
			write(message)
		} else {
			writeError(message)
		}
	}
	if vfsHandle != 0 {
		handleClose(vfsHandle)
	}
	processExit(code)
	for {
	}
}

// Output lines are assembled in one buffer and written once. Processes share
// the console, so a line split across several writes can interleave with
// another process's output and arrive as one unreadable line.
var (
	lineBuffer [256]byte
	lineSize   int
)

// Print appends value to the current output line.
func Print(value []byte) {
	if lineSize+len(value) > len(lineBuffer) {
		finish(2, "output line too long\n")
	}
	lineSize += copy(lineBuffer[lineSize:], value)
}

// PrintText appends value to the current output line.
func PrintText(value string) {
	Print(unsafe.Slice(unsafe.StringData(value), len(value)))
}

// PrintUint appends value in decimal and returns how many digits it took.
func PrintUint(value uint64) int {
	var digits [20]byte
	position := len(digits)
	for {
		position--
		digits[position] = byte(value%10) + '0'
		value /= 10
		if value == 0 {
			break
		}
	}
	Print(digits[position:])
	return len(digits) - position
}

// Pad appends spaces from column until width.
func Pad(column int, width int) {
	for ; column < width; column++ {
		PrintText(" ")
	}
}

// EndLine ends the current line and writes it to standard output.
func EndLine() {
	PrintText("\n")
	writeHandle(stdoutHandle, lineBuffer[:lineSize])
	lineSize = 0
}

// Write sends value to standard output as it is, outside the line buffer.
func Write(value []byte) {
	writeHandle(stdoutHandle, value)
}

func write(message string) {
	writeHandle(stdoutHandle, unsafe.Slice(unsafe.StringData(message), len(message)))
}

// Mark writes a validation marker; normal builds print nothing.
func Mark(message string) {
	if validation.Enabled {
		writeError(message)
	}
}

func writeError(message string) {
	writeErrorBytes(unsafe.Slice(unsafe.StringData(message), len(message)))
}

func writeErrorBytes(message []byte) {
	target := stderrHandle
	if target == 0 {
		target = stdoutHandle
	}
	writeHandle(target, message)
}

func writeHandle(target uint64, value []byte) {
	for len(value) != 0 {
		size := len(value)
		if size > int(kcall.MaxStreamIO) {
			size = int(kcall.MaxStreamIO)
		}
		streamDescriptor[0] = uint64(uintptr(unsafe.Pointer(&value[0])))
		streamDescriptor[1], streamDescriptor[2] = uint64(size), 0
		status := streamWrite(target, unsafe.Pointer(&streamDescriptor[0]), 24)
		if status == uint64(kcall.StatusPermissionDenied) {
			consoleWrite(target, unsafe.Pointer(&value[0]), uintptr(len(value)))
			return
		}
		if status == uint64(kcall.StatusResourceExhausted) {
			if streamWait(target, streamWaitDeadline) != 0 {
				processExit(2)
			}
			continue
		}
		if status != 0 || streamDescriptor[2] == 0 || streamDescriptor[2] > uint64(size) {
			processExit(2)
		}
		value = value[streamDescriptor[2]:]
	}
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
