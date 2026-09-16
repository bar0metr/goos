//go:build goos_userspace

package main

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	supervisor "goos/userspace/init"
	"runtime"
	"unsafe"
)

const lifecycleHandle = uint64(0x0000000100000001)
const serviceDeviceHandle = handle.ID(0x0000000100000002)
const vfsChannelHandle = handle.ID(0x0000000100000003)

var (
	manager             supervisor.Manager
	wire                [ipc.WireMaxSize]byte
	descriptor          = [3]uint64{uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000}
	clock               uint64
	correlation         uint64
	heapPointer         uintptr
	supervisionComplete bool
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
		message, ok := receive()
		if !ok {
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
					{Handle: vfsChannelHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Endpoint: 2},
				}, Restart: supervisor.RestartNever}) != supervisor.StatusOK ||
				manager.Register(supervisor.Definition{Name: "shell", Executable: "/system/shell", Dependencies: []string{"vfs"},
					Capabilities: []supervisor.Capability{
						{Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Read | capability.Write},
						{Handle: vfsChannelHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Endpoint: 1},
					}, Restart: supervisor.RestartNever}) != supervisor.StatusOK ||
				manager.StartReady(clock+supervisor.RestartBackoff, launchService) != 2 {
				fatal("SHELL_LAUNCH_FAILED\n")
			}
			supervisionComplete = true
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
	reply, ok := receive()
	if !ok {
		return 0, false
	}
	event, valid := supervisor.DecodeLifecycle(reply)
	if !valid || event.Operation != supervisor.LifecycleLaunchResult || reply.Correlation != correlation || event.Code != uint64(supervisor.LaunchOK) {
		return 0, false
	}
	write("INIT_SERVICE_STARTED_OK\n")
	return event.Process, true
}

func receive() (ipc.Message, bool) {
	for {
		descriptor[0] = uint64(uintptr(unsafe.Pointer(&wire[0])))
		descriptor[1] = ipc.WireMaxSize
		descriptor[2] = 100000
		status := ipcWaitReceive(lifecycleHandle, unsafe.Pointer(&descriptor[0]), uintptr(len(descriptor)*8))
		if status == uint64(ipc.StatusTimeout) {
			continue
		}
		if status != 0 {
			return ipc.Message{}, false
		}
		break
	}
	payloadSize := int(wire[24]) | int(wire[25])<<8
	size := ipc.WireHeaderSize + payloadSize + int(wire[26])*8
	if size < ipc.WireHeaderSize || size > len(wire) {
		return ipc.Message{}, false
	}
	return ipc.Decode(wire[:size])
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
