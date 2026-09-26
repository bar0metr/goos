package pmm

import (
	"goos/kernel/boot"
	"goos/kernel/kcall"
	"testing"
)

const (
	mib = uint64(1 << 20)
	gib = uint64(1 << 30)
)

// hostView backs the bitmap with ordinary host memory. The allocator only
// decides where the bitmap lives; the view decides how it is reached.
func hostView(physical uint64, words uint64) []uint64 {
	return make([]uint64, words)
}

func usable(start, bytes uint64) boot.MemoryRegion {
	return boot.MemoryRegion{Start: start, Pages: bytes / PageSize, Kind: boot.MemoryUsable}
}

func reserved(start, bytes uint64) boot.MemoryRegion {
	return boot.MemoryRegion{Start: start, Pages: bytes / PageSize, Kind: boot.MemoryReserved}
}

func newAllocator(t testing.TB, regions ...boot.MemoryRegion) *Allocator {
	t.Helper()
	allocator := &Allocator{}
	if !allocator.Init(regions, hostView) {
		t.Fatalf("Init rejected %v", regions)
	}
	return allocator
}

// The page counts SystemInfo reports are converted to bytes in userspace.
func TestSystemInfoPageSizeMatchesTheAllocator(t *testing.T) {
	if kcall.SystemInfoPageSize != PageSize {
		t.Fatalf("SystemInfo page size %d, allocator page size %d", kcall.SystemInfoPageSize, PageSize)
	}
}

// A map shaped like the QEMU q35 machines the boot test uses: firmware and
// loader memory between usable runs, the 32-bit PCI hole, and at 4 GiB RAM
// listed out of order, as OVMF lists it.
func TestInitBuildsSegmentsFromMemoryMaps(t *testing.T) {
	tests := []struct {
		name      string
		regions   []boot.MemoryRegion
		segments  [][2]uint64
		managed   uint64
		high      uint64
		unmanaged uint64
	}{
		{
			name: "ram below one GiB",
			regions: []boot.MemoryRegion{
				usable(0, 640*1024),
				usable(mib, 3*mib),
				reserved(4*mib, 2*mib),
				usable(6*mib, 506*mib),
			},
			segments: [][2]uint64{{mib, 4 * mib}, {6 * mib, 512 * mib}},
			managed:  (3*mib + 506*mib) / PageSize,
		},
		{
			name: "ram across and above one GiB",
			regions: []boot.MemoryRegion{
				usable(mib, 2*gib-mib-64*mib),
				reserved(2*gib-64*mib, 64*mib),
			},
			segments: [][2]uint64{{mib, gib}, {gib, 2*gib - 64*mib}},
			managed:  (2*gib - mib - 64*mib) / PageSize,
			high:     (gib - 64*mib) / PageSize,
		},
		{
			name: "ram above four GiB listed first",
			regions: []boot.MemoryRegion{
				usable(4*gib, 2*gib),
				reserved(3*gib+512*mib, 256*mib),
				usable(mib, 2*gib-mib),
				reserved(2*gib, 16*mib),
			},
			segments: [][2]uint64{{mib, gib}, {gib, 2 * gib}, {4 * gib, 6 * gib}},
			managed:  (4*gib - mib) / PageSize,
			high:     3 * gib / PageSize,
		},
		{
			name: "adjacent and overlapping usable regions merge",
			regions: []boot.MemoryRegion{
				usable(8*mib, 8*mib),
				usable(mib, 7*mib),
				usable(12*mib, 8*mib),
			},
			segments: [][2]uint64{{mib, 20 * mib}},
			managed:  19 * mib / PageSize,
		},
		{
			name: "memory above the managed limit is counted, not managed",
			regions: []boot.MemoryRegion{
				usable(mib, 15*mib),
				usable(MaxPhysical-2*mib, 6*mib),
				usable(MaxPhysical+gib, 4*mib),
			},
			segments:  [][2]uint64{{mib, 16 * mib}, {MaxPhysical - 2*mib, MaxPhysical}},
			managed:   (15*mib + 2*mib) / PageSize,
			high:      2 * mib / PageSize,
			unmanaged: (4*mib + 4*mib) / PageSize,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			allocator := newAllocator(t, test.regions...)
			for index, want := range test.segments {
				start, end, ok := allocator.Segment(index)
				if !ok || start != want[0] || end != want[1] {
					t.Fatalf("segment %d = [%#x, %#x) %v, want [%#x, %#x)", index, start, end, ok, want[0], want[1])
				}
			}
			if _, _, ok := allocator.Segment(len(test.segments)); ok {
				t.Fatalf("more than %d segments", len(test.segments))
			}
			stats := allocator.Statistics()
			if stats.ManagedPages != test.managed || stats.HighPages != test.high || stats.UnmanagedPages != test.unmanaged {
				t.Fatalf("stats %+v, want managed %d high %d unmanaged %d", stats, test.managed, test.high, test.unmanaged)
			}
			if stats.FreePages+stats.UsedPages != stats.ManagedPages || stats.UsedPages == 0 {
				t.Fatalf("inconsistent accounting %+v", stats)
			}
		})
	}
}

