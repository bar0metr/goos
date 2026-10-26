package arena

import (
	"sync"
	"testing"
	"unsafe"
)

// The tests here cover thread caches of several slabs per class, the
// collections that keep them, the goal and retention policies, and the idle
// quorum (ADR 0205).

const bigClass = uint8(classCount - 1) // 2048 bytes, two slots, four slabs

// cached returns the pages c's cache names for class.
func (c *cacheThread) cached(class uint8) []uint32 {
	cc := &c.cache.classes[class]
	pages := make([]uint32, 0, cc.count)
	for index := uint8(0); index < cc.count; index++ {
		pages = append(pages, cc.pages[index]-1)
	}
	return pages
}

// collectKeeping is an automatic collection with every thread held: each
// keeps its slabs, their remote frees are folded before marking, and the
// sweep leaves them owned.
func (th *testHeap) collectKeeping(threads []*cacheThread, r roots) {
	for _, c := range threads {
		th.diag.preserved += th.drainCache(&c.cache, &c.counts)
		th.foldCounts(&c.counts)
	}
	th.foldStrayFrees()
	th.beginMark()
	r.mark(&th.heap)
	th.finishMark()
	th.sweep()
	th.trimAfter(collectAutomatic)
	for _, c := range threads {
		th.checkCache(&c.cache, c.owner)
	}
	th.allowOwned = true
	th.verify()
}

// flush gives every thread's slabs back and folds its counts, as a
// collection other than an automatic one does first.
func (th *testHeap) flush(threads []*cacheThread) {
	for _, c := range threads {
		th.returnCache(&c.cache, &c.counts)
		th.foldCounts(&c.counts)
	}
	th.foldStrayFrees()
}

func TestCacheTargetsFollowSlotsPerPage(t *testing.T) {
	want := map[uintptr]uint32{16: 1, 32: 1, 48: 1, 64: 1, 96: 1, 128: 1, 192: 1, 256: 1, 384: 2, 512: 2, 768: 4, 1024: 4, 1360: 4, 2048: 4}
	for class, size := range classSizes {
		if got := cacheTarget(uint8(class)); got != want[size] {
			t.Errorf("class %d: target %d slabs, want %d", size, got, want[size])
		}
	}
	if bound := cacheBound(); bound != 28 {
		t.Fatalf("a thread can own %d slab pages, want 28", bound)
	}
}

func TestCachedSlabsReuseEveryPosition(t *testing.T) {
	th := newTestHeap(t, 64, 64)
	var lock sync.Mutex
	c := &cacheThread{owner: 1}
	var held []uintptr
	for len(held) < 8 {
		held = append(held, c.alloc(t, th, &lock, 2048))
	}
	pages := c.cached(bigClass)
	if len(pages) != 4 {
		t.Fatalf("one refill took %d slabs, want 4", len(pages))
	}
	if th.cacheAlloc(&c.cache, &c.counts, bigClass, false) != 0 || c.counts.misses == 0 {
		t.Fatal("full slabs handed out a slot, or the miss was not counted")
	}
	// The hint is on the last slab; frees into the first, a middle, and
	// the last slab must all be found, the first by wrapping around.
	if c.cache.classes[bigClass].hint != 3 {
		t.Fatalf("hint %d, want 3", c.cache.classes[bigClass].hint)
	}
	freed := map[uintptr]bool{}
	for _, index := range []int{0, 3, 7} {
		page, slot, owner, ok := th.ownedSlot(held[index])
		if !ok || owner != 1 || !th.cacheFree(&c.counts, page, slot) {
			t.Fatalf("local free of object %d", index)
		}
		freed[held[index]] = true
	}
	refills := c.counts.refills
	for count := len(freed); count > 0; count-- {
		address := th.cacheAlloc(&c.cache, &c.counts, bigClass, false)
		if !freed[address] {
			t.Fatalf("allocation gave %#x, not a freed slot", address)
		}
		delete(freed, address)
	}
	if c.counts.refills != refills || th.cacheAlloc(&c.cache, &c.counts, bigClass, false) != 0 {
		t.Fatal("freed slots needed a refill, or a slot appeared from nowhere")
	}
	// A refill of four full slabs gives them up, full and unlisted, and
	// takes four more.
	c.alloc(t, th, &lock, 2048)
	if got := c.cached(bigClass); len(got) != 4 || got[0] == pages[0] {
		t.Fatalf("refill after full slabs kept %v", got)
	}
	for _, page := range pages {
		if m := &th.meta[page]; m.owner != 0 || m.flags&pageListed != 0 {
			t.Fatalf("given-up full slab %d: owner %d flags %#x", page, m.owner, m.flags)
		}
	}
	if th.diag.disownedFull != 4 || th.cachedPages != 4 {
		t.Fatalf("disowned %d, cached %d", th.diag.disownedFull, th.cachedPages)
	}
	c.finish(th)
	th.verify()
}

