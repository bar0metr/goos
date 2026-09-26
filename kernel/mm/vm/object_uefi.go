//go:build uefi

package vm

import (
	"goos/kernel/mm/pmm"
	"unsafe"
)

// NewObject allocates a zeroed T in whole, physically contiguous pages below
// the identity-map limit, where a kernel pointer may name it. Registries hold
// their entries in such objects, taken as entries are created, instead of in
// static tables sized for the worst case (ADR 0111).
func NewObject[T any](allocator *pmm.Allocator) (*T, bool) {
	pages := objectPages(sizeOf[T]())
	physical, ok := allocator.AllocateContiguous(pages)
	if !ok {
		return nil, false
	}
	if physical < identityMapStart || physical+pages*PageSize > IdentityMapLimit {
		freeObjectPages(allocator, physical, pages)
		return nil, false
	}
	// SAFETY: AllocateContiguous returned pages exclusively owned by the
	// caller below LowLimit, which the kernel identity-maps; the object lies
	// wholly inside them.
	zeroBytes(uintptr(physical), pages*PageSize)
	return (*T)(unsafe.Pointer(uintptr(physical))), true
}

// FreeObject returns the pages of an object NewObject allocated. The caller
// holds no other pointer to it.
func FreeObject[T any](allocator *pmm.Allocator, object *T) bool {
	if object == nil {
		return false
	}
	pages := objectPages(sizeOf[T]())
	return freeObjectPages(allocator, uint64(uintptr(unsafe.Pointer(object))), pages)
}

func objectPages(size uint64) uint64 { return (size + PageSize - 1) / PageSize }

// sizeOf is the size of a T, measured as the distance between two elements of
// an array at a made-up address. unsafe.Sizeof on a value of a type parameter
// would do, but TinyGo loads the value to measure it, a whole T copied onto
// the stack — a process record overflowed the boot stack that way.
func sizeOf[T any]() uint64 {
	const base = uintptr(PageSize)
	// SAFETY: the pointer is never dereferenced; only element addresses are
	// computed from it.
	array := (*[2]T)(unsafe.Pointer(base))
	return uint64(uintptr(unsafe.Pointer(&array[1])) - base)
}

func freeObjectPages(allocator *pmm.Allocator, physical, pages uint64) bool {
	freed := true
	for page := uint64(0); page < pages; page++ {
		freed = allocator.Free(physical+page*PageSize) && freed
	}
	return freed
}