func TestBitmapIsCarvedFromTheTopOfLowMemory(t *testing.T) {
	var carved uint64
	var words uint64
	view := func(physical uint64, count uint64) []uint64 {
		carved, words = physical, count
		return make([]uint64, count)
	}
	var allocator Allocator
	regions := []boot.MemoryRegion{usable(mib, 63*mib), usable(4*gib, gib)}
	if !allocator.Init(regions, view) {
		t.Fatal("Init failed")
	}
	// 63 MiB and 1 GiB of pages need 252 + 4096 bitmap words, nine pages.
	if words != 252+4096 || carved != 64*mib-9*PageSize {
		t.Fatalf("bitmap of %d words at %#x", words, carved)
	}
	stats := allocator.Statistics()
	if stats.UsedPages != 9 {
		t.Fatalf("bitmap pages not marked used: %+v", stats)
	}
	for page := carved; page < 64*mib; page += PageSize {
		if !allocator.IsAllocated(page) || allocator.Free(page) {
			t.Fatalf("bitmap page %#x is not permanently used", page)
		}
	}
}

func TestInitRejectsUnusableMaps(t *testing.T) {
	tests := []struct {
		name    string
		regions []boot.MemoryRegion
		view    MetadataView
	}{
		{name: "no usable memory", regions: []boot.MemoryRegion{reserved(mib, 4*mib)}, view: hostView},
		{name: "only the first MiB", regions: []boot.MemoryRegion{usable(0, mib)}, view: hostView},
		{name: "no low memory for the bitmap", regions: []boot.MemoryRegion{usable(2*gib, gib)}, view: hostView},
		{name: "no view", regions: []boot.MemoryRegion{usable(mib, mib)}},
		{name: "short view", regions: []boot.MemoryRegion{usable(mib, mib)}, view: func(uint64, uint64) []uint64 { return nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var allocator Allocator
			if allocator.Init(test.regions, test.view) {
				t.Fatal("Init accepted the map")
			}
			if _, ok := allocator.Allocate(); ok {
				t.Fatal("allocated from a failed allocator")
			}
			if stats := allocator.Statistics(); stats.ManagedPages != 0 || stats.FreePages != 0 {
				t.Fatalf("failed Init left state %+v", stats)
			}
		})
	}
}

func TestHighSegmentsAreDroppedWhenTheBitmapDoesNotFit(t *testing.T) {
	// One low page can hold the bitmap of the low segment only.
	allocator := newAllocator(t, usable(mib, PageSize), usable(2*gib, gib))
	stats := allocator.Statistics()
	if stats.ManagedPages != 1 || stats.FreePages != 0 || stats.UnmanagedPages != gib/PageSize {
		t.Fatalf("stats %+v", stats)
	}
}

func TestSegmentLimitLeavesExcessRangesUnmanaged(t *testing.T) {
	var regions []boot.MemoryRegion
	for index := uint64(0); index < MaxSegments+6; index++ {
		regions = append(regions, usable((2+2*index)*mib, mib))
	}
	allocator := newAllocator(t, regions...)
	stats := allocator.Statistics()
	if stats.ManagedPages != MaxSegments*mib/PageSize || stats.UnmanagedPages != 6*mib/PageSize {
		t.Fatalf("stats %+v", stats)
	}
	if _, _, ok := allocator.Segment(MaxSegments - 1); !ok {
		t.Fatal("last segment missing")
	}
}

