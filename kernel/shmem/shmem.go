// Package shmem holds shared-memory objects: zeroed physical pages a process
// maps by handle (ADR 0013, ADR 0107). Objects are created by the kernel at
// boot — the file cache, the exec and staging pages — and by processes with
// MemoryCreate. An object lives while anything refers to it: every handle
// naming it and every mapping of it is one reference, and the last reference
// gone frees its pages and returns their charge to the process that paid.
package shmem

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/mm/pagelist"
	"goos/kernel/objects"
)

const (
	// MaxPages bounds one object: what one page list indexes, 256 MiB. The
	// page quota of the creator binds first.
	MaxPages = pagelist.MaxFrames
	// MaxObjects bounds the objects in existence at once, an intentional
	// system limit. The table holds a pointer per slot; an object's record,
	// about 1 KiB with its page-list root, takes a page when the object is
	// created and returns it when the object is freed (ADR 0111).
	MaxObjects = 256
	// MaxObjectsPerProcess bounds the objects one process pays for, so one
	// process cannot take the whole table from the others.
	MaxObjectsPerProcess = 16

	identityNamespace = uint64(1 << 63)
	identityLimit     = identityNamespace - 1
)

type Object struct {
	id     uint64
	frames pagelist.List
	count  uint32
	refs   uint32
	// payer is the process charged for the object's pages; zero for the
	// objects the kernel creates at boot, which are never freed.
	payer uint64
}

// Table is every shared-memory object in existence.
type Table struct {
	objects [MaxObjects]*Object
	nextID  uint64
	storage objects.Storage[Object]
}

// SetStorage installs where object records are allocated; see ipc.Registry.
func (table *Table) SetStorage(storage objects.Storage[Object]) { table.storage = storage }

// Zero clears one physical page before it is exposed.
type Zero func(physical uint64) bool

// Create makes an object of pages zeroed pages paid for by payer, zero for a
// permanent kernel object. It starts with no reference; the first grant
// takes one.
func (table *Table) Create(frames pagelist.Frames, view pagelist.View, zero Zero, pages uint32, payer uint64) (*Object, bool) {
	guard.Check()
	if table == nil || frames == nil || pages == 0 || uint64(pages) > MaxPages || table.nextID == identityLimit {
		return nil, false
	}
	if payer != 0 && table.CountFor(payer) >= MaxObjectsPerProcess {
		return nil, false
	}
	slot := -1
	for index := range table.objects[:] {
		if table.objects[index] == nil {
			slot = index
			break
		}
	}
	if slot < 0 {
		return nil, false
	}
	object, allocated := objects.Or(table.storage).New()
	if !allocated {
		return nil, false
	}
	table.objects[slot] = object
	table.nextID++
	object.reset()
	object.id, object.payer = identityNamespace|table.nextID, payer
	for object.count < pages {
		physical, ok := frames.Allocate()
		if ok && !zero(physical) {
			frames.Free(physical)
			ok = false
		}
		if !ok || !object.frames.Append(frames, view, physical) {
			if ok {
				frames.Free(physical)
			}
			object.frames.Release(frames, view)
			table.discard(slot)
			return nil, false
		}
		object.count++
	}
	return object, true
}

// Find returns the object with id.
func (table *Table) Find(id uint64) (*Object, bool) {
	guard.Check()
	if table == nil || id&identityNamespace == 0 || id == identityNamespace {
		return nil, false
	}
	for _, object := range table.objects[:] {
		if object != nil && object.id == id {
			return object, true
		}
	}
	return nil, false
}

// discard frees the record in slot and empties the slot.
func (table *Table) discard(slot int) {
	object := table.objects[slot]
	table.objects[slot] = nil
	object.reset()
	objects.Or(table.storage).Free(object)
}

// CountFor counts the objects payer pays for.
func (table *Table) CountFor(payer uint64) int {
	guard.Check()
	count := 0
	for _, object := range table.objects[:] {
		if object != nil && object.payer == payer {
			count++
		}
	}
	return count
}

// Live counts the objects in existence.
func (table *Table) Live() int {
	guard.Check()
	count := 0
	for _, object := range table.objects[:] {
		if object != nil {
			count++
		}
	}
	return count
}

// Drop removes one reference. When it was the last reference to an object a
// process pays for, the object's pages are freed and Drop returns the payer
// and the pages to uncharge. Kernel objects are never freed.
func (table *Table) Drop(object *Object, frames pagelist.Frames, view pagelist.View) (uint64, uint32, bool) {
	guard.Check()
	if object == nil || object.id == 0 {
		return 0, 0, false
	}
	if object.refs != 0 {
		object.refs--
	}
	if object.refs != 0 || object.payer == 0 {
		return 0, 0, false
	}
	payer, charge := object.payer, object.Charge()
	object.frames.Release(frames, view)
	for slot := range table.objects[:] {
		if table.objects[slot] == object {
			table.discard(slot)
			break
		}
	}
	return payer, charge, true
}

func (object *Object) reset() {
	object.id, object.count, object.refs, object.payer = 0, 0, 0, 0
	object.frames.Reset()
}

func (object *Object) ID() uint64 {
	guard.Check()
	if object == nil {
		return 0
	}
	return object.id
}

// PageCount is the number of pages the object holds.
func (object *Object) PageCount() uint32 {
	guard.Check()
	if object == nil {
		return 0
	}
	return object.count
}

// Charge is every page the object costs: its pages and its index pages.
func (object *Object) Charge() uint32 {
	guard.Check()
	return object.count + uint32(pagelist.IndexPagesFor(uint64(object.count)))
}

// Payer is the process that pays for the object, zero for a kernel object.
func (object *Object) Payer() uint64 { return object.payer }

// Retain adds a reference: a handle placed or a mapping made.
func (object *Object) Retain() { object.refs++ }

// References is the number of handles and mappings that name the object.
func (object *Object) References() uint32 { return object.refs }

// Frame returns the physical page at index.
func (object *Object) Frame(view pagelist.View, index uint32) (uint64, bool) {
	guard.Check()
	return object.frames.At(view, uint64(index))
}

// Grant installs authority for this object in one process-local handle table
// and counts the handle as a reference. Execute is never a valid
// shared-memory right.
func (object *Object) Grant(table *handle.Table, rights capability.Rights) (handle.ID, bool) {
	guard.Check()
	allowed := capability.Read | capability.Write | capability.Transfer
	if object == nil || object.id == 0 || table == nil || rights == 0 || rights&^allowed != 0 {
		return 0, false
	}
	id, ok := table.Insert(handle.TypeMemory, capability.New(object.id, rights))
	if ok {
		object.refs++
	}
	return id, ok
}

// IsObject reports whether id is in the shared-memory namespace.
func IsObject(id uint64) bool { return id&identityNamespace != 0 && id != identityNamespace }

func validMapping(objectPages uint32, pageOffset uint32, pageCount uint32) bool {
	end := uint64(pageOffset) + uint64(pageCount)
	return objectPages != 0 && uint64(objectPages) <= MaxPages && pageCount != 0 && end <= uint64(objectPages)
}
