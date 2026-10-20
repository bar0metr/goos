package arena

import (
	"math/bits"
	"sync/atomic"
	"unsafe"
)

// Thread-owned slabs (ADR 0203). Once a process runs several threads, each
// thread owns at most one slab per class. Only the owner allocates from it
// and frees into it, without the heap lock; its page record names the owner.
// A thread that frees a slot of a slab another thread owns sets the slot's
// bit in the slab's mark bitmap, which no collection uses until the
// collection takes the slab back, and the owner folds those bits into its
// allocation bitmap when the slab runs out. A second free of one slot finds
// its bit already set.
//
// Ownership changes only under the heap lock. A thread that frees remotely
// reads the owner without it, so its bit can land after the owner gave the
// slab up: the slab then keeps the bit until the next owner folds it or a
// collection does. A lost or late bit is never unsafe: the slot stays
// allocated, and a collection frees it once nothing points to it.
//
// The functions here run without the heap lock only where they say so.

// threadCache is the slabs a thread owns: page plus one per class, zero for
// none.
type threadCache struct {
	slabs [classCount]uint32
}

// threadCounts is what a thread counted without the heap lock. foldCounts
// adds it to the heap's counters.
type threadCounts struct {
	allocs, allocBytes, frees, freeBytes, cleared uint64
	localFrees, remoteFrees, refills, drained     uint64
	sizeCounts, sizeBytes                         [threadSizeBuckets]uint64
}

// cacheAlloc takes a slot from the slab c owns for class, or returns zero
// when c owns none or it is full. Only c's thread calls it, and it needs no
// lock: no other thread allocates from the slab or changes its bitmap.
func (h *heap) cacheAlloc(c *threadCache, n *threadCounts, class uint8, pointerFree bool) uintptr {
	page := c.slabs[class]
	if page == 0 {
		return 0
	}
	page--
	if uint32(h.meta[page].used) == uint32(h.classSlots[class]) {
		return 0
	}
	address, cleared := h.takeSlot(page, pointerFree)
	size := uint64(classSizes[class])
	n.allocs++
	n.allocBytes += size
	n.cleared += uint64(cleared)
	return address
}

// ownedSlot resolves address to its slot in a slab some thread owns. It
// returns that thread's owner value, zero when the page is not an owned
// slab, and ok false when address is not the start of a slot.
func (h *heap) ownedSlot(address uintptr) (page, slot, owner uint32, ok bool) {
	page = uint32((address - h.base) >> pageShift)
	m := &h.meta[page]
	owner = atomic.LoadUint32(&m.owner)
	if owner == 0 || m.kind != pageSlab {
		return page, 0, 0, true
	}
	size := uint32(classSizes[m.class])
	offset := uint32(address & (pageBytes - 1))
	slot = offset / size
	if offset != slot*size || offset+size > uint32(pageBytes) {
		return page, slot, owner, false
	}
	return page, slot, owner, true
}

// cacheFree frees a slot of a slab the calling thread owns, without the heap
// lock. It reports false for a slot that is not allocated.
func (h *heap) cacheFree(n *threadCounts, page, slot uint32) bool {
	m := &h.meta[page]
	bit := uint64(1) << (slot % 64)
	if m.alloc[slot/64]&bit == 0 || atomic.LoadUint64(&m.mark[slot/64])&bit != 0 {
		return false
	}
	m.alloc[slot/64] &^= bit
	m.head = min(m.head, slot/64)
	m.used--
	n.frees++
	n.freeBytes += uint64(classSizes[m.class])
	n.localFrees++
	return true
}

// remoteFree records the free of a slot in a slab another thread owns, with
// no lock. It reports false when the slot was already freed this way.
func (h *heap) remoteFree(n *threadCounts, page, slot uint32) bool {
	word := &h.meta[page].mark[slot/64]
	bit := uint64(1) << (slot % 64)
	for {
		old := atomic.LoadUint64(word)
		if old&bit != 0 {
			return false
		}
		if atomic.CompareAndSwapUint64(word, old, old|bit) {
			n.remoteFrees++
			return true
		}
	}
}

// drainRemote folds the remote frees of a slab into its allocation bitmap.
// The slab's owner calls it, or a thread holding the heap lock while the
// slab has no owner or its owner is held.
func (h *heap) drainRemote(n *threadCounts, page uint32) {
	m := &h.meta[page]
	drained := uint64(0)
	for word := uint32(0); word < slotWords; word++ {
		if atomic.LoadUint64(&m.mark[word]) == 0 {
			continue
		}
		freed := atomic.SwapUint64(&m.mark[word], 0)
		if freed&^m.alloc[word] != 0 {
			panic("arena: remote free of a free slot")
		}
		m.alloc[word] &^= freed
		m.head = min(m.head, word)
		drained += uint64(bits.OnesCount64(freed))
	}
	if drained == 0 {
		return
	}
	m.used -= uint16(drained)
	n.frees += drained
	n.freeBytes += drained * uint64(classSizes[m.class])
	n.drained += drained
}

