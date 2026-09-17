//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/kernel/arch/amd64"
	"goos/kernel/boot"
	bootuefi "goos/kernel/boot/uefi"
	"goos/kernel/device"
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
	"goos/kernel/tty"
	"goos/kernel/vfs"
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
	bootRAMFS               ramfs.FileSystem
	deviceRAMFS             ramfs.FileSystem
	bootVFS                 vfs.Namespace
	ramfsReadback           [16]byte
	vfsReadback             [16]byte
	initAnnounced           uint64
	processExitDelivered    uint64
	processReclaimConfirmed uint64
	processWaitConfirmed    uint64
	supervisorExitDelivered uint64
	serviceLaunchConfirmed  uint64
	userspaceSupervisorOK   uint64
	capabilityManifestOK    uint64
	startupABIConfirmed     uint64
	initSendObserved        uint64
	initSendStatus          kcall.Status
	initExitObserved        uint64
	initExitCode            uint64
	kernelPageTable         uint64
	spawnedProcesses        [process.MaxProcesses]process.Process
	nextProcessID           process.ID = 4
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
	console := amd64.NewCOM1()
	console.WriteString("USER_EXIT_OK\n")
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
		console.WriteString("USER_COPYIN_OK\n")
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
		exitCode := amd64.TaskExitCode(task)
		if !processRegistry.ReportTaskExit(sched.TaskID(task), exitCode) {
			continue
		}
		if !amd64.ReclaimExitedTask(&pageAllocator, kernelPageTable, task) {
			continue
		}
		event, ok := processRegistry.NextExit()
		if !ok {
			continue
		}
		processExitDelivered = 1
		if waited, status := processRegistry.Wait(event.Process); status == process.WaitExited && waited == event {
			processWaitConfirmed = 1
		}
		wakeProcessWaiters(event.Process)
		if event.Process == 1 {
			initExitObserved = 1
			initExitCode = event.Code
		}
		if event.Process != 1 && lifecycleChannel != nil {
			message, encoded := supervisor.ProcessExited(uint64(event.Process), event.Code)
			if encoded && lifecycleChannel.Send(0, message, nil, nil, nil) == ipc.StatusOK {
				wakeIPCWaiters(lifecycleChannelID, 1, ipc.WaitMessage)
				if event.Process == 3 {
					supervisorExitDelivered = 1
				}
			}
		}
		if event.Process != 1 {
			if exited, found := processRegistry.ByID(event.Process); found {
				exited.Handles().CloseAll(func(typeID handle.Type, cap capability.Capability) {
					if typeID != handle.TypeChannel {
						return
					}
					channelID, side, valid := ipc.DecodeEndpointObject(cap.Object())
					channel, exists := channelRegistry.Lookup(channelID)
					if valid && exists && channel.Close(side) {
						wakeIPCWaiters(channelID, side^1, ipc.WaitPeerDied)
						channelRegistry.Release(channelID)
					}
				})
				before := pageAllocator.Statistics().FreePages
				if !vm.DestroyUserAddressSpace(&pageAllocator, exited.AddressSpace().Root) {
					continue
				}
				if pageAllocator.Statistics().FreePages <= before {
					continue
				}
				processReclaimConfirmed = 1
			}
			processRegistry.Remove(event.Process)
			for index := range spawnedProcesses {
				if spawnedProcesses[index].ID() == event.Process {
					spawnedProcesses[index].Reset()
					break
				}
			}
		}
	}
}

