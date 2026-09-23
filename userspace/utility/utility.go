//go:build goos_userspace

package utility

import (
	"encoding/binary"
	"goos/internal/validation"
	"goos/internal/version"
	"goos/kernel/clock"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/shell"
	"goos/userspace/startup"
	userwait "goos/userspace/wait"
	"runtime"
	"unsafe"
)

var wire [ipc.WireMaxSize]byte
var descriptor = [3]uint64{uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000}
var startupWire [startup.Size]byte
var pathBuffer [shell.MaxPath]byte
var correlation uint64
var heapPointer uintptr
var stdoutHandle uint64
var stderrHandle uint64
var stdinHandle uint64
var vfsHandle uint64
var clockValue uint64
var systemInfo [kcall.SystemInfoSize]byte
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

//go:linkname processInfo goos_process_info
func processInfo(index uint64, address unsafe.Pointer) uint64

//go:linkname systemInfoRead goos_system_info
func systemInfoRead(address unsafe.Pointer, size uintptr) uint64

func Run() {
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
	if !stdinFound || !stdoutFound || !stderrFound || needsVFS(data.Command) && !vfsFound {
		finish(1, "startup handles unavailable\n")
	}
	switch {
	case equal(data.Command, "pwd"):
		lineBytes(data.CWD)
		lineFlush()
		mark("PWD_PROCESS_OK\n")
	case equal(data.Command, "ls"):
		runLs(data)
	case equal(data.Command, "cat"):
		runCat(data)
	case equal(data.Command, "args"):
		writeArguments(data, 1)
		lineFlush()
		if path, found := data.Env("PATH"); found {
			lineText("PATH=")
			lineBytes(path)
			lineFlush()
		}
		mark("ARGS_PROCESS_OK\nARGV_ENV_PROCESS_OK\nSTDIN_HANDLE_OK\nSTDERR_HANDLE_OK\n")
	case equal(data.Command, "echo"):
		writeArguments(data, 1)
		lineFlush()
		mark("ECHO_PROCESS_OK\n")
	case equal(data.Command, "readline"):
		runReadline()
	case equal(data.Command, "mkdir"):
		runCreate(data, true)
	case equal(data.Command, "touch"):
		runCreate(data, false)
	case equal(data.Command, "rm"):
		runUnlink(data)
	case equal(data.Command, "mv"):
		runMove(data)
	case equal(data.Command, "cp"):
		runCopy(data)
	case equal(data.Command, "stat"):
		runStat(data)
	case equal(data.Command, "write"):
		runWrite(data)
	case equal(data.Command, "uptime"):
		runUptime()
	case equal(data.Command, "uname"):
		lineText(version.Name + " " + version.Release + " " + version.Machine)
		lineFlush()
		mark("UNAME_PROCESS_OK\n")
	case equal(data.Command, "sleep"):
		runSleep(data)
	case equal(data.Command, "ps"):
		runPS()
	case equal(data.Command, "mem"):
		runMem()
	case equal(data.Command, "bench"):
		runBench(data)
	default:
		finish(1, "invalid utility\n")
	}
	finish(0, "")
}

func needsVFS(command []byte) bool {
	return equal(command, "pwd") || equal(command, "ls") || equal(command, "cat") || equal(command, "mkdir") ||
		equal(command, "touch") || equal(command, "rm") || equal(command, "cp") || equal(command, "mv") || equal(command, "stat") || equal(command, "write") || equal(command, "bench")
}

