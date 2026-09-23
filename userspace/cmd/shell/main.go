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
	"goos/userspace/config"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	shellcmd "goos/userspace/shell"
	"goos/userspace/startup"
	userwait "goos/userspace/wait"
	"runtime"
	"strconv"
	"unsafe"
)

var (
	consoleHandle handle.ID
	vfsHandle     handle.ID
	execHandle    handle.ID
	configHandle  handle.ID
	configWire    [ipc.WireMaxSize]byte
	// prompt is read from the configuration service rather than compiled in.
	// It falls back to the built-in text when there is no session, because the
	// shell has to come up whether or not configuration is available.
	promptText        = [config.MaxText]byte{'g', 'o', 'o', 's', '>', ' '}
	promptSize        = 6
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
	// requestSize is the length of the request left in the job's exec page
	// slot; a launch failure is described from it.
	requestSize int
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

//go:linkname consoleInterrupt goos_console_interrupt
func consoleInterrupt(device uint64) uint64

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
	// The configuration session is optional: the service warns and continues if
	// it cannot start, and the shell must still come up without it.
	configHandle, _ = startupData.Handle("config")
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
	loadPrompt()
	write("\n")
	prompt()
	for {
		// Ctrl-C reaches the shell as an interrupt rather than as a byte, so
		// asking for it takes nothing away from a command that is reading.
		if consoleInterrupt(uint64(consoleHandle)) == 0 {
			write("^C\n")
			editor.Reset()
			prompt()
			continue
		}
		status := consoleRead(uint64(consoleHandle), unsafe.Pointer(&inputByte), 1)
		if status == uint64(kcall.StatusNotFound) {
			if pollReplies() {
				prompt()
				writeBytes(editor.Pending())
			}
			awaitInput()
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
		heapPointer = arenaBase
		editor.Reset()
		prompt()
	}
}

func prompt() { writeBytes(promptText[:promptSize]) }

// loadPrompt asks the configuration service what the prompt is. A shell that
// cannot reach the service, or a value it will not accept, leaves the built-in
// text in place: configuration decides what the prompt is, not whether the
// shell starts.
func loadPrompt() {
	if configHandle == 0 {
		return
	}
	reply, ok := callConfig(config.Request{Operation: config.OperationGet, Key: []byte("system.shell.prompt")})
	if !ok || reply.Status != config.StatusOK || reply.Value.Kind != config.KindText || reply.Value.Size == 0 {
		return
	}
	promptSize = copy(promptText[:], reply.Value.Bytes())
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
	if count != 1 || !shellcmd.IsBuiltin(invocation.Arguments[0]) {
		executeExternal(line, false)
		return
	}
	// A builtin with the wrong arguments is answered with its usage here. It
	// used to fall through to the launch path and fail as a missing executable.
	info, _ := shellcmd.LookupCommand(invocation.Arguments[0])
	arguments := int(invocation.Count) - 1
	if arguments == 1 && shellcmd.IsHelpFlag(invocation.Arguments[1]) {
		writeCommandHelp(info)
		return
	}
	if !info.Accepts(arguments) {
		write("usage: ")
		write(info.Usage)
		write("\n")
		return
	}
	switch info.Name {
	case "help":
		printHelp(invocation)
	case "log":
		printLog()
	case "config":
		runConfig(invocation)
	case "services":
		printServices()
	case "jobs":
		printJobs()
	case "kill":
		killJob(invocation.Arguments[1])
	case "reboot":
		reboot()
	case "handles":
		printHandles(false)
	case "caps":
		printHandles(true)
	case "diag":
		if diagnostics(uint64(consoleHandle), 0) != 0 {
			write("diag: unavailable in this build\n")
		}
	case "time":
		command, _ := shellcmd.SplitTime(line)
		executeTimed(command)
	case "clear":
		write("\x1b[2J\x1b[H")
		mark("SHELL_CLEAR_OK\n")
	case "cd":
		if invocation.Count == 2 {
			executeCd(invocation.Arguments[1])
		} else {
			executeCd(nil)
		}
	}
}

// printHelp lists every documented command, or describes the one named.
func printHelp(invocation shellcmd.Invocation) {
	if invocation.Count == 2 {
		info, found := shellcmd.LookupCommand(invocation.Arguments[1])
		if !found {
			write("help: no such command: ")
			writeBytes(invocation.Arguments[1])
			write("\n")
			return
		}
		writeCommandHelp(info)
		mark("SHELL_HELP_COMMAND_OK\n")
		return
	}
	write("Shell builtins:\n")
	writeCommandList(true)
	write("Commands in /system/bin:\n")
	writeCommandList(false)
	write("Run 'help COMMAND' or 'COMMAND --help' for usage.\n")
	mark("SHELL_HELP_OK\n")
}

func writeCommandList(builtin bool) {
	for _, info := range shellcmd.Commands {
		if info.Builtin != builtin {
			continue
		}
		write("  ")
		writePadded([]byte(info.Name), 10)
		write(info.Summary)
		write("\n")
	}
}

func writeCommandHelp(info shellcmd.CommandInfo) {
	write("usage: ")
	write(info.Usage)
	write("\n  ")
	write(info.Summary)
	write("\n")
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

// awaitInput blocks until a key arrives or PID 1 answers. Retrying the read
// immediately is what made the shell hold a CPU for as long as it waited for a
// prompt to be typed at; both things it waits for are waitable objects, so it
// waits on them.
func awaitInput() {
	var interests [userwait.MaxObjects]userwait.Interest
	interests[0] = userwait.Interest{Handle: consoleHandle, Events: kcall.WaitReadable}
	interests[1] = userwait.Interest{Handle: execHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
	if _, status := userwait.Any(&interests, 2, kcall.WaitAnyInfiniteTimeout); status != kcall.StatusOK {
		// A wait that cannot be established must not become a spin. Sleeping a
		// tick degrades to slow polling, which is visible in the CPU column
		// rather than hidden in a loop.
		userwait.Sleep(1)
	}
}

// interruptPoll bounds one receive so the shell can look for Ctrl-C while a
// foreground command runs. A command may run for as long as it likes; a dead
// PID 1 is reported as a closed channel.
const interruptPoll = clock.TicksPerSecond / 10

// interrupted reports whether Ctrl-C was typed. It consumes only the interrupt:
// the bytes a foreground command is being typed are left for that command,
// which is what the shell reading and discarding them used to take away.
func interrupted() bool {
	return consoleInterrupt(uint64(consoleHandle)) == 0
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
			writeBytes(invocation.Arguments[0])
			write(": not a command name; commands are run by name from /system/bin\n")
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
	jobs[slot] = job{active: true, background: background, correlation: correlation, requestSize: len(data)}
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
		writeLaunchFailure(int(slot), reply)
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
// writeLaunchFailure names the stage PID 1 could not start. The request is
// still in the job's exec page slot, so the name is read back from it rather
// than kept for every job.
func writeLaunchFailure(slot int, reply execproto.Reply) {
	status := supervisor.LaunchStatus(reply.LaunchStatus)
	var name []byte
	if request, ok := execproto.DecodeRequest(execPage[slot][:jobs[slot].requestSize]); ok && reply.Stage < request.Count {
		name, _ = execproto.CommandFromSystemPath(request.Stages[reply.Stage].Path)
	}
	if len(name) != 0 && status.NamesExecutable() {
		writeBytes(name)
		write(": ")
	} else if len(name) != 0 {
		write("exec: cannot start ")
		writeBytes(name)
		write(": ")
	} else {
		write("exec: cannot start: ")
	}
	write(status.Reason())
	write("\n")
}

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
			write("] ")
			writeLaunchFailure(index, reply)
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

// argumentIs compares an argument against a literal without building a string,
// which would allocate on this runtime's bump heap for every comparison.
func argumentIs(argument []byte, expected string) bool {
	if len(argument) != len(expected) {
		return false
	}
	for index := range argument {
		if argument[index] != expected[index] {
			return false
		}
	}
	return true
}

// runConfig is a builtin because it administers this session rather than
// processing data: the configuration session belongs to the shell, and an
// external utility would need one of its own.
func runConfig(invocation shellcmd.Invocation) {
	if configHandle == 0 {
		write("config: service unavailable\n")
		return
	}
	switch {
	case invocation.Count == 1, invocation.Count == 2 && argumentIs(invocation.Arguments[1], "list"):
		listConfig()
	case invocation.Count == 3 && argumentIs(invocation.Arguments[1], "get"):
		getConfig(invocation.Arguments[2])
	case invocation.Count == 4 && argumentIs(invocation.Arguments[1], "set"):
		setConfig(invocation.Arguments[2], invocation.Arguments[3])
	default:
		write("usage: config [list] | config get KEY | config set KEY VALUE\n")
	}
}

func listConfig() {
	for cursor := uint32(0); ; {
		reply, ok := callConfig(config.Request{Operation: config.OperationList, Cursor: cursor})
		if !ok {
			write("config: service unavailable\n")
			return
		}
		if reply.Status == config.StatusNotFound {
			mark("CONFIG_LIST_OK\n")
			return
		}
		if reply.Status != config.StatusOK || reply.Cursor <= cursor {
			writeConfigStatus(reply.Status)
			return
		}
		writeBytes(reply.Key)
		write(" ")
		writeConfigValue(reply.Value)
		if reply.ReadOnly {
			write(" (read-only)")
		}
		write("\n")
		cursor = reply.Cursor
	}
}

func getConfig(key []byte) {
	reply, ok := callConfig(config.Request{Operation: config.OperationGet, Key: key})
	if !ok {
		write("config: service unavailable\n")
		return
	}
	if reply.Status != config.StatusOK {
		writeConfigStatus(reply.Status)
		return
	}
	writeConfigValue(reply.Value)
	write("\n")
	mark("CONFIG_GET_OK\n")
}

// setConfig reads the declared type first and parses the argument against it,
// so a value is converted by what the key is rather than by how the text looks.
func setConfig(key []byte, text []byte) {
	current, ok := callConfig(config.Request{Operation: config.OperationGet, Key: key})
	if !ok {
		write("config: service unavailable\n")
		return
	}
	if current.Status != config.StatusOK {
		writeConfigStatus(current.Status)
		return
	}
	value, parsed := parseConfigValue(current.Value.Kind, text)
	if !parsed {
		write("config: value does not match the declared type\n")
		return
	}
	reply, ok := callConfig(config.Request{Operation: config.OperationSet, Key: key, Value: value})
	if !ok {
		write("config: service unavailable\n")
		return
	}
	if reply.Status != config.StatusOK {
		writeConfigStatus(reply.Status)
		return
	}
	// The shell owns this setting, so a change to it applies now rather than at
	// the next boot.
	loadPrompt()
	mark("CONFIG_SET_OK\n")
}

func parseConfigValue(kind config.Kind, text []byte) (config.Value, bool) {
	switch kind {
	case config.KindBool:
		if argumentIs(text, "true") {
			return config.Bool(true), true
		}
		if argumentIs(text, "false") {
			return config.Bool(false), true
		}
		return config.Value{}, false
	case config.KindUint:
		number := uint64(0)
		if len(text) == 0 || len(text) > 20 {
			return config.Value{}, false
		}
		for _, character := range text {
			if character < '0' || character > '9' {
				return config.Value{}, false
			}
			number = number*10 + uint64(character-'0')
		}
		return config.Uint(number), true
	case config.KindText:
		return config.Text(text)
	default:
		return config.Value{}, false
	}
}

func writeConfigValue(value config.Value) {
	switch value.Kind {
	case config.KindBool:
		if value.Uint == 1 {
			write("true")
		} else {
			write("false")
		}
	case config.KindUint:
		writeUint(value.Uint)
	case config.KindText:
		writeBytes(value.Bytes())
	default:
		write("?")
	}
}

func writeConfigStatus(status config.Status) {
	switch status {
	case config.StatusNotFound:
		write("config: no such setting\n")
	case config.StatusReadOnly:
		write("config: setting is read-only\n")
	case config.StatusTypeMismatch:
		write("config: value does not match the declared type\n")
	case config.StatusPermissionDenied:
		write("config: not permitted in this session\n")
	default:
		write("config: request refused\n")
	}
}

func callConfig(request config.Request) (config.Reply, bool) {
	correlation++
	message, ok := config.EncodeRequest(request, correlation)
	if !ok {
		return config.Reply{}, false
	}
	size, ok := ipc.Encode(message, configWire[:])
	if !ok || ipcSend(uint64(configHandle), unsafe.Pointer(&configWire[0]), uintptr(size)) != 0 {
		return config.Reply{}, false
	}
	configDescriptor := [3]uint64{uint64(uintptr(unsafe.Pointer(&configWire[0]))), ipc.WireMaxSize, 100000}
	if ipcWaitReceive(uint64(configHandle), unsafe.Pointer(&configDescriptor[0]), uintptr(len(configDescriptor)*8)) != 0 {
		return config.Reply{}, false
	}
	payloadSize := int(configWire[24]) | int(configWire[25])<<8
	received, valid := ipc.Decode(configWire[:ipc.WireHeaderSize+payloadSize+int(configWire[26])*8])
	if !valid || received.Correlation != correlation {
		return config.Reply{}, false
	}
	reply, valid := config.DecodeReply(received)
	return reply, valid && reply.Operation == request.Operation
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
		writeCdError(argument, "invalid path")
		return
	}
	reply, received := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
	if !received {
		writeCdError(path, "filesystem service unavailable")
		return
	}
	if reply.Status != vfs.StatusOK {
		writeCdError(path, shellcmd.VFSReason(reply.Status, false))
		return
	}
	if reply.Kind != ramfs.KindDirectory {
		writeCdError(path, "not a directory")
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

func writeCdError(path []byte, reason string) {
	write("cd: ")
	writeBytes(path)
	write(": ")
	write(reason)
	write("\n")
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
