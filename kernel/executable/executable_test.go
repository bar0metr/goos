package executable

import (
	"goos/kernel/mm/vm"
	"testing"
)

// memory is a fake frame allocator whose pages are Go slices.
type memory struct {
	pages map[uint64][]byte
	next  uint64
}

func newMemory() *memory { return &memory{pages: map[uint64][]byte{}, next: vm.PageSize} }

func (m *memory) Allocate() (uint64, bool) {
	address := m.next
	m.next += vm.PageSize
	m.pages[address] = make([]byte, vm.PageSize)
	return address, true
}

func (m *memory) Free(physical uint64) bool {
	_, ok := m.pages[physical]
	delete(m.pages, physical)
	return ok
}

func (m *memory) view(physical uint64) ([]byte, bool) {
	page, ok := m.pages[physical]
	return page, ok
}

// filled reserves an object of size bytes and gives it every frame it needs.
func filled(t *testing.T, table *Table, m *memory, size uint64) *Object {
	t.Helper()
	object, ok := table.Reserve(size, 1)
	if !ok {
		t.Fatalf("reserve %d refused", size)
	}
	for page := 0; page < object.Pages(); page++ {
		frame, _ := m.Allocate()
		if !object.AddFrame(m, m.view, frame) {
			t.Fatalf("frame %d refused", page)
		}
	}
	return object
}

func TestObjectSealsWhenItsLastByteIsWritten(t *testing.T) {
	var table Table
	m := newMemory()
	object := filled(t, &table, m, vm.PageSize+10)
	if object.Pages() != 2 || !object.Complete() || object.Sealed() || object.Charge() != 3 {
		t.Fatalf("pages %d complete %v sealed %v charge %d", object.Pages(), object.Complete(), object.Sealed(), object.Charge())
	}
	for _, length := range []uint64{vm.PageSize, 10} {
		offset, ok := object.NextWrite(length)
		if !ok || object.Sealed() {
			t.Fatalf("write of %d at %d refused or sealed early", length, offset)
		}
		object.Advance(length)
	}
	if !object.Sealed() {
		t.Fatal("full object not sealed")
	}
	if _, ok := object.NextWrite(1); ok {
		t.Fatal("sealed object accepted a write")
	}
}

func TestWritesAreBoundedAndAppendOnly(t *testing.T) {
	var table Table
	object := filled(t, &table, newMemory(), 100)
	for _, length := range []uint64{0, 101, MaxWrite + 1} {
		if _, ok := object.NextWrite(length); ok {
			t.Fatalf("write of %d accepted", length)
		}
	}
	offset, _ := object.NextWrite(60)
	object.Advance(60)
	if next, ok := object.NextWrite(40); !ok || offset != 0 || next != 60 {
		t.Fatalf("offsets %d then %d", offset, next)
	}
	if _, ok := object.NextWrite(41); ok {
		t.Fatal("write past the end accepted")
	}
}

func TestIncompleteObjectAcceptsNoWrites(t *testing.T) {
	var table Table
	m := newMemory()
	object, _ := table.Reserve(2*vm.PageSize, 1)
	first, _ := m.Allocate()
	object.AddFrame(m, m.view, first)
	if _, ok := object.NextWrite(1); ok || object.Complete() {
		t.Fatal("object missing a frame accepted a write")
	}
	for _, frame := range []uint64{0, vm.PageSize + 1} {
		if object.AddFrame(m, m.view, frame) {
			t.Fatalf("frame %#x accepted", frame)
		}
	}
	second, _ := m.Allocate()
	third, _ := m.Allocate()
	object.AddFrame(m, m.view, second)
	if object.AddFrame(m, m.view, third) {
		t.Fatal("frame beyond the object's size accepted")
	}
	if frame, ok := object.Frame(m.view, 1); !ok || frame != second {
		t.Fatalf("frame 1 = %#x", frame)
	}
}

// TestLargeObjectsAreIndexedNotArrayed builds an object past the old 1 MiB
// bound, reads its frames back, and returns them all.
func TestLargeObjectsAreIndexedNotArrayed(t *testing.T) {
	var table Table
	m := newMemory()
	object := filled(t, &table, m, 5<<20+1)
	if object.Pages() != 1281 || object.Charge() != 1281+3 {
		t.Fatalf("pages %d charge %d", object.Pages(), object.Charge())
	}
	for _, index := range []uint64{0, 511, 512, 1280} {
		if frame, ok := object.Frame(m.view, index); !ok || frame == 0 {
			t.Fatalf("frame %d missing", index)
		}
	}
	if !object.ReleaseFrames(m, m.view) || len(m.pages) != 0 {
		t.Fatalf("%d pages left", len(m.pages))
	}
}

func TestReferencesAndPayer(t *testing.T) {
	var table Table
	object, _ := table.Reserve(1, 7)
	if object.Payer() != 7 || object.Drop() != true {
		t.Fatal("a new object has one reference, its creator's")
	}
	object.Retain()
	object.Retain()
	if object.Drop() || !object.Drop() {
		t.Fatal("references miscounted")
	}
	object.SetPayer(9)
	object.MarkLaunched()
	if object.Payer() != 9 || !object.Launched() {
		t.Fatal("payer or launch not recorded")
	}
}

func TestTableBoundsObjectsAndSizes(t *testing.T) {
	var table Table
	for _, size := range []uint64{0, MaxSize + 1} {
		if _, ok := table.Reserve(size, 1); ok {
			t.Fatalf("size %d accepted", size)
		}
	}
	first, _ := table.Reserve(MaxSize, 1)
	for index := 1; index < MaxObjects; index++ {
		if _, ok := table.Reserve(1, 1); !ok {
			t.Fatalf("object %d refused", index)
		}
	}
	if _, ok := table.Reserve(1, 1); ok || table.Live() != MaxObjects {
		t.Fatal("more objects than MaxObjects")
	}
	id := first.ID()
	if !table.Remove(id) || table.Remove(id) {
		t.Fatal("remove failed, or removed twice")
	}
	if _, ok := table.Find(id); ok {
		t.Fatal("removed object still found")
	}
	third, ok := table.Reserve(1, 1)
	if !ok || third.ID() == id {
		t.Fatal("slot not reused, or an id reused")
	}
}

func TestNamespaceIsDisjointFromSharedMemory(t *testing.T) {
	for _, id := range []uint64{0, 1, 1 << 63, 1<<63 | 1, namespace} {
		if IsObject(id) {
			t.Fatalf("%#x taken for an executable object", id)
		}
	}
	var table Table
	if _, ok := table.Find(1<<63 | 1); ok {
		t.Fatal("shared-memory id found")
	}
}
