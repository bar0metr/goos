package arena

import "sync/atomic"

// The fast-path window (ADR 0203). A thread changes its owned slabs and its
// counts without the heap lock only between storing its fast flag and
// clearing it. A collection raises gcFlag, holds every other thread, and
// collects only when none was held inside a window; otherwise it lets them
// go and tries again. So a held thread is either outside every window, and
// its slabs and counts are consistent, or the collection does not touch
// them. Every change a window makes, the unfolded flag included, is inside
// it: a thread held just before its window finds the flag it sets still set
// afterwards.

// gcFlag is raised while a collection begins and runs: a thread that sees it
// inside a window takes the locked path instead.
var gcFlag atomic.Uint32

// threadAlloc is the part of a thread's state the fast path uses.
type threadAlloc struct {
	// fast is set while the thread is inside a window.
	fast atomic.Uint32
	// cache is the thread's own slabs (cache.go).
	cache threadCache
	// counts is what the thread counted on its own. Only the thread
	// changes it, inside a window or under the heap lock; it reaches the
	// heap's counters only through fold, and nothing else reads it.
	// unfolded is set while counts holds anything fold has not folded.
	counts   threadCounts
	unfolded atomic.Uint32
}

// windowPause, in host tests, runs at every point where a collector could
// hold the thread: just before and after a window and inside it.
var windowPause func(a *threadAlloc)

func (a *threadAlloc) pause() {
	if windowTesting && windowPause != nil {
		windowPause(a)
	}
}

// noteUnfolded marks that a's counts hold something to fold. The store
// happens once per fold.
func (a *threadAlloc) noteUnfolded() {
	if a.unfolded.Load() == 0 {
		a.unfolded.Store(1)
	}
}

// fold publishes a's counts into h's. The caller holds the heap lock and is
// a's thread, or a's thread is held outside a window or has ended.
func (a *threadAlloc) fold(h *heap) {
	h.foldCounts(&a.counts)
	a.unfolded.Store(0)
}

// allocFast takes a slot of class from a's own slab inside a window, or
// returns zero when the slab is full, a owns none, or a collection is
// starting. The request is counted in the size histogram either way.
func (a *threadAlloc) allocFast(h *heap, class uint8, size uintptr, noPointers bool) uintptr {
	a.pause()
	a.fast.Store(1)
	a.pause()
	a.noteUnfolded()
	if sizeProfile {
		bucket := (size - 1) >> 4
		a.counts.sizeCounts[bucket]++
		a.counts.sizeBytes[bucket] += uint64(size)
	}
	a.pause()
	address := uintptr(0)
	if gcFlag.Load() == 0 {
		address = h.cacheAlloc(&a.cache, &a.counts, class, noPointers)
	}
	a.pause()
	a.fast.Store(0)
	a.pause()
	return address
}

// freeFast frees address inside a window if it is a slot of a slab some
// thread owns, and reports whether it did. owner is the caller's slot plus
// one.
func (a *threadAlloc) freeFast(h *heap, address uintptr, owner uint32) bool {
	a.pause()
	a.fast.Store(1)
	a.pause()
	a.noteUnfolded()
	a.pause()
	freed := gcFlag.Load() == 0 && a.freeOwned(h, address, owner)
	a.pause()
	a.fast.Store(0)
	a.pause()
	return freed
}

// freeOwned frees address if it is a slot of an owned slab: into the slab
// when the caller owns it, into its remote-free bits otherwise. It runs
// inside a window or under the heap lock.
func (a *threadAlloc) freeOwned(h *heap, address uintptr, owner uint32) bool {
	page, slot, slabOwner, ok := h.ownedSlot(address)
	if slabOwner == 0 {
		return false
	}
	if ok && slabOwner == owner {
		ok = h.cacheFree(&a.counts, page, slot)
	} else if ok {
		ok = h.remoteFree(&a.counts, page, slot)
	}
	if !ok {
		a.fast.Store(0)
		panic("arena: double free or free of a non-allocation")
	}
	return true
}
