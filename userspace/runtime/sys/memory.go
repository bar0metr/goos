package sys

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/kernel/vfs"
	"goos/userspace/runtime/tls"
	"unsafe"
)

// ImageMap maps the read-only boot system image at its fixed window and
// returns it. The image capability authorizes the mapping (ADR 0070).
func ImageMap(device handle.ID) ([]byte, kcall.Status) {
	w := current()
	defer tls.Unpin()
	w.imageWire[0], w.imageWire[1] = 0, 0
	if result := status(imageMap(uint64(device), vm.UserImageAddress, unsafe.Pointer(&w.imageWire[0]))); result != kcall.StatusOK {
		return nil, result
	}
	if w.imageWire[0] != vm.UserImageAddress || w.imageWire[1] == 0 || w.imageWire[1] > vm.UserImageLimit-vm.UserImageAddress {
		return nil, kcall.StatusInvalidArgument
	}
	return window(w.imageWire[0], w.imageWire[1]), kcall.StatusOK
}

// MemoryMap maps the shared-memory object the handle names at address, which
// must be free in the caller's address space, and returns size bytes of it.
// The object is at least as large as the caller's protocol says; size is that
// bound, not a value the kernel reports.
func MemoryMap(memory handle.ID, address uint64, size uint64, writable bool) ([]byte, kcall.Status) {
	flags := uint64(0)
	if writable {
		flags = kcall.MemoryMapWritable
	}
	if size == 0 || address%vm.PageSize != 0 || address < vm.IdentityMapLimit || address >= vm.UserAddressLimit || size > vm.UserAddressLimit-address {
		return nil, kcall.StatusInvalidArgument
	}
	if result := status(memoryMap(uint64(memory), address, flags)); result != kcall.StatusOK {
		return nil, result
	}
	return window(address, size), kcall.StatusOK
}

// MemoryMapAnywhere maps the whole shared-memory object at an address the
// kernel chooses and returns the address and the object's size, which the
// region the mapping made records.
func MemoryMapAnywhere(memory handle.ID, writable bool) (uint64, uint64, kcall.Status) {
	w := current()
	defer tls.Unpin()
	flags := kcall.MemoryMapAnywhere
	if writable {
		flags |= kcall.MemoryMapWritable
	}
	w.valueWire = 0
	if result := status(memoryMap(uint64(memory), uint64(uintptr(unsafe.Pointer(&w.valueWire))), flags)); result != kcall.StatusOK {
		return 0, 0, result
	}
	base := w.valueWire
	region, result := VMQuery(base)
	if result != kcall.StatusOK || region.Base != base {
		VMUnmap(base, vmPage)
		return 0, 0, kcall.StatusInvalidArgument
	}
	return base, region.Length, kcall.StatusOK
}

const vmPage = 4096

// DMARegion is memory a device may read and write: the processor sees Memory,
// the device sees Physical.
type DMARegion struct {
	Physical uint64
	Memory   []byte
}

// DMAAllocate allocates a DMA region of size bytes, a whole number of pages,
// at virtual, which must be the DMA window, and returns it, or
// InvalidArgument if the kernel reported a region other than the one asked
// for.
func DMAAllocate(device handle.ID, virtual uint64, size uint64) (DMARegion, kcall.Status) {
	w := current()
	defer tls.Unpin()
	w.dmaWire[0], w.dmaWire[1], w.dmaWire[2] = 0, 0, size
	if result := status(dmaAllocate(uint64(device), virtual, unsafe.Pointer(&w.dmaWire[0]))); result != kcall.StatusOK {
		return DMARegion{}, result
	}
	if w.dmaWire[0] == 0 || w.dmaWire[1] != virtual || w.dmaWire[2] != size {
		return DMARegion{}, kcall.StatusInvalidArgument
	}
	return DMARegion{Physical: w.dmaWire[0], Memory: window(virtual, size)}, kcall.StatusOK
}

// String views bytes as a string without copying them, for code that must not
// allocate. The caller must not change the bytes while the string is in use.
func String(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	// SAFETY: a string header is a pointer and a length, the prefix of a slice
	// header; strings are never written through, and the caller keeps the
	// bytes unchanged for as long as it uses the result.
	return unsafe.String(&value[0], len(value))
}

// Bytes views a string as bytes without copying it. The result must not be
// written to: string data may live in read-only memory.
func Bytes(value string) []byte {
	if len(value) == 0 {
		return nil
	}
	// SAFETY: the slice aliases the string's immutable data and has its
	// length; callers only read it.
	return unsafe.Slice(unsafe.StringData(value), len(value))
}

// MappedAtStart returns size bytes at address that the kernel mapped into this
// process before it started, at an address the native ABI fixes: the launch
// staging page PID 1 shares with the kernel (ADR 0062). Nothing checks that
// the range is mapped, so it is for those ABI-defined ranges only.
func MappedAtStart(address uint64, size uint64) []byte { return window(address, size) }

// ExecutableCreate creates an empty executable object of size bytes. The
// loader capability authorizes it; the handle it returns fills the object with
// ExecutableWrite and names it in a launch once full (ADR 0097).
func ExecutableCreate(loader handle.ID, size uint64) (handle.ID, kcall.Status) {
	w := current()
	defer tls.Unpin()
	w.valueWire = 0
	result := status(executableCreate(uint64(loader), size, unsafe.Pointer(&w.valueWire)))
	if result != kcall.StatusOK {
		return 0, result
	}
	return handle.ID(w.valueWire), kcall.StatusOK
}

// ExecutableWrite appends data, at most kcall.MaxExecutableWrite bytes, to an
// executable object. The object seals when its last byte is written.
func ExecutableWrite(executable handle.ID, data []byte) kcall.Status {
	return status(executableWrite(uint64(executable), address(data), uintptr(len(data))))
}

// ExecutableSource consumes session and creates an immutable executable source
// for file. header is the number of prepared bytes in the shared transport
// page; size is the stable file size.
func ExecutableSource(loader, session handle.ID, file vfs.Handle, object handle.ID, size uint64) (handle.ID, kcall.Status) {
	var descriptor [4]uint64
	descriptor[0], descriptor[1], descriptor[2] = uint64(file), size, uint64(object)
	result := status(executableSource(uint64(loader), uint64(session), unsafe.Pointer(&descriptor[0])))
	if result != kcall.StatusOK {
		return 0, result
	}
	return handle.ID(descriptor[3]), kcall.StatusOK
}