func TestAllocationClasses(t *testing.T) {
	allocator := newAllocator(t, usable(mib, 2*gib-mib), usable(4*gib, 16*mib))
	high, ok := allocator.Allocate()
	if !ok || high != 4*gib {
		t.Fatalf("Allocate = %#x %v, want the highest segment first", high, ok)
	}
	low, ok := allocator.AllocateLow()
	if !ok || low != mib {
		t.Fatalf("AllocateLow = %#x %v", low, ok)
	}
	block, ok := allocator.AllocateContiguous(4)
	if !ok || block != mib+PageSize || block+4*PageSize > LowLimit {
		t.Fatalf("AllocateContiguous = %#x %v", block, ok)
	}
	for _, page := range []uint64{high, low, block, block + 3*PageSize} {
		if !allocator.IsAllocated(page) {
			t.Fatalf("page %#x not marked", page)
		}
	}
}

func TestAllocateCrossesSegmentBoundaries(t *testing.T) {
	allocator := newAllocator(t,
		usable(mib, 4*mib),
		usable(gib, 2*PageSize),
		reserved(gib+2*PageSize, PageSize),
		usable(gib+3*PageSize, PageSize),
	)
	// The low segment holds the bitmap; the two high segments are taken from
	// the highest down, then allocation falls back to low memory.
	want := []uint64{gib + 3*PageSize, gib, gib + PageSize, mib}
	for _, expected := range want {
		page, ok := allocator.Allocate()
		if !ok || page != expected {
			t.Fatalf("Allocate = %#x %v, want %#x", page, ok, expected)
		}
	}
	if allocator.IsAllocated(gib+2*PageSize) || allocator.Free(gib+2*PageSize) {
		t.Fatal("the reserved hole between segments was treated as managed")
	}
}

func TestAllocateContiguousNeverSpansAHole(t *testing.T) {
	allocator := newAllocator(t,
		usable(mib, 3*PageSize),
		reserved(mib+3*PageSize, PageSize),
		usable(mib+4*PageSize, 3*PageSize),
		usable(2*mib, 64*PageSize),
	)
	block, ok := allocator.AllocateContiguous(5)
	if !ok || block != 2*mib {
		t.Fatalf("AllocateContiguous(5) = %#x %v, want the first segment large enough", block, ok)
	}
	run, ok := allocator.AllocateContiguous(3)
	if !ok || run != mib {
		t.Fatalf("AllocateContiguous(3) = %#x %v", run, ok)
	}
	if _, ok := allocator.AllocateContiguous(0); ok {
		t.Fatal("accepted an empty contiguous allocation")
	}
	if _, ok := allocator.AllocateContiguous(128); ok {
		t.Fatal("allocated a run larger than any segment")
	}
}

func TestContiguousAllocationStaysLow(t *testing.T) {
	allocator := newAllocator(t, usable(mib, 4*PageSize), usable(gib, 64*PageSize))
	// Three low pages remain after the one-page bitmap.
	if _, ok := allocator.AllocateContiguous(4); ok {
		t.Fatal("a contiguous run came from high memory")
	}
	if block, ok := allocator.AllocateContiguous(3); !ok || block != mib {
		t.Fatalf("AllocateContiguous(3) = %#x %v", block, ok)
	}
	if page, ok := allocator.AllocateLow(); ok {
		t.Fatalf("AllocateLow returned %#x from exhausted low memory", page)
	}
	if page, ok := allocator.Allocate(); !ok || page < LowLimit {
		t.Fatalf("Allocate = %#x %v", page, ok)
	}
}

