package arena

import (
	"math/rand"
	"strconv"
	"testing"
)

// The benchmarks in this file measure the allocator on workloads shaped like
// the system's. They use only what every revision of the heap since ADR 0201
// has, plus freeAt and trimAfterCollection, so tools/allocreport can run the
// same file against an earlier revision and compare (docs/development/testing.md).
//
// Each one reports, besides ns/op, the pages the heap held at its peak
// (peak-pages), the pages it held at the end (held-pages), and the discards
// per thousand operations (discards/kop): the work of giving pages back and
// taking them again.

// benchHeap is a test heap whose discards are counted.
type benchHeap struct {
	*testHeap
	discards uint64
	rootSet  roots
}

func newBenchHeap(b *testing.B, pages uint32) *benchHeap {
	bh := &benchHeap{testHeap: newTestHeap(b, pages, 4096)}
	bh.heap.discard = func(address, length uintptr) bool {
		bh.discards++
		return bh.discardRange(address, length)
	}
	return bh
}

// collect is a collection as the runtime runs one, without the invariant
// walk the tests add.
func (bh *benchHeap) collect() {
	bh.beginMark()
	bh.rootSet.mark(&bh.heap)
	bh.finishMark()
	bh.sweep()
	bh.trimAfterCollection()
}

// alloc is runtime.alloc: it collects at the threshold and when the heap is
// full. Whatever a benchmark keeps must be in rootSet, or a collection frees
// it.
func (bh *benchHeap) alloc(b *testing.B, size uintptr, pointerFree bool) uintptr {
	if bh.liveBytes >= bh.nextCollection {
		bh.collect()
	}
	address := bh.allocObject(size, pointerFree)
	if address == 0 {
		bh.collect()
		address = bh.allocObject(size, pointerFree)
	}
	if address == 0 {
		b.Fatalf("heap exhausted at %d bytes", size)
	}
	return address
}

func (bh *benchHeap) report(b *testing.B) {
	b.ReportMetric(float64(bh.peakHeld>>pageShift), "peak-pages")
	b.ReportMetric(float64(bh.inUsePages+bh.dirtyFree), "held-pages")
	b.ReportMetric(float64(bh.discards)*1000/float64(b.N), "discards/kop")
}

// BenchmarkBoundary keeps 512 objects of one size live, replacing the oldest
// each operation: sizes at and just past a class boundary.
func BenchmarkBoundary(b *testing.B) {
	for _, size := range []uintptr{16, 17, 256, 257, 336, 337, 384, 385, 400, 401, 512, 513, 768, 769, 816, 817, 1024, 1025, 1360, 1361, 2048, 2049} {
		b.Run("size="+strconv.Itoa(int(size)), func(b *testing.B) {
			bh := newBenchHeap(b, 1<<14)
			bh.rootSet = make(roots, 512)
			ring := bh.rootSet
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				slot := index % len(ring)
				if ring[slot] != 0 {
					bh.freeAt(ring[slot])
				}
				ring[slot] = bh.alloc(b, size, false)
			}
			b.StopTimer()
			bh.report(b)
		})
	}
}

// BenchmarkShortLivedGarbage allocates small objects that are never freed
// and keeps one in sixteen alive for a while in a 256-entry root ring, so
// collections run at the allocation threshold over a small live set.
func BenchmarkShortLivedGarbage(b *testing.B) {
	bh := newBenchHeap(b, 1<<14)
	bh.rootSet = make(roots, 256)
	sizes := []uintptr{16, 24, 32, 48, 64, 96, 128, 200}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		address := bh.alloc(b, sizes[index%len(sizes)], index%3 == 0)
		if index%16 == 0 {
			bh.rootSet[(index/16)%len(bh.rootSet)] = address
		}
	}
	b.StopTimer()
	bh.report(b)
}

// BenchmarkLongAndShortLived interleaves objects that stay with garbage of
// the same sizes, which leaves slabs partly full when the garbage goes.
func BenchmarkLongAndShortLived(b *testing.B) {
	bh := newBenchHeap(b, 1<<15)
	bh.rootSet = make(roots, 0, 1<<16)
	sizes := []uintptr{32, 80, 300, 700, 1500}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		address := bh.alloc(b, sizes[index%len(sizes)], false)
		if index%13 == 0 && len(bh.rootSet) < cap(bh.rootSet) {
			bh.rootSet = append(bh.rootSet, address)
		}
	}
	b.StopTimer()
	bh.report(b)
}

// BenchmarkLargeRuns frees and allocates page runs of several lengths with
// small allocations between them, so runs split and merge.
func BenchmarkLargeRuns(b *testing.B) {
	bh := newBenchHeap(b, 1<<15)
	sizes := []uintptr{5000, 9000, 20000, 70000, 300000, 12000}
	bh.rootSet = make(roots, 96)
	ring, small := bh.rootSet[:32], bh.rootSet[32:]
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		slot := index % len(ring)
		if ring[slot] != 0 {
			bh.freeAt(ring[slot])
		}
		ring[slot] = bh.alloc(b, sizes[index%len(sizes)], index%2 == 0)
		smallSlot := index % len(small)
		if small[smallSlot] != 0 {
			bh.freeAt(small[smallSlot])
		}
		small[smallSlot] = bh.alloc(b, uintptr(16+index%500), false)
	}
	b.StopTimer()
	bh.report(b)
}

