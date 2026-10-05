package boot

import "testing"

func TestLowPagePicksHighestUsablePageBelowCeiling(t *testing.T) {
	regions := []MemoryRegion{
		{Start: 0, Pages: 0x9f, Kind: MemoryUsable},
		{Start: 0x9f000, Pages: 1, Kind: MemoryReserved},
		{Start: 0x100000, Pages: 1000, Kind: MemoryUsable},
	}
	page, found := LowPage(regions)
	if !found || page != 0x9e000 {
		t.Fatalf("page = %#x, %v", page, found)
	}
}

func TestLowPageSkipsReservedAndTooLowMemory(t *testing.T) {
	regions := []MemoryRegion{
		{Start: 0x1000, Pages: 8, Kind: MemoryUsable},
		{Start: 0x20000, Pages: 4, Kind: MemoryLoader},
		{Start: 0x40000, Pages: 2, Kind: MemoryUsable},
	}
	page, found := LowPage(regions)
	if !found || page != 0x41000 {
		t.Fatalf("page = %#x, %v", page, found)
	}
	if _, found := LowPage(regions[:2]); found {
		t.Fatal("a page below the floor was chosen")
	}
}
