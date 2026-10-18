package arena

import (
	"math/rand"
	"testing"
	"unsafe"
)

// testHeap is a heap over memory the host test allocated. Discarding zeroes
// the range, as the kernel's demand-zero pages would read.
type testHeap struct {
	heap
	memory    []byte
	metadata  []pageMeta
	stack     []uintptr
	discarded uint64
	refuse    bool
}

func newTestHeap(t testing.TB, pages uint32, work int) *testHeap {
	t.Helper()
	th := &testHeap{memory: make([]byte, (int(pages)+1)<<pageShift), metadata: make([]pageMeta, pages), stack: make([]uintptr, work)}
	base := (uintptr(unsafe.Pointer(&th.memory[0])) + pageBytes - 1) &^ (pageBytes - 1)
	th.init(base, pages, th.metadata, th.stack, th.discardRange)
	return th
}

func (th *testHeap) discardRange(address, length uintptr) bool {
	if th.refuse {
		return false
	}
	if address&(pageBytes-1) != 0 || length&(pageBytes-1) != 0 || !th.contains(address) || !th.contains(address+length-1) {
		panic("discard outside the heap or unaligned")
	}
	clearMemory(address, length)
	th.discarded += uint64(length)
	return true
}

func (th *testHeap) mustAlloc(t testing.TB, size uintptr) uintptr {
	t.Helper()
	address := th.alloc(size)
	if address == 0 {
		t.Fatalf("alloc %d failed", size)
	}
	return address
}

func bytesAt(address, length uintptr) []byte {
	return unsafe.Slice((*byte)(pointerAt(address)), length)
}

func wordAt(address uintptr) *uintptr { return (*uintptr)(pointerAt(address)) }

// roots is a root range held in host memory.
type roots []uintptr

func (r roots) mark(h *heap) {
	if len(r) == 0 {
		return
	}
	start := uintptr(unsafe.Pointer(&r[0]))
	h.markRange(start, start+uintptr(len(r))*wordBytes)
}

func (th *testHeap) collect(r roots) {
	th.beginMark()
	r.mark(&th.heap)
	th.finishMark()
	th.sweep()
	th.releaseFree()
	th.verify()
}

// verify checks every invariant the metadata keeps.
func (th *testHeap) verify() {
	h := &th.heap
	var inUse, dirtyFree, objects, bytes uint64
	listedRuns := map[uint32]bool{}
	for bucket := range h.runs {
		previous := noPage
		for page := h.runs[bucket]; page != noPage; page = h.meta[page].next {
			m := &h.meta[page]
			if m.kind != pageFree || m.head != page || runBucket(m.count) != bucket || m.prev != previous || listedRuns[page] {
				panic("free run list is inconsistent")
			}
			last := &h.meta[page+m.count-1]
			if last.kind != pageFree || last.head != page {
				panic("free run tail is inconsistent")
			}
			listedRuns[page] = true
			previous = page
		}
	}
	listedSlabs := map[uint32]bool{}
	for class := range h.partial {
		previous := noPage
		for page := h.partial[class]; page != noPage; page = h.meta[page].next {
			m := &h.meta[page]
			if m.kind != pageSlab || int(m.class) != class || m.prev != previous || m.flags&pageListed == 0 || uint32(m.used) >= slotsPerPage(m.class) {
				panic("slab list is inconsistent")
			}
			listedSlabs[page] = true
			previous = page
		}
	}
	previousFree := false
	for page := uint32(0); page < h.frontier; {
		m := &h.meta[page]
		switch m.kind {
		case pageFree:
			if previousFree || !listedRuns[page] {
				panic("free runs are not coalesced or not listed")
			}
			for index := page; index < page+m.count; index++ {
				if h.meta[index].kind != pageFree {
					panic("free run holds a used page")
				}
				if h.meta[index].flags&pageDirty != 0 {
					dirtyFree++
				}
			}
			previousFree = true
			page += m.count
			continue
		case pageSlab:
			used := 0
			for word := 0; word < slotWords; word++ {
				for bits := m.alloc[word]; bits != 0; bits &= bits - 1 {
					used++
				}
				if m.mark[word] != 0 {
					panic("mark bit left after a collection")
				}
			}
			if used != int(m.used) || (used == 0 && !listedSlabs[page]) || listedSlabs[page] != (uint32(used) < slotsPerPage(m.class)) {
				panic("slab bookkeeping is inconsistent")
			}
			objects += uint64(used)
			bytes += uint64(used) * uint64(classSizes[m.class])
			inUse++
			page++
		case pageRun:
			for index := page + 1; index < page+m.count; index++ {
				if h.meta[index].kind != pageRunTail || h.meta[index].head != page {
					panic("run tail is inconsistent")
				}
			}
			objects++
			bytes += uint64(m.count) << pageShift
			inUse += uint64(m.count)
			page += m.count
		default:
			panic("unexpected page kind")
		}
		previousFree = false
	}
	if inUse != h.inUsePages || dirtyFree != h.dirtyFree || objects != h.liveObjects || bytes != h.liveBytes {
		panic("heap counters disagree with metadata")
	}
}

