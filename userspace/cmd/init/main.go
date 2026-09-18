//go:build goos_userspace

package main

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/vfs"
	blockproto "goos/userspace/block"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	"goos/userspace/startup"
	"runtime"
	"unsafe"
)

const lifecycleHandle = uint64(0x0000000100000001)
const serviceDeviceHandle = handle.ID(0x0000000100000002)
const vfsChannelHandle = handle.ID(0x0000000100000003)
const vfsServerHandle = handle.ID(0x0000000100000004)
const vfsControlHandle = uint64(0x0000000100000005)
const vfsControlServerHandle = handle.ID(0x0000000100000006)
const execServerHandle = uint64(0x0000000100000007)
const execClientHandle = handle.ID(0x0000000100000008)
const blockClientHandle = uint64(0x0000000100000009)
const blockServerHandle = handle.ID(0x000000010000000a)
const virtioPortHandle = handle.ID(0x000000010000000b)
const virtioIRQHandle = handle.ID(0x000000010000000c)
const virtioDMAHandle = handle.ID(0x000000010000000d)
const driverClientHandle = handle.ID(0x000000010000000e)
const driverServerHandle = handle.ID(0x000000010000000f)
const filesystemBlockClientHandle = handle.ID(0x0000000100000010)
const filesystemBlockServerHandle = handle.ID(0x0000000100000011)

var (
	manager              supervisor.Manager
	wire                 [ipc.WireMaxSize]byte
	descriptor           = [3]uint64{uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000}
	clock                uint64
	correlation          uint64
	heapPointer          uintptr
	arenaBase            uintptr
	supervisionComplete  bool
	processWaitAnnounced bool
	lifecyclePending     [16]ipc.Message
	lifecycleHead        uint8
	lifecycleCount       uint8
	streamPair           [2]uint64
	streamDescriptor     [3]uint64
	streamReadback       [16]byte
)

//go:extern _heap_start
var heapStartSymbol [0]byte

//go:extern _heap_end
var heapEndSymbol [0]byte

//go:linkname debugWrite goos_debug_write
func debugWrite(address unsafe.Pointer, size uintptr) uint64