// BenchmarkFreeReallocate allocates a batch of mixed sizes, frees all of it,
// and does it again: the cycle of a request that builds and drops its state.
func BenchmarkFreeReallocate(b *testing.B) {
	bh := newBenchHeap(b, 1<<15)
	sizes := []uintptr{40, 100, 600, 1800, 4096, 9000}
	bh.rootSet = make(roots, 0, 600)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		bh.rootSet = append(bh.rootSet, bh.alloc(b, sizes[index%len(sizes)], index%2 == 0))
		if len(bh.rootSet) == cap(bh.rootSet) {
			for _, address := range bh.rootSet {
				bh.freeAt(address)
			}
			bh.rootSet = bh.rootSet[:0]
		}
	}
	b.StopTimer()
	bh.report(b)
}

// representativeSizes is the mix of request sizes the VFS service made in
// the guest (build/reports/allocator-guest-baseline.txt, 509264 requests):
// each entry is a size and its share per thousand requests. The service made
// one page-run request in that time, so the mix has none.
var representativeSizes = []struct {
	size   uintptr
	weight int
}{
	{14, 21}, {22, 1}, {328, 293}, {504, 48}, {1024, 48}, {1026, 325}, {1048, 20}, {1104, 145}, {1120, 87}, {1152, 13},
}

// BenchmarkRepresentative draws sizes from the measured mix with mixed
// lifetimes: most objects die young, some live through several collections,
// a few are freed explicitly.
func BenchmarkRepresentative(b *testing.B) {
	bh := newBenchHeap(b, 1<<15)
	bh.rootSet = make(roots, 1024+16)
	kept, freed := bh.rootSet[:1024], bh.rootSet[1024:]
	random := rand.New(rand.NewSource(7))
	total := 0
	for _, entry := range representativeSizes {
		total += entry.weight
	}
	draws := make([]uintptr, 4096)
	for index := range draws {
		pick := random.Intn(total)
		for _, entry := range representativeSizes {
			if pick < entry.weight {
				draws[index] = entry.size
				break
			}
			pick -= entry.weight
		}
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		size := draws[index%len(draws)]
		address := bh.alloc(b, size, size%3 == 0)
		switch {
		case index%7 == 0:
			kept[(index/7)%len(kept)] = address
		case index%29 == 0:
			slot := (index / 29) % len(freed)
			if freed[slot] != 0 {
				bh.freeAt(freed[slot])
			}
			freed[slot] = address
		}
	}
	b.StopTimer()
	bh.report(b)
}

// benchmarkCollectObjects collects a heap of 20000 objects of 64 and 600
// bytes with one in keepEvery rooted, pointer-free or not.
func benchmarkCollectObjects(b *testing.B, keepEvery int, pointerFree bool) {
	bh := newBenchHeap(b, 1<<14)
	bh.rootSet = make(roots, 0, 20000)
	for index := 0; index < 20000; index++ {
		size := uintptr(64)
		if index%5 == 0 {
			size = 600
		}
		address := bh.allocObject(size, pointerFree)
		if index%keepEvery == 0 {
			bh.rootSet = append(bh.rootSet, address)
		}
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		bh.beginMark()
		bh.rootSet.mark(&bh.heap)
		bh.finishMark()
		bh.sweep()
	}
}

func BenchmarkCollectScannedMostlyLive(b *testing.B)     { benchmarkCollectObjects(b, 1, false) }
func BenchmarkCollectScannedMostlyDead(b *testing.B)     { benchmarkCollectObjects(b, 50, false) }
func BenchmarkCollectPointerFreeMostlyLive(b *testing.B) { benchmarkCollectObjects(b, 1, true) }
func BenchmarkCollectPointerFreeMostlyDead(b *testing.B) { benchmarkCollectObjects(b, 50, true) }

// BenchmarkFind resolves interior addresses into a mixed heap, the lookup
// marking does for every word that points into the heap.
func BenchmarkFind(b *testing.B) {
	bh := newBenchHeap(b, 1<<14)
	random := rand.New(rand.NewSource(3))
	probes := make([]uintptr, 4096)
	for index := range probes {
		size := uintptr(16 + random.Intn(3000))
		address := bh.allocObject(size, false)
		probes[index] = address + uintptr(random.Intn(int(size)))
	}
	b.ResetTimer()
	found := 0
	for index := 0; index < b.N; index++ {
		if _, _, _, ok := bh.find(probes[index%len(probes)]); ok {
			found++
		}
	}
	if found != b.N {
		b.Fatal("a probe missed its allocation")
	}
}