func TestBatchRefillTakesListedThenFreeThenFreshPages(t *testing.T) {
	t.Run("listed", func(t *testing.T) {
		th := newTestHeap(t, 64, 64)
		var objects []uintptr
		for index := 0; index < 8; index++ {
			objects = append(objects, th.mustAlloc(t, 2048))
		}
		for index := 0; index < 8; index += 2 {
			th.freeAt(objects[index])
		}
		frontier, slabs := th.frontier, th.diag.newSlabs
		var lock sync.Mutex
		c := &cacheThread{owner: 1}
		c.alloc(t, th, &lock, 2048)
		if th.frontier != frontier || th.diag.newSlabs != slabs || len(c.cached(bigClass)) != 4 || th.partial[bigClass] != noPage {
			t.Fatal("the refill did not take the four listed slabs")
		}
		c.finish(th)
		th.verify()
	})
	t.Run("free runs", func(t *testing.T) {
		th := newTestHeap(t, 64, 64)
		run := th.mustAlloc(t, 4*pageBytes)
		th.mustAlloc(t, pageBytes) // keeps the run from merging into the frontier
		th.freeAt(run)
		frontier, fromRuns := th.frontier, th.diag.pagesFromRuns
		var lock sync.Mutex
		c := &cacheThread{owner: 1}
		c.alloc(t, th, &lock, 2048)
		if th.frontier != frontier || th.diag.pagesFromRuns != fromRuns+4 {
			t.Fatalf("frontier %d->%d, pages from runs %d->%d", frontier, th.frontier, fromRuns, th.diag.pagesFromRuns)
		}
		c.finish(th)
	})
	t.Run("fresh, untouched", func(t *testing.T) {
		th := newTestHeap(t, 64, 64)
		var lock sync.Mutex
		c := &cacheThread{owner: 1}
		c.alloc(t, th, &lock, 2048)
		if th.diag.pagesFromFresh != 4 {
			t.Fatalf("%d fresh pages, want 4", th.diag.pagesFromFresh)
		}
		// Only the slab allocated from holds a slot; the others are as
		// the kernel supplied them, never written.
		for _, page := range c.cached(bigClass)[1:] {
			if m := &th.meta[page]; m.flags&pageDirty != 0 || m.count != 0 || m.used != 0 {
				t.Fatalf("slab %d was touched: flags %#x count %d", page, m.flags, m.count)
			}
		}
		c.finish(th)
	})
	t.Run("short heap", func(t *testing.T) {
		th := newTestHeap(t, 2, 64)
		var lock sync.Mutex
		c := &cacheThread{owner: 1}
		c.alloc(t, th, &lock, 2048)
		if len(c.cached(bigClass)) != 2 {
			t.Fatalf("a two-page heap gave %d slabs", len(c.cached(bigClass)))
		}
		c.finish(th)
		th.verify()
	})
	t.Run("one slab under pressure", func(t *testing.T) {
		th := newTestHeap(t, 64, 64)
		c := &cacheThread{owner: 1}
		if !th.refill(&c.cache, &c.counts, bigClass, 1, 1) || len(c.cached(bigClass)) != 1 {
			t.Fatal("a refill asked for one slab took another count")
		}
		th.flush([]*cacheThread{c})
	})
}

