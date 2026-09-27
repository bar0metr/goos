//go:build uefi

package main

// Executable sources: sealed snapshots and immutable VFS-backed open objects
// used without pathname lookup in the kernel (ADR 0097, ADR 0121).

import (
	"goos/internal/capability"
	bootuefi "goos/kernel/boot/uefi"
	"goos/kernel/device"
	"goos/kernel/executable"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/loader"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	supervisor "goos/userspace/init"
)

var (
	executables          executable.Table
	executableWire       [8]byte
	executableSourceWire [32]byte
	// launchObject is the object a launch is reading, for executablePage.
	// Launches run one at a time in the kernel exclusion domain.
	launchObject *executable.Object
)

// executablePage is the page source of the object a launch reads. It is a
// plain function, not a closure, because the kernel heap never frees.
func executablePage(index uint64) ([]byte, bool) {
	if launchObject == nil {
		return nil, false
	}
	frame, ok := launchObject.Frame(vm.PhysicalPage, index)
	if !ok {
		return nil, false
	}
	return vm.PhysicalPage(frame)
}

// ExecutableSource consumes a prepared VFS session and creates a sealed
// executable source. descriptor contains the file handle, file size, and the
// sealed header object, then receives that object's handle as the source.
func (handler nativeCallHandler) ExecutableSource(id uint64, session uint64, descriptor uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(id), handle.TypeDevice, capability.Write)
	if !allowed || authority.Object() != device.LoaderObject {
		return kcall.StatusPermissionDenied
	}
	endpointCap, allowed := owner.Handles().Lookup(handle.ID(session), handle.TypeChannel, capability.Read|capability.Write|capability.Wait)
	channelID, side, valid := ipc.DecodeEndpointObject(endpointCap.Object())
	channel, live := channelRegistry.Lookup(channelID)
	if !allowed || !valid || !live || !channel.Alive(side) {
		return kcall.StatusPermissionDenied
	}
	clear(executableSourceWire[:])
	if !vm.CopyIn(handler.root, descriptor, executableSourceWire[:]) {
		return kcall.StatusInvalidArgument
	}
	if !vm.CopyOut(handler.root, descriptor, executableSourceWire[:]) {
		return kcall.StatusInvalidArgument
	}
	file := decodeUint64(executableSourceWire[0:8])
	size := decodeUint64(executableSourceWire[8:16])
	headerHandle := handle.ID(decodeUint64(executableSourceWire[16:24]))
	headerCap, allowed := owner.Handles().Lookup(headerHandle, handle.TypeMemory, capability.Execute|capability.Write)
	object, known := executables.Find(headerCap.Object())
	headerSize := min(size, uint64(vm.PageSize))
	if file == 0 || size == 0 || size > executable.MaxSize || !allowed || !known || !object.Sealed() || object.FileBacked() || object.Size() != headerSize {
		return kcall.StatusInvalidArgument
	}
	if !object.SetSource(size, channelID, side, file) {
		return kcall.StatusInvalidArgument
	}
	// Ownership moves into the source. The same object handle now names the
	// full file-backed image and cannot accept further writes.
	owner.Handles().Close(handle.ID(session))
	putUint64(executableSourceWire[24:32], uint64(headerHandle))
	if !vm.CopyOut(handler.root, descriptor, executableSourceWire[:]) {
		panicSerial("executable source descriptor changed after validation")
	}
	return kcall.StatusOK
}

