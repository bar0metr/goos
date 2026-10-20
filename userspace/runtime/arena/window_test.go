package arena

import (
	"sync"
	"testing"
)

// empty reports whether n counted nothing.
func (n *threadCounts) empty() bool {
	if n.allocs|n.allocBytes|n.frees|n.freeBytes|n.cleared|n.localFrees|n.remoteFrees|n.refills|n.drained != 0 {
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
// at one pause point. Outside a window it takes the thread's slabs back and
// folds its counts, as collectThreaded does; inside one it raises gcFlag and
// backs off, and finishes once the thread has left the window.
type windowCollector struct {
	th      *testHeap
	at      int
	seen    int
	pending bool
	held    int
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
	c.th.returnCache(&a.cache, &a.counts)
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
// path's window in turn, for an allocation, a free into its own slab, and
// a free into another thread's slab.
func TestWindowHoldAtEveryPoint(t *testing.T) {
	defer func() { windowPause = nil; gcFlag.Store(0) }()
	cases := []struct {
		name string
		run  func(th *testHeap, a, other *threadAlloc, target uintptr) bool
	}{
		{"alloc", func(th *testHeap, a, _ *threadAlloc, _ uintptr) bool {
			return a.allocFast(&th.heap, 0, 16, false) != 0
		}},
		{"own free", func(th *testHeap, a, _ *threadAlloc, target uintptr) bool {
			return a.freeFast(&th.heap, target, 1)
		}},
		{"remote free", func(th *testHeap, _, other *threadAlloc, target uintptr) bool {
			return other.freeFast(&th.heap, target, 2)
		}},
	}
	for _, c := range cases {
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
			a.cache, a.counts = owner.cache, owner.counts
			a.fold(&th.heap)
			collector := &windowCollector{th: th, at: point}
			windowPause = collector.pause
			c.run(th, a, other, target)
			windowPause = nil
			if point == 0 {
				points = collector.seen
				if points == 0 {
					t.Fatalf("%s: no window hooks executed", c.name)
				}
			} else if collector.seen != points || collector.held == 0 && !collector.pending {
				t.Fatalf("%s: point %d was not held (hooks=%d, want %d)", c.name, point, collector.seen, points)
			}
			// The collector holds the thread just after it ran too.
			checkPublished(t, c.name, point, a)
			checkPublished(t, c.name, point, other)
			collector.finish(t, a)
			collector.finish(t, other)
			for _, thread := range []*threadAlloc{a, other} {
				th.returnCache(&thread.cache, &thread.counts)
				thread.fold(&th.heap)
			}
			th.foldStrayFrees()
			th.verify()
			th.collect(roots{first, target})
			if th.liveObjects > 2 {
				t.Fatalf("%s, held at point %d: %d objects live", c.name, point, th.liveObjects)
			}
		}
	}
}
