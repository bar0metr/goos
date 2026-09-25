package vm

import (
	"goos/kernel/mm/pmm"
	"testing"
)

func TestDirectMapLayout(t *testing.T) {
	if directMapSlot != 256 || DirectMapBase>>47 != 0x1ffff {
		t.Fatalf("direct map at %#x is not the first upper-half PML4 slot", DirectMapBase)
	}
	// The whole managed range fits in the one slot every address space shares.
	if pmm.MaxPhysical > 1<<39 {
		t.Fatalf("managed limit %#x exceeds one PML4 slot", pmm.MaxPhysical)
	}
	if pmm.LowLimit != IdentityMapLimit {
		t.Fatalf("low allocation limit %#x differs from the identity map %#x", pmm.LowLimit, IdentityMapLimit)
	}
}

func TestLargePageEntryIsSupervisorDataOnly(t *testing.T) {
	entry := largePageEntry(5<<30 + 0x3000)
	if entry&addressMask != 5<<30 || entry&entryLarge == 0 || entry&entryNX == 0 ||
		entry&entryWritable == 0 || entry&entryPresent == 0 || entry&entryUser != 0 {
		t.Fatalf("direct-map leaf %#x", entry)
	}
}

func TestKernelAddress(t *testing.T) {
	var mapped directMap
	if !mapped.add(1<<20, 1<<30) || !mapped.add(1<<30, 2<<30) || !mapped.add(4<<30, 6<<30) {
		t.Fatal("ranges rejected")
	}
	tests := []struct {
		name     string
		physical uint64
		length   uint64
		address  uint64
		ok       bool
	}{
		{name: "low page", physical: 0x200000, length: PageSize, address: 0x200000, ok: true},
		{name: "last low byte", physical: IdentityMapLimit - 1, length: 1, address: IdentityMapLimit - 1, ok: true},
		{name: "first MiB", physical: 0x1000, length: PageSize},
		{name: "zero length", physical: 0x200000},
		{name: "straddles the identity limit", physical: IdentityMapLimit - PageSize, length: 2 * PageSize},
		{name: "first high page", physical: 1 << 30, length: PageSize, address: DirectMapBase + 1<<30, ok: true},
		{name: "above four GiB", physical: 5<<30 + 0x123, length: 16, address: DirectMapBase + 5<<30 + 0x123, ok: true},
		{name: "hole between ranges", physical: 3 << 30, length: PageSize},
		{name: "crosses a range end", physical: 2<<30 - PageSize, length: 2 * PageSize},
		{name: "above every range", physical: 6 << 30, length: PageSize},
		{name: "wraps", physical: ^uint64(0) - 8, length: 16},
	}
	for _, test := range tests {
		address, ok := mapped.kernelAddress(test.physical, test.length)
		if ok != test.ok || (ok && address != test.address) {
			t.Fatalf("%s: kernelAddress(%#x, %d) = %#x %v", test.name, test.physical, test.length, address, ok)
		}
	}
}

func TestDirectMapRangesAreBounded(t *testing.T) {
	var mapped directMap
	for _, invalid := range [][2]uint64{{2 << 30, 2 << 30}, {1<<30 + 1, 2 << 30}, {pmm.MaxPhysical - PageSize, pmm.MaxPhysical + PageSize}} {
		if mapped.add(invalid[0], invalid[1]) {
			t.Fatalf("accepted [%#x, %#x)", invalid[0], invalid[1])
		}
	}
	for index := 0; index < pmm.MaxSegments; index++ {
		start := uint64(index+1) << 30
		if !mapped.add(start, start+PageSize) {
			t.Fatalf("range %d rejected", index)
		}
	}
	if mapped.add(100<<30, 101<<30) {
		t.Fatal("accepted a range past the segment limit")
	}
}
