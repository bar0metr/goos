package pmm

import (
	"goos/kernel/boot"
	"goos/kernel/kcall"
	"testing"
)

// The page counts SystemInfo reports are converted to bytes in userspace.
func TestSystemInfoPageSizeMatchesTheAllocator(t *testing.T) {
	if kcall.SystemInfoPageSize != PageSize {
		t.Fatalf("SystemInfo page size %d, allocator page size %d", kcall.SystemInfoPageSize, PageSize)
	}
}

func TestAllocateFreeAndReuse(t *testing.T) {
	var allocator Allocator
	allocator.Init([]boot.MemoryRegion{{
		Start: lowMemoryPages * PageSize,
		Pages: 3,
		Kind:  boot.MemoryUsable,
	}})
	first, ok := allocator.Allocate()
	if !ok || first != lowMemoryPages*PageSize {
		t.Fatalf("first allocation = %#x, %v", first, ok)
	}
	if !allocator.Free(first) {
		t.Fatal("valid free rejected")
	}
	reused, ok := allocator.Allocate()
	if !ok || reused != first {
		t.Fatalf("freed page not reused: %#x, %v", reused, ok)
	}
}

func TestReservedAndLowMemoryNeverAllocated(t *testing.T) {
	var allocator Allocator
	allocator.Init([]boot.MemoryRegion{
		{Start: 0, Pages: lowMemoryPages, Kind: boot.MemoryUsable},
		{Start: lowMemoryPages * PageSize, Pages: 1, Kind: boot.MemoryReserved},
	})
	if _, ok := allocator.Allocate(); ok {
		t.Fatal("allocated low or reserved memory")
	}
}

func TestRejectsInvalidAndDoubleFree(t *testing.T) {
	var allocator Allocator
	allocator.Init([]boot.MemoryRegion{{
		Start: lowMemoryPages * PageSize,
		Pages: 1,
		Kind:  boot.MemoryUsable,
	}})
	page, ok := allocator.Allocate()
	if !ok {
		t.Fatal("allocation failed")
	}
	if allocator.Free(page+1) || !allocator.Free(page) || allocator.Free(page) {
		t.Fatal("invalid free handling")
	}
}

func TestUsableRegionIsClippedToManagedLimit(t *testing.T) {
	var allocator Allocator
	allocator.Init([]boot.MemoryRegion{{
		Start: managedBytes - PageSize,
		Pages: 4,
		Kind:  boot.MemoryUsable,
	}})
	stats := allocator.Statistics()
	if stats.ManagedPages != managedPages || stats.FreePages != 1 {
		t.Fatalf("unexpected clipped stats: %#v", stats)
	}
}

func TestAllocateContiguous(t *testing.T) {
	var allocator Allocator
	allocator.Init([]boot.MemoryRegion{{
		Start: lowMemoryPages * PageSize,
		Pages: 8,
		Kind:  boot.MemoryUsable,
	}})
	start, ok := allocator.AllocateContiguous(4)
	if !ok || start != lowMemoryPages*PageSize {
		t.Fatalf("contiguous allocation = %#x, %v", start, ok)
	}
	for page := uint64(0); page < 4; page++ {
		if !allocator.Free(start + page*PageSize) {
			t.Fatalf("allocated page %d was not owned", page)
		}
	}
	if _, ok := allocator.AllocateContiguous(0); ok {
		t.Fatal("accepted empty contiguous allocation")
	}
}

func BenchmarkAllocateFree(b *testing.B) {
	var allocator Allocator
	allocator.Init([]boot.MemoryRegion{{
		Start: lowMemoryPages * PageSize,
		Pages: 4096,
		Kind:  boot.MemoryUsable,
	}})
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		page, ok := allocator.Allocate()
		if !ok || !allocator.Free(page) {
			b.Fatal("allocator failed")
		}
	}
}

func fragmentedAllocator() *Allocator {
	allocator := &Allocator{}
	allocator.Init([]boot.MemoryRegion{
		{Start: lowMemoryPages * PageSize, Pages: 1, Kind: boot.MemoryUsable},
		{Start: (lowMemoryPages + 40000) * PageSize, Pages: 4096, Kind: boot.MemoryUsable},
	})
	return allocator
}

func TestAllocateSkipsLongUsedRuns(t *testing.T) {
	allocator := fragmentedAllocator()
	low, _ := allocator.Allocate()
	high, ok := allocator.Allocate()
	if !ok || low != lowMemoryPages*PageSize || high != (lowMemoryPages+40000)*PageSize {
		t.Fatalf("allocations %#x %#x %v", low, high, ok)
	}
	block, ok := allocator.AllocateContiguous(5)
	if !ok || block != (lowMemoryPages+40001)*PageSize {
		t.Fatalf("contiguous allocation %#x %v", block, ok)
	}
	for page := uint64(0); page < 5; page++ {
		if !allocator.IsAllocated(block + page*PageSize) {
			t.Fatalf("page %d of the block is not marked", page)
		}
	}
	if !allocator.Free(low) {
		t.Fatal("low page not freed")
	}
	if again, ok := allocator.Allocate(); !ok || again != low {
		t.Fatalf("freed low page not reused first: %#x", again)
	}
}

func BenchmarkAllocateAcrossUsedRun(b *testing.B) {
	allocator := fragmentedAllocator()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		low, _ := allocator.Allocate()
		high, ok := allocator.Allocate()
		if !ok || !allocator.Free(low) || !allocator.Free(high) {
			b.Fatal("allocator failed")
		}
	}
}
