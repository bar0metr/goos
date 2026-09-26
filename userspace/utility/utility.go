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
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/shell"
	"goos/userspace/startup"
)

var wire [ipc.WireMaxSize]byte
var startupWire [startup.Size]byte
var pathBuffer [shell.MaxPath]byte
var targetBuffer [shell.MaxPath]byte
var correlation uint64
var stdoutHandle uint64
var stderrHandle uint64
var stdinHandle uint64
var vfsHandle uint64
var inputByte [1]byte

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
	arena.Init()
	arena.OnExhausted(func() { finish(1, "UTILITY_OUT_OF_MEMORY\n") })
	if sys.StartupRead(&startupWire) != 0 {
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
	value, status := sys.ClockRead()
	if status != kcall.StatusOK {
		Fail(nil, "clock unavailable")
	}
	return value
}

// ClockNanos reads the calibrated nanosecond clock.
func ClockNanos() uint64 {
	value, status := sys.ClockReadNanos()
	if status != kcall.StatusOK {
		Fail(nil, "clock unavailable")
	}
	return value
}

// SystemInfo fills record with the kernel's system summary.
func SystemInfo(record *[kcall.SystemInfoSize]byte) {
	if sys.SystemInfo(record) != kcall.StatusOK {
		Fail(nil, "system information unavailable")
	}
}

// ProcessInfo fills record with process index and reports whether it exists.
func ProcessInfo(index uint64, record *[kcall.ProcessInfoSize]byte) bool {
	return sys.ProcessInfo(index, record) == kcall.StatusOK
}

// HeapMark and HeapReset let a command that repeats an operation discard what
// each repetition allocated; the arena never frees on its own.
func HeapMark() arena.Mark { return arena.Current() }

func HeapReset(mark arena.Mark) { arena.Reset(mark) }

// Equal compares bytes with a string without converting either.
func Equal(value []byte, expected string) bool { return equal(value, expected) }

// ReadByte reads one byte of standard input; the status is as for ReadInput.
func ReadByte() (byte, uint64) {
	status, _ := ReadInput(inputByte[:])
	return inputByte[0], status
}

// ReadInput reads what standard input has available into target.
// StatusNotFound and StatusResourceExhausted mean nothing arrived yet and the
// caller should retry; StatusPeerDied means end of input.
func ReadInput(target []byte) (uint64, int) {
	count, status := sys.StreamRead(handle.ID(stdinHandle), target)
	if status == kcall.StatusResourceExhausted {
		waitStatus := sys.StreamWait(handle.ID(stdinHandle), streamWaitDeadline)
		if waitStatus == kcall.StatusTimeout {
			return uint64(kcall.StatusResourceExhausted), 0
		}
		if waitStatus != kcall.StatusOK {
			return uint64(waitStatus), 0
		}
		return uint64(kcall.StatusNotFound), 0
	}
	if status == kcall.StatusPermissionDenied && len(target) != 0 {
		// Standard input that is the console rather than a stream.
		value, consoleStatus := sys.ConsoleRead(handle.ID(stdinHandle))
		if consoleStatus == kcall.StatusOK {
			target[0] = value
			return 0, 1
		}
		status = consoleStatus
	}
	return uint64(status), count
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
func CallVFS(request vfs.Request) (vfs.Reply, bool) { return callVFS(request, 0) }

// callVFS is CallVFS with a handle attached, for the bulk operations; the
// handle moves to the VFS with the request.
func callVFS(request vfs.Request, attached handle.ID) (vfs.Reply, bool) {
	correlation++
	message, ok := vfs.EncodeRequest(request, correlation)
	if !ok {
		return vfs.Reply{}, false
	}
	if attached != 0 {
		message.HandleCount, message.Handles[0] = 1, attached
	}
	size, ok := ipc.Encode(message, wire[:])
	if !ok || sys.IPCSend(handle.ID(vfsHandle), wire[:size]) != 0 {
		return vfs.Reply{}, false
	}
	// Bound the wait: a service that never answers must surface an error rather
	// than freeze the utility, and with it the shell, indefinitely.
	for attempt := 0; attempt < vfsReplyAttempts; attempt++ {
		status := sys.IPCWaitReceive(handle.ID(vfsHandle), wire[:], vfsReplyDeadline)
		if status == ipc.StatusTimeout {
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
		sys.HandleClose(handle.ID(vfsHandle))
	}
	sys.Exit(code)
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
	Print(sys.Bytes(value))
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
	writeHandle(stdoutHandle, sys.Bytes(message))
}

// Mark writes a validation marker; normal builds print nothing.
func Mark(message string) {
	if validation.Enabled {
		writeError(message)
	}
}

func writeError(message string) {
	writeErrorBytes(sys.Bytes(message))
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
		written, status := sys.StreamWrite(handle.ID(target), value[:size])
		if status == kcall.StatusPermissionDenied {
			sys.ConsoleWrite(handle.ID(target), value)
			return
		}
		if status == kcall.StatusResourceExhausted {
			if sys.StreamWait(handle.ID(target), streamWaitDeadline) != kcall.StatusOK {
				sys.Exit(2)
			}
			continue
		}
		if status != kcall.StatusOK || written == 0 || written > size {
			sys.Exit(2)
		}
		value = value[written:]
	}
}
