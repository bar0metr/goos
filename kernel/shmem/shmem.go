package shmem

import (
	"goos/internal/capability"
	"goos/kernel/handle"
)

const MaxPages = 16

const (
	identityNamespace = uint64(1 << 63)
	identityLimit     = identityNamespace - 1
)

type Object struct {
	id    uint64
	pages [MaxPages]uint64
	count uint16
}

type Registry struct {
	nextID uint64
}

func (registry *Registry) allocateID() (uint64, bool) {
	if registry == nil || registry.nextID == identityLimit {
		return 0, false
	}
	registry.nextID++
	return identityNamespace | registry.nextID, registry.nextID != 0
}

func (object *Object) ID() uint64 {
	if object == nil {
		return 0
	}
	return object.id
}

func (object *Object) PageCount() uint16 {
	if object == nil {
		return 0
	}
	return object.count
}

// Grant installs authority for this object in one process-local handle table.
// Execute is never a valid shared-memory right.
func (object *Object) Grant(table *handle.Table, rights capability.Rights) (handle.ID, bool) {
	allowed := capability.Read | capability.Write | capability.Transfer
	if object == nil || object.id == 0 || table == nil || rights == 0 || rights&^allowed != 0 {
		return 0, false
	}
	return table.Insert(handle.TypeMemory, capability.New(object.id, rights))
}

func validMapping(objectPages uint16, pageOffset uint16, pageCount uint16) bool {
	end := uint32(pageOffset) + uint32(pageCount)
	return objectPages != 0 && objectPages <= MaxPages && pageCount != 0 && end <= uint32(objectPages)
}
