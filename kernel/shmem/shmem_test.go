package shmem

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/objects"
	"testing"
)

// memory is a fake frame allocator whose pages are Go slices.
type memory struct {
	pages map[uint64][]byte
	next  uint64
}

func newMemory() *memory { return &memory{pages: map[uint64][]byte{}, next: 4096} }

func (m *memory) Allocate() (uint64, bool) {
	address := m.next
	m.next += 4096
	m.pages[address] = make([]byte, 4096)
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

func zeroed(uint64) bool { return true }

func TestMappingBounds(t *testing.T) {
	tests := []struct {
		offset uint32
		count  uint32
		valid  bool
	}{
		{offset: 0, count: 1, valid: true},
		{offset: 1, count: 1, valid: true},
		{offset: 0, count: 2, valid: true},
		{offset: 0, count: 0},
		{offset: 2, count: 1},
		{offset: 1, count: 2},
		{offset: ^uint32(0), count: 2},
	}
	for _, test := range tests {
		if got := validMapping(2, test.offset, test.count); got != test.valid {
			t.Errorf("validMapping(2, %d, %d) = %v", test.offset, test.count, got)
		}
	}
}

func TestCreateGrantAndFree(t *testing.T) {
	var table Table
	m := newMemory()
	object, ok := table.Create(m, m.view, zeroed, 600, 7)
	if !ok || object.PageCount() != 600 || object.Charge() != 602 || !IsObject(object.ID()) || object.Payer() != 7 {
		t.Fatalf("create: %v %+v", ok, object)
	}
	if found, ok := table.Find(object.ID()); !ok || found != object {
		t.Fatal("not found")
	}
	var handles handle.Table
	if _, ok := object.Grant(&handles, capability.Execute); ok {
		t.Fatal("executable shared-memory grant accepted")
	}
	if _, ok := object.Grant(&handles, capability.Read|capability.Write); !ok || object.References() != 1 {
		t.Fatal("grant")
	}
	object.Retain()
	if _, _, freed := table.Drop(object, m, m.view); freed {
		t.Fatal("freed with a reference left")
	}
	id := object.ID()
	payer, charge, freed := table.Drop(object, m, m.view)
	if !freed || payer != 7 || charge != 602 || len(m.pages) != 0 {
		t.Fatalf("drop: %d %d %v, %d pages left", payer, charge, freed, len(m.pages))
	}
	if _, ok := table.Find(id); ok {
		t.Fatal("freed object still found")
	}
}

func TestKernelObjectsAreNeverFreed(t *testing.T) {
	var table Table
	m := newMemory()
	object, _ := table.Create(m, m.view, zeroed, 1, 0)
	var handles handle.Table
	object.Grant(&handles, capability.Read)
	if _, _, freed := table.Drop(object, m, m.view); freed || len(m.pages) != 2 {
		t.Fatal("a kernel object was freed")
	}
}

func TestTableAndProcessBounds(t *testing.T) {
	var table Table
	m := newMemory()
	if _, ok := table.Create(m, m.view, zeroed, 0, 1); ok {
		t.Fatal("empty object created")
	}
	if _, ok := table.Create(m, m.view, zeroed, uint32(MaxPages+1), 1); ok {
		t.Fatal("object past MaxPages created")
	}
	for index := 0; index < MaxObjectsPerProcess; index++ {
		if _, ok := table.Create(m, m.view, zeroed, 1, 5); !ok {
			t.Fatalf("object %d refused", index)
		}
	}
	if _, ok := table.Create(m, m.view, zeroed, 1, 5); ok {
		t.Fatal("more objects than one process may pay for")
	}
	for payer := uint64(6); table.Live() < MaxObjects; payer++ {
		if _, ok := table.Create(m, m.view, zeroed, 1, payer); !ok {
			t.Fatal("refused below the table bound")
		}
	}
	if _, ok := table.Create(m, m.view, zeroed, 1, 99); ok {
		t.Fatal("table overflow accepted")
	}
}

func TestZeroFailureLeavesNothing(t *testing.T) {
	var table Table
	m := newMemory()
	calls := 0
	failing := func(uint64) bool { calls++; return calls < 3 }
	if _, ok := table.Create(m, m.view, failing, 5, 1); ok || len(m.pages) != 0 || table.Live() != 0 {
		t.Fatalf("a failed create left %d pages, %d objects", len(m.pages), table.Live())
	}
}

func TestNamespace(t *testing.T) {
	for _, id := range []uint64{0, 1, 1 << 62, identityNamespace} {
		if IsObject(id) {
			t.Fatalf("%#x is a shared-memory id", id)
		}
	}
}

var sharedBenchmarkSink byte

// BenchmarkSharedPagePayloadAccess measures reading a payload through two
// views of the same page, the cost a zero-copy transfer pays instead of a copy.
func BenchmarkSharedPagePayloadAccess(b *testing.B) {
	page := make([]byte, 4096)
	writer, reader := page, page
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		writer[index&4095] = byte(index)
		sharedBenchmarkSink += reader[(index+1)&4095]
	}
}

// TestObjectRecordsComeAndGo covers ADR 0111: an object's record is taken
// from the storage when the object is created and returned when its last
// reference goes, and a create without memory for its record is refused.
func TestObjectRecordsComeAndGo(t *testing.T) {
	m := newMemory()
	storage := &objects.Limited[Object]{}
	var table Table
	table.SetStorage(storage)
	created := make([]*Object, 0, MaxObjectsPerProcess)
	for index := 0; index < MaxObjectsPerProcess; index++ {
		object, ok := table.Create(m, m.view, zeroed, 1, 7)
		if !ok {
			t.Fatalf("create %d refused", index)
		}
		object.Retain()
		created = append(created, object)
	}
	if storage.Live != MaxObjectsPerProcess || table.Live() != MaxObjectsPerProcess {
		t.Fatalf("%d records for %d objects", storage.Live, table.Live())
	}
	for _, object := range created {
		id := object.ID()
		if _, _, freed := table.Drop(object, m, m.view); !freed {
			t.Fatal("last reference did not free the object")
		}
		if _, found := table.Find(id); found {
			t.Fatal("a freed object is still found")
		}
	}
	if storage.Live != 0 || table.Live() != 0 || len(m.pages) != 0 {
		t.Fatalf("%d records, %d objects, %d pages left", storage.Live, table.Live(), len(m.pages))
	}
	storage.Limit = 1
	if _, ok := table.Create(m, m.view, zeroed, 1, 8); !ok {
		t.Fatal("create refused")
	}
	if _, ok := table.Create(m, m.view, zeroed, 1, 8); ok {
		t.Fatal("create succeeded without memory for its record")
	}
}
