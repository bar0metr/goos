//go:build goos_userspace

package main

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/vfs"
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

//export userspaceMain
func userspaceMain() {
	runtimeInitHeap()
	if manager.Register(supervisor.Definition{Name: "fault-probe", Executable: "/system/fault-probe",
		Capabilities: []supervisor.Capability{{Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write}}, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK ||
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
		message, status := receive(lifecycleHandle, timeout)
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
					{Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
					{Handle: vfsServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
					{Handle: vfsControlServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
				}, Restart: supervisor.RestartNever}) != supervisor.StatusOK ||
				manager.Register(supervisor.Definition{Name: "shell", Executable: "/system/shell", Dependencies: []string{"vfs"},
					Capabilities: []supervisor.Capability{
						{Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Read | capability.Write},
						{Handle: vfsChannelHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
						{Handle: execClientHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
					}, Restart: supervisor.RestartNever}) != supervisor.StatusOK ||
				manager.StartReady(clock+supervisor.RestartBackoff, launchService) != 2 {
				fatal("SHELL_LAUNCH_FAILED\n")
			}
			supervisionComplete = true
			arenaBase = heapPointer
			write("USERSPACE_SUPERVISOR_OK\n")
		}
	}
}

func bootstrapService(supervisor.LaunchSpec) (uint64, bool) { return 3, true }

func launchService(spec supervisor.LaunchSpec) (uint64, bool) {
	correlation++
	request, ok := supervisor.LaunchRequest(1, bytesString(spec.Executable[:spec.ExecutableSize]), spec.Capabilities[:spec.CapabilityCount], correlation)
	if !ok {
		return 0, false
	}
	size, ok := ipc.Encode(request, wire[:])
	if !ok || ipcSend(lifecycleHandle, unsafe.Pointer(&wire[0]), uintptr(size)) != 0 {
		return 0, false
	}
	write("INIT_SERVICE_LAUNCH_OK\n")
	reply, status := receive(lifecycleHandle, 100000)
	if status != ipc.StatusOK {
		return 0, false
	}
	event, valid := supervisor.DecodeLifecycle(reply)
	if !valid || event.Operation != supervisor.LifecycleLaunchResult || reply.Correlation != correlation || event.Code != uint64(supervisor.LaunchOK) {
		return 0, false
	}
	if handleClose(uint64(event.ProcessHandle)) != 0 {
		return 0, false
	}
	write("INIT_SERVICE_STARTED_OK\n")
	return event.Process, true
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
		sendExecReply(message.Correlation, 0, uint64(supervisor.LaunchInvalidImage))
		return
	}
	var path [execproto.MaxPath]byte
	pathSize := copy(path[:], request.Path)
	var cwd [96]byte
	cwdSize := copy(cwd[:], request.CWD)
	var arguments [128]byte
	argumentSize := copy(arguments[:], request.Arguments)
	if !allowedExecutable(path[:pathSize]) {
		sendExecReply(message.Correlation, 0, uint64(supervisor.LaunchCapabilityDenied))
		return
	}
	pid, processHandle, launchStatus := launchApplication(bytesString(path[:pathSize]), cwd[:cwdSize], arguments[:argumentSize])
	if launchStatus == supervisor.LaunchOK {
		waitApplicationExit(processHandle)
	}
	sendExecReply(message.Correlation, pid, uint64(launchStatus))
}

func launchApplication(path string, cwd, arguments []byte) (uint64, handle.ID, supervisor.LaunchStatus) {
	var pair [2]uint64
	if channelCreate(unsafe.Pointer(&pair[0]), 16) != 0 {
		return 0, 0, supervisor.LaunchResourceExhausted
	}
	correlation++
	control, ok := vfs.AddSessionRequest(handle.ID(pair[1]), correlation)
	if !ok || !sendMessage(vfsControlHandle, control) {
		closePair(pair)
		return 0, 0, supervisor.LaunchCapabilityInstallFailed
	}
	controlReply, status := receive(vfsControlHandle, 1000)
	vfsStatus, valid := vfs.DecodeAddSessionReply(controlReply)
	if status != ipc.StatusOK || !valid || vfsStatus != vfs.StatusOK {
		handleClose(pair[0])
		return 0, 0, supervisor.LaunchCapabilityInstallFailed
	}
	var startupWire [startup.Size]byte
	startupData, ok := startup.Encode(commandForPath(path), cwd, arguments, &startupWire)
	if !ok {
		handleClose(pair[0])
		return 0, 0, supervisor.LaunchInvalidImage
	}
	grants := []supervisor.Capability{
		{Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
		{Handle: handle.ID(pair[0]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
	}
	correlation++
	request, ok := supervisor.LaunchRequestWithStartup(1, path, grants, startupData, correlation)
	if !ok || !sendMessage(lifecycleHandle, request) {
		handleClose(pair[0])
		return 0, 0, supervisor.LaunchInvalidImage
	}
	reply, status := receive(lifecycleHandle, 100000)
	event, valid := supervisor.DecodeLifecycle(reply)
	if status != ipc.StatusOK || !valid || event.Operation != supervisor.LifecycleLaunchResult || reply.Correlation != correlation {
		return 0, 0, supervisor.LaunchProcessFailed
	}
	if event.Code != uint64(supervisor.LaunchOK) {
		handleClose(pair[0])
	}
	return event.Process, event.ProcessHandle, supervisor.LaunchStatus(event.Code)
}

func waitApplicationExit(processHandle handle.ID) {
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
		return
	}
}

func sendExecReply(correlation, pid, status uint64) {
	reply, ok := execproto.EncodeReply(execproto.Reply{Process: pid, Status: status}, correlation)
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

func allowedExecutable(path []byte) bool {
	return equalBytes(path, "/system/bin/pwd") || equalBytes(path, "/system/bin/ls") || equalBytes(path, "/system/bin/cat")
}

func commandForPath(path string) byte {
	if path == "/system/bin/pwd" {
		return 1
	}
	if path == "/system/bin/ls" {
		return 2
	}
	if path == "/system/bin/cat" {
		return 3
	}
	return 0
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
