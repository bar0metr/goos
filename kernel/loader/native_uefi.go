//go:build uefi && amd64

package loader

import (
	"goos/kernel/mm/pmm"
	"goos/kernel/mm/region"
	"goos/kernel/mm/vm"
)

type LoadedImage struct {
	Entry    uint64
	StackTop uint64
	// SharedPagesAllocated counts frames this load added to the shared cache.
	// They belong to the system image, so the caller charges them to no
	// process.
	SharedPagesAllocated uint64
}

// pageScratch gathers one page of a segment whose bytes cross a page of a
// paged source. It is shared because a local array escapes and the kernel's
// leaking heap never frees it; only one image is loaded at a time.
var pageScratch [vm.PageSize]byte

// LoadNative loads a private copy of every segment of an image into an empty
// process address space that has no region map, the kernel's self-test
// processes. Such an image must carry its own stack.
func LoadNative(allocator *pmm.Allocator, root uint64, data []byte) (LoadedImage, bool) {
	return LoadImage(allocator, root, Contiguous(data), nil, Origin{}, nil)
}

// LoadImage validates an image and maps it into an empty process address
// space. All payload bytes are copied before executable permission is
// exposed.
//
// The bytes come from source, an extent of the system image or the pages of a
// sealed executable object (ADR 0097).
//
// With a region map (ADR 0104) and an origin the bytes outlive the load in,
// each segment becomes a pinned image region that records its origin, and no
// private page is loaded now: a page is copied from the origin, or zeroed past
// the segment's file bytes, when the process first touches it (ADR 0106). A
// large program therefore costs only the pages it uses, and is never copied
// whole. An image that leaves its stack to the kernel gets a stack region
// below vm.UserStackTop, with an unmapped guard below it. Without a map, or
// without an origin, every page is loaded.
//
// When shared is not nil and the origin is an extent of the immutable system
// image, each read-only segment is mapped from frames in shared, filling them
// on the first load. Those frames are mapped without the owned bit, so the
// process's teardown leaves them for the next process, and never writable, so
// no process can change what another runs. Writable segments are always
// private.
//
// A failure leaves whatever was mapped in the address space; the caller
// destroys it, which frees every owned page.
func LoadImage(allocator *pmm.Allocator, root uint64, source Source, shared *SharedPages, origin Origin, regions *region.Map) (LoadedImage, bool) {
	key := origin.Key
	lazy := regions != nil && (origin.Key != 0 || origin.Object != 0)
	image, ok := ParseSource(source, vm.PageSize, vm.UserAddressStart, vm.UserImageAreaLimit)
	if !ok || root == 0 || image.StackTop == 0 && regions == nil {
		return LoadedImage{}, false
	}
	if image.StackTop != 0 {
		// ParseNative keeps other segments out of the stack's guard gap; the
		// kernel's own mappings must stay out of it too, now and after the
		// load (ADR 0083, ADR 0088).
		guardStart, guardEnd, guarded := image.StackGuard()
		if !guarded || KernelMapsUserRange(guardStart, guardEnd) {
			return LoadedImage{}, false
		}
		for page := guardStart; page < guardEnd; page += vm.PageSize {
			if vm.UserPageMapped(root, page) {
				return LoadedImage{}, false
			}
		}
		if regions != nil && regions.Insert(region.Region{Base: guardStart, Length: guardEnd - guardStart, Backing: region.Guard, Flags: region.Pinned}) != region.OK {
			return LoadedImage{}, false
		}
	}
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		length := roundUp(segment.MemorySize)
		for offset := uint64(0); offset < length; offset += vm.PageSize {
			if vm.UserPageMapped(root, segment.VirtualAddress+offset) {
				return LoadedImage{}, false
			}
		}
		if regions == nil {
			continue
		}
		entry := region.Region{Base: segment.VirtualAddress, Length: length, Protection: protection(segment.Flags),
			Backing: region.Image, Flags: region.Pinned, Object: origin.Key, Offset: segment.FileOffset}
		if lazy {
			entry.Data = segment.FileSize
			if origin.Object != 0 {
				entry.Object, entry.Flags = origin.Object, region.Pinned|region.FromObject
			}
		}
		if regions.Insert(entry) != region.OK {
			return LoadedImage{}, false
		}
	}
	var loaded LoadedImage
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		pages := int(roundUp(segment.MemorySize) / vm.PageSize)
		shareable := shared != nil && key != 0 && Shareable(segment)
		switch {
		case lazy && !shareable:
			// Every page arrives on first touch.
			continue
		case regions != nil:
			// Only pages with file bytes; the rest is supplied on demand.
			pages = int(roundUp(segment.FileSize) / vm.PageSize)
		}
		permissions := vm.Permissions{
			User:       true,
			Writable:   segment.Flags&SegmentWritable != 0,
			Executable: segment.Flags&SegmentExecutable != 0,
		}
		if pages != 0 && shareable {
			frames, hit := shared.Lookup(key, index, pages)
			if !hit {
				frames, hit = fillShared(allocator, shared, key, index, segment, source, pages)
				if hit {
					loaded.SharedPagesAllocated += uint64(pages)
				}
			}
			if hit {
				for page := 0; page < pages; page++ {
					if _, mapped := vm.MapUserPageAt(allocator, root, segment.VirtualAddress+uint64(page)*vm.PageSize, frames[page], permissions, false); !mapped {
						return LoadedImage{}, false
					}
				}
				continue
			}
		}
		for page := 0; page < pages; page++ {
			offset := uint64(page) * vm.PageSize
			physical, ok := allocatePage(allocator, segment, offset, source)
			if !ok {
				return LoadedImage{}, false
			}
			if _, mapped := vm.MapUserPageAt(allocator, root, segment.VirtualAddress+offset, physical, permissions, true); !mapped {
				allocator.Free(physical)
				return LoadedImage{}, false
			}
		}
	}
	loaded.Entry, loaded.StackTop = image.Entry, image.StackTop
	if image.StackTop == 0 {
		if loaded.StackTop, ok = setUpStack(allocator, root, regions, image.StackReserve); !ok {
			return LoadedImage{}, false
		}
	}
	return loaded, true
}

