//go:build uefi

package main

// Executable objects: kernel memory PID 1 fills with a program it read from a
// filesystem, and launches once the object has sealed (ADR 0097).

import (
	"goos/internal/capability"
	bootuefi "goos/kernel/boot/uefi"
	"goos/kernel/device"
	"goos/kernel/executable"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/loader"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	supervisor "goos/userspace/init"
)

var (
	executables    executable.Table
	executableWire [8]byte
	// executablePages views the frames of the object a launch loads. Launches
	// run one at a time in the kernel exclusion domain.
	executablePages [executable.MaxPages][]byte
)

// ExecutableCreate creates an empty executable object of size bytes, charged
// to the caller's page quota, and writes its handle to address. The loader
// capability authorizes it. The handle carries Write, to fill the object, and
// Execute, to launch it; it is not transferable, so the object has exactly one
// holder and is freed with that handle.
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
	object, reserved := executables.Reserve(size)
	if !reserved {
		return kcall.StatusResourceExhausted
	}
	objectID := object.ID()
	if !owner.Charge(process.ResourcePages, uint32(object.Pages())) {
		executables.Remove(objectID)
		return kcall.StatusResourceExhausted
	}
	for !object.Complete() {
		physical, ok := pageAllocator.Allocate()
		if ok && !vm.InitializePhysical(physical, nil) {
			pageAllocator.Free(physical)
			ok = false
		}
		if !ok || !object.AddFrame(physical) {
			if ok {
				pageAllocator.Free(physical)
			}
			destroyExecutable(owner, objectID)
			return kcall.StatusResourceExhausted
		}
	}
	created, inserted := owner.Handles().Insert(handle.TypeMemory, capability.New(objectID, capability.Write|capability.Execute))
	if !inserted {
		destroyExecutable(owner, objectID)
		return kcall.StatusResourceExhausted
	}
	putUint64(executableWire[:], uint64(created))
	if !vm.CopyOut(handler.root, address, executableWire[:]) {
		owner.Handles().Close(created)
		destroyExecutable(owner, objectID)
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
	frames := object.Frames()
	for copied := uint64(0); copied < length; {
		position := offset + copied
		page, mapped := vm.PhysicalPage(frames[position/vm.PageSize])
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

// destroyExecutable frees an executable object's frames. owner is nil during
// process teardown, which returns the whole page reservation at once.
func destroyExecutable(owner *process.Process, id uint64) {
	object, found := executables.Find(id)
	if !found {
		return
	}
	for _, frame := range object.Frames() {
		pageAllocator.Free(frame)
	}
	if owner != nil {
		owner.Release(process.ResourcePages, uint32(object.Pages()))
	}
	executables.Remove(id)
}

// closeExecutable closes the one handle to an executable object and frees it.
// It reports false when id is not such a handle.
func closeExecutable(owner *process.Process, id handle.ID, cap capability.Capability) bool {
	if !executable.IsObject(cap.Object()) || !owner.Handles().Close(id) {
		return false
	}
	destroyExecutable(owner, cap.Object())
	return true
}

// launchSource returns the bytes a launch names and the shared-page key for
// them. Execute on the handle is the authority: on the system image object,
// for an extent of the image (ADR 0093), or on a sealed executable object, for
// the whole object (ADR 0097). An object's pages are loaded privately: its
// identity is not the identity of its bytes, so they are not shared.
func launchSource(named supervisor.Executable) (loader.Source, uint64, supervisor.LaunchStatus) {
	handles := bootInitProcess.Handles()
	if authority, allowed := handles.Lookup(named.Image, handle.TypeDevice, capability.Execute); allowed {
		if _, isImage := device.DecodeImageObject(authority.Object()); !isImage {
			return loader.Source{}, 0, supervisor.LaunchCapabilityDenied
		}
		image, key, inside := loader.ImageExtent(bootuefi.Initramfs(&bootInfo), uint64(named.Offset), uint64(named.Size))
		if !inside {
			return loader.Source{}, 0, supervisor.LaunchInvalidImage
		}
		return loader.Contiguous(image), key, supervisor.LaunchOK
	}
	authority, allowed := handles.Lookup(named.Image, handle.TypeMemory, capability.Execute)
	object, known := executables.Find(authority.Object())
	if !allowed || !known {
		return loader.Source{}, 0, supervisor.LaunchCapabilityDenied
	}
	if !object.Sealed() || named.Offset != 0 || uint64(named.Size) != object.Size() {
		return loader.Source{}, 0, supervisor.LaunchInvalidImage
	}
	frames := object.Frames()
	for index, frame := range frames {
		page, mapped := vm.PhysicalPage(frame)
		if !mapped {
			return loader.Source{}, 0, supervisor.LaunchInvalidImage
		}
		executablePages[index] = page
	}
	source, ok := loader.Paged(executablePages[:len(frames)], vm.PageSize, object.Size())
	if !ok {
		return loader.Source{}, 0, supervisor.LaunchInvalidImage
	}
	return source, 0, supervisor.LaunchOK
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
