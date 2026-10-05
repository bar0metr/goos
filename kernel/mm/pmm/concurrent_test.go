package pmm

import (
	"sync"
	"testing"
)

// TestConcurrentProcessorsNeverShareAPage allocates and frees from several
// goroutines at once, as several processors do, and checks that no page is
// handed out twice and that every page comes back.
func TestConcurrentProcessorsNeverShareAPage(t *testing.T) {
	allocator := newAllocator(t, usable(mib, 8*mib))
	before := allocator.Statistics().FreePages
	var owners sync.Map
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			var pages [64]uint64
			for round := 0; round < 40; round++ {
				for index := range pages {
					page, ok := allocator.Allocate()
					if !ok {
						t.Error("allocation failed with memory free")
						return
					}
					if other, taken := owners.LoadOrStore(page, worker); taken {
						t.Errorf("page %#x given to workers %d and %d", page, other, worker)
						return
					}
					pages[index] = page
				}
				for _, page := range pages {
					owners.Delete(page)
					if !allocator.Free(page) {
						t.Errorf("free of %#x refused", page)
						return
					}
				}
			}
		}(worker)
	}
	group.Wait()
	if after := allocator.Statistics().FreePages; after != before {
		t.Fatalf("free pages %d, want %d", after, before)
	}
}
