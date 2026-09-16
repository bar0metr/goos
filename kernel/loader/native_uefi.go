//go:build uefi && amd64

package loader

import (
	"goos/kernel/mm/pmm"
	"goos/kernel/mm/vm"
)

type LoadedImage struct {
	Entry    uint64
	StackTop uint64
}

// LoadNative validates, allocates, initializes, and maps a native image into an
// empty process address space. All payload bytes are copied before executable
// permission is exposed.
func LoadNative(allocator *pmm.Allocator, root uint64, data []byte) (LoadedImage, bool) {
	image, ok := ParseNative(data, vm.PageSize, vm.IdentityMapLimit, vm.UserAddressLimit)
	if !ok || root == 0 {
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
	var pages [MaxImagePages]uint64
	pageCount := 0
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		for offset := uint64(0); offset < segment.MemorySize; offset += vm.PageSize {
			physical, allocated := allocator.Allocate()
			if !allocated {
				for rollback := 0; rollback < pageCount; rollback++ {
					allocator.Free(pages[rollback])
				}
				return LoadedImage{}, false
			}
			pages[pageCount] = physical
			fileOffset := offset
			fileEnd := fileOffset + vm.PageSize
			if fileEnd > segment.FileSize {
				fileEnd = segment.FileSize
			}
			var payload []byte
			if fileOffset < segment.FileSize {
				payload = data[segment.FileOffset+fileOffset : segment.FileOffset+fileEnd]
			}
			if !vm.InitializePhysical(physical, payload) {
				for rollback := 0; rollback <= pageCount; rollback++ {
					allocator.Free(pages[rollback])
				}
				return LoadedImage{}, false
			}
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
		for offset := uint64(0); offset < segment.MemorySize; offset += vm.PageSize {
			if !vm.MapOwnedUserPage(root, segment.VirtualAddress+offset, pages[pageIndex], permissions) {
				return LoadedImage{}, false
			}
			pageIndex++
		}
	}
	return LoadedImage{Entry: image.Entry, StackTop: image.StackTop}, true
}