//go:linkname ipcSend goos_ipc_send
func ipcSend(channel uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname ipcWaitReceive goos_ipc_wait_receive
func ipcWaitReceive(channel uint64, descriptor unsafe.Pointer, size uintptr) uint64

//go:linkname ipcReceive goos_ipc_receive
func ipcReceive(channel uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname channelCreate goos_channel_create
func channelCreate(address unsafe.Pointer, size uintptr) uint64

//go:linkname handleClose goos_handle_close
func handleClose(id uint64) uint64

//go:linkname processWait goos_process_wait
func processWait(process uint64, result unsafe.Pointer, timeout uint64) uint64

//go:linkname streamCreate goos_stream_create
func streamCreate(address unsafe.Pointer, size uintptr) uint64

//go:linkname streamRead goos_stream_read
func streamRead(stream uint64, descriptor unsafe.Pointer, size uintptr) uint64

//go:linkname streamWrite goos_stream_write
func streamWrite(stream uint64, descriptor unsafe.Pointer, size uintptr) uint64

//export userspaceMain
func userspaceMain() {
	runtimeInitHeap()
	if !probeStreamEndpoint() {
		fatal("STREAM_ENDPOINT_FAILED\n")
	}
	write("STREAM_ENDPOINT_OK\n")
	if manager.Register(supervisor.Definition{Name: "fault-probe", Executable: "/system/fault-probe",
		Capabilities: []supervisor.Capability{{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write}}, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK ||
		manager.StartReady(0, bootstrapService) != 1 {
		fatal("INIT_POLICY_SETUP_FAILED\n")
	}
	write("INIT_PROCESS_OK\n")
	write("INIT_LIFECYCLE_IPC_OK\n")
	for {
		if supervisionComplete {
			pollExec()
		}
		timeout := uint64(100000)
		if supervisionComplete {
			timeout = 1
		}
		message, status := receiveLifecycle(timeout)
		if status == ipc.StatusTimeout {
			continue
		}
		if status != ipc.StatusOK {
			fatal("INIT_LIFECYCLE_FAILED\n")
		}
		event, valid := supervisor.DecodeLifecycle(message)
		if !valid || event.Operation != supervisor.LifecycleProcessExited {
			continue
		}
		clock += supervisor.RestartBackoff
		if manager.Exit(event.Process, event.Code, clock) == supervisor.StatusNotFound {
			continue
		}
		manager.StartReady(clock+supervisor.RestartBackoff, launchService)
		snapshot, found := manager.Snapshot("fault-probe")
		if found && snapshot.State == supervisor.StateFailed && !supervisionComplete {
			if manager.Register(supervisor.Definition{Name: "vfs", Executable: "/system/vfs",
				Capabilities: []supervisor.Capability{
					{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
					{Name: "session", Handle: vfsServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
					{Name: "control", Handle: vfsControlServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
					{Name: "block", Handle: filesystemBlockClientHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
				}, Restart: supervisor.RestartNever}) != supervisor.StatusOK ||
				manager.Register(supervisor.Definition{Name: "block", Executable: "/system/block",
					Capabilities: []supervisor.Capability{
						{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
						{Name: "block", Handle: blockServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
						{Name: "driver", Handle: driverClientHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
						{Name: "filesystem", Handle: filesystemBlockServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
					}, Restart: supervisor.RestartNever}) != supervisor.StatusOK ||
				manager.Register(supervisor.Definition{Name: "virtio-blk", Executable: "/system/virtio-blk",
					Capabilities: []supervisor.Capability{
						{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
						{Name: "ports", Handle: virtioPortHandle, Type: handle.TypeDevice, Rights: capability.Read | capability.Write, Consume: true},
						{Name: "irq", Handle: virtioIRQHandle, Type: handle.TypeIRQ, Rights: capability.Wait, Consume: true},
						{Name: "dma", Handle: virtioDMAHandle, Type: handle.TypeDevice, Rights: capability.Write, Consume: true},
						{Name: "service", Handle: driverServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
					}, Restart: supervisor.RestartNever}) != supervisor.StatusOK ||
				manager.Register(supervisor.Definition{Name: "shell", Executable: "/system/shell", Dependencies: []string{"vfs"},
					Capabilities: []supervisor.Capability{
						{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Read | capability.Write},
						{Name: "vfs", Handle: vfsChannelHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
						{Name: "exec", Handle: execClientHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
					}, Restart: supervisor.RestartNever}) != supervisor.StatusOK ||
				manager.StartReady(clock+supervisor.RestartBackoff, launchService) != 4 || !probeBlockService() {
				fatal("SHELL_LAUNCH_FAILED\n")
			}
			supervisionComplete = true
			arenaBase = heapPointer
			write("USERSPACE_SUPERVISOR_OK\n")
		}
	}
}

func probeStreamEndpoint() bool {
	if streamCreate(unsafe.Pointer(&streamPair[0]), 16) != 0 {
		return false
	}
	payload := "bounded-stream"
	streamDescriptor[0] = uint64(uintptr(unsafe.Pointer(unsafe.StringData(payload))))
	streamDescriptor[1], streamDescriptor[2] = uint64(len(payload)), 0
	if streamWrite(streamPair[1], unsafe.Pointer(&streamDescriptor[0]), 24) != 0 || streamDescriptor[2] != uint64(len(payload)) {
		return false
	}
	streamDescriptor[0] = uint64(uintptr(unsafe.Pointer(&streamReadback[0])))
	streamDescriptor[1], streamDescriptor[2] = uint64(len(streamReadback)), 0
	if streamRead(streamPair[0], unsafe.Pointer(&streamDescriptor[0]), 24) != 0 || streamDescriptor[2] != uint64(len(payload)) || !equalBytes(streamReadback[:len(payload)], payload) {
		return false
	}
	if handleClose(streamPair[1]) != 0 {
		return false
	}
	streamDescriptor[2] = 0
	if streamRead(streamPair[0], unsafe.Pointer(&streamDescriptor[0]), 24) != uint64(ipc.StatusPeerDied) {
		return false
	}
	return handleClose(streamPair[0]) == 0
}

func probeBlockService() bool {
	correlation++
	info, ok := blockproto.EncodeRequest(blockproto.Request{Operation: blockproto.OperationInfo}, correlation)
	if !ok || !sendMessage(blockClientHandle, info) {
		return false
	}
	message, status := receive(blockClientHandle, 1000)
	infoReply, valid := blockproto.DecodeReply(message)
	if status != ipc.StatusOK || !valid || infoReply.Status != blockproto.StatusOK || infoReply.Capacity != 64*1024*1024 || infoReply.BlockSize != 512 {
		return false
	}
	correlation++
	writeRequest, ok := blockproto.EncodeRequest(blockproto.Request{Operation: blockproto.OperationWrite, Offset: 4093, Length: 6, Data: []byte("GBLKIO")}, correlation)
	if !ok || !sendMessage(blockClientHandle, writeRequest) {
		return false
	}
	message, status = receive(blockClientHandle, 1000)
	writeReply, valid := blockproto.DecodeReply(message)
	if status != ipc.StatusOK || !valid || writeReply.Status != blockproto.StatusOK {
		return false
	}
	correlation++
	readRequest, ok := blockproto.EncodeRequest(blockproto.Request{Operation: blockproto.OperationRead, Offset: 4093, Length: 6}, correlation)
	if !ok || !sendMessage(blockClientHandle, readRequest) {
		return false
	}
	message, status = receive(blockClientHandle, 1000)
	readReply, valid := blockproto.DecodeReply(message)
	if status != ipc.StatusOK || !valid || readReply.Status != blockproto.StatusOK || !equalBytes(readReply.Data, "GBLKIO") {
		return false
	}
	correlation++
	flushRequest, ok := blockproto.EncodeRequest(blockproto.Request{Operation: blockproto.OperationFlush}, correlation)
	if !ok || !sendMessage(blockClientHandle, flushRequest) {
		return false
	}
	message, status = receive(blockClientHandle, 1000)
	flushReply, valid := blockproto.DecodeReply(message)
	if status != ipc.StatusOK || !valid || flushReply.Status != blockproto.StatusOK {
		return false
	}
	write("BLOCK_SERVICE_IO_OK\n")
	return true
}

func equalBytes(value []byte, expected string) bool {
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

func bootstrapService(supervisor.LaunchSpec) (uint64, bool) { return 3, true }

func launchService(spec supervisor.LaunchSpec) (uint64, bool) {
	correlation++
	var startupWire [startup.Size]byte
	var vectorWire [96]byte
	var bindings [supervisor.MaxCapabilities]startup.Handle
	for index := 0; index < int(spec.CapabilityCount); index++ {
		bindings[index].Name = []byte(spec.Capabilities[index].Name)
	}
	arguments, environment, ok := startup.EncodeVectors([][]byte{spec.Name[:spec.NameSize]}, nil, &vectorWire)
	if !ok {
		return 0, false
	}
	startupData, ok := startup.Encode(spec.Name[:spec.NameSize], []byte("/"), arguments, environment, bindings[:spec.CapabilityCount], &startupWire)
	if !ok {
		return 0, false
	}
	request, ok := supervisor.LaunchRequestWithStartup(1, bytesString(spec.Executable[:spec.ExecutableSize]), spec.Capabilities[:spec.CapabilityCount], startupData, correlation)
	if !ok {
		return 0, false
	}
	size, ok := ipc.Encode(request, wire[:])
	if !ok || ipcSend(lifecycleHandle, unsafe.Pointer(&wire[0]), uintptr(size)) != 0 {
		return 0, false
	}
	write("INIT_SERVICE_LAUNCH_OK\n")
	event, status := receiveLaunchResult(correlation)
	if status != ipc.StatusOK || event.Code != uint64(supervisor.LaunchOK) {
		return 0, false
	}
	if handleClose(uint64(event.ProcessHandle)) != 0 {
		return 0, false
	}
	write("INIT_SERVICE_STARTED_OK\n")
	return event.Process, true
}

func receiveLifecycle(timeout uint64) (ipc.Message, ipc.Status) {
	if lifecycleCount != 0 {
		message := lifecyclePending[lifecycleHead]
		lifecyclePending[lifecycleHead] = ipc.Message{}
		lifecycleHead = (lifecycleHead + 1) % uint8(len(lifecyclePending))
		lifecycleCount--
		return message, ipc.StatusOK
	}
	return receive(lifecycleHandle, timeout)
}

func receiveLaunchResult(expectedCorrelation uint64) (supervisor.LifecycleEvent, ipc.Status) {
	for {
		message, status := receive(lifecycleHandle, 100000)
		if status != ipc.StatusOK {
			return supervisor.LifecycleEvent{}, status
		}
		event, valid := supervisor.DecodeLifecycle(message)
		if valid && event.Operation == supervisor.LifecycleLaunchResult && message.Correlation == expectedCorrelation {
			return event, ipc.StatusOK
		}
		if !valid || event.Operation != supervisor.LifecycleProcessExited || lifecycleCount == uint8(len(lifecyclePending)) {
			return supervisor.LifecycleEvent{}, ipc.StatusInvalidArgument
		}
		tail := (int(lifecycleHead) + int(lifecycleCount)) % len(lifecyclePending)
		lifecyclePending[tail] = message
		lifecycleCount++
	}
}

func receive(channel uint64, timeout uint64) (ipc.Message, ipc.Status) {
	for {
		descriptor[0] = uint64(uintptr(unsafe.Pointer(&wire[0])))
		descriptor[1] = ipc.WireMaxSize
		descriptor[2] = timeout
		status := ipcWaitReceive(channel, unsafe.Pointer(&descriptor[0]), uintptr(len(descriptor)*8))
		if status == uint64(ipc.StatusTimeout) {
			return ipc.Message{}, ipc.StatusTimeout
		}
		if status != 0 {
			return ipc.Message{}, ipc.Status(status)
		}
		break
	}
	payloadSize := int(wire[24]) | int(wire[25])<<8
	size := ipc.WireHeaderSize + payloadSize + int(wire[26])*8
	if size < ipc.WireHeaderSize || size > len(wire) {
		return ipc.Message{}, ipc.StatusInvalidArgument
	}
	message, ok := ipc.Decode(wire[:size])
	if !ok {
		return ipc.Message{}, ipc.StatusInvalidArgument
	}
	return message, ipc.StatusOK
}

func pollExec() {
	heapPointer = arenaBase
	status := ipc.Status(ipcReceive(execServerHandle, unsafe.Pointer(&wire[0]), ipc.WireMaxSize))
	if status == ipc.StatusNotFound {
		return
	}
	if status != ipc.StatusOK {
		fatal("INIT_EXEC_RECEIVE_FAILED\n")
	}
	payloadSize := int(wire[24]) | int(wire[25])<<8
	size := ipc.WireHeaderSize + payloadSize + int(wire[26])*8
	message, ok := ipc.Decode(wire[:size])
	if !ok {
		fatal("INIT_EXEC_CORRUPT\n")
	}
	request, ok := execproto.DecodeRequest(message)
	if !ok {
		sendExecReply(message.Correlation, 0, uint64(supervisor.LaunchInvalidImage), 0)
		return
	}
	// Own every invocation before launch/VFS operations reuse the IPC buffer.
	var paths [execproto.MaxStages][execproto.MaxPath]byte
	var arguments [execproto.MaxStages][128]byte
	var cwd [execproto.MaxPath]byte
	var environment [96]byte
	request.CWD = cwd[:copy(cwd[:], request.CWD)]
	request.Environment = environment[:copy(environment[:], request.Environment)]
	for index := 0; index < int(request.Count); index++ {
		stage := &request.Stages[index]
		stage.Path = paths[index][:copy(paths[index][:], stage.Path)]
		stage.Arguments = arguments[index][:copy(arguments[index][:], stage.Arguments)]
	}
	pid, launchStatus, exitCode := supervisor.RunPipeline(request, serviceDeviceHandle, nativePipelineRunner{})
	sendExecReply(message.Correlation, pid, uint64(launchStatus), exitCode)
}

type nativePipelineRunner struct{}

func (nativePipelineRunner) CreateStream() (handle.ID, handle.ID, bool) {
	var pair [2]uint64
	if streamCreate(unsafe.Pointer(&pair[0]), 16) != 0 {
		return 0, 0, false
	}
	return handle.ID(pair[0]), handle.ID(pair[1]), true
}

func (nativePipelineRunner) Close(id handle.ID) bool { return handleClose(uint64(id)) == 0 }

func (nativePipelineRunner) Launch(stage execproto.Stage, cwd, environment []byte, input handle.ID, inputType handle.Type, output handle.ID, outputType handle.Type) (uint64, handle.ID, supervisor.LaunchStatus) {
	command, allowed := execproto.CommandFromSystemPath(stage.Path)
	if !allowed {
		return 0, 0, supervisor.LaunchCapabilityDenied
	}
	return launchApplication(bytesString(stage.Path), command, cwd, stage.Arguments, environment, input, inputType, output, outputType)
}

func (nativePipelineRunner) Wait(process handle.ID) uint64 { return waitApplicationExit(process) }

func launchApplication(path string, command, cwd, arguments, environment []byte, input handle.ID, inputType handle.Type, output handle.ID, outputType handle.Type) (uint64, handle.ID, supervisor.LaunchStatus) {
	var pair [2]uint64
	useVFS := commandNeedsVFS(command)
	if useVFS && channelCreate(unsafe.Pointer(&pair[0]), 16) != 0 {
		write("APP_CHANNEL_CREATE_FAILED\n")
		return 0, 0, supervisor.LaunchResourceExhausted
	}
	if useVFS {
		correlation++
	}
	control, ok := vfs.AddSessionRequest(handle.ID(pair[1]), correlation)
	if useVFS && (!ok || !sendMessage(vfsControlHandle, control)) {
		write("APP_VFS_REQUEST_FAILED\n")
		closePair(pair)
		return 0, 0, supervisor.LaunchCapabilityInstallFailed
	}
	var status ipc.Status
	vfsStatus, valid := vfs.StatusOK, true
	if useVFS {
		controlReply, received := receive(vfsControlHandle, 1000)
		status = received
		vfsStatus, valid = vfs.DecodeAddSessionReply(controlReply)
	}
	if useVFS && (status != ipc.StatusOK || !valid || vfsStatus != vfs.StatusOK) {
		write("APP_VFS_SESSION_FAILED\n")
		if status != ipc.StatusOK {
			write("APP_VFS_RECEIVE_STATUS\n")
		} else if !valid {
			write("APP_VFS_REPLY_INVALID\n")
		} else {
			write("APP_VFS_RESOURCE_EXHAUSTED\n")
		}
		handleClose(pair[0])
		return 0, 0, supervisor.LaunchCapabilityInstallFailed
	}
	var startupWire [startup.Size]byte
	bindings := []startup.Handle{{Name: []byte("in")}, {Name: []byte("stdout")}, {Name: []byte("stderr")}}
	if useVFS {
		bindings = append(bindings, startup.Handle{Name: []byte("vfs")})
	}
	startupData, ok := startup.Encode(command, cwd, arguments, environment, bindings, &startupWire)
	if !ok {
		write("APP_STARTUP_ENCODE_FAILED\n")
		handleClose(pair[0])
		return 0, 0, supervisor.LaunchInvalidImage
	}
	grants := []supervisor.Capability{
		{Name: "in", Handle: input, Type: inputType, Rights: streamRights(inputType, capability.Read), Consume: inputType == handle.TypeStream},
		{Name: "stdout", Handle: output, Type: outputType, Rights: streamRights(outputType, capability.Write), Consume: outputType == handle.TypeStream},
		{Name: "stderr", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
	}
	if useVFS {
		grants = append(grants, supervisor.Capability{Name: "vfs", Handle: handle.ID(pair[0]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true})
	}
	correlation++
	request, ok := supervisor.LaunchRequestWithStartup(1, path, grants, startupData, correlation)
	if !ok {
		write("APP_LAUNCH_ENCODE_FAILED\n")
		handleClose(pair[0])
		return 0, 0, supervisor.LaunchInvalidImage
	}
	if !sendMessage(lifecycleHandle, request) {
		write("APP_LAUNCH_SEND_FAILED\n")
		handleClose(pair[0])
		return 0, 0, supervisor.LaunchInvalidImage
	}
	event, status := receiveLaunchResult(correlation)
	if status != ipc.StatusOK {
		write("APP_LAUNCH_REPLY_FAILED\n")
		return 0, 0, supervisor.LaunchProcessFailed
	}
	if event.Code != uint64(supervisor.LaunchOK) {
		handleClose(pair[0])
	}
	return event.Process, event.ProcessHandle, supervisor.LaunchStatus(event.Code)
}

func commandNeedsVFS(command []byte) bool {
	return equalSlice(command, "pwd") || equalSlice(command, "ls") || equalSlice(command, "cat") || equalSlice(command, "mkdir") ||
		equalSlice(command, "touch") || equalSlice(command, "rm") || equalSlice(command, "cp") || equalSlice(command, "mv") || equalSlice(command, "stat")
}

func equalSlice(value []byte, expected string) bool {
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

func streamRights(typeID handle.Type, rights capability.Rights) capability.Rights {
	if typeID == handle.TypeStream {
		return rights | capability.Wait
	}
	return rights
}

func waitApplicationExit(processHandle handle.ID) uint64 {
	var exitCode uint64
	for {
		status := processWait(uint64(processHandle), unsafe.Pointer(&exitCode), 100000)
		if status == uint64(ipc.StatusTimeout) {
			continue
		}
		if status != 0 || handleClose(uint64(processHandle)) != 0 {
			fatal("INIT_EXEC_WAIT_FAILED\n")
		}
		if !processWaitAnnounced {
			write("PROCESS_HANDLE_WAIT_OK\n")
			processWaitAnnounced = true
		}
		return exitCode
	}
}

func sendExecReply(correlation, pid, status, exitCode uint64) {
	reply, ok := execproto.EncodeReply(execproto.Reply{Process: pid, LaunchStatus: status, ExitCode: exitCode}, correlation)
	if !ok || !sendMessage(execServerHandle, reply) {
		fatal("INIT_EXEC_REPLY_FAILED\n")
	}
}

func sendMessage(channel uint64, message ipc.Message) bool {
	size, ok := ipc.Encode(message, wire[:])
	return ok && ipcSend(channel, unsafe.Pointer(&wire[0]), uintptr(size)) == 0
}

func closePair(pair [2]uint64) {
	if pair[0] != 0 {
		handleClose(pair[0])
	}
	if pair[1] != 0 {
		handleClose(pair[1])
	}
}

func bytesString(value []byte) string {
	return *(*string)(unsafe.Pointer(&value))
}

func write(message string) {
	debugWrite(unsafe.Pointer(unsafe.StringData(message)), uintptr(len(message)))
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
		fatal("INIT_OUT_OF_MEMORY\n")
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