func consumeInitControl() {
	if lifecycleChannel == nil {
		return
	}
	message, status := lifecycleChannel.Receive(0)
	if status != ipc.StatusOK {
		return
	}
	event, valid := supervisor.DecodeLifecycle(message)
	if !valid || event.Operation != supervisor.LifecycleLaunch {
		return
	}
	pid, processHandle, launchStatus := spawnExecutable(event.ExecutablePath(), event.Capabilities[:event.CapabilityCount], event.Startup[:event.StartupSize])
	if launchStatus != supervisor.LaunchOK {
		console := amd64.NewCOM1()
		console.WriteString("INIT_LAUNCH_ERROR: ")
		console.WriteHex(uint64(launchStatus))
		console.WriteString("\n")
	}
	reply, encoded := supervisor.LaunchResult(uint64(pid), processHandle, launchStatus, message.Correlation)
	if encoded && lifecycleChannel.Send(0, reply, nil, nil, nil) == ipc.StatusOK {
		wakeIPCWaiters(lifecycleChannelID, 1, ipc.WaitMessage)
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
			return 0, 0, supervisor.LaunchCapabilityDenied
		}
		if _, allowed = source.Derive(grant.Rights); !allowed {
			return 0, 0, supervisor.LaunchCapabilityDenied
		}
	}
	image, found := initramfs.Find(bootuefi.Initramfs(&bootInfo), path[1:])
	if !found {
		return 0, 0, supervisor.LaunchNotFound
	}
	root, ok := vm.CreateUserAddressSpace(&pageAllocator, kernelPageTable)
	if !ok {
		return 0, 0, supervisor.LaunchAddressSpaceFailed
	}
	loaded, ok := loader.LoadNative(&pageAllocator, root, image)
	if !ok {
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
		return 0, 0, supervisor.LaunchResourceExhausted
	}
	*spawned, ok = process.NewWithStartup(pid, process.AddressSpace{Root: root}, startupData)
	if !ok || spawned.Handles().Available() < len(grants) {
		return 0, 0, supervisor.LaunchProcessFailed
	}
	var installed [supervisor.MaxCapabilities]handle.ID
	for index, grant := range grants {
		installed[index], ok = bootInitProcess.Handles().Transfer(spawned.Handles(), grant.Handle, grant.Type, grant.Rights)
		if !ok {
			for rollback := 0; rollback < index; rollback++ {
				spawned.Handles().Close(installed[rollback])
			}
			return 0, 0, supervisor.LaunchCapabilityInstallFailed
		}
	}
	for index, grant := range grants {
		installedCapability, found := spawned.Handles().Lookup(installed[index], grant.Type, grant.Rights)
		if !found || installedCapability.Rights() != grant.Rights {
			for rollback := range grants {
				spawned.Handles().Close(installed[rollback])
			}
			return 0, 0, supervisor.LaunchCapabilityInstallFailed
		}
	}
	if len(startupData) != 0 {
		var boundWire [startup.Size]byte
		bound, boundOK := startup.BindHandles(startupData, installed[:len(grants)], &boundWire)
		if !boundOK || !spawned.SetStartup(bound) {
			for index := range grants {
				spawned.Handles().Close(installed[index])
			}
			return 0, 0, supervisor.LaunchInvalidImage
		}
		startupABIConfirmed = 1
	}
	if !processRegistry.Add(spawned) {
		for index := range grants {
			spawned.Handles().Close(installed[index])
		}
		return 0, 0, supervisor.LaunchProcessFailed
	}
	task, ok := amd64.AddUserTask(&pageAllocator, root, loaded.Entry, loaded.StackTop)
	if !ok || !processRegistry.BindTask(pid, sched.TaskID(task)) {
		return 0, 0, supervisor.LaunchTaskFailed
	}
	processHandle, ok := bootInitProcess.Handles().Insert(handle.TypeProcess, capability.New(uint64(pid), capability.Wait))
	if !ok {
		return 0, 0, supervisor.LaunchCapabilityInstallFailed
	}
	nextProcessID++
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
	if !found || virtual != vm.UserDMAAddress || !vm.CanMapUserPage(handler.root, virtual) {
		return kcall.StatusInvalidArgument
	}
	cap, ok := owner.Handles().Lookup(handle.ID(id), handle.TypeDevice, capability.Write)
	if _, valid := device.DecodeDMAObject(cap.Object()); !ok || !valid {
		return kcall.StatusPermissionDenied
	}
	var encoded [24]byte
	if !vm.CopyOut(handler.root, address, encoded[:]) {
		return kcall.StatusInvalidArgument
	}
	physical, allocated := pageAllocator.AllocateContiguous(1)
	if !allocated {
		return kcall.StatusResourceExhausted
	}
	for index := range kernelCallData {
		kernelCallData[index] = 0
	}
	if !vm.InitializePhysical(physical, kernelCallData[:]) || !vm.MapOwnedUserPage(handler.root, virtual, physical, vm.Permissions{User: true, Writable: true}) {
		pageAllocator.Free(physical)
		return kcall.StatusResourceExhausted
	}
	putUint64(encoded[0:8], physical)
	putUint64(encoded[8:16], virtual)
	putUint64(encoded[16:24], vm.PageSize)
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
		wakeIPCWaiters(channelID, side^1, ipc.WaitPeerDied)
		channelRegistry.Release(channelID)
	}
	if !owner.Handles().Close(handle.ID(id)) {
		return kcall.StatusNotFound
	}
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
		var encoded [8]byte
		putUint64(encoded[:], event.Code)
		if !vm.CopyOut(handler.root, resultAddress, encoded[:]) {
			return kcall.StatusInvalidArgument, false
		}
		return kcall.StatusOK, false
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
	return kcall.StatusNotFound, true
}

