package handle

import (
	"goos/internal/capability"
	"goos/kernel/lock"
)

// MaxHandles is the per-process handle limit. A table is 24 bytes a slot and
// lives in the process record, whose pages are allocated when a process slot
// is first used (ADR 0111); 256 leaves PID 1, which holds every service's
// endpoints and each job's streams, room past what it holds at peak.
const MaxHandles = 256

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

// Table is one process's handles. Its lock guards it alone (ADR 0187): the
// tables of two processes change on two processors at once. owner is the
// process's ID, which ranks the lock among the tables and names the holder
// of the channel endpoints in the table.
type Table struct {
	lock    lock.RW
	owner   uint64
	guarded bool
	slots   [MaxHandles]slot
}

// SetOwner names the process the table belongs to. A process slot calls it
// when it is prepared, before anyone else can reach the table.
func (table *Table) SetOwner(owner uint64) {
	table.owner = owner
	table.lock.SetLevel(lock.LevelHandle)
	table.lock.SetRank(uint32(owner))
}

// Owner returns the ID of the process the table belongs to.
func (table *Table) Owner() uint64 { return table.owner }

// Lock returns the table's lock.
func (table *Table) Lock() *lock.RW { return &table.lock }

// Guard makes the table check, in validation builds, that its caller holds
// its lock. A process's table is guarded from the moment other processors
// can reach the process.
func (table *Table) Guard() { table.guarded = true }

func (table *Table) reading() {
	if table != nil && table.guarded {
		table.lock.AssertHeld()
	}
}

func (table *Table) writing() {
	if table != nil && table.guarded {
		table.lock.AssertWriter()
	}
}

func (table *Table) Available() int {
	table.reading()
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
	table.writing()
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
		if typeID == TypeChannel {
			noteHolder(entry.object, table.owner, true)
		}
		return encode(index, entry.generation), true
	}
	return 0, false
}

func (table *Table) Lookup(id ID, typeID Type, required capability.Rights) (capability.Capability, bool) {
	table.reading()
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
	table.writing()
	index, generation, ok := decode(id)
	if !ok {
		return false
	}
	entry := &table.slots[index]
	if !entry.open || entry.generation != generation {
		return false
	}
	if entry.typeID == TypeChannel {
		noteHolder(entry.object, table.owner, false)
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
	table.reading()
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

// At describes the open handle stored in slot index, if any.
func (table *Table) At(index int) (ID, Type, capability.Capability, bool) {
	table.reading()
	if index < 0 || index >= MaxHandles || !table.slots[index].open {
		return 0, TypeInvalid, capability.Capability{}, false
	}
	entry := &table.slots[index]
	return encode(index, entry.generation), entry.typeID, capability.New(entry.object, entry.rights), true
}

func (table *Table) HasObject(typeID Type, object uint64) bool {
	table.reading()
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
	table.writing()
	if table == nil {
		return
	}
	for index := range table.slots {
		entry := &table.slots[index]
		if !entry.open {
			continue
		}
		if entry.typeID == TypeChannel {
			noteHolder(entry.object, table.owner, false)
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
	table.writing()
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
