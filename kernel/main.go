//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/internal/logfmt"
	"goos/internal/validation"
	"goos/internal/version"
	"goos/kernel/arch/amd64"
	"goos/kernel/boot"
	bootuefi "goos/kernel/boot/uefi"
	"goos/kernel/clock"
	"goos/kernel/device"
	"goos/kernel/diag"
	"goos/kernel/handle"
	"goos/kernel/initramfs"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/loader"
	"goos/kernel/mm/pmm"
	"goos/kernel/mm/vm"
	"goos/kernel/pci"
	"goos/kernel/process"
	"goos/kernel/ramfs"
	"goos/kernel/sched"
	"goos/kernel/shmem"
	"goos/kernel/stream"
	"goos/kernel/vfs"
	kernelwait "goos/kernel/wait"
	supervisor "goos/userspace/init"
	"goos/userspace/startup"
)

var (
	bootInfo                boot.Info
	pageAllocator           pmm.Allocator
	demoTaskA               uint64
	demoTaskB               uint64
	demoABlocked            uint64
	demoAResumed            uint64
	userExited              uint64
	kernelCallData          [kcall.MaxDebugWrite]byte
	nativeImageData         [1024]byte
	faultImageData          [1024]byte
	boundedChannel          ipc.Channel
	ipcWireData             [ipc.WireMaxSize]byte
	sharedRegistry          shmem.Registry
	channelRegistry         ipc.Registry
	streamRegistry          stream.Registry
	streamCallData          [kcall.MaxStreamIO]byte
	lifecycleChannel        *ipc.Channel
	lifecycleChannelID      uint64
	vfsChannelID            uint64
	bootProcessA            *process.Process
	bootProcessB            *process.Process
	bootInitProcess         *process.Process
	processRegistry         process.Registry
	ipcWaitSet              ipc.WaitSet
	ipcWakeResults          [ipc.MaxWaiters]ipc.WaitResult
	ipcWaitStatus           [sched.MaxTasks]kcall.Status
	processWaitSet          process.WaitSet
	processWakeResults      [process.MaxWaiters]process.WaitResult
	processWaitStatus       [sched.MaxTasks]kcall.Status
	streamWaitSet           ipc.WaitSet
	streamWakeResults       [ipc.MaxWaiters]ipc.WaitResult
	streamWaitStatus        [sched.MaxTasks]kcall.Status
	waitAnyTable            kernelwait.Table
	waitAnyCompletions      [sched.MaxTasks]kernelwait.Completion
	waitAnyEntries          [kernelwait.MaxObjects]kernelwait.Entry
	waitAnyWire             [kernelwait.MaxObjects * int(kcall.WaitAnyInterestSize)]byte
	bootRAMFS               ramfs.FileSystem
	deviceRAMFS             ramfs.FileSystem
	bootVFS                 vfs.Namespace
	ramfsReadback           [16]byte
	vfsReadback             [16]byte
	pendingLifecycle        [maxPendingLifecycle]ipc.Message
	pendingLifecycleHead    uint8
	pendingLifecycleCount   uint8
	lifecycleOverflows      uint64
	initAnnounced           uint64
	processExitDelivered    uint64
	processReclaimConfirmed uint64
	// Teardown steps that may fail are counted rather than allowed to abort
	// delivery, and reported once the boot sequence has settled.
	taskStackReclaimFailures    uint64
	addressSpaceReclaimFailures uint64
	processWaitConfirmed        uint64
	supervisorExitDelivered     uint64
	serviceLaunchConfirmed      uint64
	userspaceSupervisorOK       uint64
	capabilityManifestOK        uint64
	startupABIConfirmed         uint64
	initSendObserved            uint64
	initSendStatus              kcall.Status
	initExitObserved            uint64
	initExitCode                uint64
	kernelPageTable             uint64
	spawnedProcesses            [process.MaxProcesses]process.Process
	nextProcessID               process.ID = 4
	virtioBlockPort             uint16
)

type nativeCallHandler struct {
	root uint64
}

type pciConfig struct{}

func (pciConfig) Read32(bus, slot, function, offset uint8) uint32 {
	return amd64.PCIConfigRead32(bus, slot, function, offset)
}

func (handler nativeCallHandler) Exit(code uint64) kcall.Status {
	amd64.SetCurrentTaskExitCode(code)
	if bootProcessA != nil && handler.root == bootProcessA.AddressSpace().Root {
		userExited = 1
	}
	mark("USER_EXIT_OK")
	return kcall.StatusOK
}

func (handler nativeCallHandler) DebugWrite(address uint64, length uint64) kcall.Status {
	console := amd64.NewCOM1()
	if (bootProcessA == nil || handler.root != bootProcessA.AddressSpace().Root) &&
		(bootInitProcess == nil || handler.root != bootInitProcess.AddressSpace().Root) {
		return kcall.StatusPermissionDenied
	}
	data := kernelCallData[:int(length)]
	if !vm.CopyIn(handler.root, address, data) {
		mark("USER_COPYIN_OK")
		return kcall.StatusInvalidArgument
	}
	for _, value := range data {
		console.WriteByte(value)
	}
	if bootInitProcess != nil && handler.root == bootInitProcess.AddressSpace().Root {
		initAnnounced = 1
		if equalBytes(data, "USERSPACE_SUPERVISOR_OK\n") {
			userspaceSupervisorOK = 1
		}
	}
	return kcall.StatusOK
}

func deliverProcessExits() {
	for task := amd64.TakeExitedTask(); task != 0; task = amd64.TakeExitedTask() {
		taskID := sched.TaskID(task)
		if waitAnyTable.Cancel(taskID) && diag.Enabled {
			trace(diag.WaitCancelExit, pidOfTask(taskID), task, 0, 0)
		}
		event, report := processRegistry.ReportTaskExit(taskID, amd64.TaskExitCode(task))
		// Reclaiming the guarded kernel stack is independent bookkeeping. It must
		// not gate the steps below: a process that has exited has to release its
		// waiters and its handles even if its stack cannot be returned, or its
		// peers wait forever for an EOF that never arrives.
		if !amd64.ReclaimExitedTask(&pageAllocator, kernelPageTable, task) {
			taskStackReclaimFailures++
			if taskStackReclaimFailures == 1 {
				kernelLog(logfmt.Warn, "TASK_STACK_RECLAIM_FAILED", nil)
			}
		}
		if report != process.ExitProcessEnded {
			continue
		}
		releaseExitedProcess(event)
	}
}

// releaseExitedProcess performs the observable half of process teardown first,
// so that waiters and peers are released before any cleanup that may fail.
func releaseExitedProcess(event process.ExitEvent) {
	processExitDelivered = 1
	trace(diag.ProcessExited, event.Process, uint64(event.Task), 0, event.Code)
	if waited, status := processRegistry.Wait(event.Process); status == process.WaitExited && waited == event {
		processWaitConfirmed = 1
	}
	wakeProcessWaiters(event.Process)
	if event.Process == 1 {
		initExitObserved = 1
		initExitCode = event.Code
		return
	}
	if lifecycleChannel != nil {
		message, encoded := supervisor.ProcessExited(uint64(event.Process), event.Code)
		if encoded && queueLifecycle(message) && event.Process == 3 {
			supervisorExitDelivered = 1
		}
	}
	if exited, found := processRegistry.ByID(event.Process); found {
		releaseProcessHandles(exited)
		before := pageAllocator.Statistics().FreePages
		if !vm.DestroyUserAddressSpace(&pageAllocator, exited.AddressSpace().Root) {
			addressSpaceReclaimFailures++
			if addressSpaceReclaimFailures == 1 {
				kernelLog(logfmt.Warn, "ADDRESS_SPACE_RECLAIM_FAILED", nil)
			}
		} else if pageAllocator.Statistics().FreePages > before {
			processReclaimConfirmed = 1
		}
	}
	processRegistry.Remove(event.Process)
	for index := range spawnedProcesses {
		if spawnedProcesses[index].ID() == event.Process {
			spawnedProcesses[index].Reset()
			break
		}
	}
}

// releaseProcessHandles closes everything the exited process held so that the
// peer of each channel and stream observes closure instead of blocking.
func releaseProcessHandles(exited *process.Process) {
	exited.Handles().CloseAll(func(typeID handle.Type, cap capability.Capability) {
		if typeID == handle.TypeDevice {
			if _, dma := device.DecodeDMAObject(cap.Object()); dma && virtioBlockPort != 0 {
				amd64.PortWrite(virtioBlockPort+18, 1, 0)
				amd64.QuiesceIRQ11()
			}
		}
		if typeID == handle.TypeStream {
			closeStreamCapability(cap)
			return
		}
		if typeID != handle.TypeChannel {
			return
		}
		channelID, side, valid := ipc.DecodeEndpointObject(cap.Object())
		channel, exists := channelRegistry.Lookup(channelID)
		if valid && exists && channel.Close(side) {
			trace(diag.ChannelClosed, exited.ID(), 0, channelID|uint64(side)<<63, diag.CloseByTeardown)
			wakeIPCWaiters(channelID, side^1, ipc.WaitPeerDied)
			channelRegistry.Release(channelID)
		}
	})
}

// maxPendingLifecycle bounds the lifecycle messages retained for retry. PID 1
// launches one program at a time and blocks on the result, so a short queue
// covers a launch reply plus the exit notifications that may collide with it.
const maxPendingLifecycle = 8