func expectPanicText(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		got := recover()
		if got == nil {
			t.Fatalf("expected panic %q", want)
		}
		if text, ok := got.(string); !ok || text != want {
			t.Fatalf("panic %v, want %q", got, want)
		}
	}()
	fn()
}

func TestSizeClassesCoverEverySmallSize(t *testing.T) {
	for size := uintptr(1); size <= maxSmallSize; size++ {
		class := sizeClass(size)
		if classSizes[class] < size || (class > 0 && classSizes[class-1] >= size) {
			t.Fatalf("size %d got class %d", size, class)
		}
		if classSizes[class]%16 != 0 {
			t.Fatalf("class %d is not 16-byte aligned", class)
		}
	}
	if slotsPerPage(0) != 256 || 256 > slotWords*64 {
		t.Fatal("bitmap does not cover the smallest class")
	}
}

func TestClassTablesMatchTheSearch(t *testing.T) {
	th := newTestHeap(t, 16, 16)
	for size := uintptr(1); size <= maxSmallSize; size++ {
		if int(th.classOf[(size+15)>>4]) != sizeClass(size) {
			t.Fatalf("size %d: table class %d, search %d", size, th.classOf[(size+15)>>4], sizeClass(size))
		}
	}
	for class := range classSizes {
		if uint32(th.classSlots[class]) != slotsPerPage(uint8(class)) {
			t.Fatalf("class %d slots", class)
		}
		size := uint64(classSizes[class])
		for offset := uint64(0); offset < uint64(pageBytes); offset++ {
			if offset*uint64(th.classMagic[class])>>32 != offset/size {
				t.Fatalf("class %d: offset %d divides wrong", class, offset)
			}
		}
	}
}

// TestEverySlotOfEveryClassResolves allocates every slot of a slab of every
// class and resolves its first byte, its last, and the byte after it.
func TestEverySlotOfEveryClassResolves(t *testing.T) {
	th := newTestHeap(t, 256, 16)
	for class, size := range classSizes {
		slots := slotsPerPage(uint8(class))
		for slot := uint32(0); slot < slots; slot++ {
			address := th.mustAlloc(t, size)
			for _, probe := range []uintptr{address, address + size/2, address + size - 1} {
				page, found, start, ok := th.find(probe)
				if !ok || start != address || found == noPage || th.extent(page, found) != size {
					t.Fatalf("class %d slot %d: %#x resolved to %#x %v", size, slot, probe, start, ok)
				}
			}
		}
		// The page tail past the last slot belongs to no allocation.
		last := th.base + uintptr(th.frontier-1)<<pageShift + uintptr(slots)*size
		if last&(pageBytes-1) != 0 {
			if _, _, _, ok := th.find(last); ok {
				t.Fatalf("class %d: the page tail resolved", size)
			}
		}
	}
	th.verify()
}

// TestReusedSlotsReadZero writes every byte of a slab's slots, frees some by
// hand and the rest by a collection, and requires every slot handed out
// again, and every slot of a fresh slab on a reused page, to read zero.
func TestReusedSlotsReadZero(t *testing.T) {
	for class, size := range classSizes {
		th := newTestHeap(t, 16, 16)
		slots := slotsPerPage(uint8(class))
		var held []uintptr
		for slot := uint32(0); slot < slots; slot++ {
			address := th.mustAlloc(t, size)
			for index := range bytesAt(address, size) {
				bytesAt(address, size)[index] = 0xc3
			}
			held = append(held, address)
		}
		th.free(held[0])
		th.free(held[slots-1])
		th.collect(roots{held[1]})
		// Every slot but held[1] is free again, and its page dirty.
		for slot := uint32(1); slot < slots; slot++ {
			address := th.mustAlloc(t, size)
			for index, value := range bytesAt(address, size) {
				if value != 0 {
					t.Fatalf("class %d: reused slot byte %d is %#x", size, index, value)
				}
			}
		}
		// A slab set up on a dirty page the kernel refused to discard
		// clears every slot it hands out.
		th.refuse = true
		big := th.mustAlloc(t, 4*pageBytes)
		for index := range bytesAt(big, 4*pageBytes) {
			bytesAt(big, 4*pageBytes)[index] = 0x5a
		}
		th.free(big)
		th.collect(nil)
		for slot := uint32(0); slot < 2*slots; slot++ {
			address := th.mustAlloc(t, size)
			for index, value := range bytesAt(address, size) {
				if value != 0 {
					t.Fatalf("class %d: slot %d on a dirty page byte %d is %#x", size, slot, index, value)
				}
			}
		}
		th.verify()
	}
}