// setUpStack reserves the kernel-provided stack: reserve bytes of anonymous
// memory ending at vm.UserStackTop, which grow on demand, above StackGuardSize
// of guard. The top page is supplied now, since every task touches it first.
func setUpStack(allocator *pmm.Allocator, root uint64, regions *region.Map, reserve uint64) (uint64, bool) {
	if reserve == 0 {
		reserve = DefaultStackReserve
	}
	top := vm.UserStackTop
	base := top - reserve
	if regions.Insert(region.Region{Base: base - StackGuardSize, Length: StackGuardSize, Backing: region.Guard, Flags: region.Pinned}) != region.OK ||
		regions.Insert(region.Region{Base: base, Length: reserve, Protection: region.Read | region.Write, Backing: region.Anonymous, Flags: region.Pinned}) != region.OK {
		return 0, false
	}
	physical, ok := allocator.Allocate()
	if !ok {
		return 0, false
	}
	if !vm.InitializePhysical(physical, nil) {
		allocator.Free(physical)
		return 0, false
	}
	if _, mapped := vm.MapUserPageAt(allocator, root, top-vm.PageSize, physical, vm.Permissions{User: true, Writable: true}, true); !mapped {
		allocator.Free(physical)
		return 0, false
	}
	return top, true
}

// protection is the region protection of a segment's flags.
func protection(flags uint32) region.Protection {
	result := region.Read
	if flags&SegmentWritable != 0 {
		result |= region.Write
	}
	if flags&SegmentExecutable != 0 {
		result |= region.Execute
	}
	return result
}

func roundUp(size uint64) uint64 { return (size + vm.PageSize - 1) &^ (vm.PageSize - 1) }

// fillShared copies a read-only segment into frames it records in shared. A
// cache that is full, or an allocation that fails, leaves nothing recorded and
// the segment is loaded privately instead.
func fillShared(allocator *pmm.Allocator, shared *SharedPages, key uint64, index int, segment Segment, source Source, pages int) ([]uint64, bool) {
	frames, ok := shared.Reserve(pages)
	if !ok {
		return nil, false
	}
	for page := 0; page < pages; page++ {
		physical, ok := allocatePage(allocator, segment, uint64(page)*vm.PageSize, source)
		if !ok {
			for rollback := 0; rollback < page; rollback++ {
				allocator.Free(frames[rollback])
			}
			return nil, false
		}
		frames[page] = physical
	}
	if !shared.Commit(key, index, pages) {
		for page := 0; page < pages; page++ {
			allocator.Free(frames[page])
		}
		return nil, false
	}
	return frames, true
}

// allocatePage returns a fresh frame holding the segment's bytes at offset,
// zero past the end of its file contents.
func allocatePage(allocator *pmm.Allocator, segment Segment, offset uint64, source Source) (uint64, bool) {
	fileEnd := offset + vm.PageSize
	if fileEnd > segment.FileSize {
		fileEnd = segment.FileSize
	}
	var payload []byte
	if offset < segment.FileSize {
		// ParseSource checked that the segment's file bytes lie inside the
		// source, so this read fails only on a source that changed size.
		bytes, ok := source.Read(segment.FileOffset+offset, fileEnd-offset, pageScratch[:])
		if !ok {
			return 0, false
		}
		payload = bytes
	}
	physical, allocated := allocator.Allocate()
	if !allocated {
		return 0, false
	}
	if !vm.InitializePhysical(physical, payload) {
		allocator.Free(physical)
		return 0, false
	}
	return physical, true
}
