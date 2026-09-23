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
var targetBuffer [shell.MaxPath]byte
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
	commandName = data.Command
	info, documented := shell.LookupCommand(data.Command)
	if !documented || info.Builtin {
		failf(nil, "not a utility in this image")
	}
	current = info
	// Utility startup is on every command's latency path, so the checks read
	// the count byte and decode the vector only when there is a flag to test.
	count := data.ArgCount()
	if count == 1 {
		if first, _ := data.Arg(1); shell.IsHelpFlag(first) {
			printHelp(info)
			finish(0, "")
		}
	}
	if !info.Accepts(count) {
		usage()
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
	case equal(data.Command, "quota"):
		runQuota()
	case equal(data.Command, "mem"):
		runMem()
	case equal(data.Command, "bench"):
		runBench(data)
	default:
		failf(nil, "not a utility in this image")
	}
	finish(0, "")
}

// current is the invoking command's entry in the shared command table, and
// commandName the name it was started under; diagnostics are prefixed with it.
var (
	current     shell.CommandInfo
	commandName []byte
	errorBuffer [512]byte
)

func printHelp(info shell.CommandInfo) {
	lineText("usage: ")
	lineText(info.Usage)
	lineFlush()
	lineText("  ")
	lineText(info.Summary)
	lineFlush()
}

// usage ends a malformed invocation with the command's synopsis.
func usage() {
	size := copy(errorBuffer[:], "usage: ")
	size += copy(errorBuffer[size:len(errorBuffer)-1], current.Usage)
	errorBuffer[size] = '\n'
	writeErrorBytes(errorBuffer[:size+1])
	finish(2, "")
}

// failf ends the utility with "command: path: reason", or "command: reason"
// when path is nil. The line is written at once so it cannot interleave.
func failf(path []byte, reason string) {
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

// check ends the utility when a VFS request about path failed. A refused
// mutation of a read-only mount is named as such.
func check(reply vfs.Reply, received bool, path []byte, mutation bool) {
	if !received {
		failf(path, "filesystem service unavailable")
	}
	if reply.Status != vfs.StatusOK {
		failf(path, shell.VFSReason(reply.Status, mutation))
	}
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
	if created.Status == vfs.StatusAlreadyExists {
		created.Status = vfs.StatusOK
	}
	check(created, received, path, true)
	offset := uint64(0)
	if appendMode {
		stat, received := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
		check(stat, received, path, false)
		offset = stat.Size
	} else {
		truncated, received := callVFS(vfs.Request{Operation: vfs.OperationTruncate, Path: path, Offset: 0})
		check(truncated, received, path, true)
	}
	opened, received := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: path})
	check(opened, received, path, true)
	if appendMode {
		seek, received := callVFS(vfs.Request{Operation: vfs.OperationSeek, Handle: opened.Handle, Offset: offset})
		check(seek, received, path, false)
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
			checkWritten(reply, ok, path, chunk)
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
			failf(nil, "cannot read standard input")
		}
		reply, ok := callVFS(vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: buffer[:size]})
		checkWritten(reply, ok, path, size)
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
		if status == uint64(kcall.StatusPeerDied) {
			// A closed input is end of input, not a fault: a pipeline whose
			// producer never started ends here. With nothing read there is no
			// line, which the exit status says without a message.
			if size == 0 {
				finish(1, "")
			}
			break
		}
		if status != 0 {
			failf(nil, "cannot read standard input")
		}
		if inputByte == '\r' || inputByte == '\n' {
			break
		}
		if size == len(line) {
			failf(nil, "line longer than 128 bytes")
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
	path := resolve(data, argument)
	stat, ok := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
	check(stat, ok, path, false)
	if stat.Kind == ramfs.KindFile {
		lineBytes(baseName(path))
		lineFlush()
		mark("LS_PROCESS_OK\n")
		return
	}
	var cursor uint32
	for {
		reply, received := callVFS(vfs.Request{Operation: vfs.OperationReadDir, Path: path, Offset: uint64(cursor)})
		if received && reply.Status == vfs.StatusNotFound {
			break
		}
		check(reply, received, path, false)
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
	path := requiredPath(data, 1)
	opened, received := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	check(opened, received, path, false)
	for {
		reply, received := callVFS(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: vfs.MaxReplyData})
		check(reply, received, path, false)
		if len(reply.Data) == 0 {
			break
		}
		writeBytes(reply.Data)
	}
	callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	mark("CAT_PROCESS_OK\n")
}

