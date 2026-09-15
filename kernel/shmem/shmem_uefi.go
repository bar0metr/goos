//go:build uefi && amd64

package shmem

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/mm/pmm"
	"goos/kernel/mm/vm"
)

func (registry *Registry) Create(allocator *pmm.Allocator, pageCount uint16) (Object, bool) {
	var object Object
	if allocator == nil || pageCount == 0 || pageCount > MaxPages {
		return object, false
	}
	id, validID := registry.allocateID()
	if !validID {
		return object, false
	}
	object.id = id
	for index := uint16(0); index < pageCount; index++ {
		physical, ok := allocator.Allocate()
		if !ok {
			for rollback := uint16(0); rollback < index; rollback++ {
				allocator.Free(object.pages[rollback])
			}
			return Object{}, false
		}
		if !vm.InitializePhysical(physical, nil) {
			allocator.Free(physical)
			for rollback := uint16(0); rollback < index; rollback++ {
				allocator.Free(object.pages[rollback])
			}
			return Object{}, false
		}
		object.pages[index] = physical
		object.count++
	}
	return object, true
}

// Map derives mapping permission exclusively from the caller's local handle.
// Shared memory is always NX; executable sharing requires a future seal API.
func (object *Object) Map(table *handle.Table, memoryHandle handle.ID, root uint64, virtual uint64, pageOffset uint16, pageCount uint16, writable bool) bool {
	if object == nil || table == nil || virtual%vm.PageSize != 0 || !validMapping(object.count, pageOffset, pageCount) {
		return false
	}
	required := capability.Read
	if writable {
		required |= capability.Write
	}
	authority, ok := table.Lookup(memoryHandle, handle.TypeMemory, required)
	if !ok || authority.Object() != object.id {
		return false
	}
	for index := uint16(0); index < pageCount; index++ {
		address := virtual + uint64(index)*vm.PageSize
		if address < virtual || !vm.CanMapUserPage(root, address) {
			return false
		}
	}
	for index := uint16(0); index < pageCount; index++ {
		permissions := vm.Permissions{User: true, Writable: writable}
		if !vm.MapUserPage(root, virtual+uint64(index)*vm.PageSize, object.pages[pageOffset+index], permissions) {
			return false
		}
	}
	return true
}