// refill makes c own a slab of class with a free slot: the one it owns once
// the remote frees are folded in, or else a listed or new slab. owner is the
// thread's slot plus one. It reports false when the heap has no page. The
// caller holds the heap lock.
func (h *heap) refill(c *threadCache, n *threadCounts, class uint8, owner uint32) bool {
	n.refills++
	slots := uint32(h.classSlots[class])
	if page := c.slabs[class]; page != 0 {
		page--
		h.drainRemote(n, page)
		if uint32(h.meta[page].used) < slots {
			return true
		}
		h.disown(n, page)
		c.slabs[class] = 0
	}
	page := h.partial[class]
	if page != noPage {
		h.removePartial(page)
		// A free that read a stale owner may have left bits.
		h.drainRemote(n, page)
	} else if page = h.newSlab(class); page == noPage {
		return false
	}
	atomic.StoreUint32(&h.meta[page].owner, owner)
	c.slabs[class] = page + 1
	return true
}

// disown gives up a thread's slab: it folds the remote frees, clears the
// owner, and lists the slab, or gives its page back when it is empty. The
// caller holds the heap lock, and the owner is the caller or is held.
func (h *heap) disown(n *threadCounts, page uint32) {
	m := &h.meta[page]
	atomic.StoreUint32(&m.owner, 0)
	h.drainRemote(n, page)
	switch {
	case m.used == 0:
		h.givePages(page, 1)
	case uint32(m.used) < uint32(h.classSlots[m.class]):
		h.pushPartial(page)
	}
}

// returnCache gives every slab c owns back to the heap. The caller holds the
// heap lock, and c's thread is the caller, is held, or has ended.
func (h *heap) returnCache(c *threadCache, n *threadCounts) {
	for class := range c.slabs {
		if page := c.slabs[class]; page != 0 {
			h.disown(n, page-1)
			c.slabs[class] = 0
			h.diag.returnedSlabs++
		}
	}
}

// foldCounts adds a thread's counts to the heap's and clears them. The
// caller holds the heap lock and is the counting thread, or that thread is
// held or has ended.
func (h *heap) foldCounts(n *threadCounts) {
	// Another thread may have freed what this one allocated before this
	// one folds its allocations; the sums are exact once every thread
	// folded, and wrap in between.
	h.liveBytes += n.allocBytes - n.freeBytes
	h.liveObjects += n.allocs - n.frees
	h.totalAlloc += n.allocBytes
	h.mallocs += n.allocs
	h.frees += n.frees
	if int64(h.liveBytes) >= 0 && h.liveBytes > h.peakLive {
		h.peakLive = h.liveBytes
	}
	c := &h.diag
	c.clearedBytes += n.cleared
	c.localAllocs += n.allocs
	c.localFrees += n.localFrees
	c.remoteFrees += n.remoteFrees
	c.refills += n.refills
	c.drained += n.drained
	for bucket := range n.sizeCounts {
		c.sizeCounts[bucket] += n.sizeCounts[bucket]
		c.sizeBytes[bucket] += n.sizeBytes[bucket]
	}
	// Cleared in place: TinyGo puts a composite literal this large on the
	// heap.
	clearMemory(uintptr(unsafe.Pointer(n)), unsafe.Sizeof(*n))
}

// foldStrayFrees folds the remote frees left in slabs no thread owns. A
// collection calls it after taking every slab back, before it marks: from
// then on the mark bitmaps hold marks.
func (h *heap) foldStrayFrees() {
	// The counts are the heap's own: TinyGo would put a local this large,
	// whose address is passed on, on the heap.
	n := &h.stray
	for page := uint32(0); page < h.frontier; page++ {
		m := &h.meta[page]
		if m.kind != pageSlab || m.mark == [slotWords]uint64{} {
			continue
		}
		slots := uint32(h.classSlots[m.class])
		wasFull := uint32(m.used) == slots
		h.drainRemote(n, page)
		switch {
		case m.used == 0:
			if !wasFull {
				h.removePartial(page)
			}
			h.givePages(page, 1)
		case wasFull:
			h.pushPartial(page)
			h.diag.fullToPartial++
		}
	}
	h.foldCounts(n)
}

// clearBit clears bit in word, which other threads may set at the same time.
func clearBit(word *uint64, bit uint64) {
	for {
		old := atomic.LoadUint64(word)
		if old&bit == 0 || atomic.CompareAndSwapUint64(word, old, old&^bit) {
			return
		}
	}
}
