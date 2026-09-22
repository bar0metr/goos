//go:build goos_userspace

package main

import (
	"goos/internal/capability"
	"goos/internal/validation"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	blockproto "goos/userspace/block"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/startup"
	userwait "goos/userspace/wait"
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
const systemControlHandle = uint64(0x0000000100000012)
const execPageHandle = handle.ID(0x0000000100000014)

//go:linkname memoryMap goos_memory_map
func memoryMap(memory uint64, address uint64, flags uint64) uint64

// execJob is one shell request in flight. The request is copied out of the
// shared page, so the shell cannot change it while it runs.
type execJob struct {
	active      bool
	slot        uint8
	correlation uint64
	data        [execproto.SlotSize]byte
	job         supervisor.Job
}

//go:linkname systemReset goos_system_reset
func systemReset(device uint64) uint64

var (
	manager              supervisor.Manager
	wire                 [ipc.WireMaxSize]byte
	descriptor           = [3]uint64{uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, 100000}
	supervisionTicks     uint64
	correlation          uint64
	heapPointer          uintptr
	arenaBase            uintptr
	supervisionComplete  bool
	processWaitAnnounced bool
	nextStagingSlot      uint8
	execJobs             [execproto.Slots]execJob
	execPage             = (*[execproto.Slots][execproto.SlotSize]byte)(unsafe.Pointer(uintptr(execproto.PageAddress)))
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

//go:linkname diagnostics goos_diagnostics
func diagnostics(device uint64, address uintptr) uint64

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
	syslog.Init(serviceDeviceHandle, "init")
	if validation.Enabled {
		if !probeStreamEndpoint() {
			fatal("STREAM_ENDPOINT_FAILED\n")
		}
		mark("STREAM_ENDPOINT_OK\n")
		if manager.Register(supervisor.Definition{Name: "fault-probe", Executable: "/system/fault-probe",
			Capabilities: []supervisor.Capability{{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write}}, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK ||
			manager.StartReady(0, bootstrapService) != 1 {
			fatal("INIT_POLICY_SETUP_FAILED\n")
		}
		mark("INIT_PROCESS_OK\n")
		mark("INIT_LIFECYCLE_IPC_OK\n")
	} else {
		startServices(0)
	}
	for {
		var interests [userwait.MaxObjects]userwait.Interest
		count := 1
		interests[0] = userwait.Interest{Handle: handle.ID(lifecycleHandle), Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		var jobs [execproto.Slots]int
		if supervisionComplete {
			interests[1] = userwait.Interest{Handle: handle.ID(execServerHandle), Events: kcall.WaitReadable | kcall.WaitPeerClosed}
			count++
			for index := range execJobs {
				if process, pending := execJobs[index].job.Next(); execJobs[index].active && pending {
					interests[count] = userwait.Interest{Handle: process, Events: kcall.WaitCompleted}
					jobs[count-2] = index
					count++
				}
			}
		}
		readyMask := uint16(1)
		if lifecycleCount == 0 {
			result, status := userwait.Any(&interests, count, kcall.WaitAnyInfiniteTimeout)
			if status != kcall.StatusOK {
				fatal("INIT_WAIT_FAILED\n")
			}
			readyMask = result.ReadyMask
		}
		for bit := 2; bit < count; bit++ {
			if readyMask&(1<<bit) != 0 {
				advanceJob(jobs[bit-2])
			}
		}
		if readyMask&1 == 0 {
			if readyMask&2 != 0 {
				pollExec()
			}
			continue
		}
		message, status := receiveLifecycle(0)
		if status != ipc.StatusOK {
			fatal("INIT_LIFECYCLE_FAILED\n")
		}
		event, valid := supervisor.DecodeLifecycle(message)
		if !valid || event.Operation != supervisor.LifecycleProcessExited {
			continue
		}
		supervisionTicks += supervisor.RestartBackoff
		if manager.Exit(event.Process, event.Code, supervisionTicks) == supervisor.StatusNotFound {
			continue
		}
		syslog.Warn("service exited", syslog.Uint("pid", event.Process), syslog.Uint("code", event.Code))
		manager.StartReady(supervisionTicks+supervisor.RestartBackoff, launchService)
		snapshot, found := manager.Snapshot("fault-probe")
		if validation.Enabled && found && snapshot.State == supervisor.StateFailed && !supervisionComplete {
			startServices(supervisionTicks)
			mark("USERSPACE_SUPERVISOR_OK\n")
		}
	}
}

// startServices registers the system services and launches them in
// dependency order, reporting each on the console.
func startServices(ticks uint64) {
	if memoryMap(uint64(execPageHandle), execproto.PageAddress, kcall.MemoryMapWritable) != 0 {
		fatal("INIT_EXEC_PAGE_FAILED\n")
	}
	write("\nStarting services...\n")
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
				{Name: "exec-page", Handle: execPageHandle, Type: handle.TypeMemory, Rights: capability.Read | capability.Write},
			}, Restart: supervisor.RestartNever}) != supervisor.StatusOK {
		fatal("INIT_SERVICE_REGISTRATION_FAILED\n")
	}
	if manager.StartReady(ticks+supervisor.RestartBackoff, launchService) != 4 || validation.Enabled && !probeBlockService() {
		fatal("SHELL_LAUNCH_FAILED\n")
	}
	supervisionComplete = true
	arenaBase = heapPointer
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
	message, status := receive(blockClientHandle, storageProbeDeadline)
	infoReply, valid := blockproto.DecodeReply(message)
	if status != ipc.StatusOK || !valid || infoReply.Status != blockproto.StatusOK || infoReply.Capacity != 64*1024*1024 || infoReply.BlockSize != 512 {
		return false
	}
	correlation++
	writeRequest, ok := blockproto.EncodeRequest(blockproto.Request{Operation: blockproto.OperationWrite, Offset: 4093, Length: 6, Data: []byte("GBLKIO")}, correlation)
	if !ok || !sendMessage(blockClientHandle, writeRequest) {
		return false
	}
	message, status = receive(blockClientHandle, storageProbeDeadline)
	writeReply, valid := blockproto.DecodeReply(message)
	if status != ipc.StatusOK || !valid || writeReply.Status != blockproto.StatusOK {
		return false
	}
	correlation++
	readRequest, ok := blockproto.EncodeRequest(blockproto.Request{Operation: blockproto.OperationRead, Offset: 4093, Length: 6}, correlation)
	if !ok || !sendMessage(blockClientHandle, readRequest) {
		return false
	}
	message, status = receive(blockClientHandle, storageProbeDeadline)
	readReply, valid := blockproto.DecodeReply(message)
	if status != ipc.StatusOK || !valid || readReply.Status != blockproto.StatusOK || !equalBytes(readReply.Data, "GBLKIO") {
		return false
	}
	correlation++
	flushRequest, ok := blockproto.EncodeRequest(blockproto.Request{Operation: blockproto.OperationFlush}, correlation)
	if !ok || !sendMessage(blockClientHandle, flushRequest) {
		return false
	}
	message, status = receive(blockClientHandle, storageProbeDeadline)
	flushReply, valid := blockproto.DecodeReply(message)
	if status != ipc.StatusOK || !valid || flushReply.Status != blockproto.StatusOK {
		return false
	}
	mark("BLOCK_SERVICE_IO_OK\n")
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

// stagingSlot returns the next slot of the page shared with the kernel for
// launch startup templates.
func stagingSlot() (uint8, *[startup.Size]byte) {
	nextStagingSlot = (nextStagingSlot + 1) % supervisor.StagingSlots
	address := supervisor.StagingAddress + uint64(nextStagingSlot)*supervisor.StagingSlotSize
	return nextStagingSlot, (*[startup.Size]byte)(unsafe.Pointer(uintptr(address)))
}

func launchService(spec supervisor.LaunchSpec) (uint64, bool) {
	correlation++
	slot, staging := stagingSlot()
	var vectorWire [startup.VectorSize]byte
	var bindings [supervisor.MaxCapabilities]startup.Handle
	for index := 0; index < int(spec.CapabilityCount); index++ {
		bindings[index].Name = []byte(spec.Capabilities[index].Name)
	}
	arguments, environment, ok := startup.EncodeVectors([][]byte{spec.Name[:spec.NameSize]}, nil, &vectorWire)
	if !ok {
		return 0, false
	}
	startupData, ok := startup.Encode(spec.Name[:spec.NameSize], []byte("/"), arguments, environment, bindings[:spec.CapabilityCount], staging)
	if !ok {
		return 0, false
	}
	request, ok := supervisor.LaunchRequestWithStartup(1, bytesString(spec.Executable[:spec.ExecutableSize]), spec.Capabilities[:spec.CapabilityCount], slot, uint16(len(startupData)), correlation)
	if !ok {
		return 0, false
	}
	size, ok := ipc.Encode(request, wire[:])
	if !ok || ipcSend(lifecycleHandle, unsafe.Pointer(&wire[0]), uintptr(size)) != 0 {
		return 0, false
	}
	mark("INIT_SERVICE_LAUNCH_OK\n")
	name := bytesString(spec.Name[:spec.NameSize])
	event, status := receiveLaunchResult(correlation)
	if status != ipc.StatusOK || event.Code != uint64(supervisor.LaunchOK) || handleClose(uint64(event.ProcessHandle)) != 0 {
		write("[FAIL] " + name + "\n")
		syslog.Error("service launch failed", syslog.Str("service", name), syslog.Uint("status", event.Code))
		return 0, false
	}
	mark("INIT_SERVICE_STARTED_OK\n")
	write("[ OK ] " + name + "\n")
	syslog.Info("service started", syslog.Str("service", name), syslog.Uint("pid", event.Process))
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

const (
	// storageProbeDeadline covers the first answer from the storage stack,
	// which is still completing its own startup when PID 1 probes it.
	storageProbeDeadline = 5 * clock.TicksPerSecond
	// vfsSessionDeadline bounds one session handshake with the VFS service.
	vfsSessionDeadline = 2 * clock.TicksPerSecond
)

// launchReplyDeadline bounds the wait for a launch result. The kernel retains
// and retries these replies, so expiry means a real fault and must be reported
// instead of stalling PID 1 and every shell behind it.
const launchReplyDeadline = 5 * clock.TicksPerSecond

func receiveLaunchResult(expectedCorrelation uint64) (supervisor.LifecycleEvent, ipc.Status) {
	for {
		message, status := receive(lifecycleHandle, launchReplyDeadline)
		if status != ipc.StatusOK {
			if status == ipc.StatusTimeout {
				syslog.Warn("INIT_LAUNCH_REPLY_TIMEOUT")
			}
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
	if execproto.IsControl(message) {
		handleControl(message)
		return
	}
	slot, size, ok := execproto.DecodeSubmit(message)
	if !ok {
		sendExecReply(message.Correlation, 0, uint64(supervisor.LaunchInvalidImage), 0)
		return
	}
	job := freeJob(slot)
	if job == nil {
		sendExecReply(message.Correlation, 0, uint64(supervisor.LaunchResourceExhausted), 0)
		return
	}
	copy(job.data[:size], execPage[slot][:size])
	request, ok := execproto.DecodeRequest(job.data[:size])
	if !ok {
		sendExecReply(message.Correlation, 0, uint64(supervisor.LaunchInvalidImage), 0)
		return
	}
	job.active, job.slot, job.correlation = true, slot, message.Correlation
	job.job = supervisor.StartPipeline(request, serviceDeviceHandle, nativePipelineRunner{})
	finishJob(job)
}

// freeJob returns an unused job record, or nil when the slot is still owned
// by a running job or every record is busy.
func freeJob(slot uint8) *execJob {
	var free *execJob
	for index := range execJobs {
		if execJobs[index].active && execJobs[index].slot == slot {
			return nil
		}
		if !execJobs[index].active && free == nil {
			free = &execJobs[index]
		}
	}
	return free
}

// advanceJob reaps the process a job was waiting for; it has completed.
func advanceJob(index int) {
	job := &execJobs[index]
	if process, pending := job.job.Next(); job.active && pending {
		job.job.Complete(waitApplicationExit(process))
		finishJob(job)
	}
}

func finishJob(job *execJob) {
	if _, pending := job.job.Next(); pending {
		return
	}
	pid, launchStatus, exitCode := job.job.Result()
	job.active = false
	sendExecReply(job.correlation, pid, uint64(launchStatus), exitCode)
}

// handleControl answers the shell's requests that launch nothing.
func handleControl(message ipc.Message) {
	operation, ok := execproto.DecodeControl(message)
	switch {
	case !ok:
		sendControlStatus(execproto.ControlInvalid, message.Correlation)
	case operation == execproto.ControlServices:
		var entries [execproto.MaxServiceEntries]execproto.ServiceEntry
		count := 0
		for index := 0; count < len(entries); index++ {
			name, snapshot, found := manager.At(index)
			if !found {
				break
			}
			if len(name) > execproto.MaxServiceName {
				name = name[:execproto.MaxServiceName]
			}
			entries[count] = execproto.ServiceEntry{Name: name, State: uint8(snapshot.State), Restarts: snapshot.Restarts, PID: uint32(snapshot.PID)}
			count++
		}
		if reply, encoded := execproto.EncodeServices(entries[:count], message.Correlation); encoded {
			sendMessage(execServerHandle, reply)
		}
	case operation == execproto.ControlReboot:
		correlationOfShell := message.Correlation
		syslog.Info("reboot requested")
		correlation++
		sync, encoded := vfs.SyncRequest(correlation)
		if !encoded || !sendMessage(vfsControlHandle, sync) {
			sendControlStatus(execproto.ControlSyncFailed, correlationOfShell)
			return
		}
		reply, status := receive(vfsControlHandle, vfsSessionDeadline)
		synced, valid := vfs.DecodeAddSessionReply(reply)
		if status != ipc.StatusOK || !valid || synced != vfs.StatusOK || reply.Correlation != correlation {
			syslog.Error("reboot: storage sync failed")
			sendControlStatus(execproto.ControlSyncFailed, correlationOfShell)
			return
		}
		write("\nRebooting...\n")
		systemReset(systemControlHandle)
		syslog.Error("reboot: reset refused")
		sendControlStatus(execproto.ControlResetFailed, correlationOfShell)
	}
}

func sendControlStatus(status execproto.ControlStatus, correlation uint64) {
	if reply, encoded := execproto.EncodeControlStatus(status, correlation); encoded {
		sendMessage(execServerHandle, reply)
	}
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

func (nativePipelineRunner) Launch(stage execproto.Stage, cwd, environment []byte, streams supervisor.Streams) (uint64, handle.ID, supervisor.LaunchStatus) {
	command, allowed := execproto.CommandFromSystemPath(stage.Path)
	if !allowed {
		return 0, 0, supervisor.LaunchCapabilityDenied
	}
	return launchApplication(bytesString(stage.Path), command, cwd, stage.Arguments, environment, streams)
}

func (nativePipelineRunner) Wait(process handle.ID) uint64 { return waitApplicationExit(process) }

func launchApplication(path string, command, cwd, arguments, environment []byte, streams supervisor.Streams) (uint64, handle.ID, supervisor.LaunchStatus) {
	var pair [2]uint64
	useVFS := commandNeedsVFS(command)
	if useVFS && channelCreate(unsafe.Pointer(&pair[0]), 16) != 0 {
		syslog.Error("APP_CHANNEL_CREATE_FAILED")
		return 0, 0, supervisor.LaunchResourceExhausted
	}
	if useVFS {
		correlation++
	}
	control, ok := vfs.AddSessionRequest(handle.ID(pair[1]), correlation)
	if useVFS && (!ok || !sendMessage(vfsControlHandle, control)) {
		syslog.Error("APP_VFS_REQUEST_FAILED")
		closePair(pair)
		return 0, 0, supervisor.LaunchCapabilityInstallFailed
	}
	var status ipc.Status
	vfsStatus, valid := vfs.StatusOK, true
	if useVFS {
		controlReply, received := receive(vfsControlHandle, vfsSessionDeadline)
		status = received
		vfsStatus, valid = vfs.DecodeAddSessionReply(controlReply)
	}
	if useVFS && (status != ipc.StatusOK || !valid || vfsStatus != vfs.StatusOK) {
		syslog.Error("APP_VFS_SESSION_FAILED")
		if status != ipc.StatusOK {
			syslog.Error("APP_VFS_RECEIVE_STATUS")
		} else if !valid {
			syslog.Error("APP_VFS_REPLY_INVALID")
		} else {
			syslog.Error("APP_VFS_RESOURCE_EXHAUSTED")
		}
		handleClose(pair[0])
		return 0, 0, supervisor.LaunchCapabilityInstallFailed
	}
	slot, staging := stagingSlot()
	bindings := []startup.Handle{{Name: []byte("in")}, {Name: []byte("stdout")}, {Name: []byte("stderr")}}
	if useVFS {
		bindings = append(bindings, startup.Handle{Name: []byte("vfs")})
	}
	startupData, ok := startup.Encode(command, cwd, arguments, environment, bindings, staging)
	if !ok {
		syslog.Warn("APP_STARTUP_ENCODE_FAILED")
		handleClose(pair[0])
		return 0, 0, supervisor.LaunchInvalidImage
	}
	grants := []supervisor.Capability{
		streamGrant("in", streams.Input, capability.Read),
		streamGrant("stdout", streams.Output, capability.Write),
		streamGrant("stderr", streams.Error, capability.Write),
	}
	if useVFS {
		grants = append(grants, supervisor.Capability{Name: "vfs", Handle: handle.ID(pair[0]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true})
	}
	correlation++
	request, ok := supervisor.LaunchRequestWithStartup(1, path, grants, slot, uint16(len(startupData)), correlation)
	if !ok {
		syslog.Error("APP_LAUNCH_ENCODE_FAILED")
		handleClose(pair[0])
		return 0, 0, supervisor.LaunchInvalidImage
	}
	if !sendMessage(lifecycleHandle, request) {
		syslog.Error("APP_LAUNCH_SEND_FAILED")
		handleClose(pair[0])
		return 0, 0, supervisor.LaunchInvalidImage
	}
	event, status := receiveLaunchResult(correlation)
	if status != ipc.StatusOK {
		syslog.Error("APP_LAUNCH_REPLY_FAILED")
		return 0, 0, supervisor.LaunchProcessFailed
	}
	if event.Code != uint64(supervisor.LaunchOK) {
		handleClose(pair[0])
	}
	return event.Process, event.ProcessHandle, supervisor.LaunchStatus(event.Code)
}

func commandNeedsVFS(command []byte) bool {
	return equalSlice(command, "pwd") || equalSlice(command, "ls") || equalSlice(command, "cat") || equalSlice(command, "mkdir") ||
		equalSlice(command, "touch") || equalSlice(command, "rm") || equalSlice(command, "cp") || equalSlice(command, "mv") || equalSlice(command, "stat") || equalSlice(command, "write") || equalSlice(command, "bench")
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

// streamGrant grants one standard-stream endpoint; stream endpoints are consumed
// by a successful launch while the console device handle stays with PID 1.
func streamGrant(name string, endpoint supervisor.Endpoint, rights capability.Rights) supervisor.Capability {
	return supervisor.Capability{Name: name, Handle: endpoint.ID, Type: endpoint.Type, Rights: streamRights(endpoint.Type, rights), Consume: endpoint.Type == handle.TypeStream}
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
		var interests [userwait.MaxObjects]userwait.Interest
		interests[0] = userwait.Interest{Handle: processHandle, Events: kcall.WaitCompleted}
		if _, status := userwait.Any(&interests, 1, kcall.WaitAnyInfiniteTimeout); status != kcall.StatusOK {
			fatal("INIT_EXEC_WAIT_READY_FAILED\n")
		}
		status := processWait(uint64(processHandle), unsafe.Pointer(&exitCode), 0)
		if status != 0 {
			if status == uint64(ipc.StatusNotFound) {
				continue
			}
			fatal("INIT_EXEC_WAIT_FAILED\n")
		}
		if handleClose(uint64(processHandle)) != 0 {
			fatal("INIT_EXEC_CLOSE_FAILED\n")
		}
		if !processWaitAnnounced {
			mark("PROCESS_HANDLE_WAIT_OK\n")
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

func mark(message string) {
	if validation.Enabled {
		write(message)
	}
}

func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		write(message)
	}
	diagnostics(uint64(serviceDeviceHandle), 0)
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