func TestFreeAndReallocate(t *testing.T) {
	allocator := newAllocator(t, usable(mib, 8*PageSize), usable(3*gib, 8*PageSize))
	before := allocator.Statistics()
	var pages []uint64
	for {
		page, ok := allocator.Allocate()
		if !ok {
			break
		}
		pages = append(pages, page)
	}
	// Sixteen pages less the bitmap page.
	if len(pages) != 15 {
		t.Fatalf("allocated %d pages", len(pages))
	}
	if stats := allocator.Statistics(); stats.FreePages != 0 || stats.UsedPages != stats.ManagedPages {
		t.Fatalf("exhausted stats %+v", stats)
	}
	for _, page := range []uint64{pages[2], pages[12]} {
		if !allocator.Free(page) {
			t.Fatalf("Free(%#x) rejected", page)
		}
	}
	first, _ := allocator.Allocate()
	second, _ := allocator.Allocate()
	if first != pages[2] || second != pages[12] {
		t.Fatalf("reallocated %#x %#x, want %#x %#x", first, second, pages[2], pages[12])
	}
	for _, page := range pages {
		if !allocator.Free(page) {
			t.Fatalf("Free(%#x) rejected", page)
		}
	}
	if after := allocator.Statistics(); after != before {
		t.Fatalf("stats after freeing everything %+v, before %+v", after, before)
	}
}

func TestFreeRejectsInvalidPages(t *testing.T) {
	allocator := newAllocator(t, usable(0, mib), usable(mib, 4*PageSize), reserved(mib+4*PageSize, PageSize), usable(gib, PageSize))
	page, ok := allocator.Allocate()
	if !ok {
		t.Fatal("allocation failed")
	}
	tests := []struct {
		name    string
		address uint64
	}{
		{name: "unaligned", address: page + 1},
		{name: "page zero", address: 0},
		{name: "first MiB", address: mib - PageSize},
		{name: "reserved", address: mib + 4*PageSize},
		{name: "never allocated", address: mib},
		{name: "bitmap", address: mib + 3*PageSize},
		{name: "above every segment", address: 4 * gib},
		{name: "above the managed limit", address: MaxPhysical},
	}
	for _, test := range tests {
		if allocator.Free(test.address) {
			t.Fatalf("%s: Free(%#x) accepted", test.name, test.address)
		}
	}
	if !allocator.Free(page) || allocator.Free(page) {
		t.Fatal("double free accepted")
	}
}

func TestFirstMiBIsNeverAllocated(t *testing.T) {
	allocator := newAllocator(t, usable(0, 2*mib))
	for {
		page, ok := allocator.AllocateLow()
		if !ok {
			break
		}
		if page < mib {
			t.Fatalf("allocated %#x in the first MiB", page)
		}
	}
	if stats := allocator.Statistics(); stats.ManagedPages != mib/PageSize {
		t.Fatalf("stats %+v", stats)
	}
}

func BenchmarkAllocateFree(b *testing.B) {
	allocator := newAllocator(b, usable(mib, 4096*PageSize))
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		page, ok := allocator.Allocate()
		if !ok || !allocator.Free(page) {
			b.Fatal("allocator failed")
		}
	}
}

func fragmentedAllocator(t testing.TB) *Allocator {
	return newAllocator(t,
		usable(mib, PageSize),
		usable(mib+PageSize, 40000*PageSize),
		usable(mib+40001*PageSize, 4096*PageSize),
	)
}

func TestAllocateSkipsLongUsedRuns(t *testing.T) {
	allocator := fragmentedAllocator(t)
	for page := uint64(0); page < 40001; page++ {
		if _, ok := allocator.AllocateLow(); !ok {
			t.Fatal("allocation failed")
		}
	}
	block, ok := allocator.AllocateContiguous(5)
	if !ok || block != mib+40001*PageSize {
		t.Fatalf("contiguous allocation %#x %v", block, ok)
	}
	for page := uint64(0); page < 5; page++ {
		if !allocator.IsAllocated(block + page*PageSize) {
			t.Fatalf("page %d of the block is not marked", page)
		}
	}
	low := mib
	if !allocator.Free(low) {
		t.Fatal("low page not freed")
	}
	if again, ok := allocator.Allocate(); !ok || again != low {
		t.Fatalf("freed low page not reused first: %#x", again)
	}
}

func BenchmarkAllocateAcrossUsedRun(b *testing.B) {
	allocator := fragmentedAllocator(b)
	for page := uint64(0); page < 40000; page++ {
		allocator.AllocateLow()
	}
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		low, _ := allocator.Allocate()
		high, ok := allocator.Allocate()
		if !ok || !allocator.Free(low) || !allocator.Free(high) {
			b.Fatal("allocator failed")
		}
	}
}
