package arena

import (
	"math/rand"
	"testing"
)

// fillGarbage allocates count pages of dirty garbage in runs and slabs and
// returns what it allocated.
func fillGarbage(t *testing.T, th *testHeap, count int) []uintptr {
	t.Helper()
	var held []uintptr
	for index := 0; index < count; index++ {
		size := uintptr(pageBytes)
		if index%3 == 0 {
			size = 3000
		}
		address := th.mustAlloc(t, size)
		for offset := range bytesAt(address, size) {
			bytesAt(address, size)[offset] = 0x9c
		}
		held = append(held, address)
	}
	return held
}

func TestNoRetentionGivesEveryPageBack(t *testing.T) {
	th := newTestHeap(t, 512, 64)
	fillGarbage(t, th, 100)
	th.beginMark()
	th.finishMark()
	th.sweep()
	th.trimAfterCollection()
	th.verify()
	if th.dirtyFree != 0 {
		t.Fatalf("%d dirty free pages kept with no retention", th.dirtyFree)
	}
}

func TestRetentionKeepsAtMostItsPages(t *testing.T) {
	for _, retain := range []uint32{1, 16, 64, maxRetained} {
		th := newTestHeap(t, 2048, 64)
		th.retain = retain
		for cycle := 0; cycle < 6; cycle++ {
			fillGarbage(t, th, 200+cycle*50)
			th.beginMark()
			th.finishMark()
			th.sweep()
			th.trimAfterCollection()
			th.verify()
			if th.dirtyFree == 0 || th.dirtyFree > uint64(retain) {
				t.Fatalf("retain %d, cycle %d: %d dirty free pages", retain, cycle, th.dirtyFree)
			}
		}
		th.trim()
		th.verify()
		if th.dirtyFree != 0 || th.diag.trims == 0 {
			t.Fatalf("retain %d: trim left %d dirty pages", retain, th.dirtyFree)
		}
	}
}

func TestExplicitFreesKeepAtMostTheRetainedPages(t *testing.T) {
	th := newTestHeap(t, 2048, 64)
	th.retain = 32
	held := fillGarbage(t, th, 600)
	for _, address := range held {
		th.freeAt(address)
		if th.dirtyFree<<pageShift > collectionFloor {
			t.Fatalf("%d dirty free pages after a free", th.dirtyFree)
		}
	}
	th.verify()
	if th.dirtyFree == 0 {
		t.Fatal("explicit frees retained nothing")
	}
}

func TestRefusedTrimKeepsPagesDirtyAndRetries(t *testing.T) {
	th := newTestHeap(t, 512, 64)
	th.retain = 8
	fillGarbage(t, th, 60)
	th.refuse = true
	th.beginMark()
	th.finishMark()
	th.sweep()
	th.trimAfterCollection()
	th.verify()
	dirty := th.dirtyFree
	if dirty < 60 {
		t.Fatalf("a refused discard cleaned pages: %d dirty", dirty)
	}
	// Pages the kernel kept are cleared before they are handed out.
	for index := 0; index < 30; index++ {
		address := th.mustAlloc(t, pageBytes)
		for offset, value := range bytesAt(address, pageBytes) {
			if value != 0 {
				t.Fatalf("a dirty page was handed out uncleared at %d", offset)
			}
		}
		th.freeAt(address)
	}
	th.refuse = false
	th.trim()
	th.verify()
	if th.dirtyFree != 0 {
		t.Fatalf("a retried trim left %d dirty pages", th.dirtyFree)
	}
}

// TestRetentionRandomized interleaves allocation, explicit frees,
// collections, trims, retention changes, and refused discards, and checks
// the metadata, zeroed reuse, and the retention bound throughout.
func TestRetentionRandomized(t *testing.T) {
	th := newTestHeap(t, 4096, 16)
	random := rand.New(rand.NewSource(91))
	live := map[uintptr]uintptr{}
	var order []uintptr
	operations := 40000
	if testing.Short() {
		operations = 4000
	}
	for step := 0; step < operations; step++ {
		switch choice := random.Intn(100); {
		case choice < 50 || len(order) == 0:
			size := uintptr(1 + random.Intn(3*int(pageBytes)))
			address := th.alloc(size)
			if address == 0 {
				continue
			}
			for offset, value := range bytesAt(address, size) {
				if value != 0 {
					t.Fatalf("step %d: byte %d of a fresh allocation is %#x", step, offset, value)
				}
			}
			for offset := range bytesAt(address, size) {
				bytesAt(address, size)[offset] = 0xe1
			}
			live[address] = size
			order = append(order, address)
		case choice < 85:
			index := random.Intn(len(order))
			th.freeAt(order[index])
			delete(live, order[index])
			order[index] = order[len(order)-1]
			order = order[:len(order)-1]
		case choice < 92:
			rootSet := make(roots, 0, len(order))
			kept := order[:0]
			for _, address := range order {
				if random.Intn(3) != 0 {
					rootSet = append(rootSet, address)
					kept = append(kept, address)
				} else {
					delete(live, address)
				}
			}
			order = kept
			th.beginMark()
			rootSet.mark(&th.heap)
			th.finishMark()
			th.sweep()
			th.trimAfterCollection()
			if !th.refuse && th.dirtyFree > uint64(th.retain) {
				t.Fatalf("step %d: %d dirty free pages, retaining %d", step, th.dirtyFree, th.retain)
			}
		case choice < 95:
			th.trim()
			if !th.refuse && th.dirtyFree != 0 {
				t.Fatalf("step %d: trim left %d", step, th.dirtyFree)
			}
		case choice < 98:
			th.retain = []uint32{0, 4, 32, maxRetained}[random.Intn(4)]
		default:
			th.refuse = !th.refuse
		}
		if step%499 == 0 {
			th.verify()
			if uint64(len(live)) != th.liveObjects {
				t.Fatalf("step %d: %d live, model %d", step, th.liveObjects, len(live))
			}
		}
	}
}
