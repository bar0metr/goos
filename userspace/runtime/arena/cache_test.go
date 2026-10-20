package arena

import (
	"sync"
	"testing"
)

// cacheThread is one thread's cache and counts over a test heap.
type cacheThread struct {
	cache  threadCache
	counts threadCounts
	owner  uint32
}

// alloc is allocCached without the collector: the fast path, then a refill
// under lock.
func (c *cacheThread) alloc(t testing.TB, th *testHeap, lock *sync.Mutex, size uintptr) uintptr {
	class := th.classOf[(size+15)>>4]
	if address := th.cacheAlloc(&c.cache, &c.counts, class, false); address != 0 {
		return address
	}
	lock.Lock()
	th.foldCounts(&c.counts)
	address := uintptr(0)
	if th.refill(&c.cache, &c.counts, class, c.owner) {
		address = th.cacheAlloc(&c.cache, &c.counts, class, false)
	}
	lock.Unlock()
	if address == 0 {
		t.Errorf("thread %d: allocation of %d failed", c.owner, size)
	}
	return address
}

// free is freeThreaded without the collector.
func (c *cacheThread) free(t testing.TB, th *testHeap, lock *sync.Mutex, address uintptr) {
	if c.freeOwned(t, th, address) {
		return
	}
	lock.Lock()
	if !c.freeOwned(t, th, address) {
		th.freeAt(address)
	}
	lock.Unlock()
}

func (c *cacheThread) freeOwned(t testing.TB, th *testHeap, address uintptr) bool {
	page, slot, owner, ok := th.ownedSlot(address)
	if owner == 0 {
		return false
	}
	if ok && owner == c.owner {
		ok = th.cacheFree(&c.counts, page, slot)
	} else if ok {
		ok = th.remoteFree(&c.counts, page, slot)
	}
	if !ok {
		t.Errorf("thread %d: free of %#x refused", c.owner, address)
	}
	return true
}

func (c *cacheThread) finish(th *testHeap) {
	th.returnCache(&c.cache, &c.counts)
	th.foldCounts(&c.counts)
}

func TestOwnedSlabModel(t *testing.T) {
	th := newTestHeap(t, 64, 64)
	var lock sync.Mutex
	a, b := &cacheThread{owner: 1}, &cacheThread{owner: 2}
	slots := int(th.classSlots[0])
	first := a.alloc(t, th, &lock, 16)
	page, slot, owner, ok := th.ownedSlot(first)
	if !ok || owner != 1 || slot != 0 || th.meta[page].flags&pageListed != 0 {
		t.Fatalf("owned slot %d %d %d %v", page, slot, owner, ok)
	}
	held := []uintptr{first}
	for len(held) < slots {
		held = append(held, a.alloc(t, th, &lock, 16))
	}
	if th.cacheAlloc(&a.cache, &a.counts, 0, false) != 0 {
		t.Fatal("a full slab handed out a slot")
	}
	// b frees two of a's slots; a second free of one is refused.
	_, third, _, _ := th.ownedSlot(held[3])
	if !th.remoteFree(&b.counts, page, third) || th.remoteFree(&b.counts, page, third) {
		t.Fatal("remote free or its double-free check")
	}
	b.free(t, th, &lock, held[7])
	// The refill folds both and the lowest comes back first.
	if again := a.alloc(t, th, &lock, 16); again != held[3] {
		t.Fatalf("refill gave %#x, want %#x", again, held[3])
	}
	// Each refill folds the counts before it into the heap's.
	if drained, refills := a.counts.drained+th.diag.drained, a.counts.refills+th.diag.refills; drained != 2 || refills != 2 {
		t.Fatalf("drained %d refills %d", drained, refills)
	}
	// The owner frees its own slot; a second free is refused.
	_, own, _, _ := th.ownedSlot(held[10])
	if !th.cacheFree(&a.counts, page, own) || th.cacheFree(&a.counts, page, own) {
		t.Fatal("own free or its double-free check")
	}
	if _, _, _, ok := th.ownedSlot(held[10] + 1); ok {
		t.Fatal("an interior address resolved to a slot")
	}
	a.finish(th)
	b.finish(th)
	if th.meta[page].owner != 0 {
		t.Fatal("returned slab still owned")
	}
	th.verify()
	if want := uint64(slots - 2); th.liveObjects != want {
		t.Fatalf("%d live objects, want %d", th.liveObjects, want)
	}
	th.collect(nil)
	if th.liveObjects != 0 || th.inUsePages != 0 {
		t.Fatalf("%d objects and %d pages after collecting everything", th.liveObjects, th.inUsePages)
	}
}

func TestStrayRemoteFree(t *testing.T) {
	th := newTestHeap(t, 16, 64)
	var lock sync.Mutex
	a, b := &cacheThread{owner: 1}, &cacheThread{owner: 2}
	keep := a.alloc(t, th, &lock, 64)
	gone := a.alloc(t, th, &lock, 64)
	page, slot, _, _ := th.ownedSlot(gone)
	a.finish(th)
	// b read a's ownership before a gave the slab up, and frees after.
	if !th.remoteFree(&b.counts, page, slot) {
		t.Fatal("stray free refused")
	}
	b.finish(th)
	th.foldStrayFrees()
	th.verify()
	if th.liveObjects != 1 {
		t.Fatalf("%d live objects, want 1", th.liveObjects)
	}
	th.collect(roots{keep})
	if th.liveObjects != 1 {
		t.Fatal("the kept object was swept")
	}
}

// TestOwnedSlabsRace has threads allocate from their own slabs and free what
// their neighbour allocated, so most frees are remote. Run with -race.
func TestOwnedSlabsRace(t *testing.T) {
	const workers, rounds = 4, 4000
	th := newTestHeap(t, 1024, 256)
	var lock sync.Mutex
	threads := make([]*cacheThread, workers)
	links := make([]chan uintptr, workers)
	for index := range threads {
		threads[index] = &cacheThread{owner: uint32(index + 1)}
		links[index] = make(chan uintptr, 64)
	}
	var group sync.WaitGroup
	for index := range threads {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			c, out, in := threads[index], links[(index+1)%workers], links[index]
			for round := 0; round < rounds; round++ {
				size := classSizes[round%6]
				address := c.alloc(t, th, &lock, size)
				if address == 0 {
					return
				}
				bytesAt(address, size)[0] = byte(index)
				select {
				case out <- address:
				default:
					c.free(t, th, &lock, address)
				}
				select {
				case other := <-in:
					c.free(t, th, &lock, other)
				default:
				}
			}
		}(index)
	}
	group.Wait()
	for index := range threads {
		close(links[index])
		for address := range links[index] {
			threads[0].free(t, th, &lock, address)
		}
	}
	for _, c := range threads {
		c.finish(th)
	}
	th.foldStrayFrees()
	th.verify()
	if th.liveObjects != 0 || th.liveBytes != 0 {
		t.Fatalf("%d objects, %d bytes live after every free", th.liveObjects, th.liveBytes)
	}
	if th.diag.remoteFrees == 0 || th.diag.localAllocs != workers*rounds {
		t.Fatalf("remote frees %d local allocations %d", th.diag.remoteFrees, th.diag.localAllocs)
	}
}
