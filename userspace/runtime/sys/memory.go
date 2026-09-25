package sys

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"unsafe"
)

// ImageMap maps the read-only boot system image at its fixed window and
// returns it. The image capability authorizes the mapping (ADR 0070).
func ImageMap(device handle.ID) ([]byte, kcall.Status) {
	imageWire[0], imageWire[1] = 0, 0
	if result := status(imageMap(uint64(device), vm.UserImageAddress, unsafe.Pointer(&imageWire[0]))); result != kcall.StatusOK {
		return nil, result
	}
	if imageWire[0] != vm.UserImageAddress || imageWire[1] == 0 || imageWire[1] > vm.UserImageLimit-vm.UserImageAddress {
		return nil, kcall.StatusInvalidArgument
	}
	return window(imageWire[0], imageWire[1]), kcall.StatusOK
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

// MemoryMapAnywhere maps the shared-memory object at an address the kernel
// chooses and returns size bytes of it.
func MemoryMapAnywhere(memory handle.ID, size uint64, writable bool) ([]byte, kcall.Status) {
	flags := kcall.MemoryMapAnywhere
	if writable {
		flags |= kcall.MemoryMapWritable
	}
	valueWire = 0
	if size == 0 {
		return nil, kcall.StatusInvalidArgument
	}
	if result := status(memoryMap(uint64(memory), uint64(uintptr(unsafe.Pointer(&valueWire))), flags)); result != kcall.StatusOK {
		return nil, result
	}
	return window(valueWire, size), kcall.StatusOK
}

// DMARegion is memory a device may read and write: the processor sees Memory,
// the device sees Physical.
type DMARegion struct {
	Physical uint64
	Memory   []byte
}

// DMAAllocate allocates the DMA region the device capability's domain allows
// at virtual and returns it, or InvalidArgument if the kernel reported a
// region other than the one asked for.
func DMAAllocate(device handle.ID, virtual uint64, size uint64) (DMARegion, kcall.Status) {
	dmaWire[0], dmaWire[1], dmaWire[2] = 0, 0, 0
	if result := status(dmaAllocate(uint64(device), virtual, unsafe.Pointer(&dmaWire[0]))); result != kcall.StatusOK {
		return DMARegion{}, result
	}
	if dmaWire[0] == 0 || dmaWire[1] != virtual || dmaWire[2] != size {
		return DMARegion{}, kcall.StatusInvalidArgument
	}
	return DMARegion{Physical: dmaWire[0], Memory: window(virtual, size)}, kcall.StatusOK
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
	valueWire = 0
	result := status(executableCreate(uint64(loader), size, unsafe.Pointer(&valueWire)))
	if result != kcall.StatusOK {
		return 0, result
	}
	return handle.ID(valueWire), kcall.StatusOK
}

// ExecutableWrite appends data, at most kcall.MaxExecutableWrite bytes, to an
// executable object. The object seals when its last byte is written.
func ExecutableWrite(executable handle.ID, data []byte) kcall.Status {
	return status(executableWrite(uint64(executable), address(data), uintptr(len(data))))
}
