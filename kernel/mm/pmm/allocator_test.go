package pmm

import (
	"goos/kernel/boot"
	"testing"
)

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