func TestBoundarySizesAreAlignedZeroedAndDisjoint(t *testing.T) {
	th := newTestHeap(t, 1024, 64)
	page := uintptr(pageBytes)
	sizes := []uintptr{1, 15, 16, 17, 31, 32, 33, 64, 128, 512, 2048, 2049, page - 1, page, page + 1, 64 << 10, 1 << 20}
	type span struct{ start, end uintptr }
	var spans []span
	for round := 0; round < 2; round++ {
		for _, size := range sizes {
			address := th.mustAlloc(t, size)
			if address%16 != 0 {
				t.Fatalf("size %d at unaligned %#x", size, address)
			}
			for index, value := range bytesAt(address, size) {
				if value != 0 {
					t.Fatalf("size %d byte %d not zero", size, index)
				}
			}
			page, slot, start, ok := th.find(address + size - 1)
			if !ok || start != address || th.extent(page, slot) < size {
				t.Fatalf("size %d does not own its last byte", size)
			}
			for _, other := range spans {
				if address < other.end && other.start < address+size {
					t.Fatalf("size %d overlaps %#x-%#x", size, other.start, other.end)
				}
			}
			spans = append(spans, span{address, address + size})
			for index := range bytesAt(address, size) {
				bytesAt(address, size)[index] = 0xa5
			}
		}
	}
	th.verify()
}

func TestInteriorPointersResolveToTheirAllocation(t *testing.T) {
	th := newTestHeap(t, 64, 16)
	small := th.mustAlloc(t, 40)
	large := th.mustAlloc(t, 3*pageBytes)
	for _, probe := range []struct{ address, start uintptr }{
		{small, small}, {small + 1, small}, {small + 47, small},
		{large, large}, {large + pageBytes, large}, {large + 3*pageBytes - 1, large},
	} {
		if _, _, start, ok := th.find(probe.address); !ok || start != probe.start {
			t.Fatalf("%#x resolved to %#x %v", probe.address, start, ok)
		}
	}
	for _, miss := range []uintptr{small + 48, large + 3*pageBytes, th.base - 1, th.base + 64<<pageShift} {
		if _, _, _, ok := th.find(miss); ok {
			t.Fatalf("%#x resolved to an allocation", miss)
		}
	}
}

func TestFreeReusesSlotsAndPages(t *testing.T) {
	th := newTestHeap(t, 64, 16)
	first := th.mustAlloc(t, 100)
	bytesAt(first, 100)[5] = 7
	th.free(first)
	again := th.mustAlloc(t, 100)
	if again != first || bytesAt(again, 128)[5] != 0 {
		t.Fatal("freed slot was not reused zeroed")
	}
	run := th.mustAlloc(t, 5*pageBytes)
	bytesAt(run, pageBytes)[0] = 1
	th.free(run)
	reused := th.mustAlloc(t, 5*pageBytes)
	if reused != run || bytesAt(reused, pageBytes)[0] != 0 {
		t.Fatal("freed run was not reused zeroed")
	}
	th.verify()
	if th.liveObjects != 2 || th.frees != 2 || th.mallocs != 4 {
		t.Fatalf("counters %d %d %d", th.liveObjects, th.frees, th.mallocs)
	}
}

func TestFreeRejectsInvalidPointers(t *testing.T) {
	th := newTestHeap(t, 64, 16)
	small := th.mustAlloc(t, 32)
	neighbour := th.mustAlloc(t, 32)
	large := th.mustAlloc(t, 2*pageBytes)
	expectPanicText(t, "arena: double free or free of a non-allocation", func() { th.free(small + 16) })
	expectPanicText(t, "arena: free of a non-allocation", func() { th.free(large + pageBytes) })
	expectPanicText(t, "arena: free of a non-allocation", func() { th.free(th.base + 40<<pageShift) })
	th.free(small)
	expectPanicText(t, "arena: double free or free of a non-allocation", func() { th.free(small) })
	th.free(large)
	expectPanicText(t, "arena: free of a non-allocation", func() { th.free(large) })
	// The emptied slab stays as its class's cache, so a second free is
	// still a double free in a slab.
	th.free(neighbour)
	expectPanicText(t, "arena: double free or free of a non-allocation", func() { th.free(neighbour) })
}

