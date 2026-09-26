//go:build uefi && amd64

package shmem

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/mm/pmm"
	"goos/kernel/mm/vm"
)

// Map derives mapping permission exclusively from the caller's local handle.
// Shared memory is always NX; executable sharing requires a future seal API.
// The page tables the mapping needs are created on demand; how many were
// created is returned, even on failure, so the caller charges them.
func (object *Object) Map(allocator *pmm.Allocator, table *handle.Table, memoryHandle handle.ID, root uint64, virtual uint64, pageOffset uint32, pageCount uint32, writable bool) (int, bool) {
	if object == nil || table == nil || virtual%vm.PageSize != 0 || !validMapping(object.count, pageOffset, pageCount) {
		return 0, false
	}
	required := capability.Read
	if writable {
		required |= capability.Write
	}
	authority, ok := table.Lookup(memoryHandle, handle.TypeMemory, required)
	if !ok || authority.Object() != object.id {
		return 0, false
	}
	for index := uint32(0); index < pageCount; index++ {
		address := virtual + uint64(index)*vm.PageSize
		if address < virtual || !vm.ValidateUserRange(address, vm.PageSize) || vm.UserPageMapped(root, address) {
			return 0, false
		}
	}
	tables := 0
	for index := uint32(0); index < pageCount; index++ {
		permissions := vm.Permissions{User: true, Writable: writable}
		frame, found := object.Frame(vm.PhysicalPage, pageOffset+index)
		if !found {
			return tables, false
		}
		added, mapped := vm.MapUserPageAt(allocator, root, virtual+uint64(index)*vm.PageSize, frame, permissions, false)
		tables += added
		if !mapped {
			return tables, false
		}
	}
	return tables, true
}
