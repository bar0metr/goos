//go:build uefi && amd64

package loader

import (
	"goos/kernel/mm/pmm"
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

// loadPages records the frames LoadNative allocated, so that a failure can
// hand them back. It is shared because a local array escapes into the rollback
// loops and the kernel's leaking heap never frees it; only one image is loaded
// at a time.
var loadPages [MaxImagePages]uint64

// segmentFrames holds, per segment, the shared frames that back it, or nil for
// a segment given private pages. Shared for the same reason as loadPages.
var segmentFrames [MaxSegments][]uint64

// LoadNative loads a private copy of every segment of an image into an empty
// process address space.
func LoadNative(allocator *pmm.Allocator, root uint64, data []byte) (LoadedImage, bool) {
	return LoadImage(allocator, root, data, nil, 0)
}

// LoadImage validates, allocates, initializes, and maps a native image into an
// empty process address space. All payload bytes are copied before executable
// permission is exposed.
//
// When shared is not nil and key names the image within the immutable system
// image, each read-only segment is mapped from frames in shared, filling them
// on the first load. Those frames are mapped without the owned bit, so the
// process's teardown leaves them for the next process, and never writable, so
// no process can change what another runs. Writable segments are always
// private.
func LoadImage(allocator *pmm.Allocator, root uint64, data []byte, shared *SharedPages, key uint64) (LoadedImage, bool) {
	image, ok := ParseNative(data, vm.PageSize, vm.IdentityMapLimit, vm.UserAddressLimit)
	if !ok || root == 0 {
		return LoadedImage{}, false
	}
	// ParseNative keeps other segments out of the stack's guard page; the
	// kernel's own mappings must stay out of it too (ADR 0083).
	if stack, _ := image.StackSegment(); kernelMappedUserPage(stack.VirtualAddress - vm.PageSize) {
		return LoadedImage{}, false
	}
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		for offset := uint64(0); offset < segment.MemorySize; offset += vm.PageSize {
			if !vm.CanMapUserPage(root, segment.VirtualAddress+offset) {
				return LoadedImage{}, false
			}
		}
	}
	var loaded LoadedImage
	pageCount := 0
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		segmentFrames[index] = nil
		pages := int((segment.MemorySize + vm.PageSize - 1) / vm.PageSize)
		if shared != nil && key != 0 && Shareable(segment) {
			frames, hit := shared.Lookup(key, index, pages)
			if !hit {
				frames, hit = fillShared(allocator, shared, key, index, segment, data)
				if hit {
					loaded.SharedPagesAllocated += uint64(pages)
				}
			}
			if hit {
				segmentFrames[index] = frames
				continue
			}
		}
		for offset := uint64(0); offset < segment.MemorySize; offset += vm.PageSize {
			physical, ok := allocatePage(allocator, segment, offset, data)
			if !ok {
				for rollback := 0; rollback < pageCount; rollback++ {
					allocator.Free(loadPages[rollback])
				}
				return LoadedImage{}, false
			}
			loadPages[pageCount] = physical
			pageCount++
		}
	}
	pageIndex := 0
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		permissions := vm.Permissions{
			User:       true,
			Writable:   segment.Flags&SegmentWritable != 0,
			Executable: segment.Flags&SegmentExecutable != 0,
		}
		for page, offset := 0, uint64(0); offset < segment.MemorySize; page, offset = page+1, offset+vm.PageSize {
			virtual := segment.VirtualAddress + offset
			if frames := segmentFrames[index]; frames != nil {
				if !vm.MapUserPage(root, virtual, frames[page], permissions) {
					return LoadedImage{}, false
				}
				continue
			}
			if !vm.MapOwnedUserPage(root, virtual, loadPages[pageIndex], permissions) {
				return LoadedImage{}, false
			}
			pageIndex++
		}
	}
	loaded.Entry, loaded.StackTop = image.Entry, image.StackTop
	return loaded, true
}

// fillShared copies a read-only segment into frames it records in shared. A
// cache that is full, or an allocation that fails, leaves nothing recorded and
// the segment is loaded privately instead.
func fillShared(allocator *pmm.Allocator, shared *SharedPages, key uint64, index int, segment Segment, data []byte) ([]uint64, bool) {
	pages := int((segment.MemorySize + vm.PageSize - 1) / vm.PageSize)
	frames, ok := shared.Reserve(pages)
	if !ok {
		return nil, false
	}
	for page := 0; page < pages; page++ {
		physical, ok := allocatePage(allocator, segment, uint64(page)*vm.PageSize, data)
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
func allocatePage(allocator *pmm.Allocator, segment Segment, offset uint64, data []byte) (uint64, bool) {
	physical, allocated := allocator.Allocate()
	if !allocated {
		return 0, false
	}
	fileEnd := offset + vm.PageSize
	if fileEnd > segment.FileSize {
		fileEnd = segment.FileSize
	}
	var payload []byte
	if offset < segment.FileSize {
		payload = data[segment.FileOffset+offset : segment.FileOffset+fileEnd]
	}
	if !vm.InitializePhysical(physical, payload) {
		allocator.Free(physical)
		return 0, false
	}
	return physical, true
}

// kernelMappedUserPage reports whether the kernel may map address into a
// process after its image is loaded: the probe and startup pages below the
// image, and the shared-memory, DMA and system image windows.
func kernelMappedUserPage(address uint64) bool {
	return address >= vm.UserProbeAddress && address < vm.UserCodeAddress ||
		address >= vm.UserSharedAddress && address < vm.UserImageLimit
}
