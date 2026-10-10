//go:build goos_userspace

// Package utility is the runtime shared by the executables in /system/bin.
// Each one is its own program under userspace/bin and calls Run with its
// manifest; this package binds the startup handles, answers --help, checks the
// arguments, and provides the output, diagnostic and filesystem helpers the
// commands have in common.
package utility

import (
	"goos/internal/timekeeping"
	"goos/internal/validation"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/pathpage"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/shell"
	"goos/userspace/startup"
	"goos/userspace/terminal"
)

var wire [ipc.WireMaxSize]byte
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
	// errorBuffer holds a message that names a path of up to PathMax bytes.
	errorBuffer [shell.MaxPath + 256]byte
)

// BindVFS selects a service's already-granted filesystem endpoint. The caller
// must finish outstanding filesystem requests before replacing it.
func BindVFS(channel handle.ID) { vfsHandle = uint64(channel) }

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
	data, ok := startup.Read()
	if !ok {
		finish(1, "invalid startup data\n")
	}
	stdin, stdinFound := data.Handle("in")
	stdout, stdoutFound := data.Handle("stdout")
	stderr, stderrFound := data.Handle("stderr")
	vfsEndpoint, vfsFound := data.Handle("vfs")
	stdinHandle, stdoutHandle, stderrHandle, vfsHandle = uint64(stdin), uint64(stdout), uint64(stderr), uint64(vfsEndpoint)
	launchHandle, _ = data.Handle("launch")
	networkHandle, _ = data.Handle("net")
	resolverHandle, _ = data.Handle("resolve")
	commandName, current = data.Command(), manifest
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
			if manifest.Options != "" {
				PrintText("  ")
				PrintText(manifest.Options)
				EndLine()
			}
			Exit(0)
		}
	}
	if !manifest.Accepts(count) {
		Usage()
	}
	if manifest.Grants&commands.GrantVFS != 0 && !vfsFound {
		Fail(nil, "filesystem session not granted")
	}
	if manifest.Grants&(commands.GrantNetworkPing|commands.GrantNetworkAdmin) != 0 && networkHandle == 0 {
		Fail(nil, "network service unavailable")
	}
	// A resolver session is required only of commands that need nothing
	// else; ping keeps working on numeric addresses without one.
	if manifest.Grants&commands.GrantResolveAdmin != 0 && resolverHandle == 0 {
		Fail(nil, "resolver service unavailable")
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
// when path is nil, and status 2. The line is written at once so it cannot
// interleave.
func Fail(path []byte, reason string) { FailStatus(2, path, reason) }

// FailStatus is Fail with another exit status.
func FailStatus(code uint64, path []byte, reason string) {
	// Every copy stops one byte short so the newline always fits.
	limit := len(errorBuffer) - 1
	size := copy(errorBuffer[:limit], commandName)
	size += copy(errorBuffer[size:limit], ": ")
	if path != nil {
		// The path may have come from a directory listing: its controls are
		// shown, not sent.
		for index := 0; index < len(path) && size < limit; {
			var character []byte
			character, index = terminal.Printable(path, index)
			size += copy(errorBuffer[size:limit], character)
		}
		size += copy(errorBuffer[size:limit], ": ")
	}
	size += copy(errorBuffer[size:limit], reason)
	errorBuffer[size] = '\n'
	writeErrorBytes(errorBuffer[:size+1])
	finish(code, "")
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
	path, ok := shell.ResolvePath(data.CWD(), argument, &targetBuffer)
	if !ok {
		failPath(argument)
	}
	return path
}

// Resolve makes argument absolute against the working directory.
func Resolve(data startup.Data, argument []byte) []byte {
	path, ok := shell.ResolvePath(data.CWD(), argument, &pathBuffer)
	if !ok {
		failPath(argument)
	}
	return path
}

// failPath reports a path that could not be resolved, saying so when it is
// the length that is wrong.
func failPath(argument []byte) {
	if shell.TooLong(argument) {
		Fail(argument, "file name too long")
	}
	Fail(argument, "invalid path")
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

// ClockNow reads one of the typed native clocks.
func ClockNow(clock timekeeping.Clock) timekeeping.Time {
	value, status := sys.ClockNow(clock)
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
	return ReadInputWithin(target, streamWaitDeadline)
}

// ReadInputWithin is ReadInput waiting at most deadline ticks for input to
// arrive; StatusResourceExhausted then means the wait ran out.
func ReadInputWithin(target []byte, deadline uint64) (uint64, int) {
	count, status := sys.StreamRead(handle.ID(stdinHandle), target)
	if status == kcall.StatusResourceExhausted {
		waitStatus := sys.StreamWait(handle.ID(stdinHandle), deadline)
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
// bounded, for its reply. Paths too long for a message go in the path page,
// and a listed name too long for the reply is asked for again with the page
// attached to receive it (ADR 0110).
func CallVFS(request vfs.Request) (vfs.Reply, bool) {
	reply, ok := callVFS(request, 0)
	if ok && (request.Operation == vfs.OperationReadDir || request.Operation == vfs.OperationReadMount) && reply.Status == vfs.StatusNameTooLong {
		request.NameReturn = true
		reply, ok = callVFS(request, 0)
	}
	if ok && reply.AttachedName != 0 {
		reply.Name = pathpage.Name(int(reply.AttachedName))
	}
	return reply, ok
}

// callVFS is CallVFS with a handle attached, for the bulk operations; the
// handle moves to the VFS with the request.
func callVFS(request vfs.Request, attached handle.ID) (vfs.Reply, bool) {
	if attached == 0 {
		var ok bool
		if attached, ok = pathpage.Prepare(&request); !ok {
			return vfs.Reply{}, false
		}
	}
	correlation++
	message, ok := vfs.EncodeRequest(request, correlation)
	if ok && attached != 0 {
		message.HandleCount, message.Handles[0] = 1, attached
	}
	size := 0
	if ok {
		size, ok = ipc.Encode(message, wire[:])
	}
	if !ok || sys.IPCSend(handle.ID(vfsHandle), wire[:size]) != 0 {
		if attached != 0 {
			sys.HandleClose(attached)
		}
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
// another process's output and arrive as one unreadable line. The buffer
// holds the longest line the terminal accepts; a longer one, which only many
// arguments make, goes out a buffer at a time.
var (
	lineBuffer [shell.MaxLine]byte
	lineSize   int
)

// Print appends value to the current output line.
func Print(value []byte) {
	for len(value) != 0 {
		if lineSize == len(lineBuffer) {
			writeHandle(stdoutHandle, lineBuffer[:lineSize])
			lineSize = 0
		}
		copied := copy(lineBuffer[lineSize:], value)
		lineSize += copied
		value = value[copied:]
	}
}

// PrintName appends a name that came from a filesystem, a path or a label,
// with every character a terminal could obey shown as '?'
// (terminal.Printable). File names are data and must not reach the terminal
// as commands.
func PrintName(value []byte) {
	for index := 0; index < len(value); {
		var character []byte
		character, index = terminal.Printable(value, index)
		Print(character)
	}
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

// AppendUint appends value in decimal without allocating.
func AppendUint(target []byte, value uint64) []byte { return appendUint(target, value) }

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

// WriteError sends already escaped diagnostic bytes to standard error.
func WriteError(value []byte) { writeErrorBytes(value) }

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