// ExecutableCreate creates an empty executable object of size bytes, charged
// to the caller's page quota, and writes its handle to address. The loader
// capability authorizes it. The handle carries Write, to fill the object,
// Execute, to launch it, and Transfer, so the launcher can hand the VFS a
// Write-only copy to fill it with (ADR 0107). Every handle and every process
// started from the object is a reference; the last one frees it.
func (handler nativeCallHandler) ExecutableCreate(id uint64, size uint64, address uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(id), handle.TypeDevice, capability.Write)
	if !allowed || authority.Object() != device.LoaderObject {
		return kcall.StatusPermissionDenied
	}
	if size == 0 || size > executable.MaxSize {
		return kcall.StatusInvalidArgument
	}
	// The result pointer is checked before anything is allocated, so a bad
	// one cannot leave an object without a handle behind.
	clear(executableWire[:])
	if !vm.CopyOut(handler.root, address, executableWire[:]) {
		return kcall.StatusInvalidArgument
	}
	if owner.Handles().Available() == 0 {
		return kcall.StatusResourceExhausted
	}
	object, reserved := executables.Reserve(size, uint64(owner.ID()))
	if !reserved {
		return kcall.StatusResourceExhausted
	}
	objectID := object.ID()
	if !owner.Charge(process.ResourcePages, object.Charge()) {
		executables.Remove(objectID)
		return kcall.StatusResourceExhausted
	}
	for !object.Complete() {
		physical, ok := pageAllocator.Allocate()
		if ok && !vm.InitializePhysical(physical, nil) {
			pageAllocator.Free(physical)
			ok = false
		}
		if !ok || !object.AddFrame(&pageAllocator, vm.PhysicalPage, physical) {
			if ok {
				pageAllocator.Free(physical)
			}
			destroyExecutable(objectID)
			return kcall.StatusResourceExhausted
		}
	}
	created, inserted := owner.Handles().Insert(handle.TypeMemory, capability.New(objectID, capability.Write|capability.Execute|capability.Transfer))
	if !inserted {
		destroyExecutable(objectID)
		return kcall.StatusResourceExhausted
	}
	putUint64(executableWire[:], uint64(created))
	if !vm.CopyOut(handler.root, address, executableWire[:]) {
		owner.Handles().Close(created)
		destroyExecutable(objectID)
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// ExecutableWrite appends length bytes at address to an executable object.
// The object seals when its last byte is written and accepts no write after
// that; a write that would pass its end is refused whole.
func (handler nativeCallHandler) ExecutableWrite(id uint64, address uint64, length uint64) kcall.Status {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(id), handle.TypeMemory, capability.Write)
	object, known := executables.Find(authority.Object())
	if !allowed || !known {
		return kcall.StatusPermissionDenied
	}
	offset, ok := object.NextWrite(length)
	if !ok {
		return kcall.StatusInvalidArgument
	}
	for copied := uint64(0); copied < length; {
		position := offset + copied
		frame, found := object.Frame(vm.PhysicalPage, position/vm.PageSize)
		page, mapped := vm.PhysicalPage(frame)
		mapped = mapped && found
		within := position % vm.PageSize
		chunk := min(vm.PageSize-within, length-copied)
		// A fault part way leaves the object's length where it was, so the
		// bytes already copied are overwritten by the next write.
		if !mapped || !vm.CopyIn(handler.root, address+copied, page[within:within+chunk]) {
			return kcall.StatusInvalidArgument
		}
		copied += chunk
	}
	object.Advance(length)
	return kcall.StatusOK
}

// destroyExecutable frees an executable object's frames and returns their
// charge to the process paying for them, unless that process is the one being
// torn down, whose whole account goes at once.
func destroyExecutable(id uint64) {
	object, found := executables.Find(id)
	if !found {
		return
	}
	if channelID, side, _, fileBacked := object.Source(); fileBacked {
		if channel, live := channelRegistry.Lookup(channelID); live && channel.Close(side) {
			wakeIPCWaiters(channelID, side^1, ipc.WaitPeerDied)
			releaseChannelSlot(channelID)
		}
	}
	object.ReleaseFrames(&pageAllocator, vm.PhysicalPage)
	if payer := process.ID(object.Payer()); payer != releasingProcess {
		if holder, alive := processRegistry.ByID(payer); alive {
			holder.Release(process.ResourcePages, object.Charge())
		}
	}
	executables.Remove(id)
}

// dropExecutable removes one reference to an executable object — a closed
// handle, an exited process — and frees it with the last.
func dropExecutable(id uint64) {
	if object, found := executables.Find(id); found && object.Drop() {
		destroyExecutable(id)
	}
}

// closeExecutable closes the handle to an executable object and drops its
// reference. It reports false when id is not such a handle.
func closeExecutable(owner *process.Process, id handle.ID, cap capability.Capability) bool {
	if !executable.IsObject(cap.Object()) || !owner.Handles().Close(id) {
		return false
	}
	dropExecutable(cap.Object())
	return true
}

// adoptExecutable gives a process started from an executable object its
// reference, and the first such process the object's charge: the process is
// what keeps reading the pages, so it pays for them (ADR 0106).
func adoptExecutable(spawned *process.Process, id uint64) bool {
	object, found := executables.Find(id)
	if !found {
		return false
	}
	if !object.Launched() {
		if !spawned.Charge(process.ResourcePages, object.Charge()) {
			return false
		}
		if payer, alive := processRegistry.ByID(process.ID(object.Payer())); alive {
			payer.Release(process.ResourcePages, object.Charge())
		}
		object.SetPayer(uint64(spawned.ID()))
		object.MarkLaunched()
	}
	object.Retain()
	spawned.SetExecutable(id)
	return true
}