func runWrite(data startup.Data) {
	var buffer [vfs.MaxProtocolData]byte
	pathIndex, appendMode := 1, false
	if argument, found := data.Arg(1); found && equal(argument, "--append") {
		pathIndex, appendMode = 2, true
	}
	path := requiredPath(data, pathIndex)
	created, received := callVFS(vfs.Request{Operation: vfs.OperationCreate, Path: path})
	if !received || created.Status != vfs.StatusOK && created.Status != vfs.StatusAlreadyExists {
		finish(2, refusal(created.Status, "write: read-only filesystem\n", "write: create failed\n"))
	}
	offset := uint64(0)
	if appendMode {
		stat, received := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
		if !received || stat.Status != vfs.StatusOK {
			finish(2, "write: stat failed\n")
		}
		offset = stat.Size
	} else {
		truncated, received := callVFS(vfs.Request{Operation: vfs.OperationTruncate, Path: path, Offset: 0})
		if !received || truncated.Status != vfs.StatusOK {
			finish(2, "write: truncate failed\n")
		}
	}
	opened, received := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: path})
	if !received || opened.Status != vfs.StatusOK {
		finish(2, "write: open failed\n")
	}
	if appendMode {
		seek, received := callVFS(vfs.Request{Operation: vfs.OperationSeek, Handle: opened.Handle, Offset: offset})
		if !received || seek.Status != vfs.StatusOK {
			finish(2, "write: seek failed\n")
		}
	}
	if _, inline := data.Arg(pathIndex + 1); inline {
		var text [startup.MaxVector + 1]byte
		size := 0
		for index := pathIndex + 1; ; index++ {
			argument, found := data.Arg(index)
			if !found {
				break
			}
			if size != 0 {
				text[size] = ' '
				size++
			}
			size += copy(text[size:len(text)-1], argument)
		}
		text[size] = '\n'
		for written := 0; written <= size; {
			chunk := copy(buffer[:], text[written:size+1])
			reply, ok := callVFS(vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: buffer[:chunk]})
			if !ok || reply.Status != vfs.StatusOK || reply.Size != uint64(chunk) {
				finish(2, "write: VFS error\n")
			}
			written += chunk
		}
		callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
		mark("WRITE_PROCESS_OK\n")
		return
	}
	for {
		status, size := readInput(buffer[:])
		if status == uint64(kcall.StatusNotFound) || status == uint64(kcall.StatusResourceExhausted) {
			continue
		}
		if status == uint64(kcall.StatusPeerDied) {
			break
		}
		if status != 0 {
			finish(2, "write: input failed\n")
		}
		reply, ok := callVFS(vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: buffer[:size]})
		if !ok || reply.Status != vfs.StatusOK || reply.Size != uint64(size) {
			finish(2, "write: VFS error\n")
		}
	}
	callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	mark("WRITE_PROCESS_OK\n")
}

func runReadline() {
	const maxLine = 128
	var line [maxLine]byte
	size := 0
	for {
		status := readInputByte()
		if status == uint64(kcall.StatusNotFound) || status == uint64(kcall.StatusResourceExhausted) {
			continue
		}
		if status != 0 {
			finish(2, "readline: input unavailable\n")
		}
		if inputByte == '\r' || inputByte == '\n' {
			break
		}
		if size == len(line) {
			finish(2, "readline: input too long\n")
		}
		line[size] = inputByte
		size++
	}
	lineBytes(line[:size])
	lineFlush()
	mark("STDIN_READ_OK\n")
}

func readInputByte() uint64 {
	status, _ := readInput(unsafe.Slice(&inputByte, 1))
	return status
}

