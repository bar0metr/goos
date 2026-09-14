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