func TestRemoteFreesReachEveryCachedSlab(t *testing.T) {
	th := newTestHeap(t, 64, 64)
	var lock sync.Mutex
	a, b := &cacheThread{owner: 1}, &cacheThread{owner: 2}
	var held []uintptr
	for len(held) < 8 {
		held = append(held, a.alloc(t, th, &lock, 2048))
	}
	for index := 0; index < 8; index += 2 {
		page, slot, owner, ok := th.ownedSlot(held[index])
		if !ok || owner != 1 || !th.remoteFree(&b.counts, page, slot) || th.remoteFree(&b.counts, page, slot) {
			t.Fatalf("remote free into slab %d, or its double-free check", index/2)
		}
	}
	// The fast path does not see remote frees; the refill folds every
	// slab's and takes no new one.
	if th.cacheAlloc(&a.cache, &a.counts, bigClass, false) != 0 {
		t.Fatal("a remote free was visible to the fast path")
	}
	pages := a.cached(bigClass)
	for index := 0; index < 4; index++ {
		a.alloc(t, th, &lock, 2048)
	}
	if got := a.cached(bigClass); len(got) != 4 || got[0] != pages[0] || got[3] != pages[3] {
		t.Fatalf("the refill replaced slabs with room: %v, was %v", got, pages)
	}
	if a.counts.drained+th.diag.drained != 4 {
		t.Fatalf("drained %d remote frees, want 4", a.counts.drained+th.diag.drained)
	}
	// A free that read the owner before the slab was given back lands
	// after: the collection's stray fold takes it.
	page, slot, _, _ := th.ownedSlot(held[1])
	a.finish(th)
	if !th.remoteFree(&b.counts, page, slot) {
		t.Fatal("stray free refused")
	}
	b.finish(th)
	th.foldStrayFrees()
	th.verify()
	if want := uint64(8 - 4 + 4 - 1); th.liveObjects != want {
		t.Fatalf("%d live objects, want %d", th.liveObjects, want)
	}
}

func TestAutomaticCollectionKeepsCaches(t *testing.T) {
	th := newTestHeap(t, 128, 256)
	var lock sync.Mutex
	a, b, idle := &cacheThread{owner: 1}, &cacheThread{owner: 2}, &cacheThread{owner: 3}
	threads := []*cacheThread{a, b, idle}
	var big, small []uintptr
	for len(big) < 8 {
		big = append(big, a.alloc(t, th, &lock, 2048))
	}
	for len(small) < 40 {
		small = append(small, b.alloc(t, th, &lock, 16))
	}
	// The first slot of every slab of a survives, the second does not.
	// b freed one of the dead ones remotely; the collection folds that
	// free before it marks.
	keep := roots{big[0], big[2], big[4], big[6], small[0], small[39]}
	page, slot, _, _ := th.ownedSlot(big[1])
	if !th.remoteFree(&b.counts, page, slot) {
		t.Fatal("remote free refused")
	}
	// idle owns a slab that ends up empty.
	lone := idle.alloc(t, th, &lock, 2048)
	lonePage := uint32((lone - th.base) >> pageShift)
	pages := a.cached(bigClass)
	th.collectKeeping(threads, keep)
	if th.liveObjects != uint64(len(keep)) {
		t.Fatalf("%d live objects after the collection, want %d", th.liveObjects, len(keep))
	}
	if got := a.cached(bigClass); len(got) != 4 || got[0] != pages[0] || got[3] != pages[3] {
		t.Fatalf("a's slabs changed: %v, was %v", got, pages)
	}
	for _, page := range pages {
		if m := &th.meta[page]; m.owner != 1 || m.flags&pageListed != 0 || m.used != 1 {
			t.Fatalf("kept slab %d: owner %d flags %#x used %d", page, m.owner, m.flags, m.used)
		}
	}
	if m := &th.meta[lonePage]; m.kind != pageSlab || m.owner != 3 || m.used != 0 {
		t.Fatalf("an owned slab that emptied left its owner: kind %d owner %d", m.kind, m.owner)
	}
	// The swept slots come back on the fast path, without a refill.
	for index := 0; index < 4; index++ {
		if th.cacheAlloc(&a.cache, &a.counts, bigClass, false) == 0 {
			t.Fatalf("swept slot %d of a's slabs was not reused", index)
		}
	}
	if a.counts.refills != 0 {
		t.Fatal("reusing swept slots took a refill")
	}
	// A collection that takes the slabs back leaves nothing owned.
	th.flush(threads)
	th.allowOwned = false
	th.collect(keep)
	if th.cachedPages != 0 || th.liveObjects != uint64(len(keep)) {
		t.Fatalf("%d cached pages, %d live objects after the flush", th.cachedPages, th.liveObjects)
	}
}

