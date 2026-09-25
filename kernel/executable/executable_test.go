package executable

import (
	"goos/kernel/mm/vm"
	"testing"
)

// filled reserves an object of size bytes and gives it every frame it needs.
func filled(t *testing.T, table *Table, size uint64) *Object {
	t.Helper()
	object, ok := table.Reserve(size)
	if !ok {
		t.Fatalf("reserve %d refused", size)
	}
	for page := 0; page < object.Pages(); page++ {
		if !object.AddFrame(uint64(page+1) * vm.PageSize) {
			t.Fatalf("frame %d refused", page)
		}
	}
	return object
}

func TestObjectSealsWhenItsLastByteIsWritten(t *testing.T) {
	var table Table
	object := filled(t, &table, vm.PageSize+10)
	if object.Pages() != 2 || !object.Complete() || object.Sealed() {
		t.Fatalf("pages %d complete %v sealed %v", object.Pages(), object.Complete(), object.Sealed())
	}
	for _, length := range []uint64{MaxWrite, 10} {
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
	object := filled(t, &table, 100)
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
	object, _ := table.Reserve(2 * vm.PageSize)
	object.AddFrame(vm.PageSize)
	if _, ok := object.NextWrite(1); ok || object.Complete() {
		t.Fatal("object missing a frame accepted a write")
	}
	for _, frame := range []uint64{0, vm.PageSize + 1} {
		if object.AddFrame(frame) {
			t.Fatalf("frame %#x accepted", frame)
		}
	}
	object.AddFrame(2 * vm.PageSize)
	if object.AddFrame(3*vm.PageSize) || len(object.Frames()) != 2 {
		t.Fatal("frame beyond the object's size accepted")
	}
}

func TestTableBoundsObjectsAndSizes(t *testing.T) {
	var table Table
	for _, size := range []uint64{0, MaxSize + 1} {
		if _, ok := table.Reserve(size); ok {
			t.Fatalf("size %d accepted", size)
		}
	}
	first, _ := table.Reserve(MaxSize)
	second, _ := table.Reserve(1)
	if _, ok := table.Reserve(1); ok {
		t.Fatal("more objects than MaxObjects")
	}
	if first.ID() == second.ID() || !IsObject(first.ID()) || !IsObject(second.ID()) {
		t.Fatalf("ids %#x %#x", first.ID(), second.ID())
	}
	id := first.ID()
	if !table.Remove(id) || table.Remove(id) {
		t.Fatal("remove failed, or removed twice")
	}
	if _, ok := table.Find(id); ok {
		t.Fatal("removed object still found")
	}
	third, ok := table.Reserve(1)
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
