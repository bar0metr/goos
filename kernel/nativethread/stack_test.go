package nativethread

import (
	"testing"

	"goos/kernel/kcall"
	"goos/kernel/mm/region"
	"goos/kernel/mm/vm"
)

// largeProcess fills regions with what a large native process holds besides
// its threads, generously counted: the image's stack-gap guard and eight
// segments, the main stack and its guard, the startup page, the heap's three
// reservations, the four per-thread record reservations (snapshots, arena
// states, call scratch, goroutine Ms), four goroutine stack batches, and
// eight regions the program maps itself.
func largeProcess(t *testing.T, regions *region.Map) int {
	t.Helper()
	const page = vm.PageSize
	insert := func(r region.Region) {
		t.Helper()
		if status := regions.Insert(r); status != region.OK {
			t.Fatalf("insert %+v: %d", r, status)
		}
	}
	insert(region.Region{Base: vm.UserCodeAddress - page, Length: page, Backing: region.Guard, Flags: region.Pinned})
	for index := uint64(0); index < 8; index++ {
		insert(region.Region{Base: vm.UserCodeAddress + index*16*page, Length: 8 * page, Protection: region.Read, Backing: region.Image, Flags: region.Pinned})
	}
	stackReserve := uint64(64 << 20)
	stackBase := vm.UserStackTop - stackReserve
	insert(region.Region{Base: stackBase - 64<<10, Length: 64 << 10, Backing: region.Guard, Flags: region.Pinned})
	insert(region.Region{Base: stackBase, Length: stackReserve, Protection: region.Read | region.Write, Backing: region.Anonymous, Flags: region.Pinned})
	insert(region.Region{Base: vm.UserStartupAddress, Length: page, Protection: region.Read, Backing: region.Anonymous, Flags: region.Pinned})
	reservations := 3 + 4 + 8
	for index := 0; index < reservations; index++ {
		base, ok := regions.FindFree(16*page, vm.UserMapBase, vm.UserMapLimit)
		if !ok {
			t.Fatal("no room for a reservation")
		}
		insert(region.Region{Base: base, Length: 16 * page, Protection: region.Read | region.Write, Backing: region.Anonymous})
	}
	for index := 0; index < 4; index++ {
		base, ok := regions.FindFree(8*region.StackStride, vm.UserMapBase, vm.UserMapLimit)
		if !ok {
			t.Fatal("no room for a stack batch")
		}
		insert(region.Region{Base: base, Length: 8 * region.StackStride, Protection: region.Read | region.Write, Backing: region.Anonymous, Flags: region.StackBatch})
	}
	return regions.Count()
}

// A large process runs ThreadSlots threads, the main one and 63 others,
// within region.MaxRegions next to everything else it maps. With a guard
// region and a stack region a thread, this layout fits only 48 threads.
func TestSixtyFourThreadsFitTheRegionTable(t *testing.T) {
	var regions region.Map
	if !regions.Init(vm.UserAddressStart, vm.UserAddressLimit) {
		t.Fatal("init")
	}
	before := largeProcess(t, &regions)
	if before+2*(Slots-1) <= region.MaxRegions {
		t.Fatalf("the layout holds %d regions; two a thread would fit, so the test proves nothing", before)
	}
	for slot := uint64(1); slot < Slots; slot++ {
		if status := regions.Insert(StackRegion(slot)); status != region.OK {
			t.Fatalf("slot %d: Insert = %d with %d regions", slot, status, regions.Count())
		}
	}
	if threads := regions.Count() - before; threads != Slots-1 {
		t.Fatalf("%d thread regions, want one per slot", threads)
	}
	if spare := region.MaxRegions - regions.Count(); spare < 32 {
		t.Fatalf("64 threads leave %d of %d regions", spare, region.MaxRegions)
	}
}

// A thread's region spans its slot exactly: the guard is its lowest
// ThreadGuardSize bytes and the stack ends at ThreadStackTop.
func TestStackRegionLayout(t *testing.T) {
	for slot := uint64(1); slot < Slots; slot++ {
		r := StackRegion(slot)
		top := kcall.ThreadStackTop(slot)
		guard := top - kcall.ThreadStride
		if r.Base != guard || r.End() != top || r.Flags&(region.Pinned|region.LowGuard) != region.Pinned|region.LowGuard {
			t.Fatalf("slot %d: %+v", slot, r)
		}
		if !r.GuardedAt(guard) || !r.GuardedAt(guard+kcall.ThreadGuardSize-1) || r.GuardedAt(guard+kcall.ThreadGuardSize) || r.GuardedAt(top-1) {
			t.Fatalf("slot %d: the guard is not the lowest %d bytes", slot, kcall.ThreadGuardSize)
		}
		if kcall.ThreadSlotOf(guard) != slot || kcall.ThreadSlotOf(top-1) != slot {
			t.Fatalf("slot %d: ThreadSlotOf disagrees", slot)
		}
	}
}
