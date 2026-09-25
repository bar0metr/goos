// Package executable holds executable objects: kernel memory a launcher fills
// with the bytes of one program. An object seals itself when its last byte is
// written; from then on it can be launched and never changed (ADR 0097).
//
// The object is never mapped writable into any process. Its bytes enter only
// through Write and leave only through the loader, which copies a page into a
// process when the process first touches it (ADR 0106), so a sealed object is
// immutable and the bytes the loader validates are the bytes it loads.
//
// Its frames are kept in a page list, so an object is bounded by the page
// quota of whoever pays for it rather than by a fixed array. It is reference
// counted: the launcher's handle holds one reference and every process started
// from it holds another, because the process reads its pages on demand.
package executable

import (
	"goos/kernel/kcall"
	"goos/kernel/mm/pagelist"
	"goos/kernel/mm/vm"
)

const (
	// MaxSize is the largest program an object can hold, the most one page
	// list indexes. In practice the page quota of the launcher binds first.
	MaxSize = uint64(pagelist.MaxFrames) * vm.PageSize
	// MaxObjects bounds the objects in existence at once: those being filled
	// and those still read by a running process. It is an intentional system
	// limit; each object costs a fixed root of page-list entries.
	MaxObjects = 16
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
	frames pagelist.List
	refs   uint32
	// payer is the process the object's pages are charged to: the launcher
	// that created it, until the first process started from it takes the
	// charge over.
	payer    uint64
	launched bool
}

func (object *Object) ID() uint64 { return object.id }

func (object *Object) Size() uint64 { return object.size }

// Sealed reports whether every byte has been written.
func (object *Object) Sealed() bool { return object.id != 0 && object.filled == object.size }

// Pages is the number of data pages the object needs for its size.
func (object *Object) Pages() int { return int((object.size + vm.PageSize - 1) / vm.PageSize) }

// Charge is every page the object costs: its data pages and the index pages
// that list them.
func (object *Object) Charge() uint32 {
	return uint32(uint64(object.Pages()) + pagelist.IndexPagesFor(uint64(object.Pages())))
}

// Payer is the process charged for the object's pages.
func (object *Object) Payer() uint64 { return object.payer }

// SetPayer moves the charge to another process.
func (object *Object) SetPayer(payer uint64) { object.payer = payer }

// Launched reports whether a process has been started from the object; the
// first launch takes the charge over.
func (object *Object) Launched() bool { return object.launched }

// MarkLaunched records the first launch.
func (object *Object) MarkLaunched() { object.launched = true }

// AddFrame appends the next physical page. The caller has zeroed it.
func (object *Object) AddFrame(frames pagelist.Frames, view pagelist.View, physical uint64) bool {
	if object.id == 0 || object.frames.Len() == uint64(object.Pages()) {
		return false
	}
	return object.frames.Append(frames, view, physical)
}

// Frame returns the physical page holding the object's page number index.
func (object *Object) Frame(view pagelist.View, index uint64) (uint64, bool) {
	return object.frames.At(view, index)
}

// ReleaseFrames gives every page back.
func (object *Object) ReleaseFrames(frames pagelist.Frames, view pagelist.View) bool {
	return object.frames.Release(frames, view)
}

// Complete reports whether every page the object needs has been added.
func (object *Object) Complete() bool {
	return object.id != 0 && object.frames.Len() == uint64(object.Pages())
}

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

// Retain adds a reference.
func (object *Object) Retain() { object.refs++ }

// Drop removes a reference and reports whether it was the last.
func (object *Object) Drop() bool {
	if object.refs != 0 {
		object.refs--
	}
	return object.refs == 0
}

// Table is every executable object in existence.
type Table struct {
	objects [MaxObjects]Object
	nextID  uint64
}

// Reserve claims a slot for an object of size bytes, paid for by payer, with
// the one reference of the handle its creator will hold. The caller adds its
// frames, or removes it again if it cannot.
func (table *Table) Reserve(size uint64, payer uint64) (*Object, bool) {
	if table == nil || size == 0 || size > MaxSize || table.nextID == idMask {
		return nil, false
	}
	for index := range table.objects {
		if object := &table.objects[index]; object.id == 0 {
			table.nextID++
			object.reset()
			object.id, object.size, object.refs, object.payer = namespace|table.nextID, size, 1, payer
			return object, true
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
		object.reset()
	}
	return found
}

// reset empties a slot in place. Assigning an Object literal would build the
// whole page-list root as a temporary, which the kernel's heap never frees.
func (object *Object) reset() {
	object.id, object.size, object.filled, object.refs, object.payer, object.launched = 0, 0, 0, 0, 0, false
	object.frames.Reset()
}

// Live counts the objects in existence.
func (table *Table) Live() int {
	count := 0
	for index := range table.objects {
		if table.objects[index].id != 0 {
			count++
		}
	}
	return count
}
