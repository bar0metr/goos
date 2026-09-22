//go:build goos_userspace

package main

import (
	"encoding/binary"
	"goos/internal/validation"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/ramfs"
	"goos/kernel/tty"
	"goos/kernel/vfs"
	execproto "goos/userspace/exec"
	shellcmd "goos/userspace/shell"
	"goos/userspace/startup"
	"runtime"
	"strconv"
	"unsafe"
)

var (
	consoleHandle     handle.ID
	vfsHandle         handle.ID
	execHandle        handle.ID
	startupWire       [startup.Size]byte
	editor            tty.LineEditor
	inputByte         byte
	heapPointer       uintptr
	arenaBase         uintptr
	wire              [ipc.WireMaxSize]byte
	descriptor        = [3]uint64{uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000}
	correlation       uint64
	startupData       startup.Data
	handleRecord      [kcall.HandleInfoSize]byte
	cwd               = [shellcmd.MaxPath]byte{'/'}
	cwdSize           = 1
	pathBuffer        [shellcmd.MaxPath]byte
	executableBuffers [execproto.MaxStages][execproto.MaxPath]byte
	argumentStorage   [shellcmd.MaxArguments][shellcmd.MaxArgumentSize]byte
	vectorWires       [execproto.MaxStages][startup.VectorSize]byte
	clockNanos        uint64
	jobs              [execproto.Slots]job
	execPage          = (*[execproto.Slots][execproto.SlotSize]byte)(unsafe.Pointer(uintptr(execproto.PageAddress)))
)

// job is an exec request in flight; its slot of the shared page is reused
// only after PID 1 replies.
type job struct {
	active      bool
	background  bool
	correlation uint64
	command     [tty.MaxLine]byte
	commandSize int
}

//go:linkname memoryMap goos_memory_map
func memoryMap(memory uint64, address uint64, flags uint64) uint64

//go:linkname ipcReceive goos_ipc_receive
func ipcReceive(channel uint64, address unsafe.Pointer, capacity uintptr) uint64

//go:extern _heap_start
var heapStartSymbol [0]byte

//go:extern _heap_end
var heapEndSymbol [0]byte

//go:linkname handleInfo goos_handle_info
func handleInfo(slot uint64, address unsafe.Pointer) uint64

//go:linkname logRead goos_log_read
func logRead(device uint64, index uint64, address unsafe.Pointer) uint64

//go:linkname diagnostics goos_diagnostics
func diagnostics(device uint64, address uintptr) uint64

//go:linkname clockReadNanos goos_clock_read_nanos
func clockReadNanos(address unsafe.Pointer, size uintptr) uint64