func wakeProcessWaiters(pid process.ID) {
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

//export goos_demo_task_a
func runDemoTaskA() {
	for demoTaskA < 3 {
		demoTaskA++
		amd64.Halt()
	}
	demoABlocked = 1
	amd64.BlockCurrentTask()
	amd64.Halt()
	demoAResumed = 1
	for {
		demoTaskA++
		amd64.Halt()
	}
}

//export goos_demo_task_b
func runDemoTaskB() {
	for {
		demoTaskB++
		expireIPCWaiters()
		expireProcessWaiters()
		if demoABlocked != 0 && demoAResumed == 0 && demoTaskB >= 6 {
			amd64.WakeTask(1)
		}
		amd64.Halt()
	}
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
	console.WriteString("GoOS 0.1-dev\n")
	console.WriteString("Kernel\n\n")
	console.WriteString("CPU: x86_64\n")
	console.WriteString("Boot: UEFI\n")
	console.WriteString("Security: capabilities (foundation)\n")

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
	console.WriteString("BOOT_SERVICES_EXITED\n")
	virtioBlock, virtioBlockFound := pci.FindLegacyVirtioBlock(pciConfig{})
	if !virtioBlockFound {
		console.WriteString("PANIC: legacy virtio-blk PCI device not found\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("PCI_DISCOVERY_OK\n")
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
	stats := pageAllocator.Statistics()
	console.WriteString("Physical pages free: ")
	console.WriteUint(stats.FreePages)
	console.WriteString("\nPMM_OK\n")

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
	console.WriteString("NX_WX_OK\n")
	console.WriteString("VM_OK\n")

	if !amd64.InstallDescriptorTables(&pageAllocator, pageTable) {
		console.WriteString("PANIC: descriptor-table installation failed\n")
		for {
			amd64.Halt()
		}
	}
	amd64.TriggerBreakpoint()
	console.WriteString("GDT_OK\n")
	console.WriteString("IDT_OK\n")

	amd64.StartBootstrapTimer()
	for amd64.TimerTicks() < 3 {
		amd64.Halt()
	}
	console.WriteString("TIMER_OK\n")

	if !amd64.SetupBootstrapScheduler(&pageAllocator, pageTable) {
		console.WriteString("PANIC: scheduler bootstrap failed\n")
		for {
			amd64.Halt()
		}
	}
	for demoTaskA < 4 || demoTaskB < 6 || demoABlocked == 0 || demoAResumed == 0 {
		amd64.Halt()
	}
	console.WriteString("STACK_GUARDS_OK\n")
	console.WriteString("TASK_BLOCK_OK\n")
	console.WriteString("TASK_WAKE_OK\n")
	console.WriteString("TASK_A_OK\n")
	console.WriteString("TASK_B_OK\n")
	console.WriteString("IDLE_TASK_OK\n")
	console.WriteString("SCHEDULER_OK\n")

	physicalA, allocatedA := pageAllocator.Allocate()
	physicalB, allocatedB := pageAllocator.Allocate()
	if !allocatedA || !allocatedB ||
		!vm.InitializePhysicalByte(physicalA, 0xa1) ||
		!vm.InitializePhysicalByte(physicalB, 0xb2) {
		console.WriteString("PANIC: process page allocation failed\n")
		for {
			amd64.Halt()
		}
	}
	rootInit, addressSpaceInit := vm.CreateUserAddressSpace(&pageAllocator, pageTable)
	rootA, addressSpaceA := vm.CreateUserAddressSpace(&pageAllocator, pageTable)
	rootB, addressSpaceB := vm.CreateUserAddressSpace(&pageAllocator, pageTable)
	if addressSpaceA {
		addressSpaceA = vm.MapUserPage(rootA, vm.UserProbeAddress, physicalA, vm.Permissions{User: true, Writable: true})
	}
	if addressSpaceB {
		addressSpaceB = vm.MapUserPage(rootB, vm.UserProbeAddress, physicalB, vm.Permissions{User: true, Writable: true})
	}
	resolvedA, resolvesA := vm.Resolve(rootA, vm.UserProbeAddress)
	resolvedB, resolvesB := vm.Resolve(rootB, vm.UserProbeAddress)
	if !addressSpaceInit || !addressSpaceA || !addressSpaceB || !resolvesA || !resolvesB || resolvedA != physicalA || resolvedB != physicalB {
		console.WriteString("PANIC: process address-space construction failed\n")
		for {
			amd64.Halt()
		}
	}
	initProcess, validInit := process.New(1, process.AddressSpace{Root: rootInit})
	processA, validA := process.New(2, process.AddressSpace{Root: rootA})
	processB, validB := process.New(3, process.AddressSpace{Root: rootB})
	bootInitProcess = &initProcess
	bootProcessA, bootProcessB = &processA, &processB
	if !validInit || !validA || !validB || !processRegistry.Add(&initProcess) || !processRegistry.Add(&processA) || !processRegistry.Add(&processB) {
		console.WriteString("PANIC: process model initialization failed\n")
		for {
			amd64.Halt()
		}
	}
	memoryHandle, inserted := processA.Handles().Insert(
		handle.TypeMemory,
		capability.New(physicalA, capability.Read|capability.Transfer),
	)
	transferred, didTransfer := processA.Handles().Transfer(
		processB.Handles(), memoryHandle, handle.TypeMemory, capability.Read,
	)
	_, targetCanRead := processB.Handles().Lookup(transferred, handle.TypeMemory, capability.Read)
	_, targetCanWrite := processB.Handles().Lookup(transferred, handle.TypeMemory, capability.Write)
	if !inserted || !didTransfer || !targetCanRead || targetCanWrite {
		console.WriteString("PANIC: process-local handle enforcement failed\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("HANDLE_TABLE_OK\n")
	console.WriteString("PROCESS_MODEL_OK\n")

	registeredChannel, registeredChannelID, channelCreated := channelRegistry.Create()
	registeredObjectA, _ := ipc.EndpointObject(registeredChannelID, 0)
	registeredObjectB, _ := ipc.EndpointObject(registeredChannelID, 1)
	channelA, channelAInstalled := processA.Handles().Insert(handle.TypeChannel, capability.New(registeredObjectA, capability.Read|capability.Write|capability.Wait))
	channelB, channelBInstalled := processB.Handles().Insert(handle.TypeChannel, capability.New(registeredObjectB, capability.Read|capability.Write|capability.Wait))
	_, channelACanWrite := processA.Handles().Lookup(channelA, handle.TypeChannel, capability.Write)
	_, channelBCanRead := processB.Handles().Lookup(channelB, handle.TypeChannel, capability.Read)
	channelsBound := true
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
	lifecycleBound := true
	request := ipc.Message{ProtocolID: 0x474f4f53, Version: 1, Kind: ipc.KindRequest, Correlation: 1, PayloadSize: 4}
	copy(request.Payload[:], []byte("ping"))
	wireSize, wireEncoded := ipc.Encode(request, ipcWireData[:])
	wireRequest, wireDecoded := ipc.Decode(ipcWireData[:wireSize])
	ipcStatus := registeredChannel.Send(0, wireRequest, processA.Handles(), processB.Handles(), []ipc.Transfer{{
		Handle: memoryHandle,
		Type:   handle.TypeMemory,
		Rights: capability.Read,
	}})
	receivedRequest, receivedStatus := registeredChannel.Receive(1)
	_, receivedMemoryReadable := processB.Handles().Lookup(receivedRequest.Handles[0], handle.TypeMemory, capability.Read)
	reply := ipc.Message{ProtocolID: request.ProtocolID, Version: request.Version, Kind: ipc.KindReply, Correlation: request.Correlation}
	replyStatus := registeredChannel.Send(1, reply, nil, nil, nil)
	_, replyReceiveStatus := registeredChannel.Receive(0)
	if !channelCreated || !channelAInstalled || !channelBInstalled || !channelACanWrite || !channelBCanRead || !channelsBound ||
		!lifecycleCreated || !lifecycleInstalled || !serviceDeviceInstalled || !vfsChannelCreated || !vfsChannelInstalled || !vfsServerInstalled || !vfsControlCreated || !vfsControlInstalled || !vfsControlServerInstalled || !execControlCreated || !execServerInstalled || !execClientInstalled || !blockChannelCreated || !blockClientInstalled || !blockServerInstalled || !portObjectValid || !dmaObjectValid || !irqObjectValid || !virtioPortInstalled || !virtioIRQInstalled || !virtioDMAInstalled || !lifecycleBound || lifecycleHandle != handle.ID(0x0000000100000001) ||
		serviceDeviceHandle != handle.ID(0x0000000100000002) || vfsChannelHandle != handle.ID(0x0000000100000003) || vfsServerHandle != handle.ID(0x0000000100000004) || vfsControlHandle != handle.ID(0x0000000100000005) || vfsControlServerHandle != handle.ID(0x0000000100000006) || execServerHandle != handle.ID(0x0000000100000007) || execClientHandle != handle.ID(0x0000000100000008) || blockClientHandle != handle.ID(0x0000000100000009) || blockServerHandle != handle.ID(0x000000010000000a) || virtioPortHandle != handle.ID(0x000000010000000b) || virtioIRQHandle != handle.ID(0x000000010000000c) || virtioDMAHandle != handle.ID(0x000000010000000d) || !wireEncoded || !wireDecoded ||
		ipcStatus != ipc.StatusOK || receivedStatus != ipc.StatusOK || receivedRequest.HandleCount != 1 || !receivedMemoryReadable ||
		string(receivedRequest.Payload[:receivedRequest.PayloadSize]) != "ping" || replyStatus != ipc.StatusOK || replyReceiveStatus != ipc.StatusOK {
		console.WriteString("PANIC: typed IPC request/reply failed\n")
		for {
			amd64.Halt()
		}
	}
	if channelA != handle.ID(0x0000000100000002) || channelB != handle.ID(0x0000000100000002) {
		console.WriteString("PANIC: bootstrap channel handle layout changed\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("IPC_WIRE_OK\n")
	console.WriteString("IPC_REQUEST_REPLY_OK\n")
	console.WriteString("IPC_HANDLE_TRANSFER_OK\n")
	consoleDeviceB, consoleGranted := processB.Handles().Insert(handle.TypeDevice, capability.New(1, capability.Write))
	_, consoleBCanWrite := processB.Handles().Lookup(consoleDeviceB, handle.TypeDevice, capability.Write)
	if !consoleGranted || !consoleBCanWrite || consoleDeviceB != handle.ID(0x0000000100000004) {
		console.WriteString("PANIC: TTY device capability setup failed\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("TTY_CAPABILITY_OK\n")

	ramfsRoot := bootRAMFS.Init()
	ramfsDirectory, ramfsDirectoryCreated := bootRAMFS.Mkdir(ramfsRoot, []byte("etc"))
	ramfsFile, ramfsFileCreated := bootRAMFS.Create(ramfsDirectory, []byte("motd"))
	ramfsWritten, ramfsWriteStatus := bootRAMFS.Write(ramfsFile, 0, []byte("GoOS RAMFS"))
	ramfsRead, ramfsReadStatus := bootRAMFS.Read(ramfsFile, 0, ramfsReadback[:])
	ramfsStat, ramfsStatStatus := bootRAMFS.Stat(ramfsFile)
	ramfsEntry, _, ramfsReadDirStatus := bootRAMFS.ReadDir(ramfsDirectory, 0)
	ramfsRenamed := bootRAMFS.Rename(ramfsFile, ramfsRoot, []byte("welcome"))
	ramfsOpened, ramfsOpenStatus := bootRAMFS.Open(ramfsRoot, []byte("welcome"))
	ramfsRequest, ramfsRequestEncoded := ramfs.EncodeRequest(ramfs.Request{Operation: ramfs.OperationOpen, Parent: ramfsRoot, Name: []byte("welcome")}, 1)
	ramfsDecoded, ramfsRequestDecoded := ramfs.DecodeRequest(ramfsRequest)
	if ramfsDirectoryCreated != ramfs.StatusOK || ramfsFileCreated != ramfs.StatusOK || ramfsWriteStatus != ramfs.StatusOK || ramfsWritten != 10 ||
		ramfsReadStatus != ramfs.StatusOK || ramfsRead != 10 || string(ramfsReadback[:ramfsRead]) != "GoOS RAMFS" || ramfsStatStatus != ramfs.StatusOK || ramfsStat.Size != 10 ||
		ramfsReadDirStatus != ramfs.StatusOK || string(ramfsEntry.Name[:ramfsEntry.Size]) != "motd" || ramfsRenamed != ramfs.StatusOK || ramfsOpenStatus != ramfs.StatusOK || ramfsOpened != ramfsFile ||
		!ramfsRequestEncoded || !ramfsRequestDecoded || ramfsDecoded.Operation != ramfs.OperationOpen || ramfsDecoded.Parent != ramfsRoot || string(ramfsDecoded.Name) != "welcome" {
		console.WriteString("PANIC: RAMFS operation failed\n")
		for {
			amd64.Halt()
		}
	}
	if bootRAMFS.Truncate(ramfsFile, 4) != ramfs.StatusOK || bootRAMFS.Unlink(ramfsRoot, []byte("welcome")) != ramfs.StatusOK {
		console.WriteString("PANIC: RAMFS lifecycle failed\n")
		for {
			amd64.Halt()
		}
	}
	if _, status := bootRAMFS.Stat(ramfsFile); status != ramfs.StatusNotFound {
		console.WriteString("PANIC: RAMFS stale node accepted\n")
		for {
			amd64.Halt()
		}
	}
	if _, status := bootRAMFS.Create(ramfsRoot, []byte("bad/name")); status != ramfs.StatusInvalidArgument {
		console.WriteString("PANIC: RAMFS invalid name accepted\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("RAMFS_PROTOCOL_OK\n")
	console.WriteString("RAMFS_METADATA_OK\n")
	console.WriteString("RAMFS_IO_OK\n")
	console.WriteString("RAMFS_BOUNDS_OK\n")

	deviceRoot := deviceRAMFS.Init()
	if bootVFS.Mount([]byte("/"), &bootRAMFS, ramfsRoot) != vfs.StatusOK || bootVFS.Create([]byte("/tmp"), true) != vfs.StatusOK ||
		bootVFS.Mount([]byte("/tmp/dev"), &deviceRAMFS, deviceRoot) != vfs.StatusOK || bootVFS.Create([]byte("/tmp/note"), false) != vfs.StatusOK ||
		bootVFS.Create([]byte("/tmp/dev/console"), false) != vfs.StatusOK {
		console.WriteString("PANIC: VFS namespace setup failed\n")
		for {
			amd64.Halt()
		}
	}
	vfsFile, vfsOpened := bootVFS.Open([]byte("//tmp/./note"), vfs.Read|vfs.Write)
	vfsWritten, vfsWriteStatus := bootVFS.Write(vfsFile, []byte("VFS works"))
	vfsSeekStatus := bootVFS.Seek(vfsFile, 0)
	vfsRead, vfsReadStatus := bootVFS.Read(vfsFile, vfsReadback[:])
	vfsStat, vfsStatStatus := bootVFS.Stat([]byte("/tmp/note"))
	_, mountedConsole, mountedResolveStatus := bootVFS.Resolve([]byte("/tmp/dev/console"))
	deviceConsole, deviceConsoleStatus := deviceRAMFS.Open(deviceRoot, []byte("console"))
	vfsRequest, vfsRequestEncoded := vfs.EncodeRequest(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/tmp/note")}, 2)
	vfsDecoded, vfsRequestDecoded := vfs.DecodeRequest(vfsRequest)
	if vfsOpened != vfs.StatusOK || vfsWriteStatus != vfs.StatusOK || vfsWritten != 9 || vfsSeekStatus != vfs.StatusOK ||
		vfsReadStatus != vfs.StatusOK || vfsRead != 9 || string(vfsReadback[:vfsRead]) != "VFS works" || vfsStatStatus != vfs.StatusOK || vfsStat.Size != 9 ||
		mountedResolveStatus != vfs.StatusOK || deviceConsoleStatus != ramfs.StatusOK || mountedConsole != deviceConsole ||
		!vfsRequestEncoded || !vfsRequestDecoded || vfsDecoded.Operation != vfs.OperationOpen || string(vfsDecoded.Path) != "/tmp/note" {
		console.WriteString("PANIC: VFS operation failed\n")
		for {
			amd64.Halt()
		}
	}
	if bootVFS.Close(vfsFile) != vfs.StatusOK || bootVFS.Rename([]byte("/tmp/note"), []byte("/tmp/renamed")) != vfs.StatusOK {
		console.WriteString("PANIC: VFS lifecycle failed\n")
		for {
			amd64.Halt()
		}
	}
	if _, _, status := bootVFS.Resolve([]byte("/tmp/note")); status != vfs.StatusNotFound {
		console.WriteString("PANIC: VFS stale path cache accepted\n")
		for {
			amd64.Halt()
		}
	}
	if bootVFS.Unlink([]byte("/tmp/renamed")) != vfs.StatusOK {
		console.WriteString("PANIC: VFS unlink failed\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("VFS_NAMESPACE_OK\n")
	console.WriteString("VFS_MOUNT_OK\n")
	console.WriteString("VFS_PATH_OK\n")
	console.WriteString("VFS_CACHE_OK\n")
	console.WriteString("VFS_PROTOCOL_OK\n")

	boundedChannel = ipc.NewChannel()
	notification := ipc.Message{ProtocolID: request.ProtocolID, Version: request.Version, Kind: ipc.KindNotification}
	for index := 0; index < ipc.QueueCapacity; index++ {
		if boundedChannel.Send(0, notification, nil, nil, nil) != ipc.StatusOK {
			console.WriteString("PANIC: IPC queue filled early\n")
			for {
				amd64.Halt()
			}
		}
	}
	if boundedChannel.Send(0, notification, nil, nil, nil) != ipc.StatusResourceExhausted || !boundedChannel.Close(0) {
		console.WriteString("PANIC: IPC bounds or close failed\n")
		for {
			amd64.Halt()
		}
	}
	for index := 0; index < ipc.QueueCapacity; index++ {
		if _, status := boundedChannel.Receive(1); status != ipc.StatusOK {
			console.WriteString("PANIC: IPC queue drain failed\n")
			for {
				amd64.Halt()
			}
		}
	}
	if _, status := boundedChannel.Receive(1); status != ipc.StatusPeerDied {
		console.WriteString("PANIC: IPC peer death missing\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("IPC_BOUNDED_OK\n")
	console.WriteString("IPC_PEER_DEATH_OK\n")

	sharedObject, sharedCreated := sharedRegistry.Create(&pageAllocator, 2)
	sharedHandleA, sharedGranted := sharedObject.Grant(processA.Handles(), capability.Read|capability.Write|capability.Transfer)
	sharedHandleB, sharedTransferred := processA.Handles().Transfer(processB.Handles(), sharedHandleA, handle.TypeMemory, capability.Read)
	const sharedAddress = uint64(0x40010000)
	sharedMappedA := sharedObject.Map(processA.Handles(), sharedHandleA, rootA, sharedAddress, 0, 2, true)
	sharedMappedB := sharedObject.Map(processB.Handles(), sharedHandleB, rootB, sharedAddress, 0, 2, false)
	sharedInitiallyZero := [8]byte{}
	sharedZeroRead := vm.CopyIn(rootB, sharedAddress, sharedInitiallyZero[:])
	sharedPayload := []byte("SHAREDOK")
	sharedReadback := [8]byte{}
	sharedWriteAddress := sharedAddress + vm.PageSize - 4
	sharedWritten := vm.CopyOut(rootA, sharedWriteAddress, sharedPayload)
	sharedRead := vm.CopyIn(rootB, sharedWriteAddress, sharedReadback[:])
	sharedPhysicalA0, sharedResolvedA0 := vm.Resolve(rootA, sharedAddress)
	sharedPhysicalB0, sharedResolvedB0 := vm.Resolve(rootB, sharedAddress)
	sharedPhysicalA1, sharedResolvedA1 := vm.Resolve(rootA, sharedAddress+vm.PageSize)
	sharedPhysicalB1, sharedResolvedB1 := vm.Resolve(rootB, sharedAddress+vm.PageSize)
	if !sharedCreated || !sharedGranted || !sharedTransferred || !sharedMappedA || !sharedMappedB || !sharedZeroRead || sharedInitiallyZero != [8]byte{} ||
		sharedObject.Map(processA.Handles(), memoryHandle, rootA, sharedAddress+4*vm.PageSize, 0, 1, false) ||
		sharedObject.Map(processA.Handles(), sharedHandleA, rootA, sharedAddress+4*vm.PageSize, 2, 1, false) || !sharedWritten || !sharedRead ||
		string(sharedReadback[:]) != "SHAREDOK" || vm.CopyOut(rootB, sharedAddress, []byte{1}) ||
		!sharedResolvedA0 || !sharedResolvedB0 || !sharedResolvedA1 || !sharedResolvedB1 ||
		sharedPhysicalA0 != sharedPhysicalB0 || sharedPhysicalA1 != sharedPhysicalB1 {
		console.WriteString("PANIC: shared-memory isolation failed\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("SHMEM_OBJECT_OK\n")
	console.WriteString("SHMEM_ZERO_COPY_OK\n")
	console.WriteString("SHMEM_RIGHTS_OK\n")

	probe := amd64.ProbeAddressSpaces(rootA, rootB, pageTable, vm.UserProbeAddress)
	if byte(probe) != 0xa1 || byte(probe>>8) != 0xb2 {
		console.WriteString("PANIC: address-space isolation failed\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("ADDRESS_SPACE_A_OK\n")
	console.WriteString("ADDRESS_SPACE_B_OK\n")
	console.WriteString("PROCESS_ISOLATION_OK\n")

	userProgram := [...]byte{
		0xb9, 0x00, 0x00, 0x00, 0x01, // mov ecx, 0x01000000: remain preemptible.
		0xff, 0xc9, // dec ecx.
		0x75, 0xfc, // jnz dec ecx.
		0xb8, 0x02, 0x00, 0x00, 0x00, // mov eax, 2: DebugWrite.
		0x48, 0xbf, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // mov rdi, 0: invalid pointer.
		0xbe, 0x01, 0x00, 0x00, 0x00, // mov esi, 1.
		0xcd, 0x80,
		0x83, 0xf8, 0x01, // cmp eax, InvalidArgument.
		0x74, 0x02, // je valid write.
		0x0f, 0x0b,
		0xb8, 0x02, 0x00, 0x00, 0x00, // mov eax, 2: DebugWrite.
		0x48, 0xbf, 0x00, 0x40, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00, // mov rdi, UserDataAddress.
		0xbe, 0x14, 0x00, 0x00, 0x00, // mov esi, 20.
		0xcd, 0x80,
		0x85, 0xc0, // test eax, eax.
		0x74, 0x02, // je TTY rights check.
		0x0f, 0x0b,
		0xb8, 0x06, 0x00, 0x00, 0x00, // mov eax, 6: ConsoleWrite.
		0x48, 0xbf, 0x04, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, // forged service handle.
		0x48, 0xbe, 0x00, 0x40, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00,
		0xba, 0x01, 0x00, 0x00, 0x00,
		0xcd, 0x80,
		0x83, 0xf8, 0x02, // cmp eax, PermissionDenied.
		0x74, 0x02,
		0x0f, 0x0b,
		0xb8, 0x03, 0x00, 0x00, 0x00, // mov eax, 3: IPCSend.
		0x48, 0xbf, 0x02, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, // mov rdi, channelA.
		0x48, 0xbe, 0x14, 0x40, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00, // mov rsi, UserDataAddress+20.
		0xba, 0x30, 0x00, 0x00, 0x00, // mov edx, TTY wire size.
		0xcd, 0x80,
		0x85, 0xc0,
		0x74, 0x02,
		0x0f, 0x0b,
		0xb8, 0x01, 0x00, 0x00, 0x00, // mov eax, 1: Exit.
		0x31, 0xff, // xor edi, edi: status 0.
		0xcd, 0x80,
		0x0f, 0x0b, // ud2: returning to exited user code is always a bug.
	}
	userMessage := []byte("USER_DEBUG_WRITE_OK\n")
	userData := [20 + ipc.WireHeaderSize + 16]byte{}
	copy(userData[:], userMessage)
	userNotification, userTTYEncoded := tty.Message(tty.OperationWrite, 0, []byte("TTY_SERVICE_OK\n"))
	userWireSize, userWireEncoded := ipc.Encode(userNotification, userData[len(userMessage):])
	faultProgram := []byte{
		0xb8, 0x05, 0x00, 0x00, 0x00,
		0x48, 0xbf, 0x02, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x48, 0xbe, 0x00, 0x40, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00,
		0xba, 0x18, 0x00, 0x00, 0x00,
		0xcd, 0x80,
		0x83, 0xf8, 0x06,
		0x74, 0x02,
		0x0f, 0x0b,
		0xb8, 0x02, 0x00, 0x00, 0x00,
		0x48, 0xbf, 0x30, 0x40, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00,
		0xbe, 0x14, 0x00, 0x00, 0x00,
		0xcd, 0x80,
		0xb8, 0x05, 0x00, 0x00, 0x00,
		0x48, 0xbf, 0x02, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x48, 0xbe, 0x18, 0x40, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00,
		0xba, 0x18, 0x00, 0x00, 0x00,
		0xcd, 0x80,
		0x85, 0xc0,
		0x74, 0x02,
		0x0f, 0x0b,
		0xb8, 0x06, 0x00, 0x00, 0x00,
		0x48, 0xbf, 0x04, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x48, 0xbe, 0x21, 0x30, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00,
		0xba, 0x0f, 0x00, 0x00, 0x00,
		0xcd, 0x80,
		0x85, 0xc0,
		0x74, 0x02,
		0x0f, 0x0b,
		0xb8, 0x02, 0x00, 0x00, 0x00,
		0x48, 0xbf, 0x44, 0x40, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00,
		0xbe, 0x14, 0x00, 0x00, 0x00,
		0xcd, 0x80,
		0x0f, 0x0b,
	}
	faultData := [88]byte{}
	putUint64(faultData[0:8], vm.UserStackAddress)
	putUint64(faultData[8:16], ipc.WireMaxSize)
	putUint64(faultData[16:24], 5)
	putUint64(faultData[24:32], vm.UserStackAddress)
	putUint64(faultData[32:40], ipc.WireMaxSize)
	putUint64(faultData[40:48], 1000)
	copy(faultData[48:68], []byte("IPC_WAIT_TIMEOUT_OK\n"))
	copy(faultData[68:88], []byte("IPC_WAIT_MESSAGE_OK\n"))
	nativeBytes, nativeEncoded := loader.EncodeNative(nativeImageData[:], loader.ImageSource{
		Entry:    vm.UserCodeAddress,
		StackTop: vm.UserStackAddress + vm.PageSize - 16,
		Segments: []loader.SegmentSource{
			{VirtualAddress: vm.UserCodeAddress, MemorySize: uint32(len(userProgram)), Flags: loader.SegmentReadable | loader.SegmentExecutable, Data: userProgram[:]},
			{VirtualAddress: vm.UserStackAddress, MemorySize: uint32(vm.PageSize), Flags: loader.SegmentReadable | loader.SegmentWritable},
			{VirtualAddress: vm.UserDataAddress, MemorySize: uint32(len(userData)), Flags: loader.SegmentReadable, Data: userData[:]},
		},
	})
	faultBytes, faultEncoded := loader.EncodeNative(faultImageData[:], loader.ImageSource{
		Entry:    vm.UserCodeAddress,
		StackTop: vm.UserStackAddress + vm.PageSize - 16,
		Segments: []loader.SegmentSource{
			{VirtualAddress: vm.UserCodeAddress, MemorySize: uint32(len(faultProgram)), Flags: loader.SegmentReadable | loader.SegmentExecutable, Data: faultProgram},
			{VirtualAddress: vm.UserStackAddress, MemorySize: uint32(vm.PageSize), Flags: loader.SegmentReadable | loader.SegmentWritable},
			{VirtualAddress: vm.UserDataAddress, MemorySize: uint32(len(faultData)), Flags: loader.SegmentReadable, Data: faultData[:]},
		},
	})
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
	loadedA, loadedImageA := loader.LoadNative(&pageAllocator, rootA, nativeBytes)
	loadedB, loadedImageB := loader.LoadNative(&pageAllocator, rootB, faultBytes)
	if !userTTYEncoded || !userWireEncoded || userWireSize != ipc.WireHeaderSize+16 || !nativeEncoded || !faultEncoded || !loadedImageA || !loadedImageB {
		console.WriteString("PANIC: native user image setup failed\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("USER_RX_OK\n")
	console.WriteString("USER_STACK_NX_OK\n")
	copyOutProbe := []byte{0x5a}
	if vm.CopyOut(rootA, vm.UserDataAddress, copyOutProbe) || !vm.CopyOut(rootA, vm.UserStackAddress, copyOutProbe) {
		console.WriteString("PANIC: user copyout validation failed\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("USER_COPYOUT_OK\n")
	console.WriteString("KCALL_ABI_OK\n")
	console.WriteString("NATIVE_IMAGE_LOADER_OK\n")
	initTaskID, initTaskAdded := amd64.AddUserTask(&pageAllocator, rootInit, loadedInit.Entry, loadedInit.StackTop)
	userTaskID, userTaskAdded := amd64.AddUserTask(&pageAllocator, rootA, loadedA.Entry, loadedA.StackTop)
	faultTaskID, faultTaskAdded := amd64.AddUserTask(&pageAllocator, rootB, loadedB.Entry, loadedB.StackTop)
	if !initTaskAdded || !userTaskAdded || !faultTaskAdded || !processRegistry.BindTask(1, sched.TaskID(initTaskID)) ||
		!processRegistry.BindTask(2, sched.TaskID(userTaskID)) || !processRegistry.BindTask(3, sched.TaskID(faultTaskID)) {
		console.WriteString("PANIC: userspace task scheduling failed\n")
		for {
			amd64.Halt()
		}
	}
	for userExited == 0 || amd64.UserFaults() == 0 || initAnnounced == 0 || processExitDelivered == 0 || supervisorExitDelivered == 0 || serviceLaunchConfirmed < supervisor.MaxRestarts || userspaceSupervisorOK == 0 || capabilityManifestOK == 0 {
		deliverProcessExits()
		consumeInitControl()
		if initSendObserved != 0 && initSendStatus != kcall.StatusOK {
			console.WriteString("PANIC: init launch IPC failed: ")
			console.WriteHex(uint64(initSendStatus))
			console.WriteString("\n")
			for {
				amd64.Halt()
			}
		}
		if initExitObserved != 0 {
			console.WriteString("PANIC: init process exited: ")
			console.WriteHex(initExitCode)
			console.WriteString("\n")
			for {
				amd64.Halt()
			}
		}
		amd64.Halt()
	}
	console.WriteString("INITRAMFS_DISCOVERY_OK\n")
	console.WriteString("PID1_INIT_OK\n")
	console.WriteString("PROCESS_EXIT_EVENT_OK\n")
	if processReclaimConfirmed == 0 {
		console.WriteString("PANIC: process resources were not reclaimed\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("PROCESS_RECLAIM_OK\n")
	if processWaitConfirmed == 0 {
		console.WriteString("PANIC: process wait record missing\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("PROCESS_WAIT_OK\n")
	console.WriteString("SUPERVISOR_CRASH_EVENT_OK\n")
	console.WriteString("LIFECYCLE_IPC_OK\n")
	console.WriteString("INIT_EXECUTABLE_LOOKUP_OK\n")
	console.WriteString("INIT_CAPABILITY_MANIFEST_OK\n")
	if startupABIConfirmed == 0 {
		console.WriteString("PANIC: native startup ABI not exercised\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("NATIVE_STARTUP_ABI_OK\n")
	console.WriteString("PID1_SERVICE_LAUNCH_OK\n")
	if amd64.UserPreemptions() == 0 {
		console.WriteString("PANIC: userspace task was not preempted\n")
		for {
			amd64.Halt()
		}
	}
	console.WriteString("USER_PREEMPT_OK\n")
	console.WriteString("PROCESS_CR3_SCHED_OK\n")
	console.WriteString("USER_FAULT_ISOLATION_OK\n")
	console.WriteString("USER_RING3_OK\n")
	console.WriteString("NATIVE_PROCESS_OK\n")
	console.WriteString("TTY_RIGHTS_OK\n")
	console.WriteString("Kernel initialized\n")
	console.WriteString("BOOT_OK\n")

	for {
		deliverProcessExits()
		consumeInitControl()
		amd64.Halt()
	}
}