func TestAbandonedCollectionLeavesCachesUsable(t *testing.T) {
	th := newTestHeap(t, 128, 256)
	var lock sync.Mutex
	a, b := &cacheThread{owner: 1}, &cacheThread{owner: 2}
	threads := []*cacheThread{a, b}
	var held roots
	for round := 0; round < 3; round++ {
		for index := 0; index < 12; index++ {
			held = append(held, a.alloc(t, th, &lock, 1024))
		}
		page, slot, _, _ := th.ownedSlot(held[len(held)-1])
		if !th.remoteFree(&b.counts, page, slot) {
			t.Fatal("remote free refused")
		}
		held = held[:len(held)-1]
		// What a collection does before it learns its roots are not
		// all known, then gives up: no mark, no sweep.
		for _, c := range threads {
			th.drainCache(&c.cache, &c.counts)
			th.foldCounts(&c.counts)
		}
		th.foldStrayFrees()
		th.allowOwned = true
		th.verify()
	}
	th.collectKeeping(threads, held)
	if th.liveObjects != uint64(len(held)) {
		t.Fatalf("%d live objects, want %d", th.liveObjects, len(held))
	}
	th.flush(threads)
	th.allowOwned = false
	th.collect(nil)
	if th.liveObjects != 0 || th.inUsePages != 0 {
		t.Fatalf("%d objects and %d pages left", th.liveObjects, th.inUsePages)
	}
}

