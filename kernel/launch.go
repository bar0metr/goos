//go:build uefi

package main

// Process launch: PID 1's lifecycle requests, the spawn itself, and the
// unwinding of a spawn that fails part way (ADR 0022, 0062, 0093).

import (
	"goos/internal/capability"
	"goos/internal/logfmt"
	"goos/kernel/arch/amd64"
	"goos/kernel/diag"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/loader"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/sched"
	supervisor "goos/userspace/init"
	"goos/userspace/startup"
)

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
	// The template is copied out of the staging page once, so PID 1 cannot
	// change it between validation and use.
	staged := launchTemplate[:event.StartupSize]
	pid, processHandle, launchStatus := process.ID(0), handle.ID(0), supervisor.LaunchInvalidImage
	if len(staged) == 0 || vm.CopyIn(bootInitProcess.AddressSpace().Root, supervisor.StagingAddress+uint64(event.StartupSlot)*supervisor.StagingSlotSize, staged) {
		pid, processHandle, launchStatus = spawnExecutable(event.Executable, event.ProcessName(), event.Capabilities[:event.CapabilityCount], staged)
	}
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

// spawnExecutable starts the native image that executable names. The kernel
// resolves no path here: PID 1 has already found the image under its own
// search policy, and names it by an extent of the system image or by an
// executable object it filled, together with the handle that authorizes
// executing from it (ADR 0093, ADR 0097). The only name the
// kernel knows at launch is the bootstrap program's, in bootContinue.
func spawnExecutable(executable supervisor.Executable, name []byte, grants []supervisor.Capability, startupData []byte) (process.ID, handle.ID, supervisor.LaunchStatus) {
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
	// Execute on an image or executable object is the authority to run what
	// it holds. The filesystem service reads the system image with Read alone
	// and cannot launch from it.
	image, imageKey, sourceStatus := launchSource(executable)
	began = launchStageDone(launchLookup, began)
	if sourceStatus != supervisor.LaunchOK {
		return 0, 0, sourceStatus
	}
	// The pages a launch allocates are counted from the allocator rather than
	// threaded through the loader: the difference is exactly what this address
	// space owns, and it is charged to the new process below.
	pagesBefore := pageAllocator.Statistics().FreePages
	root, ok := vm.CreateUserAddressSpace(&pageAllocator, kernelPageTable)
	began = launchStageDone(launchAddressSpace, began)
	if !ok {
		return 0, 0, supervisor.LaunchAddressSpaceFailed
	}
	loaded, ok := loader.LoadImage(&pageAllocator, root, image, &imagePages, imageKey)
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
	// Every stream grant becomes one more handle naming its end. Refusing
	// here, before anything is installed, keeps the unwinding below free of a
	// count that could not be taken.
	for _, grant := range grants {
		if grant.Type == handle.TypeStream {
			source, _ := bootInitProcess.Handles().Lookup(grant.Handle, grant.Type, capability.Transfer)
			if !streamRegistry.CanRetain(source.Object(), len(grants)) {
				abandonSpawn(root, nil, false)
				return 0, 0, supervisor.LaunchResourceExhausted
			}
		}
	}
	ok = process.Init(spawned, pid, process.AddressSpace{Root: root})
	spawned.SetName(name)
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
		if _, derived, _ := spawned.Handles().Describe(installed[index]); !streamRegistry.Placed(grant.Type, derived.Object()) {
			// Uncounted, so it is closed without dropping a reference.
			spawned.Handles().Close(installed[index])
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
		bound, boundOK := startup.BindHandles(startupData, installed[:len(grants)], &launchBound)
		if !boundOK || !mapStartup(root, bound) {
			abandonSpawn(root, spawned, false)
			return 0, 0, supervisor.LaunchInvalidImage
		}
		startupABIConfirmed = 1
	}
	if !processRegistry.Add(spawned) {
		abandonSpawn(root, spawned, false)
		return 0, 0, supervisor.LaunchProcessFailed
	}
	// Charging happens before the task exists, because a runnable task cannot
	// be withdrawn. An image whose address space does not fit the page quota
	// therefore fails to launch rather than running over its share.
	// Frames the load added to the shared image cache belong to the system
	// image rather than to this process, so they are not charged to it.
	if pagesUsed := pagesBefore - pageAllocator.Statistics().FreePages - loaded.SharedPagesAllocated; pagesUsed != 0 && !spawned.Charge(process.ResourcePages, uint32(pagesUsed)) {
		kernelLogUint(logfmt.Warn, "launch over page quota", "pages", pagesUsed)
		abandonSpawn(root, spawned, true)
		return 0, 0, supervisor.LaunchResourceExhausted
	}
	// The process handle is reserved before the task exists, so a started
	// child is never left without the handle its parent waits on.
	// Write authorizes ProcessKill, so only the parent that launched the
	// process can stop it.
	processHandle, ok := bootInitProcess.Handles().Insert(handle.TypeProcess, capability.New(uint64(pid), capability.Wait|capability.Write))
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
			closeMovedHandle(bootInitProcess.Handles(), grant.Handle)
		}
	}
	return pid, processHandle, supervisor.LaunchOK
}

// abandonSpawn releases a launch that failed before its task existed: the
// derived handles, the registry entry, the process slot, and the address space.
func abandonSpawn(root uint64, spawned *process.Process, registered bool) {
	if spawned != nil {
		// Only stream handles were counted when installed; the source handles
		// stay with PID 1, so no end can close here.
		spawned.Handles().CloseAll(abandonHandle)
		if registered {
			processRegistry.Remove(spawned.ID())
		}
		spawned.Reset()
	}
	if !vm.DestroyUserAddressSpace(&pageAllocator, root) {
		addressSpaceReclaimFailures++
	}
}

func abandonHandle(typeID handle.Type, cap capability.Capability) {
	if typeID == handle.TypeStream {
		dropStreamCapability(cap)
	}
}

// mapStartup gives a new process its bound startup block as a private
// read-only page; it is released with the address space.
func mapStartup(root uint64, bound []byte) bool {
	physical, ok := pageAllocator.Allocate()
	if !ok {
		return false
	}
	if !vm.InitializePhysical(physical, bound) || !vm.MapOwnedUserPage(root, vm.UserStartupAddress, physical, vm.Permissions{User: true}) {
		pageAllocator.Free(physical)
		return false
	}
	return true
}

// StartupRead copies the caller's startup page, or zeros for a process that
// was started without one.
func (handler nativeCallHandler) StartupRead(address uint64, length uint64) kcall.Status {
	if _, found := processRegistry.ByRoot(handler.root); !found || length != kcall.StartupSize {
		return kcall.StatusPermissionDenied
	}
	clear(launchBound[:])
	if _, mapped := vm.ResolveUser(handler.root, vm.UserStartupAddress); mapped && !vm.CopyIn(handler.root, vm.UserStartupAddress, launchBound[:]) {
		return kcall.StatusInvalidArgument
	}
	if !vm.CopyOut(handler.root, address, launchBound[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}