func readInput(target []byte) (uint64, int) {
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

func runLs(data startup.Data) {
	argument, found := data.Arg(1)
	if !found {
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
		lineBytes(baseName(path))
		lineFlush()
		mark("LS_PROCESS_OK\n")
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
		lineBytes(reply.Name)
		if reply.Kind == ramfs.KindDirectory {
			lineText("/")
		}
		lineFlush()
		cursor = reply.Cursor
	}
	mark("LS_PROCESS_OK\n")
}

func runCat(data startup.Data) {
	argument, found := data.Arg(1)
	if !found {
		finish(2, "cat: path required\n")
	}
	path, ok := shell.ResolvePath(data.CWD, argument, &pathBuffer)
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
	mark("CAT_PROCESS_OK\n")
}

func runCreate(data startup.Data, directory bool) {
	argument, found := data.Arg(1)
	if !found {
		finish(2, "path required\n")
	}
	path, ok := shell.ResolvePath(data.CWD, argument, &pathBuffer)
	if !ok {
		finish(2, "invalid path\n")
	}
	operation := vfs.OperationCreate
	marker := "TOUCH_PROCESS_OK\n"
	if directory {
		operation, marker = vfs.OperationMkdir, "MKDIR_PROCESS_OK\n"
	}
	reply, received := callVFS(vfs.Request{Operation: operation, Path: path})
	if !received || reply.Status != vfs.StatusOK && !(operation == vfs.OperationCreate && reply.Status == vfs.StatusAlreadyExists) {
		finish(2, refusal(reply.Status, "read-only filesystem\n", "create failed\n"))
	}
	mark(marker)
}

// refusal picks the message for a refused mutation. A read-only mount is named
// as such so the user can tell an immutable path from a bad one.
func refusal(status vfs.Status, denied string, generic string) string {
	if status == vfs.StatusPermissionDenied {
		return denied
	}
	return generic
}

func runUnlink(data startup.Data) {
	path := requiredPath(data, 1)
	reply, received := callVFS(vfs.Request{Operation: vfs.OperationUnlink, Path: path})
	if !received || reply.Status != vfs.StatusOK {
		finish(2, refusal(reply.Status, "rm: read-only filesystem\n", "rm: failed\n"))
	}
	mark("RM_PROCESS_OK\n")
}

func runMove(data startup.Data) {
	source := requiredPath(data, 1)
	var targetBuffer [shell.MaxPath]byte
	targetArg, found := data.Arg(2)
	if !found {
		finish(2, "mv: target required\n")
	}
	target, ok := shell.ResolvePath(data.CWD, targetArg, &targetBuffer)
	if !ok {
		finish(2, "mv: invalid target\n")
	}
	reply, received := callVFS(vfs.Request{Operation: vfs.OperationRename, Path: source, Data: target})
	if !received || reply.Status != vfs.StatusOK {
		finish(2, "mv: failed\n")
	}
	mark("MV_PROCESS_OK\n")
}

func runCopy(data startup.Data) {
	source := requiredPath(data, 1)
	var targetBuffer [shell.MaxPath]byte
	targetArg, found := data.Arg(2)
	if !found {
		finish(2, "cp: target required\n")
	}
	target, ok := shell.ResolvePath(data.CWD, targetArg, &targetBuffer)
	if !ok {
		finish(2, "cp: invalid target\n")
	}
	created, received := callVFS(vfs.Request{Operation: vfs.OperationCreate, Path: target})
	if !received || created.Status != vfs.StatusOK && created.Status != vfs.StatusAlreadyExists {
		finish(2, refusal(created.Status, "cp: read-only target\n", "cp: create failed\n"))
	}
	callVFS(vfs.Request{Operation: vfs.OperationTruncate, Path: target, Offset: 0})
	input, received := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: source})
	if !received || input.Status != vfs.StatusOK {
		finish(2, "cp: source failed\n")
	}
	output, received := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: target})
	if !received || output.Status != vfs.StatusOK {
		finish(2, "cp: target failed\n")
	}
	for {
		chunk, ok := callVFS(vfs.Request{Operation: vfs.OperationRead, Handle: input.Handle, Size: vfs.MaxProtocolData})
		if !ok || chunk.Status != vfs.StatusOK {
			finish(2, "cp: read failed\n")
		}
		if len(chunk.Data) == 0 {
			break
		}
		written, ok := callVFS(vfs.Request{Operation: vfs.OperationWrite, Handle: output.Handle, Data: chunk.Data})
		if !ok || written.Status != vfs.StatusOK || written.Size != uint64(len(chunk.Data)) {
			finish(2, "cp: write failed\n")
		}
	}
	callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: input.Handle})
	callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: output.Handle})
	mark("CP_PROCESS_OK\n")
}

func runStat(data startup.Data) {
	path := requiredPath(data, 1)
	reply, received := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
	if !received || reply.Status != vfs.StatusOK {
		finish(2, "stat: not found\n")
	}
	if reply.Kind == ramfs.KindDirectory {
		lineText("directory ")
	} else {
		lineText("file ")
	}
	lineUint(reply.Size)
	lineText(" bytes")
	lineFlush()
	mark("STAT_PROCESS_OK\n")
}

func runUptime() {
	if clockRead(unsafe.Pointer(&clockValue), 8) != 0 {
		finish(2, "uptime unavailable\n")
	}
	lineUint(clockValue)
	lineText(" ticks")
	lineFlush()
	mark("UPTIME_PROCESS_OK\n")
}

func runSleep(data startup.Data) {
	value, found := data.Arg(1)
	if !found {
		finish(2, "sleep: seconds required\n")
	}
	duration, ok := parseSeconds(value)
	if !ok {
		finish(2, "sleep: invalid duration\n")
	}
	if clockRead(unsafe.Pointer(&clockValue), 8) != 0 {
		finish(2, "sleep unavailable\n")
	}
	deadline := clockValue + duration
	for clockValue < deadline {
		// A wait with no interests blocks until the deadline instead of
		// spinning on the clock.
		userwait.Sleep(deadline - clockValue)
		if clockRead(unsafe.Pointer(&clockValue), 8) != 0 {
			finish(2, "sleep unavailable\n")
		}
	}
	mark("SLEEP_PROCESS_OK\n")
}

