package arena

import (
	"sync"
	"testing"
)

// empty reports whether n counted nothing.
func (n *threadCounts) empty() bool {
	if n.allocs|n.allocBytes|n.frees|n.freeBytes|n.cleared|n.localFrees|n.remoteFrees|n.refills|n.drained|n.misses|n.batches|n.claimed != 0 {
		return false
	}
	for bucket := range n.sizeCounts {
		if n.sizeCounts[bucket]|n.sizeBytes[bucket] != 0 {
			return false
		}
	}
	return true
}

// windowCollector stands in for a collector that tries to hold the thread
// at one pause point. Outside a window it takes the thread's slabs back, or
// with keep folds their remote frees and leaves them owned, and folds its
// counts, as prepareThreads does; inside one it raises gcFlag and backs
// off, and finishes once the thread has left the window.
type windowCollector struct {
	th      *testHeap
	at      int
	seen    int
	pending bool
	held    int
	keep    bool
}

func (c *windowCollector) pause(a *threadAlloc) {
	c.seen++
	if c.seen != c.at {
		return
	}
	if a.fast.Load() != 0 {
		gcFlag.Store(1)
		c.pending = true
		return
	}
	c.collect(a)
}

func (c *windowCollector) collect(a *threadAlloc) {
	if c.keep {
		c.th.drainCache(&a.cache, &a.counts)
	} else {
		c.th.returnCache(&a.cache, &a.counts)
	}
	a.fold(&c.th.heap)
	c.held++
}

// finish runs the collection a thread held off, once it left the window.
func (c *windowCollector) finish(t *testing.T, a *threadAlloc) {
	t.Helper()
	if c.pending {
		if a.fast.Load() != 0 {
			t.Fatal("the thread is still inside its window")
		}
		c.collect(a)
		gcFlag.Store(0)
		c.pending = false
	}
}

// checkPublished fails when a's counts hold something its unfolded flag
// does not show: an idle collection would miss it.
func checkPublished(t *testing.T, name string, point int, a *threadAlloc) {
	t.Helper()
	if !a.counts.empty() && a.unfolded.Load() == 0 {
		t.Fatalf("%s, held at point %d: counts changed with the unfolded flag clear", name, point)
	}
}

// TestWindowHoldAtEveryPoint holds the thread at each point of the fast
// path's window in turn, for an allocation, an allocation that moves to
// another of the thread's slabs, a free into its own slab, and a free into
// another thread's slab, with a collector that takes the slabs back and one
// that keeps them.
func TestWindowHoldAtEveryPoint(t *testing.T) {
	defer func() { windowPause = nil; gcFlag.Store(0) }()
	cases := []struct {
		name string
		run  func(th *testHeap, a, other *threadAlloc, target uintptr) bool
	}{
		{"alloc", func(th *testHeap, a, _ *threadAlloc, _ uintptr) bool {
			return a.allocFast(&th.heap, 0, 16, false) != 0
		}},
		{"alloc past a full slab", func(th *testHeap, a, _ *threadAlloc, _ uintptr) bool {
			return a.allocFast(&th.heap, bigClass, 2048, false) != 0
		}},
		{"own free", func(th *testHeap, a, _ *threadAlloc, target uintptr) bool {
			return a.freeFast(&th.heap, target, 1)
		}},
		{"remote free", func(th *testHeap, _, other *threadAlloc, target uintptr) bool {
			return other.freeFast(&th.heap, target, 2)
		}},
	}
	for _, keep := range []bool{false, true} {
		for _, c := range cases {
			name := c.name
			if keep {
				name += ", slabs kept"
			}
			holdEveryPoint(t, name, keep, c.run)
		}
	}
}

func holdEveryPoint(t *testing.T, name string, keep bool, run func(th *testHeap, a, other *threadAlloc, target uintptr) bool) {
	t.Helper()
	points := 0
	// Point zero counts the hooks without requesting a hold. The
	// remaining runs must hit each hook found by that pass.
	for point := 0; point == 0 || point <= points; point++ {
		th := newTestHeap(t, 32, 64)
		var lock sync.Mutex
		a, other := &threadAlloc{}, &threadAlloc{}
		owner := &cacheThread{owner: 1}
		first := owner.alloc(t, th, &lock, 16)
		target := owner.alloc(t, th, &lock, 16)
		// Two 2048-byte slots fill the first of four slabs; the hint
		// stays on it until an allocation moves on.
		big := roots{owner.alloc(t, th, &lock, 2048), owner.alloc(t, th, &lock, 2048)}
		a.cache, a.counts = owner.cache, owner.counts
		a.fold(&th.heap)
		collector := &windowCollector{th: th, at: point, keep: keep}
		windowPause = collector.pause
		run(th, a, other, target)
		windowPause = nil
		if point == 0 {
			points = collector.seen
			if points == 0 {
				t.Fatalf("%s: no window hooks executed", name)
			}
		} else if collector.seen != points || collector.held == 0 && !collector.pending {
			t.Fatalf("%s: point %d was not held (hooks=%d, want %d)", name, point, collector.seen, points)
		}
		// The collector holds the thread just after it ran too.
		checkPublished(t, name, point, a)
		checkPublished(t, name, point, other)
		collector.finish(t, a)
		collector.finish(t, other)
		if keep {
			th.checkCache(&a.cache, 1)
		}
		for _, thread := range []*threadAlloc{a, other} {
			th.returnCache(&thread.cache, &thread.counts)
			thread.fold(&th.heap)
		}
		th.foldStrayFrees()
		th.verify()
		th.collect(roots{first, target, big[0], big[1]})
		if th.liveObjects > 4 {
			t.Fatalf("%s, held at point %d: %d objects live", name, point, th.liveObjects)
		}
	}
}