func runCreate(data startup.Data, directory bool) {
	path := requiredPath(data, 1)
	operation := vfs.OperationCreate
	marker := "TOUCH_PROCESS_OK\n"
	if directory {
		operation, marker = vfs.OperationMkdir, "MKDIR_PROCESS_OK\n"
	}
	reply, received := callVFS(vfs.Request{Operation: operation, Path: path})
	if operation == vfs.OperationCreate && reply.Status == vfs.StatusAlreadyExists {
		reply.Status = vfs.StatusOK
	}
	check(reply, received, path, true)
	mark(marker)
}

func runUnlink(data startup.Data) {
	path := requiredPath(data, 1)
	reply, received := callVFS(vfs.Request{Operation: vfs.OperationUnlink, Path: path})
	check(reply, received, path, true)
	mark("RM_PROCESS_OK\n")
}

func runMove(data startup.Data) {
	source := requiredPath(data, 1)
	target := requiredTarget(data)
	reply, received := callVFS(vfs.Request{Operation: vfs.OperationRename, Path: source, Data: target})
	if reply.Status == vfs.StatusAlreadyExists {
		check(reply, received, target, true)
	}
	check(reply, received, source, true)
	mark("MV_PROCESS_OK\n")
}

func runCopy(data startup.Data) {
	source := requiredPath(data, 1)
	target := requiredTarget(data)
	// Open the source first so a missing one leaves the target untouched.
	input, received := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: source})
	check(input, received, source, false)
	created, received := callVFS(vfs.Request{Operation: vfs.OperationCreate, Path: target})
	if created.Status == vfs.StatusAlreadyExists {
		created.Status = vfs.StatusOK
	}
	check(created, received, target, true)
	truncated, received := callVFS(vfs.Request{Operation: vfs.OperationTruncate, Path: target, Offset: 0})
	check(truncated, received, target, true)
	output, received := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: target})
	check(output, received, target, true)
	for {
		chunk, ok := callVFS(vfs.Request{Operation: vfs.OperationRead, Handle: input.Handle, Size: vfs.MaxProtocolData})
		check(chunk, ok, source, false)
		if len(chunk.Data) == 0 {
			break
		}
		written, ok := callVFS(vfs.Request{Operation: vfs.OperationWrite, Handle: output.Handle, Data: chunk.Data})
		checkWritten(written, ok, target, len(chunk.Data))
	}
	callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: input.Handle})
	callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: output.Handle})
	mark("CP_PROCESS_OK\n")
}

func runStat(data startup.Data) {
	path := requiredPath(data, 1)
	reply, received := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
	check(reply, received, path, false)
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
		failf(nil, "clock unavailable")
	}
	var text [32]byte
	lineBytes(shell.FormatUptime(clockValue/clock.TicksPerSecond, &text))
	lineFlush()
	mark("UPTIME_PROCESS_OK\n")
}