// TestBatchedSlabsRace is TestOwnedSlabsRace over the classes with several
// slabs per thread, with a collection that keeps the caches between
// rounds. Run with -race.
func TestBatchedSlabsRace(t *testing.T) {
	const workers, rounds, epochs = 4, 800, 4
	th := newTestHeap(t, 2048, 256)
	var lock sync.Mutex
	threads := make([]*cacheThread, workers)
	for index := range threads {
		threads[index] = &cacheThread{owner: uint32(index + 1)}
	}
	sizes := []uintptr{384, 512, 768, 1024, 1360, 2048}
	for epoch := 0; epoch < epochs; epoch++ {
		links := make([]chan uintptr, workers)
		for index := range links {
			links[index] = make(chan uintptr, 64)
		}
		var group sync.WaitGroup
		for index := range threads {
			group.Add(1)
			go func(index int) {
				defer group.Done()
				c, out, in := threads[index], links[(index+1)%workers], links[index]
				for round := 0; round < rounds; round++ {
					size := sizes[(round+index)%len(sizes)]
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
		// Whatever is still in flight stays allocated and unrooted: the
		// collection sweeps it out of the kept slabs.
		th.collectKeeping(threads, nil)
		if th.liveObjects != 0 {
			t.Fatalf("epoch %d: %d objects survived with no roots", epoch, th.liveObjects)
		}
	}
	th.flush(threads)
	th.allowOwned = false
	th.collect(nil)
	if th.diag.remoteFrees == 0 || th.diag.claimed == 0 {
		t.Fatalf("remote frees %d, slabs claimed %d", th.diag.remoteFrees, th.diag.claimed)
	}
}

func TestGoalScalesWithAllocatingThreads(t *testing.T) {
	const mib = uint64(1 << 20)
	cases := []struct {
		name               string
		limit, live        uint64
		allocators         uint32
		floor, nextCollect uint64
	}{
		{"no quota known", 0, 0, 8, mib, mib},
		{"one thread", 64 * mib, 0, 1, mib, mib},
		{"four threads", 64 * mib, 0, 4, 4 * mib, 4 * mib},
		{"eight threads", 64 * mib, 0, 8, 8 * mib, 8 * mib},
		{"capped by a large quota", 64 * mib, 0, 16, 8 * mib, 8 * mib},
		{"capped by a normal quota", 32 * mib, 0, 8, 4 * mib, 4 * mib},
		{"tiny quota keeps the floor", 8 * mib, 0, 8, mib, mib},
		{"live heap dominates", 64 * mib, 20 * mib, 8, 8 * mib, 40 * mib},
		{"floor dominates a small live heap", 64 * mib, 2 * mib, 4, 4 * mib, 6 * mib},
	}
	for _, c := range cases {
		th := newTestHeap(t, 1<<14, 16)
		th.pageLimit, th.liveBytes, th.allocators = c.limit, c.live, c.allocators
		th.goal()
		if th.diag.lastFloor != c.floor || th.nextCollection != c.nextCollect {
			t.Errorf("%s: floor %d goal %d, want %d and %d", c.name, th.diag.lastFloor, th.nextCollection, c.floor, c.nextCollect)
		}
	}
	// The reservation caps the goal.
	th := newTestHeap(t, 64, 16)
	th.pageLimit, th.allocators = 64*mib, 8
	th.goal()
	if th.nextCollection != 64<<pageShift {
		t.Fatalf("goal %d past a 64-page reservation", th.nextCollection)
	}
}

func TestRetentionDependsOnReason(t *testing.T) {
	th := newTestHeap(t, 64, 16)
	for _, c := range []struct {
		allocators uint32
		reason     collectionReason
		want       uint32
	}{
		{1, collectAutomatic, 0}, {2, collectAutomatic, 64}, {4, collectAutomatic, 128},
		{8, collectAutomatic, 256}, {12, collectAutomatic, 256},
		{8, collectExplicit, 0}, {8, collectIdle, 0}, {8, collectPressure, 0},
	} {
		th.allocators = c.allocators
		if got := th.retention(c.reason); got != c.want {
			t.Errorf("%d allocators, reason %d: retain %d, want %d", c.allocators, c.reason, got, c.want)
		}
	}
	// A count SetRetained chose holds for automatic and explicit
	// collections; idle and pressure give everything back.
	th.setRetained(16)
	for reason, want := range []uint32{16, 16, 0, 0} {
		if got := th.retention(collectionReason(reason)); got != want {
			t.Errorf("SetRetained(16), reason %d: retain %d, want %d", reason, got, want)
		}
	}
	th.setRetained(0)
	th.allocators = 8
	if th.retention(collectAutomatic) != 0 {
		t.Fatal("SetRetained(0) did not hold for an automatic collection")
	}
}

func TestIdleCollectionTrimsEveryPage(t *testing.T) {
	th := newTestHeap(t, 512, 64)
	th.allocators = 8
	fillGarbage(t, th, 100)
	th.beginMark()
	th.finishMark()
	th.sweep()
	th.trimAfter(collectAutomatic)
	if th.dirtyFree == 0 || th.dirtyFree > 256 {
		t.Fatalf("an automatic collection with eight threads kept %d pages", th.dirtyFree)
	}
	trims := th.diag.trims
	th.trimAfter(collectIdle)
	if th.dirtyFree != 0 || th.diag.trims != trims+1 {
		t.Fatalf("an idle collection left %d dirty pages", th.dirtyFree)
	}
	th.verify()
}

func TestIdleQuorum(t *testing.T) {
	var states [4]threadAlloc
	at := func(index int) *threadAlloc { return &states[index] }
	states[0].live = true
	if !idleQuorum(0, len(states), at) {
		t.Fatal("a lone thread was refused")
	}
	states[1].live, states[2].live = true, true
	states[1].idle.Store(1)
	if idleQuorum(0, len(states), at) {
		t.Fatal("one idle thread beside a working one made the process idle")
	}
	states[2].idle.Store(1)
	// Slot 3 has no thread; its idle mark does not matter.
	if !idleQuorum(0, len(states), at) {
		t.Fatal("every thread idle, and the quorum failed")
	}
	// A thread that leaves its wait after the first look fails the look
	// a collection takes once it holds the threads.
	states[2].idle.Store(0)
	if idleQuorum(0, len(states), at) {
		t.Fatal("the second look missed a thread that left its wait")
	}
}

func TestFoldRecordsAllocatingThreads(t *testing.T) {
	th := newTestHeap(t, 32, 16)
	var a threadAlloc
	a.fold(&th.heap)
	if a.allocated {
		t.Fatal("a thread that allocated nothing counts as allocating")
	}
	var lock sync.Mutex
	owner := &cacheThread{owner: 1}
	owner.alloc(t, th, &lock, 64)
	a.cache, a.counts = owner.cache, owner.counts
	a.allocFast(&th.heap, th.classOf[4], 64, false)
	a.fold(&th.heap)
	if !a.allocated {
		t.Fatal("a fold of allocations did not mark the thread")
	}
	th.returnCache(&a.cache, &a.counts)
	a.fold(&th.heap)
}

// TestPageRecordSize keeps the page record at 128 bytes: thread caches live
// in the thread descriptor, not in the record.
func TestPageRecordSize(t *testing.T) {
	if size := unsafe.Sizeof(pageMeta{}); size != 128 {
		t.Fatalf("pageMeta is %d bytes", size)
	}
}