// launchSource returns the bytes a launch names and their origin. Execute on
// the handle is the authority: on the system image object, for an extent of
// the image (ADR 0093), or on a sealed executable source for the whole file
// (ADR 0121). Pages are copied into the process as it touches them and never
// shared: source identity is not process-page identity.
func launchSource(named supervisor.Executable) (loader.Source, loader.Origin, supervisor.LaunchStatus) {
	handles := bootInitProcess.Handles()
	if authority, allowed := handles.Lookup(named.Image, handle.TypeDevice, capability.Execute); allowed {
		if _, isImage := device.DecodeImageObject(authority.Object()); !isImage {
			return loader.Source{}, loader.Origin{}, supervisor.LaunchCapabilityDenied
		}
		image, key, inside := loader.ImageExtent(bootuefi.Initramfs(&bootInfo), uint64(named.Offset), uint64(named.Size))
		if !inside {
			return loader.Source{}, loader.Origin{}, supervisor.LaunchInvalidImage
		}
		return loader.Contiguous(image), loader.Origin{Key: key}, supervisor.LaunchOK
	}
	authority, allowed := handles.Lookup(named.Image, handle.TypeMemory, capability.Execute)
	object, known := executables.Find(authority.Object())
	if !allowed || !known {
		return loader.Source{}, loader.Origin{}, supervisor.LaunchCapabilityDenied
	}
	if !object.Sealed() || named.Offset != 0 || uint64(named.Size) != object.Size() {
		return loader.Source{}, loader.Origin{}, supervisor.LaunchInvalidImage
	}
	launchObject = object
	source, ok := loader.Paged(executablePage, vm.PageSize, object.Size())
	if !ok {
		return loader.Source{}, loader.Origin{}, supervisor.LaunchInvalidImage
	}
	return source, loader.Origin{Object: object.ID()}, supervisor.LaunchOK
}

// executableRejectsInvalid checks, in the validation build, that PID 1's
// loader capability creates a bounded object that seals, refuses writes past
// its end, cannot be mapped, and returns its pages when closed; and that
// another capability creates nothing. The staging page is PID 1's only
// writable page at this point, so it serves as the result and data buffer.
func executableRejectsInvalid(root uint64, loaderHandle handle.ID, otherDevice handle.ID) bool {
	caller := nativeCallHandler{root: root}
	owner, found := processRegistry.ByRoot(root)
	if !found {
		return false
	}
	buffer := supervisor.StagingAddress
	if caller.ExecutableCreate(uint64(otherDevice), 1, buffer) != kcall.StatusPermissionDenied ||
		caller.ExecutableCreate(uint64(loaderHandle), 0, buffer) != kcall.StatusInvalidArgument ||
		caller.ExecutableCreate(uint64(loaderHandle), executable.MaxSize+1, buffer) != kcall.StatusInvalidArgument ||
		caller.ExecutableCreate(uint64(loaderHandle), 1, 0) != kcall.StatusInvalidArgument {
		return false
	}
	free := pageAllocator.Statistics().FreePages
	used, _ := owner.Quota()
	if caller.ExecutableCreate(uint64(loaderHandle), vm.PageSize+1, buffer) != kcall.StatusOK {
		return false
	}
	var wire [8]byte
	if !vm.CopyIn(root, buffer, wire[:]) {
		return false
	}
	created := handle.ID(decodeUint64(wire[:]))
	_, cap, described := owner.Handles().Describe(created)
	object, known := executables.Find(cap.Object())
	sealedEarly := !described || !known || object.Sealed()
	valid := !sealedEarly &&
		caller.ExecutableWrite(uint64(otherDevice), buffer, 1) == kcall.StatusPermissionDenied &&
		caller.ExecutableWrite(uint64(created), 0, 1) == kcall.StatusInvalidArgument &&
		caller.ExecutableWrite(uint64(created), buffer, vm.PageSize) == kcall.StatusOK &&
		caller.ExecutableWrite(uint64(created), buffer, 2) == kcall.StatusInvalidArgument &&
		caller.ExecutableWrite(uint64(created), buffer, 1) == kcall.StatusOK &&
		object.Sealed() &&
		caller.ExecutableWrite(uint64(created), buffer, 1) == kcall.StatusInvalidArgument &&
		caller.MemoryMap(uint64(created), vm.UserSharedAddress, 0) == kcall.StatusPermissionDenied
	if !described || !closeExecutable(owner, created, cap) {
		return false
	}
	after, _ := owner.Quota()
	return valid && pageAllocator.Statistics().FreePages == free && after[process.ResourcePages] == used[process.ResourcePages]
}
