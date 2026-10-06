//go:build uefi

package main

// Process launch: PID 1's lifecycle requests, the spawn itself, and the
// unwinding of a spawn that fails part way (ADR 0022, 0062, 0093).

import (
	"goos/internal/capability"
	"goos/internal/logfmt"
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	linuxelf "goos/kernel/compat/linux/elf"
	"goos/kernel/diag"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/loader"
	"goos/kernel/mm/region"
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

// launchLocks is the set a launch takes. The new process's address space
// needs no lock: no other processor can reach it until it is registered.
const launchLocks = registryWrite | linuxState | lifecycleQueue | handleWrite | channels | streams | sharedMemory

// consumeInitControl serves one request PID 1 sent on the lifecycle channel.
func consumeInitControl() {
	takeLocks(launchLocks, nil, nil)
	consumeInitControlLocked()
	dropLocks(launchLocks, nil)
}

func consumeInitControlLocked() {
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
	initSpace := bootInitProcess.Space()
	initSpace.RLock()
	copied := len(staged) == 0 || vm.CopyIn(bootInitProcess.AddressSpace().Root, supervisor.StagingAddress+uint64(event.StartupSlot)*supervisor.StagingSlotSize, staged)
	initSpace.RUnlock()
	if copied {
		executableLock.Lock()
		pid, processHandle, launchStatus = spawnExecutable(event.Executable, event.ProcessName(), event.Capabilities[:event.CapabilityCount], event.ResourceProfile, staged)
		executableLock.Unlock()
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

// spawnExecutable starts the native or Linux image that executable names. The kernel
// resolves no path here: PID 1 has already found the image under its own
// search policy, and names it by an extent of the system image or by an
// executable object it filled, together with the handle that authorizes
// executing from it (ADR 0093, ADR 0097). The only name the
// kernel knows at launch is the bootstrap program's, in bootContinue.
func spawnExecutable(executable supervisor.Executable, name []byte, grants []supervisor.Capability, resourceProfile supervisor.ResourceProfile, startupData []byte) (process.ID, handle.ID, supervisor.LaunchStatus) {
	local := thisCPU()
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
	image, origin, sourceStatus := launchSource(executable)
	began = launchStageDone(launchLookup, began)
	if sourceStatus != supervisor.LaunchOK {
		return 0, 0, sourceStatus
	}
	actualFormat := classifyExecutable(image)
	if actualFormat == supervisor.ExecutableUnknown || executable.Format != actualFormat {
		return 0, 0, supervisor.LaunchInvalidImage
	}
	processABI := process.ABINative
	linuxImage := &linuxLaunchImage
	if actualFormat == supervisor.ExecutableLinuxX8664 {
		if !validLinuxLaunch(grants, resourceProfile, startupData) {
			return 0, 0, supervisor.LaunchCapabilityDenied
		}
		// A Linux root obeys the same native reserve as a forked child,
		// checked before anything is built for it.
		if !linuxcompat.LinuxProcessCreationAllowed(processRegistry.Count()) ||
			!linuxcompat.LinuxTaskCreationAllowed(amd64.FreeTaskSlots()) {
			return 0, 0, supervisor.LaunchResourceExhausted
		}
		if linuxelf.ParseInto(linuxImage, image, linuxelf.GoOSConfig()) != linuxelf.OK {
			return 0, 0, supervisor.LaunchInvalidImage
		}
		processABI = process.ABILinuxX8664
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
	// The region map is built while the image loads and moves into the
	// process slot once there is one (ADR 0104).
	launchRegions.Init(vm.UserAddressStart, vm.UserAddressLimit)
	loaded := loader.LoadedImage{}
	if processABI == process.ABINative {
		loaded, ok = loader.LoadImage(&pageAllocator, root, image, &imagePages, origin, &launchRegions)
	} else {
		layout, mapped := linuxelf.MapImage(linuxImage, image, origin, &launchRegions)
		loaded.Entry, loaded.StackTop, ok = layout.Entry, layout.StackTop, mapped
	}
	began = launchStageDone(launchLoad, began)
	if !ok {
		abandonSpawn(root, nil, false)
		return 0, 0, supervisor.LaunchInvalidImage
	}
	pid := nextProcessID
	spawned := freeProcessSlot()
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
	profile, validProfile := launchResourceProfile(resourceProfile)
	if !validProfile {
		abandonSpawn(root, spawned, false)
		return 0, 0, supervisor.LaunchCapabilityDenied
	}
	ok = process.InitWithProfile(spawned, pid, processABI, process.AddressSpace{Root: root}, profile)
	spawned.Regions().CopyFrom(&launchRegions)
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
		_, derived, _ := spawned.Handles().Describe(installed[index])
		if grant.Type == handle.TypeMemory {
			retainMemory(derived)
		}
		if !streamRegistry.Placed(grant.Type, derived.Object()) {
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
		// The template is the kernel's own copy, so it is bound in place.
		bound, boundOK := startup.Data{}, startup.BindHandles(startupData, installed[:len(grants)])
		if boundOK {
			bound, boundOK = startup.Decode(startupData)
		}
		if !boundOK {
			abandonSpawn(root, spawned, false)
			return 0, 0, supervisor.LaunchInvalidImage
		}
		if processABI == process.ABILinuxX8664 {
			// Every Linux image gets AT_RANDOM, as on Linux (ADR 0185).
			var random *[linuxabi.RandomSize]byte
			if randomBytes(local.linuxStackRandom[:]) {
				random = &local.linuxStackRandom
			}
			linuxLaunchStartup = bound
			stack, stackStatus := linuxabi.BuildInitialStack(linuxInitialStack[:], loaded.StackTop, linuxImage, &linuxLaunchStartup, random)
			linuxLaunchStartup = startup.Data{}
			clear(local.linuxStackRandom[:])
			brkBase, brkLimit, brkOK := linuxcompat.BrkBounds(linuxImage.LoadEnd(), vm.PageSize, vm.UserMapBase)
			streams, streamHandles, streamsOK := linuxStandardStreams(spawned, bound)
			if stackStatus != linuxabi.StackOK || !brkOK || !streamsOK || !linuxabi.InstallInitialStack(&pageAllocator, root, stack) || !linuxcompat.InstallProcess(root, pid, bound, streams, brkBase, brkLimit) {
				abandonSpawn(root, spawned, false)
				return 0, 0, supervisor.LaunchInvalidImage
			}
			// The shared descriptions now own these endpoint references. The
			// process table keeps only the VFS channel used by its requests.
			for _, id := range streamHandles {
				spawned.Handles().Close(id)
			}
			loaded.StackTop = stack.Pointer
		} else {
			if !mapStartup(root, spawned.Regions(), bound.Bytes()) {
				abandonSpawn(root, spawned, false)
				return 0, 0, supervisor.LaunchInvalidImage
			}
			startupABIConfirmed = 1
		}
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
	// A process started from an executable object keeps reading it, so it
	// holds a reference and, the first time, takes over the charge.
	if origin.Object != 0 && !adoptExecutable(spawned, origin.Object) {
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
	bound := processRegistry.BindTask(pid, sched.TaskID(task))
	amd64.StartTask(sched.TaskID(task))
	if !bound {
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

// freeProcessSlot returns an unused process record, or nil when every slot is
// taken or a new record's pages cannot be allocated.
func freeProcessSlot() *process.Process {
	for index := range spawnedProcesses {
		// A slot gets its record's pages the first time it is needed and
		// keeps them for the processes that reuse it: other kernel
		// structures point at a live record, and reuse never frees one they
		// might still name (ADR 0111).
		if spawnedProcesses[index] == nil {
			allocated, ok := vm.NewObject[process.Process](&pageAllocator)
			if !ok {
				return nil
			}
			spawnedProcesses[index] = allocated
		}
		if spawnedProcesses[index].ID() == 0 {
			return spawnedProcesses[index]
		}
	}
	return nil
}

// launchResourceProfile maps the lifecycle ABI to kernel-owned quota sets.
// PID 1 chooses among these sets and cannot submit numeric limits.
func launchResourceProfile(profile supervisor.ResourceProfile) (process.ResourceProfile, bool) {
	switch profile {
	case supervisor.ProfileTiny:
		return process.ProfileTiny, true
	case supervisor.ProfileNormal:
		return process.ProfileNormal, true
	case supervisor.ProfileLarge:
		return process.ProfileLarge, true
	default:
		return 0, false
	}
}

// abandonSpawn releases a launch that failed before its task existed: the
// derived handles, the registry entry, the process slot, and the address space.
func abandonSpawn(root uint64, spawned *process.Process, registered bool) {
	linuxcompat.RemoveProcess(root, releaseLinuxDescriptionReference)
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

// linuxStandardStreams resolves the endpoint objects installed for the three
// conventional descriptors. Their references can then move from native
// handles into compatibility-owned open-file descriptions.
func linuxStandardStreams(owner *process.Process, data startup.Data) (linuxcompat.StandardStreams, [3]handle.ID, bool) {
	if owner == nil {
		return linuxcompat.StandardStreams{}, [3]handle.ID{}, false
	}
	input, inputOK := data.Handle("in")
	output, outputOK := data.Handle("stdout")
	errorOutput, errorOK := data.Handle("stderr")
	if !inputOK || !outputOK || !errorOK {
		return linuxcompat.StandardStreams{}, [3]handle.ID{}, false
	}
	inputCapability, inputAllowed := owner.Handles().Lookup(input, handle.TypeStream, capability.Read)
	outputCapability, outputAllowed := owner.Handles().Lookup(output, handle.TypeStream, capability.Write)
	errorCapability, errorAllowed := owner.Handles().Lookup(errorOutput, handle.TypeStream, capability.Write)
	if !inputAllowed || !outputAllowed || !errorAllowed {
		return linuxcompat.StandardStreams{}, [3]handle.ID{}, false
	}
	return linuxcompat.StandardStreams{
		Input:  inputCapability.Object(),
		Output: outputCapability.Object(),
		Error:  errorCapability.Object(),
	}, [3]handle.ID{input, output, errorOutput}, true
}

func abandonHandle(typeID handle.Type, cap capability.Capability) {
	switch typeID {
	case handle.TypeStream:
		dropStreamCapability(cap)
	case handle.TypeMemory:
		releaseMemory(cap)
	}
}

// launchRegions is the region map of the process being launched. Only one
// launch runs at a time.
var launchRegions region.Map

// linuxInitialStack is shared launch scratch. Launches are serialized by PID 1
// and the final bytes are copied into private process pages before task start.
var linuxInitialStack [linuxabi.MaxInitialStackSize]byte

// linuxLaunchImage is the parsed ELF of the launch in progress. The record is
// over 1 KiB, so a local copy would be allocated on the kernel heap and never
// freed; the same serialization that guards linuxInitialStack guards it.
var linuxLaunchImage linuxelf.Image

// linuxLaunchStartup is the bound startup record of the Linux launch in
// progress. The stack builder takes it as an interface, and boxing a local
// value would allocate on the kernel heap at every launch.
var linuxLaunchStartup startup.Data

func classifyExecutable(source loader.Source) supervisor.ExecutableFormat {
	length := min(source.Len(), uint64(8))
	var scratch [8]byte
	header, ok := source.Read(0, length, scratch[:])
	if !ok {
		return supervisor.ExecutableUnknown
	}
	return supervisor.ClassifyExecutable(header)
}

// validLinuxLaunch is the kernel-side capability boundary. Handle names in
// the startup template bind each grant to its role; no device or management
// authority can be smuggled into a Linux process as an extra grant.
func validLinuxLaunch(grants []supervisor.Capability, profile supervisor.ResourceProfile, template []byte) bool {
	if profile != supervisor.ProfileLarge || len(grants) != 4 || len(template) == 0 {
		return false
	}
	data, ok := startup.DecodeTemplate(template)
	if !ok || data.HandleCount() != 4 {
		return false
	}
	names := [...]string{"in", "stdout", "stderr", "vfs"}
	types := [...]handle.Type{handle.TypeStream, handle.TypeStream, handle.TypeStream, handle.TypeChannel}
	rights := [...]capability.Rights{
		capability.Read | capability.Wait,
		capability.Write | capability.Wait,
		capability.Write | capability.Wait,
		capability.Read | capability.Write | capability.Wait,
	}
	for index := range grants {
		name, id := data.HandleAt(index)
		if id != 0 || !equalLaunchName(name, names[index]) || grants[index].Type != types[index] || grants[index].Rights != rights[index] {
			return false
		}
	}
	return grants[3].Consume
}

func equalLaunchName(name []byte, expected string) bool {
	if len(name) != len(expected) {
		return false
	}
	for index := range name {
		if name[index] != expected[index] {
			return false
		}
	}
	return true
}

// mapStartup gives a new process its bound startup object as private
// read-only pages, recorded as a pinned region; they are released with the
// address space.
func mapStartup(root uint64, regions *region.Map, bound []byte) bool {
	pages := (uint64(len(bound)) + vm.PageSize - 1) / vm.PageSize
	if regions.Insert(region.Region{Base: vm.UserStartupAddress, Length: pages * vm.PageSize, Protection: region.Read, Backing: region.Device, Flags: region.Pinned}) != region.OK {
		return false
	}
	for page := uint64(0); page < pages; page++ {
		physical, ok := pageAllocator.Allocate()
		if !ok {
			return false
		}
		end := min((page+1)*vm.PageSize, uint64(len(bound)))
		if !vm.InitializePhysical(physical, bound[page*vm.PageSize:end]) {
			pageAllocator.Free(physical)
			return false
		}
		// A page mapped here is owned by the address space, which frees it
		// if the launch is abandoned after this.
		if _, mapped := vm.MapUserPageAt(&pageAllocator, root, vm.UserStartupAddress+page*vm.PageSize, physical, vm.Permissions{User: true}, true); !mapped {
			pageAllocator.Free(physical)
			return false
		}
	}
	return true
}

// StartupRead copies the first length bytes of the caller's startup pages to
// address, or zeros past their end and for a process started without them.
// A process reads the header, learns the object's size, and then reads the
// rest or looks at the pages directly (ADR 0109).
func (handler nativeCallHandler) StartupRead(address uint64, length uint64) kcall.Status {
	local := thisCPU()
	owner, found := handler.self()
	if !found || length == 0 || length > kcall.MaxStartupSize {
		return kcall.StatusPermissionDenied
	}
	available := uint64(0)
	if entry, mapped := owner.Regions().Find(vm.UserStartupAddress); mapped && entry.Base == vm.UserStartupAddress {
		available = entry.Length
	}
	for done := uint64(0); done < length; {
		chunk := local.startupChunk[:min(uint64(len(local.startupChunk)), length-done)]
		clear(chunk)
		if done < available {
			chunk = chunk[:min(uint64(len(chunk)), available-done)]
			if !vm.CopyIn(handler.root, vm.UserStartupAddress+done, chunk) {
				return kcall.StatusInvalidArgument
			}
		}
		if !vm.CopyOut(handler.root, address+done, chunk) {
			return kcall.StatusInvalidArgument
		}
		done += uint64(len(chunk))
	}
	return kcall.StatusOK
}
