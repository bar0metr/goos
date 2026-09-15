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
		if !vm.CanMapUserPage(root, image.Segments[index].VirtualAddress) {
			return LoadedImage{}, false
		}
	}
	var pages [MaxSegments]uint64
	for index := 0; index < image.SegmentCount; index++ {
		physical, allocated := allocator.Allocate()
		if !allocated {
			for rollback := 0; rollback < index; rollback++ {
				allocator.Free(pages[rollback])
			}
			return LoadedImage{}, false
		}
		pages[index] = physical
		segment := image.Segments[index]
		payload := data[segment.FileOffset : segment.FileOffset+segment.FileSize]
		if !vm.InitializePhysical(physical, payload) {
			for rollback := 0; rollback <= index; rollback++ {
				allocator.Free(pages[rollback])
			}
			return LoadedImage{}, false
		}
	}
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		permissions := vm.Permissions{
			User:       true,
			Writable:   segment.Flags&SegmentWritable != 0,
			Executable: segment.Flags&SegmentExecutable != 0,
		}
		if !vm.MapUserPage(root, segment.VirtualAddress, pages[index], permissions) {
			return LoadedImage{}, false
		}
	}
	return LoadedImage{Entry: image.Entry, StackTop: image.StackTop}, true
}
