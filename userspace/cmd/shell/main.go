//go:build goos_userspace

package main

import (
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
	cwd               = [shellcmd.MaxPath]byte{'/'}
	cwdSize           = 1
	pathBuffer        [shellcmd.MaxPath]byte
	executableBuffers [execproto.MaxStages][execproto.MaxPath]byte
	argumentStorage   [shellcmd.MaxArguments][shellcmd.MaxArgumentSize]byte
	vectorWires       [execproto.MaxStages][96]byte
)

//go:extern _heap_start
var heapStartSymbol [0]byte

//go:extern _heap_end
var heapEndSymbol [0]byte

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
	startupData, validStartup := startup.Decode(startupWire[:])
	var found bool
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
	arenaBase = heapPointer
	root, ok := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: cwd[:cwdSize]})
	if !ok || root.Status != vfs.StatusOK || root.Kind != ramfs.KindDirectory {
		fatal("SHELL_VFS_CONNECT_FAILED\n")
	}
	heapPointer = arenaBase
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

func executeCommand(line []byte) {
	pipeline, _, _, _, _ := shellcmd.SplitOutputRedirect(line)
	stages, count, _ := shellcmd.SplitPipeline(pipeline)
	invocation, _ := shellcmd.ParseInvocation(stages[0], &argumentStorage)
	name := string(invocation.Arguments[0])
	switch {
	case count == 1 && name == "help" && invocation.Count == 1:
		write("builtins: help clear cd\nexecutables: /system/bin\nSHELL_HELP_OK\n")
	case count == 1 && name == "clear" && invocation.Count == 1:
		write("\x1b[2J\x1b[H")
		write("SHELL_CLEAR_OK\n")
	case count == 1 && name == "cd":
		if invocation.Count > 2 {
			write("cd: expected at most one path\n")
		} else if invocation.Count == 2 {
			executeCd(invocation.Arguments[1])
		} else {
			executeCd(nil)
		}
	default:
		executeExternal(line)
	}
}

func executeExternal(line []byte) {
	pipeline, redirectTarget, redirected, appendMode, validRedirect := shellcmd.SplitOutputRedirect(line)
	if !validRedirect {
		write("exec: invalid redirection\n")
		return
	}
	stages, count, split := shellcmd.SplitPipeline(pipeline)
	if !split {
		write("exec: invalid pipeline\n")
		return
	}
	correlation++
	environment := []startup.Variable{{Name: []byte("PATH"), Value: []byte(execproto.SearchRoot[:len(execproto.SearchRoot)-1])}}
	requestData := execproto.Request{Count: uint8(count), CWD: cwd[:cwdSize]}
	for index := 0; index < count; index++ {
		invocation, parsed := shellcmd.ParseInvocation(stages[index], &argumentStorage)
		if !parsed {
			write("exec: invalid arguments\n")
			return
		}
		executable, ok := execproto.ResolveExecutable(invocation.Arguments[0], &executableBuffers[index])
		if !ok {
			write("exec: invalid command\n")
			return
		}
		argumentData, environmentData, ok := startup.EncodeVectors(invocation.Arguments[:invocation.Count], environment, &vectorWires[index])
		if !ok {
			write("exec: arguments too large\n")
			return
		}
		requestData.Stages[index] = execproto.Stage{Path: executable, Arguments: argumentData}
		if index == 0 {
			requestData.Environment = environmentData
		}
	}
	if redirected {
		target, parsed := shellcmd.ParseInvocation(redirectTarget, &argumentStorage)
		if !parsed || target.Count != 1 || count == execproto.MaxStages {
			write("exec: invalid redirection\n")
			return
		}
		writePath, ok := execproto.ResolveExecutable([]byte("write"), &executableBuffers[count])
		if !ok {
			write("exec: invalid redirection\n")
			return
		}
		arguments := [][]byte{[]byte("write"), target.Arguments[0]}
		if appendMode {
			arguments = [][]byte{[]byte("write"), []byte("--append"), target.Arguments[0]}
		}
		argumentData, _, ok := startup.EncodeVectors(arguments, environment, &vectorWires[count])
		if !ok {
			write("exec: redirection target too large\n")
			return
		}
		requestData.Stages[count] = execproto.Stage{Path: writePath, Arguments: argumentData}
		count++
		requestData.Count = uint8(count)
	}
	request, ok := execproto.EncodeRequest(requestData, correlation)
	if !ok {
		write("exec: invalid request\n")
		return
	}
	size, ok := ipc.Encode(request, wire[:])
	if !ok {
		write("exec: encode failed\n")
		return
	}
	if status := ipcSend(uint64(execHandle), unsafe.Pointer(&wire[0]), uintptr(size)); status != 0 {
		write("exec: service unavailable\n")
		write("exec status: ")
		writeUint(status)
		write("\n")
		return
	}
	for {
		descriptor[0], descriptor[1], descriptor[2] = uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000
		status := ipcWaitReceive(uint64(execHandle), unsafe.Pointer(&descriptor[0]), uintptr(len(descriptor)*8))
		if status == uint64(ipc.StatusTimeout) {
			continue
		}
		if status != 0 {
			write("exec: service unavailable\n")
			return
		}
		payloadSize := int(wire[24]) | int(wire[25])<<8
		wireSize := ipc.WireHeaderSize + payloadSize + int(wire[26])*8
		message, valid := ipc.Decode(wire[:wireSize])
		reply, validReply := execproto.DecodeReply(message)
		if !valid || !validReply || message.Correlation != correlation {
			write("exec: corrupt reply\n")
			return
		}
		if reply.LaunchStatus != 0 {
			write("exec: launch failed: ")
			writeUint(reply.LaunchStatus)
			write("\n")
			return
		}
		if reply.ExitCode != 0 {
			write("exit status: ")
			writeUint(reply.ExitCode)
			write("\n")
			write("EXIT_STATUS_PROPAGATION_OK\n")
		}
		write("SHELL_EXEC_OK\n")
		if count > 1 {
			write("PIPELINE_EXEC_OK\n")
		}
		if redirected {
			write("REDIRECTION_EXEC_OK\n")
		}
		return
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
	write("SHELL_CD_OK\n")
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