// queueLifecycle delivers a lifecycle message, retaining it for retry when the
// channel is momentarily full. PID 1 blocks waiting for these, so dropping one
// strands it until its deadline expires.
func queueLifecycle(message ipc.Message) bool {
	if pendingLifecycleCount == 0 && sendLifecycle(message) {
		return true
	}
	if pendingLifecycleCount == maxPendingLifecycle {
		lifecycleOverflows++
		if lifecycleOverflows == 1 {
			kernelLog(logfmt.Error, "LIFECYCLE_QUEUE_OVERFLOW", nil)
		}
		return false
	}
	tail := (int(pendingLifecycleHead) + int(pendingLifecycleCount)) % maxPendingLifecycle
	pendingLifecycle[tail] = message
	pendingLifecycleCount++
	return true
}

func flushLifecycle() {
	for pendingLifecycleCount != 0 {
		if !sendLifecycle(pendingLifecycle[pendingLifecycleHead]) {
			return
		}
		pendingLifecycle[pendingLifecycleHead] = ipc.Message{}
		pendingLifecycleHead = (pendingLifecycleHead + 1) % maxPendingLifecycle
		pendingLifecycleCount--
	}
}

func sendLifecycle(message ipc.Message) bool {
	if lifecycleChannel == nil || lifecycleChannel.Send(0, message, nil, nil, nil) != ipc.StatusOK {
		return false
	}
	wakeIPCWaiters(lifecycleChannelID, 1, ipc.WaitMessage)
	return true
}