//go:linkname consoleWrite goos_console_write
func consoleWrite(device uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname consoleRead goos_console_read
func consoleRead(device uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname startupRead goos_startup_read
func startupRead(address unsafe.Pointer, size uintptr) uint64

//go:linkname ipcSend goos_ipc_send
func ipcSend(channel uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname ipcWaitReceive goos_ipc_wait_receive
func ipcWaitReceive(channel uint64, descriptor unsafe.Pointer, size uintptr) uint64

//export userspaceMain
func userspaceMain() {
	runtimeInitHeap()
	if startupRead(unsafe.Pointer(&startupWire[0]), startup.Size) != 0 {
		fatal("SHELL_STARTUP_UNAVAILABLE\n")
	}
	var validStartup, found bool
	startupData, validStartup = startup.Decode(startupWire[:])
	if !validStartup {
		fatal("SHELL_STARTUP_INVALID\n")
	}
	consoleHandle, found = startupData.Handle("console")
	if !found {
		fatal("SHELL_CONSOLE_MISSING\n")
	}
	vfsHandle, found = startupData.Handle("vfs")
	if !found {
		fatal("SHELL_VFS_MISSING\n")
	}
	execHandle, found = startupData.Handle("exec")
	if !found {
		fatal("SHELL_EXEC_MISSING\n")
	}
	pageHandle, found := startupData.Handle("execpage")
	if !found || memoryMap(uint64(pageHandle), execproto.PageAddress, kcall.MemoryMapWritable) != 0 {
		fatal("SHELL_EXEC_PAGE_FAILED\n")
	}
	arenaBase = heapPointer
	root, ok := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: cwd[:cwdSize]})
	if !ok || root.Status != vfs.StatusOK || root.Kind != ramfs.KindDirectory {
		fatal("SHELL_VFS_CONNECT_FAILED\n")
	}
	heapPointer = arenaBase
	mark("SHELL_PROCESS_OK\nSHELL_VFS_CONNECTED_OK\n")
	write("\ngoos> ")
	for {
		status := consoleRead(uint64(consoleHandle), unsafe.Pointer(&inputByte), 1)
		if status == uint64(kcall.StatusNotFound) {
			if pollReplies() {
				write("goos> ")
				write(string(editor.Pending()))
			}
			continue
		}
		if status != 0 {
			fatal("SHELL_INPUT_FAILED\n")
		}
		if inputByte == 3 {
			write("^C\n")
			editor.Reset()
			write("goos> ")
			continue
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
		heapPointer = arenaBase
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
	if command, background := shellcmd.SplitBackground(line); background {
		executeBackground(command)
		return
	}
	if command, timed := shellcmd.SplitTime(line); timed {
		executeTimed(command)
		return
	}
	commands, count, valid := shellcmd.ParseSequence(line, &argumentStorage)
	if !valid {
		write("exec: invalid command sequence\n")
		return
	}
	for index := 0; index < count; index++ {
		executeCommand(commands[index])
		// Sequence slices borrow the static line-editor buffer, not the arena.
		heapPointer = arenaBase
	}
}

// executeBackground starts one pipeline without waiting for it. Builtins and
// command sequences run in the shell itself and cannot be backgrounded.
func executeBackground(line []byte) {
	commands, count, valid := shellcmd.ParseSequence(line, &argumentStorage)
	if !valid || count != 1 || len(commands[0]) == 0 {
		write("exec: & applies to one command or pipeline\n")
		return
	}
	redirects, _ := shellcmd.SplitRedirections(line)
	stages, _, _ := shellcmd.SplitPipeline(redirects.Command)
	invocation, parsed := shellcmd.ParseInvocation(stages[0], &argumentStorage)
	if !parsed || invocation.Count == 0 || shellcmd.IsBuiltin(invocation.Arguments[0]) {
		write("exec: builtins cannot run in the background\n")
		return
	}
	executeExternal(line, true)
}

// executeTimed runs the rest of a command line through the normal path and
// reports the elapsed wall-clock time, including every launch it caused.
func executeTimed(line []byte) {
	if len(line) == 0 {
		write("time: expected a command\n")
		return
	}
	if clockReadNanos(unsafe.Pointer(&clockNanos), 8) != 0 {
		write("time: clock unavailable\n")
		return
	}
	start := clockNanos
	execute(line)
	if clockReadNanos(unsafe.Pointer(&clockNanos), 8) != 0 {
		write("time: clock unavailable\n")
		return
	}
	write("time: ")
	writeUint((clockNanos - start) / 1000)
	write(" us\n")
}

func executeCommand(line []byte) {
	redirects, _ := shellcmd.SplitRedirections(line)
	stages, count, _ := shellcmd.SplitPipeline(redirects.Command)
	invocation, _ := shellcmd.ParseInvocation(stages[0], &argumentStorage)
	name := string(invocation.Arguments[0])
	switch {
	case count == 1 && name == "help" && invocation.Count == 1:
		write("builtins: help clear cd services handles caps log jobs kill reboot diag time\nexecutables: /system/bin\n")
		mark("SHELL_HELP_OK\n")
	case count == 1 && name == "log" && invocation.Count == 1:
		printLog()
	case count == 1 && name == "services" && invocation.Count == 1:
		printServices()
	case count == 1 && name == "jobs" && invocation.Count == 1:
		printJobs()
	case count == 1 && name == "kill":
		if invocation.Count != 2 {
			write("kill: expected one job\n")
		} else {
			killJob(invocation.Arguments[1])
		}
	case count == 1 && name == "reboot" && invocation.Count == 1:
		reboot()
	case count == 1 && name == "handles" && invocation.Count == 1:
		printHandles(false)
	case count == 1 && name == "caps" && invocation.Count == 1:
		printHandles(true)
	case count == 1 && name == "diag" && invocation.Count == 1:
		if diagnostics(uint64(consoleHandle), 0) != 0 {
			write("diag: unavailable in this build\n")
		}
	case count == 1 && name == "time":
		command, _ := shellcmd.SplitTime(line)
		executeTimed(command)
	case count == 1 && name == "clear" && invocation.Count == 1:
		write("\x1b[2J\x1b[H")
		mark("SHELL_CLEAR_OK\n")
	case count == 1 && name == "cd":
		if invocation.Count > 2 {
			write("cd: expected at most one path\n")
		} else if invocation.Count == 2 {
			executeCd(invocation.Arguments[1])
		} else {
			executeCd(nil)
		}
	default:
		executeExternal(line, false)
	}
}

// writeSink builds the isolated `write` stage that drains its input stream into
// one VFS path. It reports its own diagnostics and returns false on failure.
func writeSink(encodedTarget []byte, appendMode bool, index int, environment []startup.Variable) (execproto.Stage, bool) {
	target, parsed := shellcmd.ParseInvocation(encodedTarget, &argumentStorage)
	if !parsed || target.Count != 1 {
		write("exec: invalid redirection\n")
		return execproto.Stage{}, false
	}
	writePath, ok := execproto.ResolveExecutable([]byte("write"), &executableBuffers[index])
	if !ok {
		write("exec: invalid redirection\n")
		return execproto.Stage{}, false
	}
	arguments := [][]byte{[]byte("write"), target.Arguments[0]}
	if appendMode {
		arguments = [][]byte{[]byte("write"), []byte("--append"), target.Arguments[0]}
	}
	argumentData, _, ok := startup.EncodeVectors(arguments, environment, &vectorWires[index])
	if !ok {
		write("exec: redirection target too large\n")
		return execproto.Stage{}, false
	}
	return execproto.Stage{Path: writePath, Arguments: argumentData}, true
}

// interruptPoll bounds one receive so the shell can look for Ctrl-C while a
// foreground command runs. A command may run for as long as it likes; a dead
// PID 1 is reported as a closed channel.
const interruptPoll = clock.TicksPerSecond / 10

// interrupted reports whether Ctrl-C was typed. Other input during a command
// is discarded: the shell has no line discipline to buffer it.
func interrupted() bool {
	for {
		status := consoleRead(uint64(consoleHandle), unsafe.Pointer(&inputByte), 1)
		if status != 0 {
			return false
		}
		if inputByte == 3 {
			return true
		}
	}
}

func printJobs() {
	found := false
	for index := range jobs {
		if !jobs[index].active || !jobs[index].background {
			continue
		}
		found = true
		write("[")
		writeUint(uint64(index) + 1)
		write("] running  ")
		write(string(jobs[index].command[:jobs[index].commandSize]))
		write("\n")
	}
	if !found {
		write("no background jobs\n")
	}
	mark("SHELL_JOBS_OK\n")
}

// killJob stops one background job, named as N or %N.
func killJob(argument []byte) {
	if len(argument) != 0 && argument[0] == '%' {
		argument = argument[1:]
	}
	number, ok := parseUint(argument)
	if !ok || number == 0 || number > uint64(len(jobs)) {
		write("kill: invalid job\n")
		return
	}
	slot := number - 1
	if !jobs[slot].active || !jobs[slot].background {
		write("kill: no such job\n")
		return
	}
	reply, ok := controlExchange(execproto.ControlKill, uint8(slot))
	status, valid := execproto.DecodeControlStatus(reply)
	if !ok || !valid || status != execproto.ControlOK {
		write("kill: job could not be stopped\n")
		return
	}
	mark("SHELL_KILL_OK\n")
}

func executeExternal(line []byte, background bool) {
	redirects, validRedirect := shellcmd.SplitRedirections(line)
	if !validRedirect {
		write("exec: invalid redirection\n")
		return
	}
	stages, count, split := shellcmd.SplitPipeline(redirects.Command)
	if !split {
		write("exec: invalid pipeline\n")
		return
	}
	redirectCount := redirects.Stages()
	if count+redirectCount > execproto.MaxStages {
		write("exec: invalid redirection\n")
		return
	}
	correlation++
	environment := []startup.Variable{{Name: []byte("PATH"), Value: []byte(execproto.SearchRoot[:len(execproto.SearchRoot)-1])}}
	requestData := execproto.Request{Count: uint8(count + redirectCount), Background: background, CWD: cwd[:cwdSize]}
	stageOffset := 0
	if len(redirects.Input) != 0 {
		target, parsed := shellcmd.ParseInvocation(redirects.Input, &argumentStorage)
		readPath, ok := execproto.ResolveExecutable([]byte("cat"), &executableBuffers[0])
		if !parsed || target.Count != 1 || !ok {
			write("exec: invalid redirection\n")
			return
		}
		argumentData, _, ok := startup.EncodeVectors([][]byte{[]byte("cat"), target.Arguments[0]}, environment, &vectorWires[0])
		if !ok {
			write("exec: redirection target too large\n")
			return
		}
		requestData.Stages[0] = execproto.Stage{Path: readPath, Arguments: argumentData}
		stageOffset = 1
	}
	for index := 0; index < count; index++ {
		requestIndex := index + stageOffset
		invocation, parsed := shellcmd.ParseInvocation(stages[index], &argumentStorage)
		if !parsed {
			write("exec: invalid arguments\n")
			return
		}
		executable, ok := execproto.ResolveExecutable(invocation.Arguments[0], &executableBuffers[requestIndex])
		if !ok {
			write("exec: invalid command\n")
			return
		}
		argumentData, environmentData, ok := startup.EncodeVectors(invocation.Arguments[:invocation.Count], environment, &vectorWires[requestIndex])
		if !ok {
			write("exec: arguments too large\n")
			return
		}
		requestData.Stages[requestIndex] = execproto.Stage{Path: executable, Arguments: argumentData}
		if index == 0 {
			requestData.Environment = environmentData
		}
	}
	nextIndex := stageOffset + count
	if len(redirects.Output) != 0 {
		stage, ok := writeSink(redirects.Output, redirects.Append, nextIndex, environment)
		if !ok {
			return
		}
		requestData.Stages[nextIndex] = stage
		nextIndex++
	}
	if len(redirects.Error) != 0 {
		stage, ok := writeSink(redirects.Error, redirects.ErrorAppend, nextIndex, environment)
		if !ok {
			return
		}
		// The sink reads the stderr of the last user stage, not stdout.
		requestData.Stages[nextIndex] = stage
		requestData.ErrorSource = uint8(stageOffset + count)
	}
	slot, ok := freeSlot(background)
	if !ok {
		write("exec: too many background jobs\n")
		return
	}
	data, ok := execproto.EncodeRequest(requestData, &execPage[slot])
	if !ok {
		write("exec: command too large\n")
		return
	}
	request, ok := execproto.EncodeSubmit(slot, len(data), correlation)
	if !ok {
		write("exec: invalid request\n")
		return
	}
	if !sendExec(request) {
		return
	}
	jobs[slot] = job{active: true, background: background, correlation: correlation}
	jobs[slot].commandSize = copy(jobs[slot].command[:], line)
	if background {
		write("[")
		writeUint(uint64(slot) + 1)
		write("] started\n")
		return
	}
	message, received := awaitReply(correlation, int(slot))
	jobs[slot].active = false
	reply, validReply := execproto.DecodeReply(message)
	if !received {
		return
	}
	if !validReply {
		write("exec: corrupt reply\n")
		return
	}
	if reply.LaunchStatus != 0 {
		write("exec: launch failed: ")
		writeUint(reply.LaunchStatus)
		write("\n")
		return
	}
	if reply.ExitCode == kcall.ExitKilled {
		mark("SHELL_KILL_OK\n")
	} else if reply.ExitCode != 0 {
		write("exit status: ")
		writeUint(reply.ExitCode)
		write("\n")
		mark("EXIT_STATUS_PROPAGATION_OK\n")
	}
	mark("SHELL_EXEC_OK\n")
	if requestData.Count > 1 {
		mark("PIPELINE_EXEC_OK\n")
	}
	if redirectCount != 0 {
		mark("REDIRECTION_EXEC_OK\n")
	}
}

// freeSlot keeps one slot for the foreground, so background jobs never
// block an interactive command.
func freeSlot(background bool) (uint8, bool) {
	free, count := -1, 0
	for index := range jobs {
		if jobs[index].active {
			count++
		} else if free < 0 {
			free = index
		}
	}
	if free < 0 || background && count >= len(jobs)-1 {
		return 0, false
	}
	return uint8(free), true
}

func sendExec(request ipc.Message) bool {
	size, ok := ipc.Encode(request, wire[:])
	if !ok {
		write("exec: encode failed\n")
		return false
	}
	if status := ipcSend(uint64(execHandle), unsafe.Pointer(&wire[0]), uintptr(size)); status != 0 {
		write("exec: service unavailable\n")
		return false
	}
	return true
}

// awaitReply receives until the reply with correlation arrives, reporting
// background jobs that finish in the meantime. While a foreground job runs,
// Ctrl-C stops it; other input typed during the command is discarded.
func awaitReply(correlation uint64, foreground int) (ipc.Message, bool) {
	for {
		if foreground >= 0 && interrupted() {
			write("^C\n")
			controlExchange(execproto.ControlKill, uint8(foreground))
		}
		descriptor[0], descriptor[1], descriptor[2] = uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, interruptPoll
		status := ipcWaitReceive(uint64(execHandle), unsafe.Pointer(&descriptor[0]), uintptr(len(descriptor)*8))
		if status == uint64(ipc.StatusTimeout) {
			continue
		}
		if status != 0 {
			write("exec: service unavailable\n")
			return ipc.Message{}, false
		}
		message, valid := decodeWire()
		if !valid {
			write("exec: corrupt reply\n")
			return ipc.Message{}, false
		}
		if message.Correlation == correlation {
			return message, true
		}
		reportJob(message)
	}
}

// pollReplies reports finished background jobs without blocking.
func pollReplies() bool {
	reported := false
	for {
		active := false
		for index := range jobs {
			active = active || jobs[index].active
		}
		if !active || ipcReceive(uint64(execHandle), unsafe.Pointer(&wire[0]), ipc.WireMaxSize) != 0 {
			return reported
		}
		if message, valid := decodeWire(); valid {
			if !reported {
				write("\n")
			}
			reported = reportJob(message) || reported
		}
	}
}

func decodeWire() (ipc.Message, bool) {
	payloadSize := int(wire[24]) | int(wire[25])<<8
	size := ipc.WireHeaderSize + payloadSize + int(wire[26])*8
	if size > len(wire) {
		return ipc.Message{}, false
	}
	return ipc.Decode(wire[:size])
}

func reportJob(message ipc.Message) bool {
	for index := range jobs {
		if !jobs[index].active || !jobs[index].background || jobs[index].correlation != message.Correlation {
			continue
		}
		jobs[index].active = false
		write("[")
		writeUint(uint64(index) + 1)
		reply, valid := execproto.DecodeReply(message)
		switch {
		case !valid:
			write("] corrupt reply\n")
		case reply.LaunchStatus != 0:
			write("] launch failed: ")
			writeUint(reply.LaunchStatus)
			write("\n")
		case reply.ExitCode == kcall.ExitKilled:
			write("] killed\n")
		case reply.ExitCode != 0:
			write("] exit status: ")
			writeUint(reply.ExitCode)
			write("\n")
		default:
			write("] done\n")
		}
		return true
	}
	return false
}

var (
	logLine        [kcall.LogText]byte
	handleTypes    = [...]string{"invalid", "process", "task", "memory", "channel", "device", "irq", "stream"}
	rightNames     = [...]string{"read", "write", "execute", "transfer", "wait"}
	serviceStates  = [...]string{"registered", "blocked", "starting", "running", "stopped", "failed"}
	controlStatus  = [...]string{"", "storage sync failed", "reset refused", "request rejected"}
	serviceEntries [execproto.MaxServiceEntries]execproto.ServiceEntry
)

// controlExchange sends one control request to PID 1 and returns its reply.
func controlExchange(operation execproto.ControlOperation, argument uint8) (ipc.Message, bool) {
	correlation++
	request, ok := execproto.EncodeControl(operation, argument, correlation)
	if !ok {
		return ipc.Message{}, false
	}
	if !sendExec(request) {
		return ipc.Message{}, false
	}
	return awaitReply(correlation, -1)
}

func printServices() {
	reply, ok := controlExchange(execproto.ControlServices, 0)
	count, valid := execproto.DecodeServices(reply, &serviceEntries)
	if !ok || !valid {
		write("services: no reply\n")
		return
	}
	write("NAME        STATE       PID   RESTARTS\n")
	for _, entry := range serviceEntries[:count] {
		writePadded(entry.Name, 12)
		state := "unknown"
		if int(entry.State) < len(serviceStates) {
			state = serviceStates[entry.State]
		}
		writePadded([]byte(state), 12)
		writePaddedUint(uint64(entry.PID), 6)
		writeUint(uint64(entry.Restarts))
		write("\n")
	}
}

func reboot() {
	reply, ok := controlExchange(execproto.ControlReboot, 0)
	if !ok || reply.PayloadSize != 1 || int(reply.Payload[0]) >= len(controlStatus) || reply.Payload[0] == 0 {
		write("reboot: no reply\n")
		return
	}
	write("reboot: ")
	write(controlStatus[reply.Payload[0]])
	write("\n")
}

// printHandles lists the shell's own handle table. With names it shows each
// startup capability by the name it was granted under.
func printHandles(names bool) {
	if names {
		write("NAME      TYPE      RIGHTS\n")
	} else {
		write("HANDLE              TYPE      RIGHTS\n")
	}
	for slot := uint64(0); slot < handle.MaxHandles; slot++ {
		if handleInfo(slot, unsafe.Pointer(&handleRecord[0])) != 0 {
			continue
		}
		id := binary.LittleEndian.Uint64(handleRecord[0:8])
		typeID := handleRecord[8]
		rights := binary.LittleEndian.Uint32(handleRecord[12:16])
		if names {
			name := capabilityName(handle.ID(id))
			if name == nil {
				continue
			}
			writePadded(name, 10)
		} else {
			writeHex(id)
			write("  ")
		}
		typeName := "unknown"
		if int(typeID) < len(handleTypes) {
			typeName = handleTypes[typeID]
		}
		writePadded([]byte(typeName), 10)
		separator := ""
		for bit, right := range rightNames {
			if rights&(1<<bit) != 0 {
				write(separator)
				write(right)
				separator = ","
			}
		}
		write("\n")
	}
}

func capabilityName(id handle.ID) []byte {
	for index := 0; index < int(startupData.HandleCount); index++ {
		if startupData.Handles[index].ID == id {
			return startupData.Handles[index].Name
		}
	}
	return nil
}

func writePadded(value []byte, width int) {
	writeBytes(value)
	for column := len(value); column < width; column++ {
		write(" ")
	}
}

func writePaddedUint(value uint64, width int) {
	var digits [20]byte
	text := strconv.AppendUint(digits[:0], value, 10)
	writePadded(text, width)
}

func writeHex(value uint64) {
	var digits [18]byte
	digits[0], digits[1] = '0', 'x'
	for index := 0; index < 16; index++ {
		digits[17-index] = "0123456789abcdef"[value>>(uint(index)*4)&0xf]
	}
	writeBytes(digits[:])
}

// printLog prints the retained system log, oldest record first.
func printLog() {
	for index := uint64(0); ; index++ {
		if logRead(uint64(consoleHandle), index, unsafe.Pointer(&logLine[0])) != 0 {
			return
		}
		size := 0
		for size < len(logLine) && logLine[size] != 0 {
			size++
		}
		writeBytes(logLine[:size])
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
	mark("SHELL_CD_OK\n")
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
	if !ok || ipcSend(uint64(vfsHandle), unsafe.Pointer(&wire[0]), uintptr(size)) != 0 {
		return vfs.Reply{}, false
	}
	for {
		descriptor[0], descriptor[1], descriptor[2] = uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000
		status := ipcWaitReceive(uint64(vfsHandle), unsafe.Pointer(&descriptor[0]), uintptr(len(descriptor)*8))
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

func mark(message string) {
	if validation.Enabled {
		write(message)
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

// parseUint reads a bounded decimal job number.
func parseUint(value []byte) (uint64, bool) {
	if len(value) == 0 || len(value) > 3 {
		return 0, false
	}
	result := uint64(0)
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false
		}
		result = result*10 + uint64(character-'0')
	}
	return result, true
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