func TestRunsSplitAndMerge(t *testing.T) {
	th := newTestHeap(t, 64, 16)
	a := th.mustAlloc(t, 3*pageBytes)
	b := th.mustAlloc(t, 4*pageBytes)
	c := th.mustAlloc(t, 5*pageBytes)
	fence := th.mustAlloc(t, pageBytes+1)
	th.free(b)
	th.verify()
	th.free(a)
	th.verify()
	th.free(c)
	th.verify()
	if th.runs[runBucket(12)] != 0 || th.meta[0].count != 12 {
		t.Fatal("three neighbouring runs did not merge")
	}
	if split := th.mustAlloc(t, 2*pageBytes); split != a || th.meta[2].kind != pageFree || th.meta[2].count != 10 {
		t.Fatal("a merged run was not split from its front")
	}
	th.free(fence)
	th.verify()
}

func TestExhaustionReturnsZero(t *testing.T) {
	th := newTestHeap(t, 8, 16)
	if th.alloc(9*pageBytes) != 0 || th.alloc(^uintptr(0)) != 0 || th.alloc(^uintptr(0)-pageBytes+2) != 0 {
		t.Fatal("an allocation larger than the heap succeeded")
	}
	th.mustAlloc(t, 8*pageBytes)
	if th.alloc(16) != 0 {
		t.Fatal("allocation succeeded with every page in use")
	}
	th.verify()
}

func TestCorruptMetadataIsRejected(t *testing.T) {
	th := newTestHeap(t, 16, 16)
	run := th.mustAlloc(t, 3*pageBytes)
	th.meta[2].head = 5
	expectPanicText(t, "arena: corrupt run metadata", func() { th.find(run + 2*pageBytes) })
	th.meta[2].head = 0

	slab := th.mustAlloc(t, 16)
	page := uint32((slab - th.base) >> pageShift)
	th.meta[page].used = 9
	th.beginMark()
	expectPanicText(t, "arena: slab count does not match its bitmap", th.sweep)
	th.meta[page].used = 1

	th.meta[page].mark[1] = 1
	expectPanicText(t, "arena: mark on a free slot", th.sweep)
	th.meta[page].mark[1] = 0

	th.free(slab)
	free := th.frontier - 1
	th.meta[free].head = free + 3
	expectPanicText(t, "arena: corrupt free run before page", func() {
		th.meta[free].kind = pageFree
		th.givePages(th.frontier, 0)
		th.free(th.base + uintptr(free+1)<<pageShift)
	})
}

func TestCollectKeepsReachableAndSweepsTheRest(t *testing.T) {
	th := newTestHeap(t, 256, 64)
	kept := th.mustAlloc(t, 64)
	child := th.mustAlloc(t, 5000)
	*wordAt(kept + 8) = child + 100 // interior pointer to child
	bytesAt(child, 5000)[4999] = 9
	for index := 0; index < 100; index++ {
		th.mustAlloc(t, uintptr(16+index*37))
	}
	r := roots{kept + 32} // interior pointer to kept
	th.collect(r)
	if th.liveObjects != 2 || bytesAt(child, 5000)[4999] != 9 {
		t.Fatalf("live %d after collection", th.liveObjects)
	}
	r[0] = 0
	th.collect(r)
	if th.liveObjects != 0 || th.liveBytes != 0 || th.inUsePages != 0 || th.dirtyFree != 0 {
		t.Fatalf("garbage left: %+v", th.stats())
	}
	if th.collections != 2 || th.released == 0 {
		t.Fatal("collection was not counted or released nothing")
	}
}

