package handle

import "goos/internal/capability"

const MaxHandles = 128

type ID uint64

type Type uint16

const (
	TypeInvalid Type = iota
	TypeProcess
	TypeTask
	TypeMemory
	TypeChannel
	TypeDevice
	TypeIRQ
	TypeStream
)

type slot struct {
	object     uint64
	rights     capability.Rights
	generation uint32
	typeID     Type
	open       bool
}

type Table struct {
	slots [MaxHandles]slot
}

func (table *Table) Available() int {
	if table == nil {
		return 0
	}
	available := 0
	for index := range table.slots {
		if !table.slots[index].open {
			available++
		}
	}
	return available
}

func (table *Table) Insert(typeID Type, cap capability.Capability) (ID, bool) {
	if typeID == TypeInvalid || cap.Object() == 0 || cap.Rights() == 0 {
		return 0, false
	}
	for index := range table.slots {
		entry := &table.slots[index]
		if entry.open {
			continue
		}
		if entry.generation == 0 {
			entry.generation = 1
		}
		entry.object = cap.Object()
		entry.rights = cap.Rights()
		entry.typeID = typeID
		entry.open = true
		return encode(index, entry.generation), true
	}
	return 0, false
}

func (table *Table) Lookup(id ID, typeID Type, required capability.Rights) (capability.Capability, bool) {
	index, generation, ok := decode(id)
	if !ok {
		return capability.Capability{}, false
	}
	entry := &table.slots[index]
	if !entry.open || entry.generation != generation || entry.typeID != typeID {
		return capability.Capability{}, false
	}
	cap := capability.New(entry.object, entry.rights)
	if !cap.Allows(required) {
		return capability.Capability{}, false
	}
	return cap, true
}

func (table *Table) Close(id ID) bool {
	index, generation, ok := decode(id)
	if !ok {
		return false
	}
	entry := &table.slots[index]
	if !entry.open || entry.generation != generation {
		return false
	}
	entry.open = false
	entry.object = 0
	entry.rights = 0
	entry.typeID = TypeInvalid
	entry.generation++
	if entry.generation == 0 {
		entry.generation = 1
	}
	return true
}

func (table *Table) Describe(id ID) (Type, capability.Capability, bool) {
	index, generation, ok := decode(id)
	if !ok {
		return TypeInvalid, capability.Capability{}, false
	}
	entry := &table.slots[index]
	if !entry.open || entry.generation != generation {
		return TypeInvalid, capability.Capability{}, false
	}
	return entry.typeID, capability.New(entry.object, entry.rights), true
}

func (table *Table) HasObject(typeID Type, object uint64) bool {
	for index := range table.slots {
		entry := &table.slots[index]
		if entry.open && entry.typeID == typeID && entry.object == object {
			return true
		}
	}
	return false
}

// CloseAll invalidates every open handle and reports each capability before it
// is discarded so the owning subsystem can release object-level resources.
func (table *Table) CloseAll(visit func(Type, capability.Capability)) {
	if table == nil {
		return
	}
	for index := range table.slots {
		entry := &table.slots[index]
		if !entry.open {
			continue
		}
		if visit != nil {
			visit(entry.typeID, capability.New(entry.object, entry.rights))
		}
		entry.open = false
		entry.object = 0
		entry.rights = 0
		entry.typeID = TypeInvalid
		entry.generation++
		if entry.generation == 0 {
			entry.generation = 1
		}
	}
}

// Transfer derives a non-escalating capability into another process-local
// table. The source handle stays valid and must carry Transfer authority.
func (table *Table) Transfer(target *Table, id ID, typeID Type, rights capability.Rights) (ID, bool) {
	if target == nil {
		return 0, false
	}
	source, ok := table.Lookup(id, typeID, capability.Transfer)
	if !ok {
		return 0, false
	}
	derived, ok := source.Derive(rights)
	if !ok {
		return 0, false
	}
	return target.Insert(typeID, derived)
}

func encode(index int, generation uint32) ID {
	return ID(uint64(generation)<<32 | uint64(index+1))
}

func decode(id ID) (int, uint32, bool) {
	indexPart := uint32(id)
	generation := uint32(uint64(id) >> 32)
	if indexPart == 0 || indexPart > MaxHandles || generation == 0 {
		return 0, 0, false
	}
	return int(indexPart - 1), generation, true
}
