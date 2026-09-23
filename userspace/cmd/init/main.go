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
	"goos/userspace/commands"
	configproto "goos/userspace/config"
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
const systemImageHandle = handle.ID(0x0000000100000015)

// logDeviceHandle authorizes reading the kernel log ring and nothing else, so
// the log service and the shell can read the log without the console's input.
const logDeviceHandle = handle.ID(0x0000000100000016)

// configControlHandle is the configuration service's control endpoint. PID 1
// holds the only client side of it, so it is the only process that can create a
// configuration session.
var configControlHandle handle.ID

var (
	// blockControl is the channel pair whose client side PID 1 keeps to tell
	// the block service about a replacement driver.
	blockControl [2]uint64
	// driverStarted marks the first launch, which uses the boot channel.
	driverStarted bool
	// driverProcessHandle is kept only by the validation build, to inject the
	// fault that proves a crashed driver is replaced.
	driverProcessHandle handle.ID
)

//go:linkname processKill goos_process_kill
func processKill(process uint64) uint64

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
			// A scheduled restart is a deadline rather than an event. While one
			// is outstanding the wait is bounded, so the service comes back
			// even when nothing else happens; otherwise it blocks indefinitely.
			timeout := uint64(kcall.WaitAnyInfiniteTimeout)
			if manager.RestartPending() {
				timeout = supervisor.RestartBackoff
			}
			result, status := userwait.Any(&interests, count, timeout)
			if status == kcall.StatusTimeout {
				supervisionTicks += supervisor.RestartBackoff
				manager.StartReady(supervisionTicks, launchService)
				continue
			}
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
	// The block service's control endpoint, over which a restarted driver's
	// replacement channel is delivered. PID 1 holds the only client side.
	if channelCreate(unsafe.Pointer(&blockControl[0]), 16) != 0 {
		fatal("INIT_BLOCK_CONTROL_FAILED\n")
	}
	if manager.Register(supervisor.Definition{Name: "vfs", Executable: "/system/vfs",
		Capabilities: []supervisor.Capability{
			{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
			{Name: "session", Handle: vfsServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
			{Name: "control", Handle: vfsControlServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
			{Name: "block", Handle: filesystemBlockClientHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
			{Name: "image", Handle: systemImageHandle, Type: handle.TypeDevice, Rights: capability.Read, Consume: true},
		}, Restart: supervisor.RestartNever}) != supervisor.StatusOK ||
		manager.Register(supervisor.Definition{Name: "block", Executable: "/system/block",
			Capabilities: []supervisor.Capability{
				{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
				{Name: "block", Handle: blockServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
				{Name: "driver", Handle: driverClientHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
				{Name: "filesystem", Handle: filesystemBlockServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
				{Name: "control", Handle: handle.ID(blockControl[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
			}, Restart: supervisor.RestartNever}) != supervisor.StatusOK ||
		// The driver's device authority stays with PID 1 rather than being
		// consumed. A driver that crashes has to be replaced by another one
		// holding the same ports, line and DMA domain, and only PID 1 can grant
		// them. The dead process's copies are released by its teardown, which
		// also resets the device and masks its interrupt.
		manager.Register(supervisor.Definition{Name: "virtio-blk", Executable: "/system/virtio-blk",
			Capabilities: []supervisor.Capability{
				{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
				{Name: "ports", Handle: virtioPortHandle, Type: handle.TypeDevice, Rights: capability.Read | capability.Write},
				{Name: "irq", Handle: virtioIRQHandle, Type: handle.TypeIRQ, Rights: capability.Wait},
				{Name: "dma", Handle: virtioDMAHandle, Type: handle.TypeDevice, Rights: capability.Write},
				{Name: "service", Handle: driverServerHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
			}, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK {
		fatal("INIT_SERVICE_REGISTRATION_FAILED\n")
	}
	if manager.StartReady(ticks+supervisor.RestartBackoff, launchService) != 3 || validation.Enabled && !probeBlockService() {
		fatal("SHELL_LAUNCH_FAILED\n")
	}
	// The log service comes after the filesystem and before the shell: its
	// session has to be asked for rather than handed over at boot, and it
	// should be persisting records by the time a prompt appears.
	// Configuration comes first: the log service consults it, so it has to be
	// answering before anything that reads a setting starts.
	startConfigService(ticks)
	startLogService(ticks)
	terminal := startTerminalService(ticks)
	// The shell administers configuration, so its session is the whole
	// namespace and may write. Every other client gets less.
	configClient, _ := openConfigSession("", configproto.Read|configproto.Write)
	// The shell writes to the console but does not read it: what is typed
	// reaches it as lines from the terminal service.
	shellCapabilities := []supervisor.Capability{
		{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
		{Name: "tty", Handle: terminal, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "log", Handle: logDeviceHandle, Type: handle.TypeDevice, Rights: capability.Read},
		{Name: "vfs", Handle: vfsChannelHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "exec", Handle: execClientHandle, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "execpage", Handle: execPageHandle, Type: handle.TypeMemory, Rights: capability.Read | capability.Write},
	}
	if configClient != 0 {
		shellCapabilities = append(shellCapabilities, supervisor.Capability{Name: "config", Handle: configClient,
			Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true})
	}
	if manager.Register(supervisor.Definition{Name: "shell", Executable: "/system/shell", Dependencies: []string{"vfs", "tty"},
		Capabilities: shellCapabilities, Restart: supervisor.RestartNever}) != supervisor.StatusOK {
		fatal("INIT_SERVICE_REGISTRATION_FAILED\n")
	}
	if manager.StartReady(ticks+supervisor.RestartBackoff, launchService) != 1 {
		fatal("SHELL_LAUNCH_FAILED\n")
	}
	// The fault goes in last, once the filesystem has mounted and the system is
	// up: the point is that a driver crashing under a working system is
	// recovered from, not that one crashing during boot is survivable.
	injectDriverFault()
	supervisionComplete = true
	arenaBase = heapPointer
}

// startTerminalService starts the terminal service and returns the client end
// of the session the shell reads its lines over. The service is the only
// process given the right to read the console (ADR 0082).
//
// The terminal and the shell form one interactive session: the channel between
// them is created here once, so neither restarts on its own; recovering the
// session as a unit is later work, as it is for the shell today.
func startTerminalService(ticks uint64) handle.ID {
	var pair [2]uint64
	if channelCreate(unsafe.Pointer(&pair[0]), 16) != 0 {
		fatal("INIT_TTY_CHANNEL_FAILED\n")
	}
	if manager.Register(supervisor.Definition{Name: "tty", Executable: "/system/tty",
		Capabilities: []supervisor.Capability{
			{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Read | capability.Write},
			{Name: "session", Handle: handle.ID(pair[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		}, Restart: supervisor.RestartNever}) != supervisor.StatusOK {
		fatal("INIT_SERVICE_REGISTRATION_FAILED\n")
	}
	if manager.StartReady(ticks+supervisor.RestartBackoff, launchService) != 1 {
		fatal("INIT_TTY_LAUNCH_FAILED\n")
	}
	return handle.ID(pair[0])
}

// startLogService registers and starts the persistent log service after the
// filesystem is up, because its session has to be asked for rather than handed
// over at boot: it is the only service whose VFS endpoint is created the same
// way an application's is.
//
// A failure here does not stop the system. The kernel ring is the system log
// and keeps working; only its persistence is lost, and that is worth a warning
// rather than a dead boot.
func startLogService(ticks uint64) {
	session, reason := openVFSSession()
	if reason != "" {
		syslog.Warn("log service has no filesystem session", syslog.Str("reason", reason))
		return
	}
	capabilities := []supervisor.Capability{
		// The log capability authorizes draining the kernel ring; console
		// write is the service's own logging. It gets no other authority,
		// and in particular cannot read what is typed.
		{Name: "log", Handle: logDeviceHandle, Type: handle.TypeDevice, Rights: capability.Read},
		{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
		{Name: "vfs", Handle: session, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
	}
	// A read-only session confined to the service's own namespace. It consults
	// the setting that governs it and can neither change it nor see anything
	// else. Without a session it keeps its default.
	if configSession, ok := openConfigSession("system.log.", configproto.Read); ok {
		capabilities = append(capabilities, supervisor.Capability{Name: "config", Handle: configSession,
			Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true})
	}
	if manager.Register(supervisor.Definition{Name: "log", Executable: "/system/log", Dependencies: []string{"vfs"},
		Capabilities: capabilities, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK {
		syslog.Warn("log service registration failed")
		handleClose(uint64(session))
		return
	}
	if manager.StartReady(ticks+supervisor.RestartBackoff, launchService) != 1 {
		syslog.Warn("log service did not start")
	}
}

// startConfigService brings up the typed configuration store and returns the
// client endpoint the shell's session runs over, or zero if it could not start.
//
// PID 1 creates that session and is the only process that decides what it may
// do. The scope and rights the service applies to it are granted here, not
// asked for by the client, which is what makes the store's checks authority
// rather than convention.
//
// Like the log service, a failure warns rather than stopping the boot: nothing
// else reads configuration yet, and a system without it is still a system.
func startConfigService(ticks uint64) handle.ID {
	session, reason := openVFSSession()
	if reason != "" {
		syslog.Warn("config service has no filesystem session", syslog.Str("reason", reason))
		return 0
	}
	var pair [2]uint64
	if channelCreate(unsafe.Pointer(&pair[0]), 16) != 0 {
		syslog.Warn("config service channel unavailable")
		handleClose(uint64(session))
		return 0
	}
	if manager.Register(supervisor.Definition{Name: "config", Executable: "/system/config", Dependencies: []string{"vfs"},
		Capabilities: []supervisor.Capability{
			{Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
			{Name: "vfs", Handle: session, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
			{Name: "control", Handle: handle.ID(pair[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		}, Restart: supervisor.RestartOnFailure}) != supervisor.StatusOK {
		syslog.Warn("config service registration failed")
		closePair(pair)
		handleClose(uint64(session))
		return 0
	}
	if manager.StartReady(ticks+supervisor.RestartBackoff, launchService) != 1 {
		syslog.Warn("config service did not start")
		handleClose(pair[0])
		return 0
	}
	configControlHandle = handle.ID(pair[0])
	return configControlHandle
}

// openConfigSession asks the configuration service for a session confined to
// scope and limited to rights, and returns the client endpoint for the process
// that will use it. PID 1 chooses both: a client receives an endpoint that is
// already limited and cannot ask for more.
func openConfigSession(scope string, rights configproto.Rights) (handle.ID, bool) {
	if configControlHandle == 0 {
		return 0, false
	}
	confined, ok := configproto.MakeScope(scope)
	if !ok {
		return 0, false
	}
	var pair [2]uint64
	if channelCreate(unsafe.Pointer(&pair[0]), 16) != 0 {
		return 0, false
	}
	correlation++
	request, ok := configproto.AddSessionRequest(handle.ID(pair[1]), confined, rights, correlation)
	if !ok || !sendMessage(uint64(configControlHandle), request) {
		closePair(pair)
		return 0, false
	}
	reply, status := receive(uint64(configControlHandle), vfsSessionDeadline)
	configStatus, valid := configproto.DecodeAddSessionReply(reply)
	if status != ipc.StatusOK || !valid || configStatus != configproto.StatusOK {
		handleClose(pair[0])
		return 0, false
	}
	return handle.ID(pair[0]), true
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

// injectDriverFault kills the storage driver once, in the validation build
// only, so that the path a crashed driver takes is exercised on every boot
// rather than only when hardware misbehaves. The supervisor restarts it, PID 1
// gives it a fresh channel, and the block service installs it.
func injectDriverFault() {
	if !validation.Enabled || driverProcessHandle == 0 {
		return
	}
	target := driverProcessHandle
	driverProcessHandle = 0
	if processKill(uint64(target)) != 0 {
		syslog.Warn("driver fault injection failed")
		return
	}
	handleClose(uint64(target))
	syslog.Warn("driver fault injected")
	mark("BLOCK_DRIVER_FAULT_INJECTED\n")
}

// driverName is the one service whose endpoints cannot survive its own restart.
const driverName = "virtio-blk"

// replaceDriverChannel gives a restarting driver a fresh channel to the block
// service. The old pair died with the process that held one half of it, so the
// replacement cannot be handed the endpoint its predecessor had: a new pair is
// created, the block service is told about the client side over its control
// endpoint, and the definition is updated so the next restart does not hand out
// a handle that is already dead.
func replaceDriverChannel(spec *supervisor.LaunchSpec) bool {
	var pair [2]uint64
	if channelCreate(unsafe.Pointer(&pair[0]), 16) != 0 {
		syslog.Error("driver channel unavailable")
		return false
	}
	correlation++
	request, ok := blockproto.SetDriverRequest(handle.ID(pair[0]), correlation)
	if !ok || !sendMessage(uint64(blockControl[0]), request) {
		closePair(pair)
		return false
	}
	reply, status := receive(uint64(blockControl[0]), vfsSessionDeadline)
	blockStatus, valid := blockproto.DecodeSetDriverReply(reply)
	if status != ipc.StatusOK || !valid || blockStatus != blockproto.StatusOK {
		handleClose(pair[1])
		syslog.Error("block service refused the replacement driver")
		return false
	}
	// The definition and the copy about to be launched both have to name the
	// new endpoint: the launcher was handed its copy before this ran, and the
	// definition is what the launch after this one will use.
	if manager.SetCapability(driverName, "service", handle.ID(pair[1])) != supervisor.StatusOK {
		handleClose(pair[1])
		return false
	}
	for index := 0; index < int(spec.CapabilityCount); index++ {
		if spec.Capabilities[index].Name == "service" {
			spec.Capabilities[index].Handle = handle.ID(pair[1])
			return true
		}
	}
	handleClose(pair[1])
	return false
}

func launchService(spec supervisor.LaunchSpec) (uint64, bool) {
	// A driver that is starting again needs a channel whose other end is live.
	// The first launch uses the pair created at boot, which still is.
	if driverStarted && bytesString(spec.Name[:spec.NameSize]) == driverName && !replaceDriverChannel(&spec) {
		return 0, false
	}
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
	// The validation build keeps the driver's process handle so it can inject
	// the fault that proves a crashed driver is replaced. Every other service's
	// handle is closed: PID 1 supervises through lifecycle events, not handles.
	keep := validation.Enabled && name == driverName && driverProcessHandle == 0
	if keep && status == ipc.StatusOK && event.Code == uint64(supervisor.LaunchOK) {
		driverProcessHandle = event.ProcessHandle
	}
	if status != ipc.StatusOK || event.Code != uint64(supervisor.LaunchOK) || !keep && handleClose(uint64(event.ProcessHandle)) != 0 {
		write("[FAIL] " + name + "\n")
		syslog.Error("service launch failed", syslog.Str("service", name), syslog.Uint("status", event.Code))
		return 0, false
	}
	mark("INIT_SERVICE_STARTED_OK\n")
	write("[ OK ] " + name + "\n")
	syslog.Info("service started", syslog.Str("service", name), syslog.Uint("pid", event.Process))
	if name == driverName {
		driverStarted = true
	}
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
		sendExecReply(message.Correlation, 0, uint64(supervisor.LaunchInvalidImage), 0, 0)
		return
	}
	job := freeJob(slot)
	if job == nil {
		sendExecReply(message.Correlation, 0, uint64(supervisor.LaunchResourceExhausted), 0, 0)
		return
	}
	copy(job.data[:size], execPage[slot][:size])
	request, ok := execproto.DecodeRequest(job.data[:size])
	if !ok {
		sendExecReply(message.Correlation, 0, uint64(supervisor.LaunchInvalidImage), 0, 0)
		return
	}
	job.active, job.slot, job.correlation = true, slot, message.Correlation
	job.job = supervisor.StartPipeline(request, serviceDeviceHandle, nativePipelineRunner{})
	// A foreground job's input is fed by the shell, which receives the writing
	// end before the job's reply. If it cannot be delivered, closing it gives
	// the job end of input instead of a stream no one will ever write.
	if input := job.job.TakeInput(); input != 0 {
		started, ok := execproto.EncodeStarted(slot, input, message.Correlation)
		if !ok || !sendMessage(execServerHandle, started) {
			handleClose(uint64(input))
		}
	}
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
	stage := uint8(0)
	if launchStatus != supervisor.LaunchOK {
		stage = job.job.FailedStage()
	}
	sendExecReply(job.correlation, pid, uint64(launchStatus), exitCode, stage)
}

// handleControl answers the shell's requests that launch nothing.
func handleControl(message ipc.Message) {
	operation, argument, ok := execproto.DecodeControl(message)
	switch {
	case !ok:
		sendControlStatus(execproto.ControlInvalid, message.Correlation)
	case operation == execproto.ControlKill:
		sendControlStatus(killJob(argument), message.Correlation)
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

// killJob stops every stage of the job in one slot. The kernel retires their
// tasks, so the reply to the job itself still arrives through the normal path.
func killJob(slot uint8) execproto.ControlStatus {
	for index := range execJobs {
		job := &execJobs[index]
		if !job.active || job.slot != slot {
			continue
		}
		stopped := false
		for _, process := range job.job.Processes() {
			if process != 0 && processKill(uint64(process)) == 0 {
				stopped = true
			}
		}
		if !stopped {
			return execproto.ControlNotFound
		}
		syslog.Info("job stopped", syslog.Uint("slot", uint64(slot)))
		return execproto.ControlOK
	}
	return execproto.ControlNotFound
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

// openVFSSession asks VFS for a new session and returns the client endpoint the
// child will be given. PID 1 keeps no copy of either end: the server end moves
// to VFS in the request and the client end is consumed into the child's
// manifest, so no two processes ever share one session queue.
//
// The second result is empty on success and otherwise names the step that
// failed, which the caller records.
func openVFSSession() (handle.ID, string) {
	var pair [2]uint64
	if channelCreate(unsafe.Pointer(&pair[0]), 16) != 0 {
		return 0, "APP_CHANNEL_CREATE_FAILED"
	}
	correlation++
	control, ok := vfs.AddSessionRequest(handle.ID(pair[1]), correlation)
	if !ok || !sendMessage(vfsControlHandle, control) {
		closePair(pair)
		return 0, "APP_VFS_REQUEST_FAILED"
	}
	controlReply, status := receive(vfsControlHandle, vfsSessionDeadline)
	vfsStatus, valid := vfs.DecodeAddSessionReply(controlReply)
	if status != ipc.StatusOK || !valid || vfsStatus != vfs.StatusOK {
		handleClose(pair[0])
		if status != ipc.StatusOK {
			return 0, "APP_VFS_RECEIVE_STATUS"
		}
		if !valid {
			return 0, "APP_VFS_REPLY_INVALID"
		}
		return 0, "APP_VFS_RESOURCE_EXHAUSTED"
	}
	return handle.ID(pair[0]), ""
}

func launchApplication(path string, command, cwd, arguments, environment []byte, streams supervisor.Streams) (uint64, handle.ID, supervisor.LaunchStatus) {
	var pair [2]uint64
	useVFS := commandGrants(command)&commands.GrantVFS != 0
	if useVFS {
		session, reason := openVFSSession()
		if reason != "" {
			syslog.Error("APP_VFS_SESSION_FAILED")
			syslog.Error(reason)
			if reason == "APP_CHANNEL_CREATE_FAILED" {
				return 0, 0, supervisor.LaunchResourceExhausted
			}
			return 0, 0, supervisor.LaunchCapabilityInstallFailed
		}
		pair[0] = uint64(session)
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

// The command index is read once, through a VFS session of PID 1's own, from
// the immutable system image. What a command is granted beyond its standard
// streams is decided from the manifest it was built with, not from a list of
// names kept here (ADR 0080). A command the index does not describe gets the
// standard streams only.
var (
	commandIndex       [commands.MaxIndex]byte
	commandIndexSize   int
	commandIndexLoaded bool
	commandIndexPath   = []byte(commands.IndexPath)
)

func commandGrants(command []byte) commands.Grant {
	if !commandIndexLoaded {
		loadCommandIndex()
	}
	entry, found := commands.Lookup(commandIndex[:commandIndexSize], command)
	if !found {
		return 0
	}
	return entry.Grants
}

// loadCommandIndex reads the index, or leaves it unloaded so the next launch
// tries again: a VFS that is briefly unavailable must not cost the commands
// their grants for the rest of the boot.
func loadCommandIndex() {
	session, reason := openVFSSession()
	if reason != "" {
		syslog.Warn("INIT_COMMAND_INDEX_UNAVAILABLE")
		return
	}
	size, ok := readSessionFile(session, commandIndexPath, commandIndex[:])
	handleClose(uint64(session))
	if !ok {
		syslog.Warn("INIT_COMMAND_INDEX_UNREADABLE")
		return
	}
	commandIndexSize, commandIndexLoaded = size, true
	mark("INIT_COMMAND_INDEX_OK\n")
}

// readSessionFile reads the whole of path into target and refuses a file that
// does not fit, rather than using a prefix of it.
func readSessionFile(session handle.ID, path []byte, target []byte) (int, bool) {
	opened, ok := callSession(session, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	if !ok || opened.Status != vfs.StatusOK {
		return 0, false
	}
	size := 0
	for {
		want := uint32(vfs.MaxReplyData)
		if remaining := len(target) - size; remaining < int(want) {
			// Ask for one byte past a full buffer, so an oversized file is
			// detected instead of silently truncated.
			want = uint32(remaining) + 1
		}
		reply, ok := callSession(session, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: want})
		if !ok || reply.Status != vfs.StatusOK || size+len(reply.Data) > len(target) {
			return 0, false
		}
		if len(reply.Data) == 0 {
			return size, true
		}
		size += copy(target[size:], reply.Data)
	}
}

func callSession(session handle.ID, request vfs.Request) (vfs.Reply, bool) {
	correlation++
	message, ok := vfs.EncodeRequest(request, correlation)
	if !ok || !sendMessage(uint64(session), message) {
		return vfs.Reply{}, false
	}
	reply, status := receive(uint64(session), vfsSessionDeadline)
	if status != ipc.StatusOK || reply.Correlation != correlation {
		return vfs.Reply{}, false
	}
	decoded, ok := vfs.DecodeReply(reply)
	return decoded, ok && decoded.Operation == request.Operation
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

func sendExecReply(correlation, pid, status, exitCode uint64, stage uint8) {
	reply, ok := execproto.EncodeReply(execproto.Reply{Process: pid, LaunchStatus: status, ExitCode: exitCode, Stage: stage}, correlation)
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