func TestPointerFreeObjectsAreKeptButNotRead(t *testing.T) {
	th := newTestHeap(t, 64, 16)
	small := th.allocObject(64, true)
	large := th.allocObject(3*pageBytes, true)
	target := th.mustAlloc(t, 32)
	// Pointer-looking words in pointer-free objects keep nothing alive.
	*wordAt(small) = target
	*wordAt(large + pageBytes) = target
	th.collect(roots{small, large + 5})
	if _, _, _, ok := th.find(target); ok {
		t.Fatal("a pointer-free object kept its referent alive")
	}
	if _, _, start, ok := th.find(small); !ok || start != small {
		t.Fatal("a rooted pointer-free slot was swept")
	}
	if _, _, start, ok := th.find(large); !ok || start != large {
		t.Fatal("a rooted pointer-free run was swept")
	}
	// A slot reused by a scanned object is scanned again.
	th.free(small)
	scanned := th.mustAlloc(t, 64)
	target = th.mustAlloc(t, 32)
	*wordAt(scanned) = target
	th.collect(roots{scanned, large})
	if _, _, _, ok := th.find(target); !ok || scanned != small {
		t.Fatal("a reused slot stayed pointer-free")
	}
}

func TestCollectReclaimsCycles(t *testing.T) {
	th := newTestHeap(t, 64, 16)
	a, b, c := th.mustAlloc(t, 32), th.mustAlloc(t, 600), th.mustAlloc(t, 3*pageBytes)
	*wordAt(a) = b
	*wordAt(b) = c
	*wordAt(c + pageBytes) = a
	r := roots{a}
	th.collect(r)
	if th.liveObjects != 3 {
		t.Fatal("rooted cycle was swept")
	}
	r[0] = 0
	th.collect(r)
	if th.liveObjects != 0 {
		t.Fatal("unreachable cycle survived")
	}
}

func TestMarkStackOverflowRescans(t *testing.T) {
	th := newTestHeap(t, 2048, 2)
	// A wide tree: one node points to many, each to a chain.
	fanout := 200
	rootNode := th.mustAlloc(t, uintptr(fanout)*wordBytes)
	for index := 0; index < fanout; index++ {
		node := th.mustAlloc(t, 32)
		*wordAt(rootNode + uintptr(index)*wordBytes) = node
		for depth := 0; depth < 20; depth++ {
			next := th.mustAlloc(t, 48)
			*wordAt(node) = next
			node = next
		}
	}
	garbage := 500
	for index := 0; index < garbage; index++ {
		th.mustAlloc(t, 64)
	}
	live := th.liveObjects - uint64(garbage)
	th.collect(roots{rootNode})
	if th.liveObjects != live || th.workHigh > 2 {
		t.Fatalf("live %d want %d, stack high %d", th.liveObjects, live, th.workHigh)
	}
}

func TestDeepListNeedsNoRecursion(t *testing.T) {
	th := newTestHeap(t, 1<<14, 4)
	head := th.mustAlloc(t, 16)
	node := head
	for index := 0; index < 200000; index++ {
		next := th.mustAlloc(t, 16)
		*wordAt(node) = next
		node = next
	}
	th.collect(roots{head})
	if th.liveObjects != 200001 {
		t.Fatalf("deep list lost nodes: %d", th.liveObjects)
	}
}

func TestReleasedPagesReadZeroAndRefusalKeepsThemDirty(t *testing.T) {
	th := newTestHeap(t, 64, 16)
	big := th.mustAlloc(t, 16*pageBytes)
	for index := range bytesAt(big, 16*pageBytes) {
		bytesAt(big, 16*pageBytes)[index] = 0xff
	}
	th.refuse = true
	th.collect(nil)
	if th.dirtyFree != 16 || th.released != 0 {
		t.Fatal("a refused discard was counted")
	}
	th.refuse = false
	th.releaseFree()
	if th.dirtyFree != 0 || th.released != 16<<pageShift || th.discarded != 16<<pageShift {
		t.Fatal("dirty pages were not released")
	}
	again := th.mustAlloc(t, 16*pageBytes)
	for _, value := range bytesAt(again, 16*pageBytes) {
		if value != 0 {
			t.Fatal("reused page not zero")
		}
	}
	stats := th.stats()
	if stats.Held != 16<<pageShift || stats.InUse != 16<<pageShift || stats.Idle != stats.Reserved-stats.InUse {
		t.Fatalf("stats %+v", stats)
	}
}

func TestNextCollectionThreshold(t *testing.T) {
	th := newTestHeap(t, 2048, 16)
	if th.nextCollection != collectionFloor {
		t.Fatal("fresh heap threshold")
	}
	keep := th.mustAlloc(t, 3<<20)
	th.collect(roots{keep})
	if th.nextCollection != 6<<20 {
		t.Fatalf("threshold %d", th.nextCollection)
	}
	th.collect(nil)
	if th.nextCollection != collectionFloor {
		t.Fatalf("threshold %d", th.nextCollection)
	}
}

