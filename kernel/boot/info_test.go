package boot

import "testing"

func TestInfoRejectsInvalidRegions(t *testing.T) {
	var info Info
	if info.AddRegion(MemoryRegion{Start: 1, Pages: 1, Kind: MemoryUsable}) {
		t.Fatal("accepted unaligned region")
	}
	if info.AddRegion(MemoryRegion{Start: PageSize, Pages: 0, Kind: MemoryUsable}) {
		t.Fatal("accepted empty region")
	}
	if info.AddRegion(MemoryRegion{Start: ^uint64(0) - PageSize + 1, Pages: 2, Kind: MemoryUsable}) {
		t.Fatal("accepted overflowing region")
	}
}

func TestInfoCountsUsableMemory(t *testing.T) {
	var info Info
	if !info.AddRegion(MemoryRegion{Start: PageSize, Pages: 3, Kind: MemoryUsable}) {
		t.Fatal("valid region rejected")
	}
	if !info.AddRegion(MemoryRegion{Start: 4 * PageSize, Pages: 2, Kind: MemoryReserved}) {
		t.Fatal("valid reserved region rejected")
	}
	if info.UsableBytes != 3*PageSize || len(info.MemoryRegions()) != 2 {
		t.Fatalf("unexpected info: %#v", info)
	}
}

func TestInfoMergesContiguousRegionsOfOneKind(t *testing.T) {
	var info Info
	for index := uint64(0); index < 3*MaxRegions; index++ {
		if !info.AddRegion(MemoryRegion{Start: (1 + index) * PageSize, Pages: 1, Kind: MemoryReclaimable}) {
			t.Fatalf("contiguous descriptor %d rejected", index)
		}
	}
	if info.RegionCount != 1 || info.Regions[0].Pages != 3*MaxRegions {
		t.Fatalf("not merged: %d regions, first %#v", info.RegionCount, info.Regions[0])
	}
	// A different kind, or a gap, starts a new region.
	info.AddRegion(MemoryRegion{Start: info.Regions[0].End(), Pages: 2, Kind: MemoryUsable})
	info.AddRegion(MemoryRegion{Start: info.Regions[1].End() + PageSize, Pages: 2, Kind: MemoryUsable})
	info.AddRegion(MemoryRegion{Start: info.Regions[2].End(), Pages: 3, Kind: MemoryUsable})
	if info.RegionCount != 3 || info.Regions[2].Pages != 5 || info.UsableBytes != 7*PageSize {
		t.Fatalf("unexpected regions: %d %#v usable %d", info.RegionCount, info.Regions[:3], info.UsableBytes)
	}
}

func TestInfoHoldsMaxRegions(t *testing.T) {
	var info Info
	kinds := [...]MemoryKind{MemoryUsable, MemoryReserved}
	for index := uint64(0); index < MaxRegions; index++ {
		if !info.AddRegion(MemoryRegion{Start: (1 + index) * PageSize, Pages: 1, Kind: kinds[index%2]}) {
			t.Fatalf("region %d rejected", index)
		}
	}
	if info.AddRegion(MemoryRegion{Start: (1 + MaxRegions) * PageSize, Pages: 1, Kind: kinds[MaxRegions%2]}) {
		t.Fatal("accepted a region past MaxRegions")
	}
	// A region that continues the last one still fits.
	if !info.AddRegion(MemoryRegion{Start: (1 + MaxRegions) * PageSize, Pages: 1, Kind: kinds[(MaxRegions-1)%2]}) {
		t.Fatal("contiguous region rejected when full")
	}
}