func consumeInitControl() {
	// Retained replies are delivered before new work is accepted, so that the
	// caller waiting on one is never overtaken by a later request.
	if lifecycleChannel == nil || pendingLifecycleCount != 0 {
		return
	}
	message, status := lifecycleChannel.Receive(0)
	if status != ipc.StatusOK {
		return
	}
	event, valid := supervisor.DecodeLifecycle(message)
	if !valid || event.Operation != supervisor.LifecycleLaunch {
		// Answer anyway: the sender is blocked on this correlation.
		if reply, encoded := supervisor.LaunchResult(0, 0, supervisor.LaunchInvalidImage, message.Correlation); encoded && message.Correlation != 0 {
			queueLifecycle(reply)
		}
		return
	}
	pid, processHandle, launchStatus := spawnExecutable(event.ExecutablePath(), event.Capabilities[:event.CapabilityCount], event.Startup[:event.StartupSize])
	if launchStatus != supervisor.LaunchOK {
		level := logfmt.Warn
		if launchStatus == supervisor.LaunchNotFound {
			level = logfmt.Info
		}
		kernelLogUint(level, "launch failed", "status", uint64(launchStatus))
	}
	reply, encoded := supervisor.LaunchResult(uint64(pid), processHandle, launchStatus, message.Correlation)
	if encoded && queueLifecycle(reply) {
		serviceLaunchConfirmed++
	}
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

func spawnExecutable(path string, grants []supervisor.Capability, startupData []byte) (process.ID, handle.ID, supervisor.LaunchStatus) {
	if kernelPageTable == 0 || nextProcessID == 0 {
		return 0, 0, supervisor.LaunchResourceExhausted
	}
	if bootInitProcess == nil || bootInitProcess.Handles().Available() == 0 || len(grants) > supervisor.MaxCapabilities {
		return 0, 0, supervisor.LaunchCapabilityDenied
	}
	for _, grant := range grants {
		source, allowed := bootInitProcess.Handles().Lookup(grant.Handle, grant.Type, capability.Transfer)
		if !allowed {
			kernelLogGrant("INIT_GRANT_DENIED", grant.Name)
			return 0, 0, supervisor.LaunchCapabilityDenied
		}
		if _, allowed = source.Derive(grant.Rights); !allowed {
			kernelLogGrant("INIT_GRANT_RIGHTS_DENIED", grant.Name)
			return 0, 0, supervisor.LaunchCapabilityDenied
		}
	}
	began := launchMark()
	image, found := initramfs.Find(bootuefi.Initramfs(&bootInfo), path[1:])
	began = launchStageDone(launchLookup, began)
	if !found {
		return 0, 0, supervisor.LaunchNotFound
	}
	root, ok := vm.CreateUserAddressSpace(&pageAllocator, kernelPageTable)
	began = launchStageDone(launchAddressSpace, began)
	if !ok {
		return 0, 0, supervisor.LaunchAddressSpaceFailed
	}
	loaded, ok := loader.LoadNative(&pageAllocator, root, image)
	began = launchStageDone(launchLoad, began)
	if !ok {
		abandonSpawn(root, nil, false)
		return 0, 0, supervisor.LaunchInvalidImage
	}
	pid := nextProcessID
	var spawned *process.Process
	for index := range spawnedProcesses {
		if spawnedProcesses[index].ID() == 0 {
			spawned = &spawnedProcesses[index]
			break
		}
	}
	if spawned == nil {
		abandonSpawn(root, nil, false)
		return 0, 0, supervisor.LaunchResourceExhausted
	}
	*spawned, ok = process.NewWithStartup(pid, process.AddressSpace{Root: root}, startupData)
	spawned.SetName(path[lastSlash(path)+1:])
	if !ok || spawned.Handles().Available() < len(grants) {
		abandonSpawn(root, spawned, false)
		return 0, 0, supervisor.LaunchProcessFailed
	}
	var installed [supervisor.MaxCapabilities]handle.ID
	for index, grant := range grants {
		installed[index], ok = bootInitProcess.Handles().Transfer(spawned.Handles(), grant.Handle, grant.Type, grant.Rights)
		if !ok {
			abandonSpawn(root, spawned, false)
			return 0, 0, supervisor.LaunchCapabilityInstallFailed
		}
	}
	for index, grant := range grants {
		installedCapability, found := spawned.Handles().Lookup(installed[index], grant.Type, grant.Rights)
		if !found || installedCapability.Rights() != grant.Rights {
			abandonSpawn(root, spawned, false)
			return 0, 0, supervisor.LaunchCapabilityInstallFailed
		}
	}
	if len(startupData) != 0 {
		var boundWire [startup.Size]byte
		bound, boundOK := startup.BindHandles(startupData, installed[:len(grants)], &boundWire)
		if !boundOK || !spawned.SetStartup(bound) {
			abandonSpawn(root, spawned, false)
			return 0, 0, supervisor.LaunchInvalidImage
		}
		startupABIConfirmed = 1
	}
	if !processRegistry.Add(spawned) {
		abandonSpawn(root, spawned, false)
		return 0, 0, supervisor.LaunchProcessFailed
	}
	// The process handle is reserved before the task exists, so a started
	// child is never left without the handle its parent waits on.
	processHandle, ok := bootInitProcess.Handles().Insert(handle.TypeProcess, capability.New(uint64(pid), capability.Wait))
	if !ok {
		abandonSpawn(root, spawned, true)
		return 0, 0, supervisor.LaunchCapabilityInstallFailed
	}
	began = launchStageDone(launchSetup, began)
	task, ok := amd64.AddUserTask(&pageAllocator, root, loaded.Entry, loaded.StackTop)
	launchStageDone(launchTask, began)
	if !ok {
		bootInitProcess.Handles().Close(processHandle)
		abandonSpawn(root, spawned, true)
		return 0, 0, supervisor.LaunchTaskFailed
	}
	// A runnable task cannot be withdrawn; its process ID is consumed either
	// way so it is never handed out twice.
	nextProcessID++
	if !processRegistry.BindTask(pid, sched.TaskID(task)) {
		kernelLogUint(logfmt.Error, "task bind failed", "pid", uint64(pid))
		return 0, 0, supervisor.LaunchTaskFailed
	}
	trace(diag.ProcessSpawned, pid, task, 0, 0)
	if len(grants) != 0 {
		capabilityManifestOK = 1
	}
	for _, grant := range grants {
		if grant.Consume {
			bootInitProcess.Handles().Close(grant.Handle)
		}
	}
	return pid, processHandle, supervisor.LaunchOK
}

func (handler nativeCallHandler) ConsoleWrite(device uint64, address uint64, length uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	cap, ok := owner.Handles().Lookup(handle.ID(device), handle.TypeDevice, capability.Write)
	if !ok || cap.Object() != 1 || length == 0 || length > kcall.MaxDebugWrite {
		return kcall.StatusPermissionDenied
	}
	data := kernelCallData[:int(length)]
	if !vm.CopyIn(handler.root, address, data) {
		return kcall.StatusInvalidArgument
	}
	console := amd64.NewCOM1()
	for _, value := range data {
		console.WriteByte(value)
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) ConsoleRead(device uint64, address uint64, length uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found || length != 1 {
		return kcall.StatusPermissionDenied
	}
	cap, ok := owner.Handles().Lookup(handle.ID(device), handle.TypeDevice, capability.Read)
	if !ok || cap.Object() != 1 {
		return kcall.StatusPermissionDenied
	}
	value, ready := amd64.NewCOM1().TryReadByte()
	if !ready {
		return kcall.StatusNotFound
	}
	if !vm.CopyOut(handler.root, address, []byte{value}) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) ClockRead(address uint64, length uint64) kcall.Status {
	if length != 8 {
		return kcall.StatusInvalidArgument
	}
	var encoded [8]byte
	putUint64(encoded[:], amd64.TimerTicks())
	if !vm.CopyOut(handler.root, address, encoded[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) SystemInfo(address uint64, length uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found || length != kcall.SystemInfoSize {
		return kcall.StatusInvalidArgument
	}
	statistics := pageAllocator.Statistics()
	var encoded [kcall.SystemInfoSize]byte
	putUint64(encoded[0:8], uint64(owner.ID()))
	putUint64(encoded[8:16], uint64(processRegistry.Count()))
	putUint64(encoded[16:24], statistics.ManagedPages)
	putUint64(encoded[24:32], statistics.FreePages)
	putUint64(encoded[32:40], amd64.TimerTicks())
	if !vm.CopyOut(handler.root, address, encoded[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func portSpecification(specification uint64) (uint16, uint8, bool) {
	if specification>>24 != 0 {
		return 0, 0, false
	}
	offset, width := uint16(specification), uint8(specification>>16)
	return offset, width, width == 1 || width == 2 || width == 4
}

func (handler nativeCallHandler) PortRead(id uint64, specification uint64, address uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	cap, ok := owner.Handles().Lookup(handle.ID(id), handle.TypeDevice, capability.Read)
	base, size, valid := device.DecodePortObject(cap.Object())
	offset, width, specValid := portSpecification(specification)
	if !ok || !valid || !specValid || uint32(offset)+uint32(width) > uint32(size) {
		return kcall.StatusPermissionDenied
	}
	value, read := amd64.PortRead(base+offset, width)
	if !read {
		return kcall.StatusInvalidArgument
	}
	var encoded [4]byte
	putUint32(encoded[:], value)
	if !vm.CopyOut(handler.root, address, encoded[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) PortWrite(id uint64, specification uint64, value uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	cap, ok := owner.Handles().Lookup(handle.ID(id), handle.TypeDevice, capability.Write)
	base, size, valid := device.DecodePortObject(cap.Object())
	offset, width, specValid := portSpecification(specification)
	valueValid := width == 1 && value <= 0xff || width == 2 && value <= 0xffff || width == 4 && value <= 0xffffffff
	if !ok || !valid || !specValid || uint32(offset)+uint32(width) > uint32(size) || !valueValid {
		return kcall.StatusPermissionDenied
	}
	if !amd64.PortWrite(base+offset, width, uint32(value)) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) DMAAllocate(id uint64, virtual uint64, address uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found || virtual != vm.UserDMAAddress {
		return kcall.StatusInvalidArgument
	}
	for page := uint64(0); page < 4; page++ {
		if !vm.CanMapUserPage(handler.root, virtual+page*vm.PageSize) {
			return kcall.StatusInvalidArgument
		}
	}
	cap, ok := owner.Handles().Lookup(handle.ID(id), handle.TypeDevice, capability.Write)
	if _, valid := device.DecodeDMAObject(cap.Object()); !ok || !valid {
		return kcall.StatusPermissionDenied
	}
	var encoded [24]byte
	if !vm.CopyOut(handler.root, address, encoded[:]) {
		return kcall.StatusInvalidArgument
	}
	physical, allocated := pageAllocator.AllocateContiguous(4)
	if !allocated {
		return kcall.StatusResourceExhausted
	}
	for index := range kernelCallData {
		kernelCallData[index] = 0
	}
	for page := uint64(0); page < 4; page++ {
		if !vm.InitializePhysical(physical+page*vm.PageSize, kernelCallData[:]) {
			for release := uint64(0); release < 4; release++ {
				pageAllocator.Free(physical + release*vm.PageSize)
			}
			return kcall.StatusResourceExhausted
		}
	}
	for page := uint64(0); page < 4; page++ {
		if !vm.MapOwnedUserPage(handler.root, virtual+page*vm.PageSize, physical+page*vm.PageSize, vm.Permissions{User: true, Writable: true}) {
			// Mapped pages are owned and return with the address space.
			for release := page; release < 4; release++ {
				pageAllocator.Free(physical + release*vm.PageSize)
			}
			return kcall.StatusResourceExhausted
		}
	}
	putUint64(encoded[0:8], physical)
	putUint64(encoded[8:16], virtual)
	putUint64(encoded[16:24], 4*vm.PageSize)
	if !vm.CopyOut(handler.root, address, encoded[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) StartupRead(address uint64, length uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found || length != process.MaxStartupData {
		return kcall.StatusPermissionDenied
	}
	var data [process.MaxStartupData]byte
	if _, ok := owner.Startup(data[:]); !ok || !vm.CopyOut(handler.root, address, data[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) ChannelCreate(address uint64, length uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found || owner.ID() != 1 || length != 16 || owner.Handles().Available() < 2 {
		return kcall.StatusPermissionDenied
	}
	channel, id, ok := channelRegistry.Create()
	if !ok {
		return kcall.StatusResourceExhausted
	}
	object0, _ := ipc.EndpointObject(id, 0)
	object1, _ := ipc.EndpointObject(id, 1)
	rights := capability.Read | capability.Write | capability.Wait | capability.Transfer
	handle0, ok0 := owner.Handles().Insert(handle.TypeChannel, capability.New(object0, rights))
	handle1, ok1 := owner.Handles().Insert(handle.TypeChannel, capability.New(object1, rights))
	if !ok0 || !ok1 {
		if ok0 {
			owner.Handles().Close(handle0)
		}
		if ok1 {
			owner.Handles().Close(handle1)
		}
		channel.Close(0)
		channel.Close(1)
		channelRegistry.Release(id)
		return kcall.StatusResourceExhausted
	}
	var encoded [16]byte
	putUint64(encoded[0:8], uint64(handle0))
	putUint64(encoded[8:16], uint64(handle1))
	if !vm.CopyOut(handler.root, address, encoded[:]) {
		owner.Handles().Close(handle0)
		owner.Handles().Close(handle1)
		channel.Close(0)
		channel.Close(1)
		channelRegistry.Release(id)
		return kcall.StatusInvalidArgument
	}
	trace(diag.ChannelCreated, owner.ID(), 0, id, uint64(handle1))
	return kcall.StatusOK
}

func (handler nativeCallHandler) HandleClose(id uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	typeID, cap, found := owner.Handles().Describe(handle.ID(id))
	if !found {
		return kcall.StatusNotFound
	}
	if typeID == handle.TypeChannel {
		channelID, side, valid := ipc.DecodeEndpointObject(cap.Object())
		channel, exists := channelRegistry.Lookup(channelID)
		if !valid || !exists || !channel.Close(side) {
			return kcall.StatusNotFound
		}
		trace(diag.ChannelClosed, owner.ID(), id, channelID|uint64(side)<<63, diag.CloseByHandle)
		wakeIPCWaiters(channelID, side^1, ipc.WaitPeerDied)
		channelRegistry.Release(channelID)
	} else if typeID == handle.TypeStream {
		if !closeStreamCapability(cap) {
			return kcall.StatusNotFound
		}
	}
	if !owner.Handles().Close(handle.ID(id)) {
		return kcall.StatusNotFound
	}
	return kcall.StatusOK
}

func closeStreamCapability(cap capability.Capability) bool {
	streamID, writer, valid := stream.DecodeEndpointObject(cap.Object())
	endpoint, exists := streamRegistry.Lookup(streamID)
	if !valid || !exists {
		return false
	}
	closed := false
	if writer {
		closed = endpoint.CloseWriter()
	} else {
		closed = endpoint.CloseReader()
	}
	if closed {
		side := uint8(1)
		if writer {
			side = 0
		}
		wakeStreamWaiters(streamID, side, ipc.WaitPeerDied)
		streamRegistry.Release(streamID)
	}
	return closed
}

func (handler nativeCallHandler) StreamCreate(address uint64, length uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found || length != 16 || owner.Handles().Available() < 2 {
		return kcall.StatusPermissionDenied
	}
	endpoint, id, created := streamRegistry.Create()
	if !created {
		return kcall.StatusResourceExhausted
	}
	readerObject, _ := stream.ReaderObject(id)
	writerObject, _ := stream.WriterObject(id)
	reader, readerOK := owner.Handles().Insert(handle.TypeStream, capability.New(readerObject, capability.Read|capability.Wait|capability.Transfer))
	writer, writerOK := owner.Handles().Insert(handle.TypeStream, capability.New(writerObject, capability.Write|capability.Wait|capability.Transfer))
	if !readerOK || !writerOK {
		if readerOK {
			owner.Handles().Close(reader)
		}
		if writerOK {
			owner.Handles().Close(writer)
		}
		endpoint.CloseReader()
		endpoint.CloseWriter()
		streamRegistry.Release(id)
		return kcall.StatusResourceExhausted
	}
	var encoded [16]byte
	putUint64(encoded[0:8], uint64(reader))
	putUint64(encoded[8:16], uint64(writer))
	if !vm.CopyOut(handler.root, address, encoded[:]) {
		owner.Handles().Close(reader)
		owner.Handles().Close(writer)
		endpoint.CloseReader()
		endpoint.CloseWriter()
		streamRegistry.Release(id)
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) streamEndpoint(id uint64, required capability.Rights, writer bool) (*stream.Stream, kcall.Status) {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return nil, kcall.StatusPermissionDenied
	}
	cap, allowed := owner.Handles().Lookup(handle.ID(id), handle.TypeStream, required)
	streamID, isWriter, valid := stream.DecodeEndpointObject(cap.Object())
	endpoint, exists := streamRegistry.Lookup(streamID)
	if !allowed || !valid || !exists || isWriter != writer {
		return nil, kcall.StatusPermissionDenied
	}
	return endpoint, kcall.StatusOK
}

func streamStatus(status stream.Status) kcall.Status {
	switch status {
	case stream.StatusOK:
		return kcall.StatusOK
	case stream.StatusWouldBlock:
		return kcall.StatusResourceExhausted
	case stream.StatusPeerClosed:
		return kcall.StatusPeerDied
	default:
		return kcall.StatusInvalidArgument
	}
}

func streamDescriptor(root, address, length uint64) (uint64, uint64, bool) {
	if length != kcall.StreamDescriptorSize {
		return 0, 0, false
	}
	var descriptor [24]byte
	if !vm.CopyIn(root, address, descriptor[:]) {
		return 0, 0, false
	}
	buffer, size := decodeUint64(descriptor[0:8]), decodeUint64(descriptor[8:16])
	if size == 0 || size > kcall.MaxStreamIO || decodeUint64(descriptor[16:24]) != 0 {
		return 0, 0, false
	}
	return buffer, size, true
}

func writeStreamResult(root, descriptor, size uint64) bool {
	var encoded [8]byte
	putUint64(encoded[:], size)
	return vm.CopyOut(root, descriptor+16, encoded[:])
}

func (handler nativeCallHandler) StreamRead(id uint64, descriptor uint64, length uint64) kcall.Status {
	endpoint, status := handler.streamEndpoint(id, capability.Read, false)
	buffer, size, valid := streamDescriptor(handler.root, descriptor, length)
	if status != kcall.StatusOK || !valid {
		if status != kcall.StatusOK {
			return status
		}
		return kcall.StatusInvalidArgument
	}
	if !vm.CanCopyOut(handler.root, buffer, size) || !vm.CanCopyOut(handler.root, descriptor+16, 8) {
		return kcall.StatusInvalidArgument
	}
	count, result := endpoint.Read(streamCallData[:size])
	if result != stream.StatusOK {
		return streamStatus(result)
	}
	if !vm.CopyOut(handler.root, buffer, streamCallData[:count]) || !writeStreamResult(handler.root, descriptor, uint64(count)) {
		return kcall.StatusInvalidArgument
	}
	cap, _ := processRegistry.ByRoot(handler.root)
	streamCap, _ := cap.Handles().Lookup(handle.ID(id), handle.TypeStream, capability.Read)
	streamID, _, _ := stream.DecodeEndpointObject(streamCap.Object())
	wakeStreamWaiters(streamID, 1, ipc.WaitMessage)
	return kcall.StatusOK
}

func (handler nativeCallHandler) StreamWrite(id uint64, descriptor uint64, length uint64) kcall.Status {
	endpoint, status := handler.streamEndpoint(id, capability.Write, true)
	buffer, size, valid := streamDescriptor(handler.root, descriptor, length)
	if status != kcall.StatusOK || !valid {
		if status != kcall.StatusOK {
			return status
		}
		return kcall.StatusInvalidArgument
	}
	if !vm.CopyIn(handler.root, buffer, streamCallData[:size]) {
		return kcall.StatusInvalidArgument
	}
	count, result := endpoint.Write(streamCallData[:size])
	if result != stream.StatusOK {
		return streamStatus(result)
	}
	if !writeStreamResult(handler.root, descriptor, uint64(count)) {
		return kcall.StatusInvalidArgument
	}
	owner, _ := processRegistry.ByRoot(handler.root)
	streamCap, _ := owner.Handles().Lookup(handle.ID(id), handle.TypeStream, capability.Write)
	streamID, _, _ := stream.DecodeEndpointObject(streamCap.Object())
	wakeStreamWaiters(streamID, 0, ipc.WaitMessage)
	return kcall.StatusOK
}

func (handler nativeCallHandler) endpoint(channel uint64, required capability.Rights) (*ipc.Channel, uint8, *process.Process, *process.Process, kcall.Status) {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return nil, 0, nil, nil, kcall.StatusPermissionDenied
	}
	cap, ok := owner.Handles().Lookup(handle.ID(channel), handle.TypeChannel, required)
	if !ok {
		return nil, 0, nil, nil, kcall.StatusPermissionDenied
	}
	channelID, side, valid := ipc.DecodeEndpointObject(cap.Object())
	endpoint, ok := channelRegistry.Lookup(channelID)
	if !valid || !ok {
		return nil, 0, nil, nil, kcall.StatusNotFound
	}
	peerObject, _ := ipc.EndpointObject(channelID, side^1)
	peer, _ := processRegistry.ByCapabilityObject(handle.TypeChannel, peerObject)
	return endpoint, side, owner, peer, kcall.StatusOK
}

func (handler nativeCallHandler) IPCSend(channel uint64, address uint64, length uint64) kcall.Status {
	endpoint, side, owner, peer, status := handler.endpoint(channel, capability.Write)
	if status != kcall.StatusOK {
		handler.recordInitSend(status)
		return status
	}
	if length < ipc.WireHeaderSize || length > ipc.WireMaxSize {
		handler.recordInitSend(kcall.StatusInvalidArgument)
		return kcall.StatusInvalidArgument
	}
	data := ipcWireData[:int(length)]
	if !vm.CopyIn(handler.root, address, data) {
		handler.recordInitSend(kcall.StatusInvalidArgument)
		return kcall.StatusInvalidArgument
	}
	message, ok := ipc.Decode(data)
	if !ok {
		handler.recordInitSend(kcall.StatusCorruptData)
		return kcall.StatusCorruptData
	}
	var peerHandles *handle.Table
	if peer != nil {
		peerHandles = peer.Handles()
	}
	var transfers [ipc.MaxAttachedHandles]ipc.Transfer
	transferCount := int(message.HandleCount)
	if transferCount != 0 && peerHandles == nil {
		return kcall.StatusPeerDied
	}
	for index := 0; index < transferCount; index++ {
		typeID, attached, found := owner.Handles().Describe(message.Handles[index])
		if !found || !attached.Allows(capability.Transfer) {
			return kcall.StatusPermissionDenied
		}
		rights := attached.Rights() &^ capability.Transfer
		if rights == 0 {
			return kcall.StatusPermissionDenied
		}
		transfers[index] = ipc.Transfer{Handle: message.Handles[index], Type: typeID, Rights: rights}
	}
	message.HandleCount = 0
	for index := range message.Handles {
		message.Handles[index] = 0
	}
	status = endpoint.Send(side, message, owner.Handles(), peerHandles, transfers[:transferCount])
	if status == ipc.StatusOK {
		channelCapability, found := owner.Handles().Lookup(handle.ID(channel), handle.TypeChannel, capability.Write)
		if found {
			channelID, _, _ := ipc.DecodeEndpointObject(channelCapability.Object())
			wakeIPCWaiters(channelID, side^1, ipc.WaitMessage)
		}
		for index := 0; index < transferCount; index++ {
			if diag.Enabled && peer != nil {
				trace(diag.HandleTransferred, owner.ID(), uint64(transfers[index].Handle), channel, uint64(peer.ID()))
			}
			owner.Handles().Close(transfers[index].Handle)
		}
	}
	handler.recordInitSend(status)
	return status
}

func (handler nativeCallHandler) recordInitSend(status kcall.Status) {
	if bootInitProcess != nil && handler.root == bootInitProcess.AddressSpace().Root {
		initSendStatus = status
		initSendObserved = 1
	}
}

func waitReceive(handler nativeCallHandler, channel uint64, descriptor uint64, size uint64, task uint64) (kcall.Status, bool) {
	if size != 24 || task >= sched.MaxTasks {
		return kcall.StatusInvalidArgument, false
	}
	taskID := sched.TaskID(amd64.TaskIDForSlot(task))
	if _, _, valid := taskID.Parts(); !valid {
		return kcall.StatusInvalidArgument, false
	}
	if ipcWaitStatus[task] != kcall.StatusOK {
		status := ipcWaitStatus[task]
		ipcWaitStatus[task] = kcall.StatusOK
		return status, false
	}
	var data [24]byte
	if !vm.CopyIn(handler.root, descriptor, data[:]) {
		return kcall.StatusInvalidArgument, false
	}
	buffer := decodeUint64(data[0:8])
	capacity := decodeUint64(data[8:16])
	timeout := decodeUint64(data[16:24])
	status := handler.IPCReceive(channel, buffer, capacity)
	if status != kcall.StatusNotFound {
		return status, false
	}
	if timeout == 0 {
		return kcall.StatusTimeout, false
	}
	endpoint, side, owner, _, endpointStatus := handler.endpoint(channel, capability.Read|capability.Wait)
	if endpointStatus != kcall.StatusOK || endpoint == nil {
		return endpointStatus, false
	}
	cap, found := owner.Handles().Lookup(handle.ID(channel), handle.TypeChannel, capability.Read|capability.Wait)
	if !found {
		return kcall.StatusPermissionDenied, false
	}
	now := amd64.TimerTicks()
	deadline := now + timeout
	channelID, _, valid := ipc.DecodeEndpointObject(cap.Object())
	if !valid || deadline < now || !ipcWaitSet.Add(taskID, channelID, side, deadline) {
		return kcall.StatusResourceExhausted, false
	}
	// Re-check after registering, because a message sent between the receive
	// above and the registration would wake nobody.
	if status := handler.IPCReceive(channel, buffer, capacity); status != kcall.StatusNotFound {
		ipcWaitSet.Cancel(taskID)
		// A waker may already have removed the registration and stored a resume
		// status. It must not surface on the next wait, which this call answers.
		ipcWaitStatus[task] = kcall.StatusOK
		return status, false
	}
	return kcall.StatusNotFound, true
}

func decodeUint64(data []byte) uint64 {
	return uint64(data[0]) | uint64(data[1])<<8 | uint64(data[2])<<16 | uint64(data[3])<<24 |
		uint64(data[4])<<32 | uint64(data[5])<<40 | uint64(data[6])<<48 | uint64(data[7])<<56
}

func putUint64(data []byte, value uint64) {
	for index := 0; index < 8; index++ {
		data[index] = byte(value >> uint(index*8))
	}
}

func putUint32(data []byte, value uint32) {
	data[0] = byte(value)
	data[1] = byte(value >> 8)
	data[2] = byte(value >> 16)
	data[3] = byte(value >> 24)
}

func wakeIPCWaiters(channel uint64, side uint8, reason ipc.WaitReason) {
	if channel == lifecycleChannelID && side == 0 {
		amd64.WakeTask(0)
	}
	event := kernelwait.Readable
	if reason == ipc.WaitPeerDied {
		event = kernelwait.PeerClosed
	}
	publishWaitAny(kernelwait.Key{Kind: kernelwait.KindChannel, Object: channel, Side: side}, event)
	count := ipcWaitSet.Wake(channel, side, reason, ipcWakeResults[:])
	for index := 0; index < count; index++ {
		task, _, valid := ipcWakeResults[index].Task.Parts()
		if !valid {
			ipcWakeResults[index] = ipc.WaitResult{}
			continue
		}
		if reason == ipc.WaitPeerDied {
			ipcWaitStatus[task] = kcall.StatusPeerDied
		}
		if !amd64.WakeTaskID(uint64(ipcWakeResults[index].Task)) {
			console := amd64.NewCOM1()
			console.WriteString("PANIC: stale IPC wake task\n")
			for {
				amd64.Halt()
			}
		}
		ipcWakeResults[index] = ipc.WaitResult{}
	}
}

func expireIPCWaiters() {
	count := ipcWaitSet.Expire(amd64.TimerTicks(), ipcWakeResults[:])
	for index := 0; index < count; index++ {
		task, _, valid := ipcWakeResults[index].Task.Parts()
		if !valid {
			ipcWakeResults[index] = ipc.WaitResult{}
			continue
		}
		ipcWaitStatus[task] = kcall.StatusTimeout
		if !amd64.WakeTaskID(uint64(ipcWakeResults[index].Task)) {
			console := amd64.NewCOM1()
			console.WriteString("PANIC: stale IPC timeout task\n")
			for {
				amd64.Halt()
			}
		}
		ipcWakeResults[index] = ipc.WaitResult{}
	}
}

func wakeStreamWaiters(streamID uint64, side uint8, reason ipc.WaitReason) {
	event := kernelwait.Readable
	if side == 1 {
		event = kernelwait.Writable
	}
	if reason == ipc.WaitPeerDied {
		event = kernelwait.PeerClosed
	}
	publishWaitAny(kernelwait.Key{Kind: kernelwait.KindStream, Object: streamID, Side: side}, event)
	count := streamWaitSet.Wake(streamID, side, reason, streamWakeResults[:])
	for index := 0; index < count; index++ {
		task, _, valid := streamWakeResults[index].Task.Parts()
		if !valid {
			streamWakeResults[index] = ipc.WaitResult{}
			continue
		}
		if reason == ipc.WaitPeerDied {
			streamWaitStatus[task] = kcall.StatusPeerDied
		}
		if !amd64.WakeTaskID(uint64(streamWakeResults[index].Task)) {
			panicSerial("stale stream wake task")
		}
		streamWakeResults[index] = ipc.WaitResult{}
	}
}

func panicSerial(message string) {
	console := amd64.NewCOM1()
	console.WriteString("PANIC: ")
	console.WriteString(message)
	console.WriteString("\n")
	for {
		amd64.Halt()
	}
}

func expireStreamWaiters() {
	count := streamWaitSet.Expire(amd64.TimerTicks(), streamWakeResults[:])
	for index := 0; index < count; index++ {
		task, _, valid := streamWakeResults[index].Task.Parts()
		if !valid {
			streamWakeResults[index] = ipc.WaitResult{}
			continue
		}
		streamWaitStatus[task] = kcall.StatusTimeout
		if !amd64.WakeTaskID(uint64(streamWakeResults[index].Task)) {
			panicSerial("stale stream timeout task")
		}
		streamWakeResults[index] = ipc.WaitResult{}
	}
}

func waitStream(handler nativeCallHandler, id, timeout, task uint64) (kcall.Status, bool) {
	if task >= sched.MaxTasks || timeout == 0 {
		return kcall.StatusInvalidArgument, false
	}
	taskID := sched.TaskID(amd64.TaskIDForSlot(task))
	if _, _, valid := taskID.Parts(); !valid {
		return kcall.StatusInvalidArgument, false
	}
	if streamWaitStatus[task] != kcall.StatusOK {
		status := streamWaitStatus[task]
		streamWaitStatus[task] = kcall.StatusOK
		return status, false
	}
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied, false
	}
	cap, allowed := owner.Handles().Lookup(handle.ID(id), handle.TypeStream, capability.Wait)
	streamID, writer, valid := stream.DecodeEndpointObject(cap.Object())
	endpoint, exists := streamRegistry.Lookup(streamID)
	if !allowed || !valid || !exists {
		return kcall.StatusPermissionDenied, false
	}
	status := endpoint.ReadStatus()
	side := uint8(0)
	if writer {
		status, side = endpoint.WriteStatus(), 1
	}
	if status != stream.StatusWouldBlock {
		return streamStatus(status), false
	}
	now := amd64.TimerTicks()
	deadline := now + timeout
	if deadline < now || !streamWaitSet.Add(taskID, streamID, side, deadline) {
		return kcall.StatusResourceExhausted, false
	}
	// Re-check after registering: data, buffer space, or peer closure arriving
	// in between would wake nobody and strand this task until its deadline.
	if writer {
		status = endpoint.WriteStatus()
	} else {
		status = endpoint.ReadStatus()
	}
	if status != stream.StatusWouldBlock {
		streamWaitSet.Cancel(taskID)
		streamWaitStatus[task] = kcall.StatusOK
		return streamStatus(status), false
	}
	return kcall.StatusNotFound, true
}

//export goos_dispatch_stream_wait
func dispatchStreamWait(id, timeout, rootAndTask uint64) uint64 {
	status, blocked := waitStream(nativeCallHandler{root: rootAndTask &^ 0xfff}, id, timeout, rootAndTask&0xfff)
	action := kcall.ActionReturn
	if blocked {
		action = kcall.ActionBlock
	}
	return uint64(action)<<32 | uint64(status)
}

func waitProcess(handler nativeCallHandler, processHandle uint64, resultAddress uint64, timeout uint64, task uint64) (kcall.Status, bool) {
	if task >= sched.MaxTasks || !vm.ValidateUserRange(resultAddress, 8) {
		return kcall.StatusInvalidArgument, false
	}
	taskID := sched.TaskID(amd64.TaskIDForSlot(task))
	if _, _, valid := taskID.Parts(); !valid {
		return kcall.StatusInvalidArgument, false
	}
	if processWaitStatus[task] != kcall.StatusOK {
		status := processWaitStatus[task]
		processWaitStatus[task] = kcall.StatusOK
		return status, false
	}
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied, false
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(processHandle), handle.TypeProcess, capability.Wait)
	if !allowed {
		return kcall.StatusPermissionDenied, false
	}
	pid := process.ID(authority.Object())
	event, status := processRegistry.Wait(pid)
	switch status {
	case process.WaitExited:
		return reportProcessExit(handler, resultAddress, event), false
	case process.WaitNotFound:
		return kcall.StatusNotFound, false
	}
	if timeout == 0 {
		return kcall.StatusTimeout, false
	}
	now := amd64.TimerTicks()
	deadline := now + timeout
	if deadline < now || !processWaitSet.Add(taskID, pid, deadline) {
		return kcall.StatusResourceExhausted, false
	}
	// Re-check after registering. The process can exit between the check above
	// and the registration, and that wakeup would find no waiter and be lost,
	// leaving this task asleep until its deadline.
	if event, status := processRegistry.Wait(pid); status != process.WaitRunning {
		processWaitSet.Cancel(taskID)
		processWaitStatus[task] = kcall.StatusOK
		if status != process.WaitExited {
			return kcall.StatusNotFound, false
		}
		return reportProcessExit(handler, resultAddress, event), false
	}
	return kcall.StatusNotFound, true
}

func reportProcessExit(handler nativeCallHandler, resultAddress uint64, event process.ExitEvent) kcall.Status {
	var encoded [8]byte
	putUint64(encoded[:], event.Code)
	if !vm.CopyOut(handler.root, resultAddress, encoded[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func wakeProcessWaiters(pid process.ID) {
	publishWaitAny(kernelwait.Key{Kind: kernelwait.KindProcess, Object: uint64(pid)}, kernelwait.Exited)
	count := processWaitSet.Wake(pid, processWakeResults[:])
	for index := 0; index < count; index++ {
		if !amd64.WakeTaskID(uint64(processWakeResults[index].Task)) {
			console := amd64.NewCOM1()
			console.WriteString("PANIC: stale process wake task\n")
			for {
				amd64.Halt()
			}
		}
		processWakeResults[index] = process.WaitResult{}
	}
}

func expireProcessWaiters() {
	count := processWaitSet.Expire(amd64.TimerTicks(), processWakeResults[:])
	for index := 0; index < count; index++ {
		task, _, valid := processWakeResults[index].Task.Parts()
		if !valid {
			processWakeResults[index] = process.WaitResult{}
			continue
		}
		processWaitStatus[task] = kcall.StatusTimeout
		if !amd64.WakeTaskID(uint64(processWakeResults[index].Task)) {
			console := amd64.NewCOM1()
			console.WriteString("PANIC: stale process timeout task\n")
			for {
				amd64.Halt()
			}
		}
		processWakeResults[index] = process.WaitResult{}
	}
}

func (handler nativeCallHandler) IPCReceive(channel uint64, address uint64, capacity uint64) kcall.Status {
	endpoint, side, _, _, status := handler.endpoint(channel, capability.Read)
	if status != kcall.StatusOK {
		return status
	}
	if capacity < ipc.WireMaxSize {
		return kcall.StatusInvalidArgument
	}
	message, status := endpoint.Receive(side)
	if status != ipc.StatusOK {
		return status
	}
	size, ok := ipc.Encode(message, ipcWireData[:])
	if !ok || uint64(size) > capacity {
		return kcall.StatusResourceExhausted
	}
	if !vm.CopyOut(handler.root, address, ipcWireData[:size]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// dispatchKernelCall is the architecture-neutral native-call boundary. The
// packed return keeps the assembly bridge simple: action occupies the upper
// half and the typed native status occupies the lower half.
//
//export goos_dispatch_kernel_call
func dispatchKernelCall(number uint64, arg0 uint64, arg1 uint64, root uint64) uint64 {
	result := kcall.Dispatch(kcall.Request{Operation: kcall.Operation(number), Arg0: arg0, Arg1: arg1, Arg2: amd64.KernelCallArg2()}, nativeCallHandler{root: root})
	return uint64(result.Action)<<32 | uint64(result.Status)
}

//export goos_dispatch_ipc_wait
func dispatchIPCWait(channel uint64, descriptor uint64, size uint64, rootAndTask uint64) uint64 {
	status, blocked := waitReceive(nativeCallHandler{root: rootAndTask &^ 0xfff}, channel, descriptor, size, rootAndTask&0xfff)
	action := kcall.ActionReturn
	if blocked {
		action = kcall.ActionBlock
	}
	return uint64(action)<<32 | uint64(status)
}

//export goos_dispatch_process_wait
func dispatchProcessWait(processHandle uint64, resultAddress uint64, timeout uint64, rootAndTask uint64) uint64 {
	status, blocked := waitProcess(nativeCallHandler{root: rootAndTask &^ 0xfff}, processHandle, resultAddress, timeout, rootAndTask&0xfff)
	action := kcall.ActionReturn
	if blocked {
		action = kcall.ActionBlock
	}
	return uint64(action)<<32 | uint64(status)
}

func waitIRQ(handler nativeCallHandler, irqHandle uint64, task uint64) (kcall.Status, bool) {
	if task >= sched.MaxTasks {
		return kcall.StatusInvalidArgument, false
	}
	taskID := sched.TaskID(amd64.TaskIDForSlot(task))
	if _, _, valid := taskID.Parts(); !valid {
		return kcall.StatusInvalidArgument, false
	}
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied, false
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(irqHandle), handle.TypeIRQ, capability.Wait)
	if !allowed || authority.Object() != 11 {
		return kcall.StatusPermissionDenied, false
	}
	if amd64.TakeIRQ11() {
		return kcall.StatusOK, false
	}
	amd64.SetIRQ11Waiter(task + 1)
	amd64.RearmIRQ11()
	// Re-check after registering. An interrupt delivered between the check
	// above and the registration would set the pending flag while no waiter was
	// recorded, and IRQ waits have no deadline to recover from that.
	if amd64.TakeIRQ11() {
		amd64.SetIRQ11Waiter(0)
		return kcall.StatusOK, false
	}
	return kcall.StatusNotFound, true
}

//export goos_dispatch_irq_wait
func dispatchIRQWait(irqHandle uint64, rootAndTask uint64) uint64 {
	status, blocked := waitIRQ(nativeCallHandler{root: rootAndTask &^ 0xfff}, irqHandle, rootAndTask&0xfff)
	action := kcall.ActionReturn
	if blocked {
		action = kcall.ActionBlock
	}
	return uint64(action)<<32 | uint64(status)
}

func waitAny(handler nativeCallHandler, interestsAddress, count, resultAddress, task uint64) (kcall.Status, bool) {
	if task >= sched.MaxTasks || count == 0 || count > kernelwait.MaxObjects ||
		!vm.ValidateUserRange(resultAddress, kcall.WaitAnyResultSize) {
		return kcall.StatusInvalidArgument, false
	}
	taskID := sched.TaskID(amd64.TaskIDForSlot(task))
	if _, _, valid := taskID.Parts(); !valid {
		return kcall.StatusInvalidArgument, false
	}
	if completion, ready := waitAnyTable.Take(taskID); ready {
		if diag.Enabled {
			trace(diag.WaitTaken, pidOfTask(taskID), uint64(taskID), 0, uint64(completion.ReadyMask))
		}
		if completion.TimedOut {
			return kcall.StatusTimeout, false
		}
		return writeWaitAnyMask(handler.root, resultAddress, completion.ReadyMask), false
	}
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		if diag.Enabled {
			trace(diag.WaitDenied, pidOfTask(taskID), uint64(taskID), handler.root, uint64(diag.DenyOwner))
		}
		return kcall.StatusPermissionDenied, false
	}
	wireSize := count * kcall.WaitAnyInterestSize
	if !vm.ValidateUserRange(interestsAddress, wireSize) ||
		!vm.CopyIn(handler.root, interestsAddress, waitAnyWire[:int(wireSize)]) {
		return kcall.StatusInvalidArgument, false
	}
	var resultWire [kcall.WaitAnyResultSize]byte
	if !vm.CopyIn(handler.root, resultAddress, resultWire[:]) {
		return kcall.StatusInvalidArgument, false
	}
	timeout := decodeUint64(resultWire[0:8])
	for index := 0; index < int(count); index++ {
		offset := index * int(kcall.WaitAnyInterestSize)
		handleID := handle.ID(decodeUint64(waitAnyWire[offset : offset+8]))
		events := kcall.WaitEvent(decodeUint64(waitAnyWire[offset+8 : offset+16]))
		entry, status, reason := resolveWaitAny(owner, handleID, events)
		if status != kcall.StatusOK {
			trace(diag.WaitDenied, owner.ID(), uint64(taskID), uint64(handleID), uint64(reason))
			if diag.Enabled {
				traceDeniedInterests(handler.root, interestsAddress, count)
			}
			return status, false
		}
		waitAnyEntries[index] = entry
	}
	entries := waitAnyEntries[:int(count)]
	if mask := readyWaitAny(entries); mask != 0 {
		trace(diag.WaitReady, owner.ID(), uint64(taskID), entries[0].Key.Object, uint64(mask))
		return writeWaitAnyMask(handler.root, resultAddress, mask), false
	}
	if timeout == 0 {
		return kcall.StatusTimeout, false
	}
	deadline := uint64(0)
	if timeout != kcall.WaitAnyInfiniteTimeout {
		now := amd64.TimerTicks()
		deadline = now + timeout
		if deadline < now {
			return kcall.StatusInvalidArgument, false
		}
	}
	if !waitAnyTable.Begin(taskID, entries, deadline) {
		return kcall.StatusResourceExhausted, false
	}
	trace(diag.WaitBegin, owner.ID(), uint64(taskID), entries[0].Key.Object, count)
	// Registration precedes the final readiness observation. The interrupt
	// gate keeps publication in the same exclusion domain until assembly
	// commits the task to BLOCKED.
	if mask := readyWaitAny(entries); mask != 0 {
		waitAnyTable.Cancel(taskID)
		trace(diag.WaitRecheck, owner.ID(), uint64(taskID), entries[0].Key.Object, uint64(mask))
		return writeWaitAnyMask(handler.root, resultAddress, mask), false
	}
	return kcall.StatusNotFound, true
}

func resolveWaitAny(owner *process.Process, id handle.ID, events kcall.WaitEvent) (kernelwait.Entry, kcall.Status, diag.Deny) {
	typeID, authority, found := owner.Handles().Describe(id)
	if !found {
		return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyHandle
	}
	switch typeID {
	case handle.TypeChannel:
		if events == 0 || events & ^(kcall.WaitReadable|kcall.WaitPeerClosed) != 0 {
			return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyEvents
		}
		if !authority.Allows(capability.Read | capability.Wait) {
			return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyRights
		}
		object, side, valid := ipc.DecodeEndpointObject(authority.Object())
		if _, exists := channelRegistry.Lookup(object); !valid || !exists {
			return kernelwait.Entry{}, kcall.StatusNotFound, diag.DenyObject
		}
		return kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindChannel, Object: object, Side: side}, Events: waitEvents(events)}, kcall.StatusOK, 0
	case handle.TypeStream:
		object, writer, valid := stream.DecodeEndpointObject(authority.Object())
		if !valid {
			return kernelwait.Entry{}, kcall.StatusNotFound, diag.DenyObject
		}
		side := uint8(0)
		allowed := kcall.WaitReadable | kcall.WaitPeerClosed
		rights := capability.Read | capability.Wait
		if writer {
			side, allowed, rights = 1, kcall.WaitWritable|kcall.WaitPeerClosed, capability.Write|capability.Wait
		}
		if events == 0 || events & ^allowed != 0 {
			return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyEvents
		}
		if !authority.Allows(rights) {
			return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyRights
		}
		if _, exists := streamRegistry.Lookup(object); !exists {
			return kernelwait.Entry{}, kcall.StatusNotFound, diag.DenyObject
		}
		return kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindStream, Object: object, Side: side}, Events: waitEvents(events)}, kcall.StatusOK, 0
	case handle.TypeProcess:
		if events != kcall.WaitCompleted {
			return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyEvents
		}
		if !authority.Allows(capability.Wait) {
			return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyRights
		}
		pid := process.ID(authority.Object())
		if _, status := processRegistry.Wait(pid); status == process.WaitNotFound {
			return kernelwait.Entry{}, kcall.StatusNotFound, diag.DenyObject
		}
		return kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindProcess, Object: uint64(pid)}, Events: kernelwait.Exited}, kcall.StatusOK, 0
	default:
		return kernelwait.Entry{}, kcall.StatusUnsupported, diag.DenyType
	}
}

func waitEvents(events kcall.WaitEvent) kernelwait.Event {
	var result kernelwait.Event
	if events&kcall.WaitReadable != 0 {
		result |= kernelwait.Readable
	}
	if events&kcall.WaitWritable != 0 {
		result |= kernelwait.Writable
	}
	if events&kcall.WaitCompleted != 0 {
		result |= kernelwait.Exited
	}
	if events&kcall.WaitPeerClosed != 0 {
		result |= kernelwait.PeerClosed
	}
	return result
}

func readyWaitAny(entries []kernelwait.Entry) uint16 {
	var mask uint16
	for index, entry := range entries {
		ready := false
		switch entry.Key.Kind {
		case kernelwait.KindChannel:
			channel, found := channelRegistry.Lookup(entry.Key.Object)
			if found {
				status := channel.ReceiveStatus(entry.Key.Side)
				ready = status == ipc.StatusOK && entry.Events&kernelwait.Readable != 0 ||
					status == ipc.StatusPeerDied && entry.Events&kernelwait.PeerClosed != 0
			}
		case kernelwait.KindStream:
			endpoint, found := streamRegistry.Lookup(entry.Key.Object)
			if found {
				status := endpoint.ReadStatus()
				if entry.Key.Side == 1 {
					status = endpoint.WriteStatus()
				}
				ready = status == stream.StatusOK || status == stream.StatusPeerClosed && entry.Events&kernelwait.PeerClosed != 0
			}
		case kernelwait.KindProcess:
			_, status := processRegistry.Wait(process.ID(entry.Key.Object))
			ready = status != process.WaitRunning
		}
		if ready {
			mask |= 1 << index
		}
	}
	return mask
}

func writeWaitAnyMask(root, resultAddress uint64, mask uint16) kcall.Status {
	var encoded [8]byte
	putUint64(encoded[:], uint64(mask))
	if !vm.CopyOut(root, resultAddress+8, encoded[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func publishWaitAny(key kernelwait.Key, event kernelwait.Event) {
	count := waitAnyTable.Publish(key, event, waitAnyCompletions[:])
	for index := 0; index < count && index < len(waitAnyCompletions); index++ {
		if diag.Enabled {
			traceWaitKey(diag.WaitComplete, pidOfTask(waitAnyCompletions[index].Task), waitAnyCompletions[index].Task, key, uint64(event))
		}
		amd64.WakeTaskID(uint64(waitAnyCompletions[index].Task))
		waitAnyCompletions[index] = kernelwait.Completion{}
	}
}

func expireWaitAny() {
	count := waitAnyTable.Expire(amd64.TimerTicks(), waitAnyCompletions[:])
	for index := 0; index < count && index < len(waitAnyCompletions); index++ {
		if diag.Enabled {
			trace(diag.WaitTimeout, pidOfTask(waitAnyCompletions[index].Task), uint64(waitAnyCompletions[index].Task), 0, 0)
		}
		amd64.WakeTaskID(uint64(waitAnyCompletions[index].Task))
		waitAnyCompletions[index] = kernelwait.Completion{}
	}
}

//export goos_dispatch_wait_any
func dispatchWaitAny(interestsAddress, count, resultAddress, rootAndTask uint64) uint64 {
	status, blocked := waitAny(nativeCallHandler{root: rootAndTask &^ 0xfff}, interestsAddress, count, resultAddress, rootAndTask&0xfff)
	action := kcall.ActionReturn
	if blocked {
		action = kcall.ActionBlock
	}
	return uint64(action)<<32 | uint64(status)
}

//export goos_demo_task_a
func runDemoTaskA() {
	for !validation.Enabled {
		amd64.KernelSleep()
	}
	for demoTaskA < 3 {
		demoTaskA++
		amd64.Halt()
	}
	demoABlocked = 1
	amd64.BlockCurrentTask()
	amd64.Halt()
	demoAResumed = 1
	demoTaskA++
	for {
		amd64.KernelSleep()
	}
}

//export goos_demo_task_b
func runDemoTaskB() {
	for !validation.Enabled {
		amd64.KernelSleep()
	}
	for {
		demoTaskB++
		if demoABlocked != 0 && demoAResumed == 0 && demoTaskB >= 6 {
			amd64.WakeTask(1)
		}
		if demoAResumed != 0 {
			break
		}
		amd64.Halt()
	}
	for {
		amd64.KernelSleep()
	}
}

// expireWaitDeadlines releases waiters whose deadline has passed. It is driven
// from serviceKernel: a deadline is the last line of defence behind an event
// wakeup, so it must not depend on any particular task being scheduled.
func expireWaitDeadlines() {
	expireIPCWaiters()
	expireProcessWaiters()
	expireStreamWaiters()
	expireWaitAny()
}

//export goos_idle_task
func runIdleTask() {
	for {
		amd64.Halt()
	}
}

func main() {
	console := amd64.NewCOM1()
	console.Init()
	console.WriteString(version.Name + " " + version.Release + "\n")
	console.WriteString("TinyGo kernel\n\n")
	console.WriteString("CPU: " + version.Machine + "\n")

	status := bootuefi.LoadInitramfs(&bootInfo)
	if status != 0 {
		console.WriteString("PANIC: initramfs load failed: ")
		console.WriteHex(uint64(status))
		console.WriteString("\n")
		for {
			amd64.Halt()
		}
	}
	status = bootuefi.CaptureAndExit(&bootInfo)
	if status != 0 {
		console.WriteString("PANIC: UEFI handoff failed: ")
		console.WriteHex(uint64(status))
		console.WriteString("\n")
		for {
			amd64.Halt()
		}
	}
	mark("BOOT_SERVICES_EXITED")
	virtioBlock, virtioBlockFound := pci.FindLegacyVirtioBlock(pciConfig{})
	if !virtioBlockFound || virtioBlock.IRQ != 11 {
		console.WriteString("PANIC: legacy virtio-blk PCI device not found\n")
		for {
			amd64.Halt()
		}
	}
	command := uint16(amd64.PCIConfigRead32(virtioBlock.Bus, virtioBlock.Slot, virtioBlock.Function, 4))
	command = command&^uint16(1<<10) | 1<<0 | 1<<2
	amd64.PCIConfigWrite16(virtioBlock.Bus, virtioBlock.Slot, virtioBlock.Function, 4, command)
	virtioBlockPort = virtioBlock.IOBase
	mark("PCI_DISCOVERY_OK")
	console.WriteString("Memory: ")
	console.WriteUint(bootInfo.UsableBytes / (1024 * 1024))
	console.WriteString(" MiB usable\n")

	pageAllocator.Init(bootInfo.MemoryRegions())
	page, ok := pageAllocator.Allocate()
	if !ok || !pageAllocator.Free(page) {
		console.WriteString("PANIC: physical page allocator self-test failed\n")
		for {
			amd64.Halt()
		}
	}
	if validation.Enabled {
		console.WriteString("Physical pages free: ")
		console.WriteUint(pageAllocator.Statistics().FreePages)
		console.WriteString("\n")
	}
	mark("PMM_OK")

	if !amd64.HasNX() {
		console.WriteString("PANIC: CPU does not support NX\n")
		for {
			amd64.Halt()
		}
	}
	image, validImage := vm.LoadedImage()
	stack := amd64.StackPointer()
	if !validImage || stack < 1024*1024 || stack >= vm.IdentityMapLimit {
		console.WriteString("PANIC: unsafe virtual-memory bootstrap state\n")
		for {
			amd64.Halt()
		}
	}
	pageTable, built := vm.BuildIdentity(&pageAllocator, image)
	if !built {
		console.WriteString("PANIC: page-table construction failed\n")
		for {
			amd64.Halt()
		}
	}
	kernelPageTable = pageTable
	amd64.EnableNXAndWriteProtect()
	amd64.LoadPageTable(pageTable)
	mark("NX_WX_OK")
	mark("VM_OK")

	if !amd64.InstallDescriptorTables(&pageAllocator, pageTable) {
		console.WriteString("PANIC: descriptor-table installation failed\n")
		for {
			amd64.Halt()
		}
	}
	if !amd64.ConfigureVirtioIRQ(pageTable) {
		console.WriteString("PANIC: virtio IRQ routing failed\n")
		for {
			amd64.Halt()
		}
	}
	if validation.Enabled {
		amd64.TriggerBreakpoint()
	}
	mark("GDT_OK")
	mark("IDT_OK")

	amd64.StartBootstrapTimer(clock.PITDivisor)
	for amd64.TimerTicks() < 3 {
		amd64.Halt()
	}
	calibrateClock()
	console.WriteString("Timer: ")
	console.WriteUint(clock.TicksPerSecond)
	console.WriteString(" Hz\n")
	mark("TIMER_OK")
	console.WriteString("Security: capabilities, NX, W^X\n")

	if !amd64.SetupBootstrapScheduler(&pageAllocator, pageTable) {
		console.WriteString("PANIC: scheduler bootstrap failed\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("Scheduler: preemptive round-robin\n")
	if validation.Enabled {
		awaitSchedulerSelfTest()
	}

	rootInit, addressSpaceInit := vm.CreateUserAddressSpace(&pageAllocator, pageTable)
	if !addressSpaceInit {
		panicSerial("PID 1 address-space construction failed")
	}
	initProcess, validInit := process.New(1, process.AddressSpace{Root: rootInit})
	initProcess.SetName("init")
	bootInitProcess = &initProcess
	if !validInit || !processRegistry.Add(&initProcess) {
		panicSerial("PID 1 process initialization failed")
	}
	var lifecycleCreated bool
	lifecycleChannel, lifecycleChannelID, lifecycleCreated = channelRegistry.Create()
	lifecycleObject, _ := ipc.EndpointObject(lifecycleChannelID, 1)
	lifecycleHandle, lifecycleInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(lifecycleObject, capability.Read|capability.Write|capability.Wait))
	serviceDeviceHandle, serviceDeviceInstalled := initProcess.Handles().Insert(handle.TypeDevice, capability.New(1, capability.Read|capability.Write|capability.Transfer))
	_, vfsChannelID, vfsChannelCreated := channelRegistry.Create()
	vfsClientObject, _ := ipc.EndpointObject(vfsChannelID, 0)
	vfsServerObject, _ := ipc.EndpointObject(vfsChannelID, 1)
	vfsChannelHandle, vfsChannelInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(vfsClientObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	vfsServerHandle, vfsServerInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(vfsServerObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	_, vfsControlID, vfsControlCreated := channelRegistry.Create()
	vfsControlClientObject, _ := ipc.EndpointObject(vfsControlID, 0)
	vfsControlServerObject, _ := ipc.EndpointObject(vfsControlID, 1)
	vfsControlHandle, vfsControlInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(vfsControlClientObject, capability.Read|capability.Write|capability.Wait))
	vfsControlServerHandle, vfsControlServerInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(vfsControlServerObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	_, execControlID, execControlCreated := channelRegistry.Create()
	execClientObject, _ := ipc.EndpointObject(execControlID, 0)
	execServerObject, _ := ipc.EndpointObject(execControlID, 1)
	execServerHandle, execServerInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(execServerObject, capability.Read|capability.Write|capability.Wait))
	execClientHandle, execClientInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(execClientObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	_, blockChannelID, blockChannelCreated := channelRegistry.Create()
	blockClientObject, _ := ipc.EndpointObject(blockChannelID, 0)
	blockServerObject, _ := ipc.EndpointObject(blockChannelID, 1)
	blockClientHandle, blockClientInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(blockClientObject, capability.Read|capability.Write|capability.Wait))
	blockServerHandle, blockServerInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(blockServerObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	portObject, portObjectValid := device.PortObject(virtioBlock.IOBase, virtioBlock.IOSize)
	dmaObject, dmaObjectValid := device.DMAObject(1)
	irqObject, irqObjectValid := device.IRQObject(virtioBlock.IRQ)
	virtioPortHandle, virtioPortInstalled := initProcess.Handles().Insert(handle.TypeDevice, capability.New(portObject, capability.Read|capability.Write|capability.Transfer))
	virtioIRQHandle, virtioIRQInstalled := initProcess.Handles().Insert(handle.TypeIRQ, capability.New(irqObject, capability.Wait|capability.Transfer))
	virtioDMAHandle, virtioDMAInstalled := initProcess.Handles().Insert(handle.TypeDevice, capability.New(dmaObject, capability.Write|capability.Transfer))
	_, driverChannelID, driverChannelCreated := channelRegistry.Create()
	driverClientObject, _ := ipc.EndpointObject(driverChannelID, 0)
	driverServerObject, _ := ipc.EndpointObject(driverChannelID, 1)
	driverClientHandle, driverClientInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(driverClientObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	driverServerHandle, driverServerInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(driverServerObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	_, filesystemBlockChannelID, filesystemBlockChannelCreated := channelRegistry.Create()
	filesystemBlockClientObject, _ := ipc.EndpointObject(filesystemBlockChannelID, 0)
	filesystemBlockServerObject, _ := ipc.EndpointObject(filesystemBlockChannelID, 1)
	filesystemBlockClientHandle, filesystemBlockClientInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(filesystemBlockClientObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	filesystemBlockServerHandle, filesystemBlockServerInstalled := initProcess.Handles().Insert(handle.TypeChannel, capability.New(filesystemBlockServerObject, capability.Read|capability.Write|capability.Wait|capability.Transfer))
	systemControlHandle, systemControlInstalled := initProcess.Handles().Insert(handle.TypeDevice, capability.New(device.SystemControlObject, capability.Write))
	if !lifecycleCreated || !lifecycleInstalled || !serviceDeviceInstalled || !vfsChannelCreated || !vfsChannelInstalled || !vfsServerInstalled || !vfsControlCreated || !vfsControlInstalled || !vfsControlServerInstalled || !execControlCreated || !execServerInstalled || !execClientInstalled || !blockChannelCreated || !blockClientInstalled || !blockServerInstalled || !portObjectValid || !dmaObjectValid || !irqObjectValid || !virtioPortInstalled || !virtioIRQInstalled || !virtioDMAInstalled || !driverChannelCreated || !driverClientInstalled || !driverServerInstalled || !filesystemBlockChannelCreated || !filesystemBlockClientInstalled || !filesystemBlockServerInstalled || !systemControlInstalled || lifecycleHandle != handle.ID(0x0000000100000001) ||
		serviceDeviceHandle != handle.ID(0x0000000100000002) || vfsChannelHandle != handle.ID(0x0000000100000003) || vfsServerHandle != handle.ID(0x0000000100000004) || vfsControlHandle != handle.ID(0x0000000100000005) || vfsControlServerHandle != handle.ID(0x0000000100000006) || execServerHandle != handle.ID(0x0000000100000007) || execClientHandle != handle.ID(0x0000000100000008) || blockClientHandle != handle.ID(0x0000000100000009) || blockServerHandle != handle.ID(0x000000010000000a) || virtioPortHandle != handle.ID(0x000000010000000b) || virtioIRQHandle != handle.ID(0x000000010000000c) || virtioDMAHandle != handle.ID(0x000000010000000d) || driverClientHandle != handle.ID(0x000000010000000e) || driverServerHandle != handle.ID(0x000000010000000f) || filesystemBlockClientHandle != handle.ID(0x0000000100000010) || filesystemBlockServerHandle != handle.ID(0x0000000100000011) || systemControlHandle != handle.ID(0x0000000100000012) {
		panicSerial("PID 1 capability layout failed")
	}
	if validation.Enabled {
		runKernelSelfTests(pageTable)
	}
	initBytes, initDiscovered := initramfs.Find(bootuefi.Initramfs(&bootInfo), "system/init")
	if !initDiscovered {
		console.WriteString("PANIC: init image not found in initramfs\n")
		for {
			amd64.Halt()
		}
	}
	loadedInit, loadedImageInit := loader.LoadNative(&pageAllocator, rootInit, initBytes)
	if !loadedImageInit {
		console.WriteString("PANIC: init native image rejected\n")
		for {
			amd64.Halt()
		}
	}
	flags := amd64.DisableInterrupts()
	initTaskID, initTaskAdded := amd64.AddUserTask(&pageAllocator, rootInit, loadedInit.Entry, loadedInit.StackTop)
	if !initTaskAdded || !processRegistry.BindTask(1, sched.TaskID(initTaskID)) {
		panicSerial("PID 1 task scheduling failed")
	}
	if validation.Enabled {
		startSelfTestTasks()
	}
	amd64.RestoreInterrupts(flags)
	if validation.Enabled {
		awaitValidationBoot()
	} else {
		console.WriteString("Kernel initialized\n")
	}
	kernelLogUint(logfmt.Info, "kernel initialized", "free-pages", pageAllocator.Statistics().FreePages)
	for {
		serviceKernel()
		if initExitObserved != 0 {
			console.WriteString("PANIC: init process exited: ")
			console.WriteHex(initExitCode)
			console.WriteString("\n")
			for {
				amd64.Halt()
			}
		}
	}
}

// mark prints a validation marker. Normal builds print none.
func mark(marker string) {
	if validation.Enabled {
		amd64.NewCOM1().WriteString(marker + "\n")
	}
}

// serviceKernel runs one step of the kernel's own work inside the exclusion
// domain native calls already run in, then sleeps until the next tick, task
// exit, or lifecycle request. Preempted mid-step, it would expose torn
// registries and handle tables to whichever task the timer selects next;
// sleeping inside the domain means no wake can fall between the last check
// and the sleep.
func serviceKernel() {
	flags := amd64.DisableInterrupts()
	expireWaitDeadlines()
	flushLifecycle()
	deliverProcessExits()
	consumeInitControl()
	amd64.KernelSleep()
	amd64.RestoreInterrupts(flags)
}

// abandonSpawn releases a launch that failed before its task existed: the
// derived handles, the registry entry, the process slot, and the address space.
func abandonSpawn(root uint64, spawned *process.Process, registered bool) {
	if spawned != nil {
		spawned.Handles().CloseAll(nil)
		if registered {
			processRegistry.Remove(spawned.ID())
		}
		spawned.Reset()
	}
	if !vm.DestroyUserAddressSpace(&pageAllocator, root) {
		addressSpaceReclaimFailures++
	}
}

func lastSlash(path string) int {
	for index := len(path) - 1; index >= 0; index-- {
		if path[index] == '/' {
			return index
		}
	}
	return -1
}