// TestRandomizedAgainstModel runs mixed allocations, frees, and collections
// against a reference model of what is live.
func TestRandomizedAgainstModel(t *testing.T) {
	th := newTestHeap(t, 4096, 8)
	random := rand.New(rand.NewSource(28))
	type object struct {
		size uintptr
		fill byte
	}
	live := map[uintptr]object{}
	var order []uintptr
	rootSet := make(roots, 0, 1<<16)
	sizes := []uintptr{1, 8, 16, 17, 48, 100, 255, 700, 1500, 2048, 2049, 4096, 4097, 9000, 33000, 120000}
	check := func(address uintptr, o object) {
		for index, value := range bytesAt(address, o.size) {
			if value != o.fill {
				t.Fatalf("object %#x byte %d changed", address, index)
			}
		}
	}
	operations := 60000
	if testing.Short() {
		operations = 6000
	}
	for step := 0; step < operations; step++ {
		switch choice := random.Intn(100); {
		case choice < 55 || len(order) == 0:
			size := sizes[random.Intn(len(sizes))]
			if random.Intn(4) == 0 {
				size = uintptr(1 + random.Intn(3000))
			}
			address := th.alloc(size)
			if address == 0 {
				continue
			}
			for index, value := range bytesAt(address, size) {
				if value != 0 {
					t.Fatalf("step %d: fresh byte %d not zero", step, index)
				}
			}
			for other, o := range live {
				if address < other+o.size && other < address+size {
					t.Fatalf("step %d: %#x+%d overlaps %#x+%d", step, address, size, other, o.size)
				}
			}
			o := object{size: size, fill: byte(1 + random.Intn(254))}
			for index := range bytesAt(address, size) {
				bytesAt(address, size)[index] = o.fill
			}
			live[address] = o
			order = append(order, address)
		case choice < 90:
			index := random.Intn(len(order))
			address := order[index]
			check(address, live[address])
			th.free(address)
			delete(live, address)
			order[index] = order[len(order)-1]
			order = order[:len(order)-1]
		default:
			// Keep a random half rooted; the rest becomes garbage. Root
			// words are host memory, so object fill bytes cannot alias
			// them, and the model stays exact: no allocation holds a
			// pointer.
			rootSet = rootSet[:0]
			kept := order[:0]
			for _, address := range order {
				if random.Intn(2) == 0 {
					rootSet = append(rootSet, address+live[address].size-1)
					kept = append(kept, address)
				} else {
					delete(live, address)
				}
			}
			order = kept
			th.collect(rootSet)
			if th.liveObjects != uint64(len(live)) {
				t.Fatalf("step %d: %d live, model %d", step, th.liveObjects, len(live))
			}
			for address, o := range live {
				check(address, o)
			}
		}
		if step%997 == 0 {
			th.verify()
		}
	}
	th.collect(nil)
	if th.liveObjects != 0 || th.inUsePages != 0 {
		t.Fatalf("heap not empty: %+v", th.stats())
	}
}

func BenchmarkSmallAllocate(b *testing.B) {
	th := newTestHeap(b, 1<<16, 1024)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if th.alloc(32) == 0 {
			b.StopTimer()
			th.collect(nil)
			b.StartTimer()
		}
	}
}

func BenchmarkSmallAllocateFree(b *testing.B) {
	th := newTestHeap(b, 1024, 64)
	for index := 0; index < b.N; index++ {
		th.free(th.alloc(48))
	}
}

func BenchmarkMixedSizes(b *testing.B) {
	th := newTestHeap(b, 1<<16, 1024)
	sizes := []uintptr{16, 40, 100, 300, 900, 2000, 5000, 20000}
	var held [64]uintptr
	for index := 0; index < b.N; index++ {
		slot := index % len(held)
		if held[slot] != 0 {
			th.free(held[slot])
		}
		held[slot] = th.alloc(sizes[index%len(sizes)])
	}
}

func benchmarkCollect(b *testing.B, keepEvery int) {
	th := newTestHeap(b, 1<<14, 4096)
	r := make(roots, 0, 20000)
	for index := 0; index < 20000; index++ {
		address := th.alloc(64)
		if index%keepEvery == 0 {
			r = append(r, address)
		}
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		th.beginMark()
		r.mark(&th.heap)
		th.finishMark()
		th.sweep()
	}
}

func BenchmarkCollectMostlyLive(b *testing.B) { benchmarkCollect(b, 1) }
func BenchmarkCollectMostlyDead(b *testing.B) { benchmarkCollect(b, 50) }
