//go:build goos_userspace

// Package utility is the runtime shared by the executables in /system/bin.
// Each one is its own program under userspace/bin and calls Run with its
// manifest; this package binds the startup handles, answers --help, checks the
// arguments, and provides the output, diagnostic and filesystem helpers the
// commands have in common.
//
// The output, diagnostic and service-call functions may be used from any
// goroutine or thread of the command. The current output line (Print through
// EndLine), the buffers Path, Target and Resolve return, and the TCP and UDP
// sessions belong to one goroutine at a time. The full contract is in
// docs/development/native-concurrency-audit.md.
package utility

import (
	"goos/internal/timekeeping"
	"goos/internal/validation"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/pathpage"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/lock"
	"goos/userspace/runtime/sys"
	"goos/userspace/shell"
	"goos/userspace/startup"
	"goos/userspace/terminal"
)

// pathBuffer and targetBuffer are where Path, Resolve and Target put the
// paths they return. They belong to the goroutine that runs main.
var pathBuffer [shell.MaxPath]byte
var targetBuffer [shell.MaxPath]byte

// stdinHandle is standard input. Run sets it before main, and nothing
// changes it afterwards.
var stdinHandle uint64

// standard is standard output and standard error.
var standard output[nativeStream]

// files is the command's filesystem session.
var files session[nativeIPC]

// current is the running command's manifest, and commandName the name it was
// started under; usage errors and diagnostics are written from them. Run
// sets both before main, and nothing changes them afterwards.
var (
	current     commands.Manifest
	commandName []byte
)

// failure is taken by the goroutine that ends the command with a diagnostic
// and never released: the process exits with it held, and another goroutine
// that fails meanwhile waits for that exit. The holder owns errorBuffer,
// which holds a message naming a path of up to PathMax bytes and is too
// large for a goroutine stack.
var (
	failure     lock.Mutex
	errorBuffer [shell.MaxPath + 256]byte
)

// BindVFS selects a service's already-granted filesystem endpoint. A request
// in flight on the old endpoint finishes first.
func BindVFS(channel handle.ID) {
	files.hold()
	files.bind(channel)
	files.end()
}

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
	stdinHandle, standard.stdout, standard.stderr = uint64(stdin), uint64(stdout), uint64(stderr)
	files.bind(vfsEndpoint)
	launchChannel, _ := data.Handle("launch")
	launcher.bind(launchChannel)
	bindResize(data)
	networkChannel, _ := data.Handle("net")
	networkSession.bind(networkChannel)
	resolverChannel, _ := data.Handle("resolve")
	resolverSession.bind(resolverChannel)
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
	if manifest.Grants&(commands.GrantNetworkPing|commands.GrantNetworkAdmin) != 0 && networkSession.bound() == 0 {
		Fail(nil, "network service unavailable")
	}
	// A resolver session is required only of commands that need nothing
	// else; ping keeps working on numeric addresses without one.
	if manifest.Grants&commands.GrantResolveAdmin != 0 && resolverSession.bound() == 0 {
		Fail(nil, "resolver service unavailable")
	}
	main(data)
	Exit(0)
}

// Exit ends the command with code. It does not return.
func Exit(code uint64) { finish(code, "") }

// Usage ends a malformed invocation with the command's synopsis.
func Usage() {
	failure.Lock()
	writeErrorBytes(appendUsage(errorBuffer[:], current.Usage))
	finish(2, "")
}

// Fail ends the command with "command: path: reason", or "command: reason"
// when path is nil, and status 2. The line is one record, so it cannot
// interleave with other output of the process. When two goroutines fail at
// once, the first one's message and status win.
func Fail(path []byte, reason string) { FailStatus(2, path, reason) }

