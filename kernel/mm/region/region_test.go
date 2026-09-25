package region

import "testing"

const (
	lower = uint64(1 << 30)
	upper = uint64(0x00007ffffffff000)
)

func newMap(t *testing.T) *Map {
	t.Helper()
	var m Map
	if !m.Init(lower, upper) {
		t.Fatal("init")
	}
	return &m
}

func anonymous(base, pages uint64, protection Protection) Region {
	return Region{Base: base, Length: pages * PageSize, Protection: protection, Backing: Anonymous}
}

func expect(t *testing.T, m *Map, want ...Region) {
	t.Helper()
	if m.Count() != len(want) {
		t.Fatalf("%d regions, want %d: %+v", m.Count(), len(want), m.regions[:m.count])
	}
	for index, region := range want {
		got, _ := m.At(index)
		if got.Backing == Anonymous {
			// An anonymous region's offset names nothing; splits advance it.
			got.Offset = region.Offset
		}
		if got != region {
			t.Fatalf("region %d = %+v, want %+v", index, got, region)
		}
	}
}

func TestInsertValidates(t *testing.T) {
	m := newMap(t)
	cases := []struct {
		name   string
		region Region
		want   Status
	}{
		{"unaligned base", anonymous(lower+1, 1, Read), InvalidArgument},
		{"empty", Region{Base: lower, Protection: Read, Backing: Anonymous}, InvalidArgument},
		{"unaligned length", Region{Base: lower, Length: 100, Protection: Read, Backing: Anonymous}, InvalidArgument},
		{"below the user range", anonymous(lower-PageSize, 1, Read), InvalidArgument},
		{"past the user range", anonymous(upper-PageSize, 2, Read), InvalidArgument},
		{"wrapping", Region{Base: upper - PageSize, Length: ^uint64(0) &^ (PageSize - 1), Protection: Read, Backing: Anonymous}, InvalidArgument},
		{"write and execute", anonymous(lower, 1, Read|Write|Execute), InvalidArgument},
		{"write without read", anonymous(lower, 1, Write), InvalidArgument},
		{"no backing", Region{Base: lower, Length: PageSize, Protection: Read}, InvalidArgument},
		{"accessible guard", Region{Base: lower, Length: PageSize, Protection: Read, Backing: Guard}, InvalidArgument},
		{"kernel address", anonymous(0xffff800000000000, 1, Read), InvalidArgument},
	}
	for _, c := range cases {
		if got := m.Insert(c.region); got != c.want {
			t.Fatalf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if m.Count() != 0 {
		t.Fatal("a refused region was kept")
	}
}

func TestInsertKeepsOrderAndRefusesOverlap(t *testing.T) {
	m := newMap(t)
	b := anonymous(lower+10*PageSize, 2, Read|Write)
	a := anonymous(lower, 4, Read)
	c := anonymous(lower+20*PageSize, 1, Read)
	for _, region := range []Region{b, c, a} {
		if status := m.Insert(region); status != OK {
			t.Fatal(status)
		}
	}
	expect(t, m, a, b, c)
	for _, region := range []Region{
		anonymous(lower+3*PageSize, 2, Read),
		anonymous(lower+11*PageSize, 1, Read),
		anonymous(lower+9*PageSize, 2, Read),
		anonymous(lower, 100, Read),
	} {
		if status := m.Insert(region); status != Overlap {
			t.Fatalf("overlap %+v: %v", region, status)
		}
	}
	if status := m.Insert(anonymous(lower+4*PageSize, 6, Read)); status != OK {
		t.Fatalf("exact fit between regions: %v", status)
	}
	if found, ok := m.Find(lower + 12*PageSize - 1); !ok || found != b {
		t.Fatalf("find inside b: %+v %v", found, ok)
	}
	if _, ok := m.Find(lower + 12*PageSize); ok {
		t.Fatal("found a region in a hole")
	}
}

func TestTableIsBounded(t *testing.T) {
	m := newMap(t)
	for index := uint64(0); index < MaxRegions; index++ {
		if status := m.Insert(anonymous(lower+index*2*PageSize, 1, Read)); status != OK {
			t.Fatal(status)
		}
	}
	if status := m.Insert(anonymous(upper-PageSize, 1, Read)); status != Exhausted {
		t.Fatalf("insert past the bound: %v", status)
	}
	// Splitting needs a slot too, and a refused split changes nothing.
	if status := m.Remove(lower, PageSize, false); status != OK {
		t.Fatal(status)
	}
	whole := anonymous(upper-4*PageSize, 3, Read)
	m.Insert(whole)
	if status := m.Remove(upper-3*PageSize, PageSize, false); status != Exhausted {
		t.Fatalf("split with a full table: %v", status)
	}
	if found, _ := m.Find(upper - 3*PageSize); found != whole {
		t.Fatal("a refused split changed the region")
	}
}

func TestFindFree(t *testing.T) {
	m := newMap(t)
	m.Insert(anonymous(lower, 2, Read))
	m.Insert(anonymous(lower+3*PageSize, 1, Read))
	if base, ok := m.FindFree(PageSize, lower, upper); !ok || base != lower+2*PageSize {
		t.Fatalf("one page: %#x %v", base, ok)
	}
	if base, ok := m.FindFree(2*PageSize, lower, upper); !ok || base != lower+4*PageSize {
		t.Fatalf("two pages: %#x %v", base, ok)
	}
	if _, ok := m.FindFree(2*PageSize, lower, lower+5*PageSize); ok {
		t.Fatal("found space past the limit")
	}
	if base, ok := m.FindFree(PageSize, 0, upper); !ok || base != lower+2*PageSize {
		t.Fatalf("from below the bounds: %#x", base)
	}
	if _, ok := m.FindFree(upper, lower, upper); ok {
		t.Fatal("found more space than exists")
	}
	if _, ok := m.FindFree(100, lower, upper); ok {
		t.Fatal("accepted an unaligned length")
	}
}

func TestRemoveSplitsAndReportsPieces(t *testing.T) {
	m := newMap(t)
	a := Region{Base: lower, Length: 10 * PageSize, Protection: Read | Write, Backing: Shared, Object: 7, Offset: 3 * PageSize}
	b := anonymous(lower+12*PageSize, 4, Read)
	m.Insert(a)
	m.Insert(b)
	var pieces Pieces
	count := m.Overlapping(lower+2*PageSize, 12*PageSize, &pieces)
	if count != 2 || pieces[0].Base != lower+2*PageSize || pieces[0].Length != 8*PageSize ||
		pieces[1].Base != lower+12*PageSize || pieces[1].Length != 2*PageSize {
		t.Fatalf("pieces %+v", pieces[:count])
	}
	// Cut a hole out of the middle of a: two regions, the upper one's offset
	// advanced so it still names the same object pages.
	if status := m.Remove(lower+4*PageSize, 2*PageSize, false); status != OK {
		t.Fatal(status)
	}
	upperHalf := a
	upperHalf.Base, upperHalf.Length, upperHalf.Offset = lower+6*PageSize, 4*PageSize, 9*PageSize
	lowerHalf := a
	lowerHalf.Length = 4 * PageSize
	expect(t, m, lowerHalf, upperHalf, b)
	// A range spanning a hole and two regions trims both.
	if status := m.Remove(lower+8*PageSize, 6*PageSize, false); status != OK {
		t.Fatal(status)
	}
	upperHalf.Length = 2 * PageSize
	trimmed := b
	trimmed.Base, trimmed.Length = lower+14*PageSize, 2*PageSize
	expect(t, m, lowerHalf, upperHalf, trimmed)
	// Removing everything, holes included.
	if status := m.Remove(lower, 100*PageSize, false); status != OK || m.Count() != 0 {
		t.Fatalf("remove all: %v %d", status, m.Count())
	}
}

func TestPinnedRegionsResistUserChanges(t *testing.T) {
	m := newMap(t)
	image := Region{Base: lower, Length: 4 * PageSize, Protection: Read | Execute, Backing: Image, Flags: Pinned}
	m.Insert(image)
	if status := m.Remove(lower, PageSize, false); status != PermissionDenied {
		t.Fatalf("unmap pinned: %v", status)
	}
	if status := m.Protect(lower, PageSize, Read|Write); status != PermissionDenied {
		t.Fatalf("protect pinned: %v", status)
	}
	if status := m.Remove(lower, PageSize, true); status != OK {
		t.Fatalf("kernel removal of pinned: %v", status)
	}
}

func TestProtectSplits(t *testing.T) {
	m := newMap(t)
	a := anonymous(lower, 8, Read|Write)
	b := anonymous(lower+8*PageSize, 2, Read)
	m.Insert(a)
	m.Insert(b)
	if status := m.Protect(lower+2*PageSize, 2*PageSize, Read); status != OK {
		t.Fatal(status)
	}
	expect(t, m,
		anonymous(lower, 2, Read|Write),
		anonymous(lower+2*PageSize, 2, Read),
		anonymous(lower+4*PageSize, 4, Read|Write),
		b)
	// Across two regions, ending inside the second.
	if status := m.Protect(lower+6*PageSize, 3*PageSize, 0); status != OK {
		t.Fatal(status)
	}
	expect(t, m,
		anonymous(lower, 2, Read|Write),
		anonymous(lower+2*PageSize, 2, Read),
		anonymous(lower+4*PageSize, 2, Read|Write),
		anonymous(lower+6*PageSize, 2, 0),
		anonymous(lower+8*PageSize, 1, 0),
		anonymous(lower+9*PageSize, 1, Read))
	if status := m.Protect(lower, 20*PageSize, Read); status != NotMapped {
		t.Fatalf("protect over a hole: %v", status)
	}
	if status := m.Protect(lower, PageSize, Read|Write|Execute); status != InvalidArgument {
		t.Fatalf("W+X: %v", status)
	}
}

func TestGuardCannotBeOpened(t *testing.T) {
	m := newMap(t)
	m.Insert(Region{Base: lower, Length: 16 * PageSize, Backing: Guard})
	if status := m.Protect(lower, PageSize, Read); status != PermissionDenied {
		t.Fatalf("open a guard: %v", status)
	}
	if Guard.DemandZero() || !Anonymous.DemandZero() || !Image.DemandZero() || Shared.DemandZero() {
		t.Fatal("demand-zero backings")
	}
}