func runSleep(data startup.Data) {
	value, _ := data.Arg(1)
	duration, ok := parseSeconds(value)
	if !ok {
		failf(value, "not a number of seconds between 0 and 86400")
	}
	if clockRead(unsafe.Pointer(&clockValue), 8) != 0 {
		failf(nil, "clock unavailable")
	}
	deadline := clockValue + duration
	for clockValue < deadline {
		// A wait with no interests blocks until the deadline instead of
		// spinning on the clock.
		userwait.Sleep(deadline - clockValue)
		if clockRead(unsafe.Pointer(&clockValue), 8) != 0 {
			failf(nil, "clock unavailable")
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

// pad writes spaces until the current column reaches width.
func pad(column int, width int) {
	for ; column < width; column++ {
		lineText(" ")
	}
}

func processName(record []byte) []byte {
	name := record[16 : 16+kcall.MaxProcessName]
	size := 0
	for size < len(name) && name[size] != 0 {
		size++
	}
	return name[:size]
}

func runPS() {
	var record [kcall.ProcessInfoSize]byte
	lineText("PID   TYPE     STATE      MEM  CPU   COMMAND")
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
		pages := binary.LittleEndian.Uint32(record[kcall.ProcessInfoUsedOffset+kcall.ProcessPages*4:])
		cpu := binary.LittleEndian.Uint64(record[kcall.ProcessInfoCPUOffset:])
		pad(lineUintWidth(pid), 6)
		lineText("native   ")
		lineText(state)
		pad(len(state), 11)
		// MEM is the pages the kernel allocated for the process; CPU is the
		// timer ticks it was found running on.
		pad(lineUintWidth(uint64(pages)), 5)
		pad(lineUintWidth(cpu), 6)
		lineBytes(processName(record[:]))
		lineFlush()
	}
	mark("PS_PROCESS_OK\n")
}

// runQuota prints every process's resource account: what it holds of each
// bounded kernel resource and the share it may hold.
func runQuota() {
	var record [kcall.ProcessInfoSize]byte
	names := [kcall.ProcessResources]string{"chan", "strm", "task", "page"}
	lineText("PID   COMMAND           RESOURCE  HELD/LIMIT")
	lineFlush()
	for index := uint64(0); processInfo(index, unsafe.Pointer(&record[0])) == 0; index++ {
		pid := binary.LittleEndian.Uint64(record[0:8])
		name := processName(record[:])
		for resource := 0; resource < kcall.ProcessResources; resource++ {
			held := binary.LittleEndian.Uint32(record[kcall.ProcessInfoUsedOffset+resource*4:])
			limit := binary.LittleEndian.Uint32(record[kcall.ProcessInfoLimitsOffset+resource*4:])
			pad(lineUintWidth(pid), 6)
			lineBytes(name)
			pad(len(name), 18)
			lineText(names[resource])
			pad(len(names[resource]), 10)
			lineUint(uint64(held))
			lineText("/")
			lineUint(uint64(limit))
			lineFlush()
		}
	}
	mark("QUOTA_PROCESS_OK\n")
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
		failf(nil, "system information unavailable")
	}
	// total is the memory the page allocator manages, not the machine's RAM:
	// firmware, the kernel image and unmanaged regions are outside it.
	total := binary.LittleEndian.Uint64(systemInfo[16:24]) * kcall.SystemInfoPageSize
	free := binary.LittleEndian.Uint64(systemInfo[24:32]) * kcall.SystemInfoPageSize
	memLine("total: ", total)
	memLine("used:  ", total-free)
	memLine("free:  ", free)
	mark("MEM_PROCESS_OK\n")
}

func memLine(label string, bytes uint64) {
	var text [24]byte
	lineText(label)
	lineBytes(shell.FormatSize(bytes, &text))
	lineFlush()
}

// requiredPath resolves argument index against the working directory; a
// missing argument is a usage error.
func requiredPath(data startup.Data, index int) []byte {
	argument, found := data.Arg(index)
	if !found {
		usage()
	}
	return resolve(data, argument)
}

// requiredTarget resolves the second path of a two-path command into its own
// buffer, so it does not overwrite the first.
func requiredTarget(data startup.Data) []byte {
	argument, found := data.Arg(2)
	if !found {
		usage()
	}
	path, ok := shell.ResolvePath(data.CWD, argument, &targetBuffer)
	if !ok {
		failf(argument, "invalid path")
	}
	return path
}

func resolve(data startup.Data, argument []byte) []byte {
	path, ok := shell.ResolvePath(data.CWD, argument, &pathBuffer)
	if !ok {
		failf(argument, "invalid path")
	}
	return path
}

// checkWritten also refuses a short write, which would silently truncate.
func checkWritten(reply vfs.Reply, received bool, path []byte, size int) {
	check(reply, received, path, true)
	if reply.Size != uint64(size) {
		failf(path, "short write")
	}
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