// FailStatus is Fail with another exit status.
func FailStatus(code uint64, path []byte, reason string) {
	failure.Lock()
	// The path may have come from a directory listing: its controls are
	// shown, not sent.
	writeErrorBytes(appendFailure(errorBuffer[:], commandName, path, reason))
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
// paths keeps the first. Path, Target and Resolve return package buffers
// that belong to the goroutine running main; other goroutines use
// ResolveInto.
func Target(data startup.Data, index int) []byte {
	argument, found := data.Arg(index)
	if !found {
		Usage()
	}
	return ResolveInto(data, argument, &targetBuffer)
}

// Resolve makes argument absolute against the working directory.
func Resolve(data startup.Data, argument []byte) []byte {
	return ResolveInto(data, argument, &pathBuffer)
}

// ResolveInto makes argument absolute against the working directory in
// target, which the caller owns, and returns the path.
func ResolveInto(data startup.Data, argument []byte, target *[shell.MaxPath]byte) []byte {
	path, ok := shell.ResolvePath(data.CWD(), argument, target)
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

// Equal compares bytes with a string without converting either.
func Equal(value []byte, expected string) bool { return equal(value, expected) }

// ReadByte reads one byte of standard input; the status is as for ReadInput.
func ReadByte() (byte, uint64) {
	var value [1]byte
	status, _ := ReadInput(value[:])
	return value[0], status
}

// ReadInput reads what standard input has available into target.
// StatusNotFound and StatusResourceExhausted mean nothing arrived yet and the
// caller should retry; StatusPeerDied means end of input.
//
// Each call is one native read, so goroutines reading standard input at once
// get different bytes and no byte twice. Which goroutine gets which bytes is
// not defined: a program that reads lines or keystrokes reads them from one
// goroutine.
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
// attached to receive it (ADR 0110). Such a name is copied out of the page
// before CallVFS returns, so the whole reply belongs to the caller.
//
// Concurrent callers take turns: the session has one request outstanding,
// and the session and the path page stay held until the reply is in.
func CallVFS(request vfs.Request) (vfs.Reply, bool) {
	files.hold()
	pathpage.Hold()
	reply, ok := callVFS(request, 0)
	if ok && (request.Operation == vfs.OperationReadDir || request.Operation == vfs.OperationReadMount) && reply.Status == vfs.StatusNameTooLong {
		request.NameReturn = true
		reply, ok = callVFS(request, 0)
	}
	if ok && reply.AttachedName != 0 {
		// Only names longer than a reply carries come this way.
		reply.Name = append([]byte(nil), pathpage.Name(int(reply.AttachedName))...)
	}
	pathpage.Release()
	files.end()
	return reply, ok
}

// attachedVFS is CallVFS with a handle attached, for the bulk operations;
// the handle moves to the VFS with the request.
func attachedVFS(request vfs.Request, attached handle.ID) (vfs.Reply, bool) {
	files.hold()
	reply, ok := callVFS(request, attached)
	files.end()
	return reply, ok
}

// callVFS makes one exchange on the filesystem session, which the caller
// holds; when attached is zero the caller holds the path page too.
func callVFS(request vfs.Request, attached handle.ID) (vfs.Reply, bool) {
	if attached == 0 {
		var ok bool
		if attached, ok = pathpage.Prepare(&request); !ok {
			return vfs.Reply{}, false
		}
	}
	correlation := files.next()
	var ok bool
	files.outgoing, ok = vfs.EncodeRequest(request, correlation)
	if ok && attached != 0 {
		files.outgoing.HandleCount, files.outgoing.Handles[0] = 1, attached
	}
	if !ok || !files.send(&files.outgoing) {
		if attached != 0 {
			sys.HandleClose(attached)
		}
		return vfs.Reply{}, false
	}
	// Bound the wait: a service that never answers must surface an error rather
	// than freeze the utility, and with it the shell, indefinitely.
	for attempt := 0; attempt < vfsReplyAttempts; attempt++ {
		status, valid := files.receive(vfsReplyDeadline)
		if status == kcall.StatusTimeout {
			continue
		}
		if status != kcall.StatusOK || !valid || files.incoming.Correlation != correlation {
			return vfs.Reply{}, false
		}
		// DecodeReply works on a copy, so the reply's name does not alias the
		// session's message.
		reply, valid := vfs.DecodeReply(files.incoming)
		return reply, valid && reply.Operation == request.Operation
	}
	return vfs.Reply{}, false
}

// finish writes message, closes the filesystem session and exits. It takes
// no session lock: it may run while the calling goroutine holds one, from an
// allocation that ran out of memory.
func finish(code uint64, message string) {
	if message != "" {
		if code == 0 {
			write(message)
		} else {
			writeError(message)
		}
	}
	if channel := files.bound(); channel != 0 {
		sys.HandleClose(channel)
	}
	sys.Exit(code)
	for {
	}
}

// Line assembles one line of standard output in storage the caller owns and
// writes it as one record. A goroutine other than the one running main prints
// through its own Line. A line longer than its storage goes out a buffer at a
// time, and then only each buffer is one record. A Line belongs to one
// goroutine at a time.
type Line struct {
	buffer []byte
	size   int
}

// NewLine returns a Line that assembles in storage.
func NewLine(storage []byte) Line { return Line{buffer: storage} }

// Print appends value.
func (line *Line) Print(value []byte) {
	for len(value) != 0 {
		if line.size == len(line.buffer) {
			standard.writeRecord(standard.stdout, line.buffer[:line.size])
			line.size = 0
		}
		copied := copy(line.buffer[line.size:], value)
		line.size += copied
		value = value[copied:]
	}
}

// PrintText appends value.
func (line *Line) PrintText(value string) { line.Print(sys.Bytes(value)) }

// PrintName appends a name that came from a filesystem, a path or a label,
// with every character a terminal could obey shown as '?'
// (terminal.Printable). File names are data and must not reach the terminal
// as commands.
func (line *Line) PrintName(value []byte) {
	for index := 0; index < len(value); {
		var character []byte
		character, index = terminal.Printable(value, index)
		line.Print(character)
	}
}

// PrintUint appends value in decimal and returns how many digits it took.
func (line *Line) PrintUint(value uint64) int {
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
	line.Print(digits[position:])
	return len(digits) - position
}

// Pad appends spaces from column until width.
func (line *Line) Pad(column int, width int) {
	for ; column < width; column++ {
		line.PrintText(" ")
	}
}

// End ends the line and writes it to standard output as one record.
func (line *Line) End() {
	line.PrintText("\n")
	standard.writeRecord(standard.stdout, line.buffer[:line.size])
	line.size = 0
}

// The current output line. Processes share the console, so a line split
// across several writes could interleave with another process's output and
// arrive as one unreadable line; the line is assembled here and written
// once. The buffer holds the longest line the terminal accepts. Print,
// PrintText, PrintName, PrintUint, Pad, Arguments and EndLine all work on
// this one line, so they belong to one goroutine: the one running main,
// unless the program hands the line over.
var (
	lineBuffer  [shell.MaxLine]byte
	currentLine = Line{buffer: lineBuffer[:]}
)

// Print appends value to the current output line.
func Print(value []byte) { currentLine.Print(value) }

// PrintName appends a name to the current output line as Line.PrintName
// does.
func PrintName(value []byte) { currentLine.PrintName(value) }

// PrintText appends value to the current output line.
func PrintText(value string) { currentLine.PrintText(value) }

// PrintUint appends value in decimal and returns how many digits it took.
func PrintUint(value uint64) int { return currentLine.PrintUint(value) }

// AppendUint appends value in decimal without allocating.
func AppendUint(target []byte, value uint64) []byte { return appendUint(target, value) }

// Pad appends spaces from column until width.
func Pad(column int, width int) { currentLine.Pad(column, width) }

// EndLine ends the current line and writes it to standard output.
func EndLine() { currentLine.End() }

// Write sends value to standard output as it is, outside the line buffer, as
// one record.
func Write(value []byte) { standard.writeRecord(standard.stdout, value) }

// WriteError sends already escaped diagnostic bytes to standard error as one
// record.
func WriteError(value []byte) { writeErrorBytes(value) }

func write(message string) { Write(sys.Bytes(message)) }

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
	standard.writeRecord(standard.errorTarget(), message)
}

// nativeStream writes the standard streams with the kernel.
type nativeStream struct{}

// write moves a prefix of value, one native stream write, waiting while the
// stream is full. A console handle takes the whole value at once. A stream
// that fails or stays full ends the process with status 2.
func (nativeStream) write(target uint64, value []byte) int {
	size := len(value)
	if size > int(kcall.MaxStreamIO) {
		size = int(kcall.MaxStreamIO)
	}
	for {
		written, status := sys.StreamWrite(handle.ID(target), value[:size])
		if status == kcall.StatusPermissionDenied {
			sys.ConsoleWrite(handle.ID(target), value)
			return len(value)
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
		return written
	}
}

// nativeIPC sends and receives service messages with the kernel.
type nativeIPC struct{}

func (nativeIPC) send(channel handle.ID, wire []byte) kcall.Status {
	return sys.IPCSend(channel, wire)
}

func (nativeIPC) waitReceive(channel handle.ID, wire []byte, ticks uint64) kcall.Status {
	return sys.IPCWaitReceive(channel, wire, ticks)
}