// parseSeconds converts seconds, with up to three decimals, into timer ticks.
// A non-zero duration never rounds down to no wait at all.
func parseSeconds(value []byte) (uint64, bool) {
	var whole, fraction uint64
	digits, seen, point := 0, false, false
	for _, character := range value {
		switch {
		case character >= '0' && character <= '9':
			seen = true
			if point {
				if digits < 3 {
					fraction = fraction*10 + uint64(character-'0')
					digits++
				}
				continue
			}
			whole = whole*10 + uint64(character-'0')
			if whole > 86400 {
				return 0, false
			}
		case character == '.' && !point:
			point = true
		default:
			return 0, false
		}
	}
	if !seen {
		return 0, false
	}
	for digits < 3 {
		fraction *= 10
		digits++
	}
	ticks := whole*clock.TicksPerSecond + fraction*clock.TicksPerSecond/1000
	if ticks == 0 && (whole != 0 || fraction != 0) {
		ticks = 1
	}
	return ticks, true
}

func runPS() {
	var record [kcall.ProcessInfoSize]byte
	lineText("PID   TYPE     STATE      COMMAND")
	lineFlush()
	for index := uint64(0); processInfo(index, unsafe.Pointer(&record[0])) == 0; index++ {
		pid := binary.LittleEndian.Uint64(record[0:8])
		state := "waiting"
		switch record[8] {
		case kcall.ProcessRunning:
			state = "running"
		case kcall.ProcessReady:
			state = "ready"
		}
		name := record[16 : 16+kcall.MaxProcessName]
		size := 0
		for size < len(name) && name[size] != 0 {
			size++
		}
		column := lineUintWidth(pid)
		for ; column < 6; column++ {
			lineText(" ")
		}
		lineText("native   ")
		lineText(state)
		for column = len(state); column < 11; column++ {
			lineText(" ")
		}
		lineBytes(name[:size])
		lineFlush()
	}
	mark("PS_PROCESS_OK\n")
}

func lineUintWidth(value uint64) int {
	lineUint(value)
	width := 1
	for value >= 10 {
		value /= 10
		width++
	}
	return width
}

func runMem() {
	if systemInfoRead(unsafe.Pointer(&systemInfo[0]), uintptr(len(systemInfo))) != 0 {
		finish(2, "mem unavailable\n")
	}
	lineText("pages managed ")
	lineUint(binary.LittleEndian.Uint64(systemInfo[16:24]))
	lineText(" free ")
	lineUint(binary.LittleEndian.Uint64(systemInfo[24:32]))
	lineFlush()
	mark("MEM_PROCESS_OK\n")
}

func requiredPath(data startup.Data, index int) []byte {
	argument, found := data.Arg(index)
	if !found {
		finish(2, "path required\n")
	}
	path, ok := shell.ResolvePath(data.CWD, argument, &pathBuffer)
	if !ok {
		finish(2, "invalid path\n")
	}
	return path
}

func writeArguments(data startup.Data, start int) {
	for index := start; ; index++ {
		argument, found := data.Arg(index)
		if !found {
			return
		}
		if index != start {
			lineText(" ")
		}
		lineBytes(argument)
	}
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

func lineBytes(value []byte) {
	if lineSize+len(value) > len(lineBuffer) {
		finish(2, "output line too long\n")
	}
	lineSize += copy(lineBuffer[lineSize:], value)
}

func lineText(value string) {
	lineBytes(unsafe.Slice(unsafe.StringData(value), len(value)))
}

func lineUint(value uint64) {
	var digits [20]byte
	position := len(digits)
	if value == 0 {
		lineText("0")
		return
	}
	for value != 0 {
		position--
		digits[position] = byte(value%10) + '0'
		value /= 10
	}
	lineBytes(digits[position:])
}

func lineFlush() {
	lineText("\n")
	writeHandle(stdoutHandle, lineBuffer[:lineSize])
	lineSize = 0
}

func write(message string) {
	writeHandle(stdoutHandle, unsafe.Slice(unsafe.StringData(message), len(message)))
}
func writeBytes(value []byte) {
	writeHandle(stdoutHandle, value)
}
func mark(message string) {
	if validation.Enabled {
		writeError(message)
	}
}

func writeError(message string) {
	target := stderrHandle
	if target == 0 {
		target = stdoutHandle
	}
	writeHandle(target, unsafe.Slice(unsafe.StringData(message), len(message)))
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
