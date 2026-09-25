// Package executable holds executable objects: kernel memory a launcher fills
// with the bytes of one program. An object seals itself when its last byte is
// written; from then on it can be launched and never changed (ADR 0097).
//
// The object is never mapped into any process. Its bytes enter only through
// Write and leave only through the loader, so a sealed object is immutable
// without tracking mappings, and the bytes the loader validates are the bytes
// it loads.
package executable

import (
	"goos/kernel/kcall"
	"goos/kernel/loader"
	"goos/kernel/mm/vm"
)

const (
	// MaxPages bounds one object. A native image maps at most
	// loader.MaxImagePages pages, and its file is never larger than that.
	MaxPages = loader.MaxImagePages
	// MaxSize is the largest program an object can hold.
	MaxSize = uint64(MaxPages) * vm.PageSize
	// MaxObjects bounds the objects in existence at once. PID 1 launches one
	// program at a time and closes its object when the launch is answered.
	MaxObjects = 2
	// MaxWrite bounds the bytes one Write copies.
	MaxWrite = kcall.MaxExecutableWrite

	// Object IDs have bit 62 set and bit 63 clear, so they never collide with
	// shared-memory objects, which set bit 63.
	namespace = uint64(1) << 62
	idMask    = namespace - 1
)

// IsObject reports whether id is in the executable-object namespace.
func IsObject(id uint64) bool { return id>>62 == 1 && id&idMask != 0 }

// Object is one program's bytes in whole pages. Bytes past Size in the last
// page are zero and never read.
type Object struct {
	id     uint64
	size   uint64
	filled uint64
	pages  [MaxPages]uint64
	count  int
}

func (object *Object) ID() uint64 { return object.id }

func (object *Object) Size() uint64 { return object.size }

// Sealed reports whether every byte has been written.
func (object *Object) Sealed() bool { return object.id != 0 && object.filled == object.size }

// Pages is the number of pages the object needs for its size.
func (object *Object) Pages() int { return int((object.size + vm.PageSize - 1) / vm.PageSize) }

// Frames returns the physical pages added so far, in order.
func (object *Object) Frames() []uint64 { return object.pages[:object.count] }

// AddFrame appends the next physical page. The caller has zeroed it.
func (object *Object) AddFrame(physical uint64) bool {
	if object.id == 0 || object.count == object.Pages() || physical == 0 || physical%vm.PageSize != 0 {
		return false
	}
	object.pages[object.count] = physical
	object.count++
	return true
}

// Complete reports whether every page the object needs has been added.
func (object *Object) Complete() bool { return object.id != 0 && object.count == object.Pages() }

// NextWrite returns the object offset the next length bytes go to. Writes
// append: a caller cannot go back and change bytes it wrote, and an object
// cannot be written past its size or once sealed.
func (object *Object) NextWrite(length uint64) (uint64, bool) {
	if !object.Complete() || length == 0 || length > MaxWrite || length > object.size-object.filled {
		return 0, false
	}
	return object.filled, true
}

// Advance records length bytes written at the offset NextWrite returned.
func (object *Object) Advance(length uint64) {
	object.filled += length
}

// Table is every executable object in existence.
type Table struct {
	objects [MaxObjects]Object
	nextID  uint64
}

// Reserve claims a slot for an object of size bytes. The caller adds its
// frames, or removes it again if it cannot.
func (table *Table) Reserve(size uint64) (*Object, bool) {
	if table == nil || size == 0 || size > MaxSize || table.nextID == idMask {
		return nil, false
	}
	for index := range table.objects {
		if table.objects[index].id == 0 {
			table.nextID++
			table.objects[index] = Object{id: namespace | table.nextID, size: size}
			return &table.objects[index], true
		}
	}
	return nil, false
}

// Find returns the object with id.
func (table *Table) Find(id uint64) (*Object, bool) {
	if table == nil || !IsObject(id) {
		return nil, false
	}
	for index := range table.objects {
		if table.objects[index].id == id {
			return &table.objects[index], true
		}
	}
	return nil, false
}

// Remove forgets the object with id. The caller frees its frames first.
func (table *Table) Remove(id uint64) bool {
	object, found := table.Find(id)
	if found {
		*object = Object{}
	}
	return found
}
